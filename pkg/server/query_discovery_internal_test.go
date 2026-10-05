package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

// The discovery data of the query profile: what /.well-known/openvaultdb and the
// database metadata say about relational queries must be what the relational
// handler does. The helpers of this file all start with discovery so they cannot
// clash with the others of the package.

func discoveryGet(t *testing.T, host *httptest.Server, path string) map[string]any {
	t.Helper()
	resp := relFakeDo(t, host, http.MethodGet, path, "", "", nil)
	if resp.status != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", path, resp.status, resp.raw)
	}
	return resp.body
}

func discoveryObject(t *testing.T, parent map[string]any, name string) map[string]any {
	t.Helper()
	value, ok := parent[name].(map[string]any)
	if !ok {
		t.Fatalf("no object %q in %v", name, parent)
	}
	return value
}

// discoveryDatabase returns the entry of the well-known database list with id.
func discoveryDatabase(t *testing.T, doc map[string]any, id string) map[string]any {
	t.Helper()
	list, _ := doc["databases"].([]any)
	for _, entry := range list {
		if row, _ := entry.(map[string]any); row["id"] == id {
			return row
		}
	}
	t.Fatalf("database %q is not listed: %v", id, doc["databases"])
	return nil
}

// discoveryAccepted sends a relational document that reads one collection of the
// database to /v1/dtql and reports whether the handler took it past every check of
// the database (the executor runs).
func discoveryAccepted(t *testing.T, host *httptest.Server, id string) bool {
	t.Helper()
	resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", "from: {database: "+id+", name: orders}\n", nil)
	switch resp.status {
	case http.StatusOK:
		return true
	case http.StatusUnprocessableEntity, http.StatusNotImplemented:
		return false
	}
	t.Fatalf("database %q: status %d is neither an answer nor a refusal of the database: %s", id, resp.status, resp.raw)
	return false
}

func TestWellKnownQueryBlockStatesTheProfileAsData(t *testing.T) {
	limits := QueryLimits{
		Timeout:        7 * time.Second,
		InMemory:       3,
		Database:       5,
		QueueWait:      250 * time.Millisecond,
		MaxSourceRows:  1234,
		MaxSourceBytes: 5678,
		JoinEngines:    []string{"sqlite", core.EngineInGitDBGitHub, "firestore"},
	}
	_, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts(), WithQueryLimits(limits))
	query := discoveryObject(t, discoveryGet(t, host, "/.well-known/openvaultdb"), "query")
	if query["endpoint"] != "/v1/dtql" || query["format"] != "dtql-yaml+json" {
		t.Fatalf("endpoint %v, format %v", query["endpoint"], query["format"])
	}
	features := discoveryObject(t, query, "features")
	want := map[string]any{
		"joins":              []any{"inner", "left"},
		"groupBy":            true,
		"having":             true,
		"aggregates":         []any{"count", "sum", "avg", "min", "max", "first", "last"},
		"subqueries":         true,
		"crossDatabase":      true,
		"externalSources":    false,
		"windowFunctions":    false,
		"protectedDatabases": false,
		"fieldNames":         "plain",
	}
	if !reflect.DeepEqual(features, want) {
		t.Fatalf("features = %v, want %v", features, want)
	}
	wantLimits := map[string]any{
		"timeoutMs":          float64(7000),
		"queueWaitMs":        float64(250),
		"maxSourceRows":      float64(1234),
		"maxSourceBytes":     float64(5678),
		"maxResultRows":      float64(joinexec.MaxResultRows),
		"maxResultBytes":     float64(joinexec.MaxResultBytes),
		"concurrentInMemory": float64(3),
		"concurrentDatabase": float64(5),
	}
	if got := discoveryObject(t, query, "limits"); !reflect.DeepEqual(got, wantLimits) {
		t.Fatalf("limits = %v, want %v", got, wantLimits)
	}
	// The list is the one the handler applies: without the GitHub engine.
	if got := query["joinEngines"]; !reflect.DeepEqual(got, []any{"sqlite", "firestore"}) {
		t.Fatalf("joinEngines = %v", got)
	}
}

func TestWellKnownQueryBlockFollowsTheServerDefaults(t *testing.T) {
	_, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())
	query := discoveryObject(t, discoveryGet(t, host, "/.well-known/openvaultdb"), "query")
	defaults := DefaultQueryLimits()
	limits := discoveryObject(t, query, "limits")
	if limits["timeoutMs"] != float64(defaults.Timeout.Milliseconds()) || limits["maxSourceRows"] != float64(defaults.MaxSourceRows) ||
		limits["maxSourceBytes"] != float64(defaults.MaxSourceBytes) || limits["concurrentInMemory"] != float64(defaults.InMemory) {
		t.Fatalf("limits = %v, defaults %+v", limits, defaults)
	}
	if got := query["joinEngines"]; !reflect.DeepEqual(got, []any{"sqlite", "ingitdb"}) {
		t.Fatalf("joinEngines = %v", got)
	}
}

// A negative queue wait means a query that finds the gate full is refused at once,
// which discovery reports as no wait.
func TestWellKnownQueryBlockReportsAnImmediateRefusalAsNoQueueWait(t *testing.T) {
	_, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts(), WithQueryLimits(QueryLimits{QueueWait: -1}))
	limits := discoveryObject(t, discoveryObject(t, discoveryGet(t, host, "/.well-known/openvaultdb"), "query"), "limits")
	if limits["queueWaitMs"] != float64(0) {
		t.Fatalf("queueWaitMs = %v", limits["queueWaitMs"])
	}
}

// With auth on, the document lists no database, and still states the profile: it
// holds nothing that belongs to a database.
func TestWellKnownQueryBlockIsPresentWithAuthOn(t *testing.T) {
	_, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts(), WithAuth(&auth.Config{OwnerToken: "owner"}))
	doc := discoveryGet(t, host, "/.well-known/openvaultdb")
	if _, listed := doc["databases"]; listed {
		t.Fatalf("an authenticated server lists databases: %v", doc["databases"])
	}
	query := discoveryObject(t, doc, "query")
	if query["endpoint"] != "/v1/dtql" || !reflect.DeepEqual(query["joinEngines"], []any{"sqlite", "ingitdb"}) {
		t.Fatalf("query = %v", query)
	}
	if features := discoveryObject(t, query, "features"); features["crossDatabase"] != true || features["protectedDatabases"] != false {
		t.Fatalf("features = %v", features)
	}
}

// Every engine row, under every operator list, and a mount with access policies:
// the value a database advertises is the answer a real relational request to that
// database gets.
func TestAdvertisedJoinsEqualWhatARelationalRequestGets(t *testing.T) {
	protected, _ := relProtectedMount(t)
	mounts := []*core.Database{
		relFakeMount("lite", "sqlite", ""),
		relFakeMount("local", "ingitdb", ""),
		relFakeGitHubMount("hosted"),
		relFakeMount("docs", "firestore", ""),
		relFakeMount("pg", "postgres", ""),
		relFakeMount("my", "mysql", ""),
		relFakeMount("other", "oracle", ""),
		protected,
	}
	wantByList := map[string]map[string]bool{
		"default":                  {"lite": true, "local": true},
		"sqlite only":              {"lite": true},
		"firestore added":          {"lite": true, "local": true, "docs": true},
		"the GitHub engine listed": {"lite": true, "local": true},
		"not queryable listed":     {"lite": true, "local": true, "docs": false},
	}
	lists := map[string][]string{
		"default":                  nil,
		"sqlite only":              {"sqlite"},
		"firestore added":          {"sqlite", "ingitdb", "firestore"},
		"the GitHub engine listed": {"sqlite", "ingitdb", core.EngineInGitDBGitHub},
		"not queryable listed":     {"sqlite", "ingitdb", "postgres", "mysql", "oracle"},
	}
	for name, engines := range lists {
		t.Run(name, func(t *testing.T) {
			var opts []Option
			if engines != nil {
				opts = append(opts, WithQueryLimits(QueryLimits{JoinEngines: engines}))
			}
			_, host := relFakeServer(t, &relFakeExecutor{}, mounts, opts...)
			doc := discoveryGet(t, host, "/.well-known/openvaultdb")
			for _, db := range mounts {
				entry := discoveryDatabase(t, doc, db.ID())
				joins, aggregation := entry["joins"], entry["aggregation"]
				accepted := discoveryAccepted(t, host, db.ID())
				if joins != accepted || aggregation != accepted {
					t.Errorf("database %q (%s): advertised joins %v and aggregation %v, a relational request is accepted: %v", db.ID(), db.Engine(), joins, aggregation, accepted)
				}
				if want, ok := wantByList[name][db.ID()]; ok && accepted != want {
					t.Errorf("database %q: accepted %v, want %v", db.ID(), accepted, want)
				}
			}
			for _, id := range []string{"hosted", "pg", "my", "other", "crm"} {
				if entry := discoveryDatabase(t, doc, id); entry["joins"] != false {
					t.Errorf("database %q advertises joins %v", id, entry["joins"])
				}
			}
		})
	}
}

// The metadata of one database carries the same two flags, read the same way.
func TestDatabaseMetadataAdvertisesJoinsAndAggregation(t *testing.T) {
	mounts := []*core.Database{
		relFakeMount("lite", "sqlite", ""),
		relFakeMount("docs", "firestore", ""),
		relFakeMount("pg", "postgres", ""),
		relFakeGitHubMount("hosted"),
	}
	_, host := relFakeServer(t, &relFakeExecutor{}, mounts)
	for _, db := range mounts {
		meta := discoveryGet(t, host, "/v1/databases/"+db.ID())
		accepted := discoveryAccepted(t, host, db.ID())
		if meta["joins"] != accepted || meta["aggregation"] != accepted {
			t.Errorf("database %q (%s): metadata joins %v, aggregation %v, a relational request is accepted: %v", db.ID(), db.Engine(), meta["joins"], meta["aggregation"], accepted)
		}
	}
	if meta := discoveryGet(t, host, "/v1/databases/lite"); meta["joins"] != true {
		t.Fatalf("sqlite metadata = %v", meta)
	}
}

// A database with access policies is not described by its metadata: the route
// refuses it, so the flags cannot be read from there, and the well-known list says
// false for it.
func TestProtectedDatabaseAdvertisesNoJoins(t *testing.T) {
	protected, _ := relProtectedMount(t)
	_, host := relFakeServer(t, &relFakeExecutor{}, []*core.Database{protected})
	entry := discoveryDatabase(t, discoveryGet(t, host, "/.well-known/openvaultdb"), "crm")
	if entry["joins"] != false || entry["aggregation"] != false || entry["capabilities"].(map[string]any)["query"] != true {
		t.Fatalf("entry = %v", entry)
	}
	if resp := relFakeDo(t, host, http.MethodGet, "/v1/databases/crm", "", "", nil); resp.status != http.StatusUnprocessableEntity {
		t.Fatalf("metadata status %d: %s", resp.status, resp.raw)
	}
}

// The aggregate names the profile advertises are the ones the classifier takes,
// and one it does not list is refused.
func TestAdvertisedAggregatesAreTheOnesTheClassifierTakes(t *testing.T) {
	var query struct {
		Features struct {
			Aggregates []string `json:"aggregates"`
		} `json:"features"`
	}
	raw, err := json.Marshal(New("test", nil).queryProfile())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &query); err != nil {
		t.Fatal(err)
	}
	if len(query.Features.Aggregates) != 7 {
		t.Fatalf("aggregates = %v", query.Features.Aggregates)
	}
	_, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())
	doc := func(function string) string {
		return "from: {database: alpha, name: orders, alias: o}\ncolumns:\n  - {aggregate: {function: " + function + ", args: [{field: total, source: o}]}, as: x}\n"
	}
	for _, function := range query.Features.Aggregates {
		resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", doc(function), nil)
		if resp.status != http.StatusOK {
			t.Errorf("%s: status %d: %s", function, resp.status, resp.raw)
		}
	}
	if resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", doc("stddev"), nil); resp.status != http.StatusBadRequest || resp.code() != "invalid_dtql" {
		t.Fatalf("an unlisted aggregate: status %d: %s", resp.status, resp.raw)
	}
	if !strings.Contains(strings.Join(query.Features.Aggregates, ","), "count") {
		t.Fatal("count is not advertised")
	}
}
