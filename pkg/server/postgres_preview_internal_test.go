package server

import (
	"net/http"
	"os"
	"reflect"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// The preview of structured queries on PostgreSQL mounts, as the server says it
// and enforces it. The helpers of this file all start with previewPG so they
// cannot clash with the others of the package.

// previewPGSwitch sets the preview switch for the test, or removes it, and
// restores the environment afterwards. Mounts read it when they open, so call it
// before previewPGMounts.
func previewPGSwitch(t *testing.T, value string, set bool) {
	t.Helper()
	t.Setenv(core.PreviewPostgresQueriesEnv, value)
	if !set {
		if err := os.Unsetenv(core.PreviewPostgresQueriesEnv); err != nil {
			t.Fatal(err)
		}
	}
}

// previewPGMounts are a SQLite mount and a PostgreSQL mount, both fake-backed.
func previewPGMounts() []*core.Database {
	return []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeMount("pg", "postgres", "")}
}

func previewPGEntry(t *testing.T, id string, doc map[string]any) (capabilities map[string]any, joinEngines []any) {
	t.Helper()
	capabilities = discoveryObject(t, discoveryDatabase(t, doc, id), "capabilities")
	joinEngines, _ = discoveryObject(t, doc, "query")["joinEngines"].([]any)
	return capabilities, joinEngines
}

// TestDiscoveryFollowsThePreviewSwitchOfAPostgresMount: the server never advertises
// what it refuses. A PostgreSQL mount is advertised as queryable (query and dtql)
// only when it read the switch on, as joining and aggregating only when, besides,
// the operator's list of join engines holds postgres, and the list of join engines
// of the query block holds postgres exactly then. Without the switch the
// advertisement is the one it was: no query, no dtql, no joins, and no postgres in
// the list whatever the operator's list says.
func TestDiscoveryFollowsThePreviewSwitchOfAPostgresMount(t *testing.T) {
	for _, c := range []struct {
		name        string
		value       string
		set         bool
		engines     []string // the operator's list of join engines
		queryable   bool
		joins       bool
		joinEngines []any
	}{
		{"switch off, postgres listed", "", false, []string{"sqlite", "postgres"}, false, false, []any{"sqlite"}},
		{"switch 0, postgres listed", "0", true, []string{"sqlite", "postgres"}, false, false, []any{"sqlite"}},
		{"switch on, postgres not listed", "1", true, []string{"sqlite", "ingitdb"}, true, false, []any{"sqlite", "ingitdb"}},
		{"switch on, postgres listed", "1", true, []string{"sqlite", "postgres"}, true, true, []any{"sqlite", "postgres"}},
		{"switch on, mysql listed", "1", true, []string{"sqlite", "mysql"}, true, false, []any{"sqlite"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			previewPGSwitch(t, c.value, c.set)
			_, host := relFakeServer(t, &relFakeExecutor{}, previewPGMounts(), WithQueryLimits(QueryLimits{JoinEngines: c.engines}))
			doc := discoveryGet(t, host, "/.well-known/openvaultdb")
			capabilities, joinEngines := previewPGEntry(t, "pg", doc)
			if capabilities["query"] != c.queryable || capabilities["dtql"] != c.queryable {
				t.Errorf("query %v, dtql %v, want %v", capabilities["query"], capabilities["dtql"], c.queryable)
			}
			if capabilities["joins"] != c.joins || capabilities["aggregation"] != c.joins {
				t.Errorf("joins %v, aggregation %v, want %v", capabilities["joins"], capabilities["aggregation"], c.joins)
			}
			if !reflect.DeepEqual(joinEngines, c.joinEngines) {
				t.Errorf("joinEngines = %v, want %v", joinEngines, c.joinEngines)
			}
			// The metadata of the database says what the list says.
			meta := discoveryGet(t, host, "/v1/databases/pg")
			metaCapabilities := discoveryObject(t, meta, "capabilities")
			if metaCapabilities["query"] != c.queryable || metaCapabilities["joins"] != c.joins {
				t.Errorf("metadata capabilities = %v, want query %v and joins %v", metaCapabilities, c.queryable, c.joins)
			}
			// What is advertised is what the handler does.
			if got := discoveryAccepted(t, host, "pg"); got != c.joins {
				t.Errorf("a relational document naming the database is taken: %v, advertised %v", got, c.joins)
			}
			// SQLite is advertised as it always was.
			alpha, _ := previewPGEntry(t, "alpha", doc)
			if alpha["query"] != true || alpha["joins"] != true {
				t.Errorf("sqlite capabilities = %v", alpha)
			}
		})
	}
}

// TestTheDatabaseRouteIsOfferedToPostgresOnlyWhenAMountIsCleared: the engines that
// run a whole document themselves are SQLite, as they always were, and PostgreSQL
// when a mounted PostgreSQL database is cleared for queries by the switch it read.
// Without the switch no option is passed at all, so the executor runs as it did.
func TestTheDatabaseRouteIsOfferedToPostgresOnlyWhenAMountIsCleared(t *testing.T) {
	for _, c := range []struct {
		name  string
		value string
		set   bool
		want  []string
	}{
		{"switch off", "", false, nil},
		{"switch 0", "0", true, nil},
		{"switch on", "1", true, []string{"sqlite", "postgres"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			previewPGSwitch(t, c.value, c.set)
			service, _ := relFakeServer(t, &relFakeExecutor{}, previewPGMounts(), WithQueryLimits(QueryLimits{JoinEngines: []string{"sqlite", "postgres"}}))
			if got := service.nativeEngines(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("nativeEngines = %v, want %v", got, c.want)
			}
		})
	}
	t.Run("switch on, no PostgreSQL mount", func(t *testing.T) {
		previewPGSwitch(t, "1", true)
		service, _ := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())
		if got := service.nativeEngines(); got != nil {
			t.Fatalf("nativeEngines = %v, want none", got)
		}
	})
	t.Run("a mount that read the switch off beside one that read it on", func(t *testing.T) {
		previewPGSwitch(t, "0", true)
		off := relFakeMount("off", "postgres", "")
		previewPGSwitch(t, "1", true)
		on := relFakeMount("on", "postgres", "")
		service, host := relFakeServer(t, &relFakeExecutor{}, []*core.Database{off, on})
		if got := service.nativeEngines(); !reflect.DeepEqual(got, []string{"sqlite", "postgres"}) {
			t.Fatalf("nativeEngines = %v", got)
		}
		// Each mount answers by what it read.
		resp := relFakeDo(t, host, http.MethodPost, "/v1/databases/off/dtql", "", "from: {name: orders}\n", nil)
		if resp.status != http.StatusNotImplemented || resp.code() != "query_unsupported" {
			t.Fatalf("the mount that read the switch off: status %d: %s", resp.status, resp.raw)
		}
	})
}
