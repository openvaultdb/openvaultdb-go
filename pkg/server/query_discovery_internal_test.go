package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
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

// discoveryFlags reads the joins and aggregation flags of a database entry, which
// sit in its capabilities map beside read, query, dtql and write, and fails when
// either is also written at the top level of the entry.
func discoveryFlags(t *testing.T, entry map[string]any) (joins, aggregation any) {
	t.Helper()
	for _, name := range []string{"joins", "aggregation"} {
		if _, stray := entry[name]; stray {
			t.Errorf("%q is written at the top level of %v", name, entry)
		}
	}
	capabilities := discoveryObject(t, entry, "capabilities")
	for _, name := range []string{"read", "query", "dtql", "write", "joins", "aggregation"} {
		if _, ok := capabilities[name].(bool); !ok {
			t.Errorf("capabilities.%s is not a boolean: %v", name, capabilities)
		}
	}
	return capabilities["joins"], capabilities["aggregation"]
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
		"streaming":          true,
		"completionField":    "complete",
		"errorStreamAccept":  queryErrorStreamMediaType,
		"errorField":         "error",
		"errorCompletion":    false,
		"groupBy":            true,
		"having":             true,
		"aggregates":         []any{"count", "sum", "avg", "min", "max"},
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
	bounds := core.RelationalBounds()
	wantLimits := map[string]any{
		"timeoutMs":            float64(7000),
		"maxSourceRows":        float64(1234),
		"maxSourceBytes":       float64(5678),
		"maxResultRows":        float64(joinexec.MaxResultRows),
		"maxRequestBytes":      float64(maxRequestBodyBytes),
		"maxResultBytes":       float64(joinexec.MaxResultBytes),
		"maxSources":           float64(bounds.MaxSources),
		"maxSubqueryDepth":     float64(bounds.MaxSubqueryDepth),
		"maxLimit":             float64(bounds.MaxLimit),
		"maxOffset":            float64(bounds.MaxOffset),
		"maxInMemoryJoinRows":  float64(joinexec.MaxInMemoryJoinRows),
		"maxInMemoryJoinBytes": float64(joinexec.MaxInMemoryJoinBytes),
		"maxGroups":            float64(joinexec.MaxInMemoryGroups),
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
		limits["maxSourceBytes"] != float64(defaults.MaxSourceBytes) {
		t.Fatalf("limits = %v, defaults %+v", limits, defaults)
	}
	if got := query["joinEngines"]; !reflect.DeepEqual(got, []any{"sqlite", "ingitdb"}) {
		t.Fatalf("joinEngines = %v", got)
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
				joins, aggregation := discoveryFlags(t, entry)
				accepted := discoveryAccepted(t, host, db.ID())
				if joins != accepted || aggregation != accepted {
					t.Errorf("database %q (%s): advertised joins %v and aggregation %v, a relational request is accepted: %v", db.ID(), db.Engine(), joins, aggregation, accepted)
				}
				if want, ok := wantByList[name][db.ID()]; ok && accepted != want {
					t.Errorf("database %q: accepted %v, want %v", db.ID(), accepted, want)
				}
			}
			// An engine is advertised in joinEngines exactly when a database on it, that
			// has no access policies, advertises joins.
			listed, _ := discoveryObject(t, doc, "query")["joinEngines"].([]any)
			for _, db := range mounts {
				if db.HasAccessPolicies() {
					continue
				}
				joins, _ := discoveryFlags(t, discoveryDatabase(t, doc, db.ID()))
				if inList := slices.Contains(listed, any(db.Engine())); inList != (joins == true) {
					t.Errorf("engine %q (database %q): in joinEngines %v (%v), advertises joins %v", db.Engine(), db.ID(), inList, listed, joins)
				}
			}
			for _, id := range []string{"hosted", "pg", "my", "other", "crm"} {
				if joins, _ := discoveryFlags(t, discoveryDatabase(t, doc, id)); joins != false {
					t.Errorf("database %q advertises joins %v", id, joins)
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
		joins, aggregation := discoveryFlags(t, meta)
		accepted := discoveryAccepted(t, host, db.ID())
		if joins != accepted || aggregation != accepted {
			t.Errorf("database %q (%s): metadata joins %v, aggregation %v, a relational request is accepted: %v", db.ID(), db.Engine(), joins, aggregation, accepted)
		}
	}
	if joins, _ := discoveryFlags(t, discoveryGet(t, host, "/v1/databases/lite")); joins != true {
		t.Fatalf("sqlite metadata joins = %v", joins)
	}
}

// A database with access policies is not described by its metadata: the route
// refuses it, so the flags cannot be read from there, and the well-known list says
// false for it.
func TestProtectedDatabaseAdvertisesNoJoins(t *testing.T) {
	protected, _ := relProtectedMount(t)
	_, host := relFakeServer(t, &relFakeExecutor{}, []*core.Database{protected})
	entry := discoveryDatabase(t, discoveryGet(t, host, "/.well-known/openvaultdb"), "crm")
	if joins, aggregation := discoveryFlags(t, entry); joins != false || aggregation != false || entry["capabilities"].(map[string]any)["query"] != true {
		t.Fatalf("entry = %v", entry)
	}
	if resp := relFakeDo(t, host, http.MethodGet, "/v1/databases/crm", "", "", nil); resp.status != http.StatusUnprocessableEntity {
		t.Fatalf("metadata status %d: %s", resp.status, resp.raw)
	}
}

// The aggregate names the profile advertises are the ones the classifier takes: the
// list is the one pkg/core exports, every name in it is classified in any letter
// case and answered, and no name outside it (first and last included) is
// classified.
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
	if !reflect.DeepEqual(query.Features.Aggregates, core.AggregateFunctions()) || len(query.Features.Aggregates) != 5 {
		t.Fatalf("advertised aggregates = %v, the classifier takes %v", query.Features.Aggregates, core.AggregateFunctions())
	}
	doc := func(function string) string {
		return "from: {database: alpha, name: orders, alias: o}\ncolumns:\n  - {aggregate: {function: " + function + ", args: [{field: total, source: o}]}, as: x}\n"
	}
	_, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())
	for _, function := range query.Features.Aggregates {
		for _, spelling := range []string{function, strings.ToUpper(function)} {
			if _, _, err := classifyDTQLDocument([]byte(doc(spelling))); err != nil {
				t.Errorf("the classifier refuses the advertised %s: %v", spelling, err)
			}
			if resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", doc(spelling), nil); resp.status != http.StatusOK {
				t.Errorf("%s: status %d: %s", spelling, resp.status, resp.raw)
			}
		}
	}
	for _, function := range []string{"first", "last", "FIRST", "Last", "stddev"} {
		if _, _, err := classifyDTQLDocument([]byte(doc(function))); !errors.Is(err, core.ErrInvalidDTQL) {
			t.Errorf("the classifier takes %s, which is not advertised: %v", function, err)
		}
		if resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", doc(function), nil); resp.status != http.StatusBadRequest || resp.code() != "invalid_dtql" {
			t.Errorf("%s: status %d: %s", function, resp.status, resp.raw)
		}
	}
}

// Every bound the documentation states as a number is in the limits block as data,
// and each value is the constant that enforces it. The capacity of the server (the
// slots of the two routes and the queue wait) is not there, with auth on or off.
func TestWellKnownLimitsStateEveryBoundAndNoCapacity(t *testing.T) {
	bounds := core.RelationalBounds()
	want := map[string]float64{
		"timeoutMs":            float64(DefaultQueryLimits().Timeout.Milliseconds()),
		"maxSourceRows":        float64(DefaultQueryLimits().MaxSourceRows),
		"maxSourceBytes":       float64(DefaultQueryLimits().MaxSourceBytes),
		"maxResultRows":        float64(joinexec.MaxResultRows),
		"maxRequestBytes":      float64(maxRequestBodyBytes),
		"maxResultBytes":       float64(joinexec.MaxResultBytes),
		"maxSources":           float64(bounds.MaxSources),
		"maxSubqueryDepth":     float64(bounds.MaxSubqueryDepth),
		"maxLimit":             float64(bounds.MaxLimit),
		"maxOffset":            float64(bounds.MaxOffset),
		"maxInMemoryJoinRows":  float64(joinexec.MaxInMemoryJoinRows),
		"maxInMemoryJoinBytes": float64(joinexec.MaxInMemoryJoinBytes),
		"maxGroups":            float64(joinexec.MaxInMemoryGroups),
	}
	for mode, opts := range map[string][]Option{"auth off": nil, "auth on": {WithAuth(&auth.Config{OwnerToken: "owner"})}} {
		t.Run(mode, func(t *testing.T) {
			_, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts(), opts...)
			limits := discoveryObject(t, discoveryObject(t, discoveryGet(t, host, "/.well-known/openvaultdb"), "query"), "limits")
			got := map[string]float64{}
			for name, value := range limits {
				number, ok := value.(float64)
				if !ok {
					t.Fatalf("limit %q is not a number: %v", name, value)
				}
				got[name] = number
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("limits = %v, want %v", got, want)
			}
			for _, capacity := range []string{"concurrentInMemory", "concurrentDatabase", "queueWaitMs"} {
				if _, stated := limits[capacity]; stated {
					t.Errorf("the limits block states the capacity of the server: %q", capacity)
				}
			}
		})
	}
}

// The values of the bounds are the ones the code enforces: a document just over each
// cheap bound (sources, subquery depth, limit, offset) is refused with the profile's
// refusal, and a document at the bound is not. The documents are built from the
// advertised values, so a bound that changes without the discovery value fails here.
func TestAdvertisedBoundsAreTheOnesTheClassifierEnforces(t *testing.T) {
	_, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())
	limits := discoveryObject(t, discoveryObject(t, discoveryGet(t, host, "/.well-known/openvaultdb"), "query"), "limits")
	bound := func(name string) int {
		value, ok := limits[name].(float64)
		if !ok {
			t.Fatalf("the limits block states no %q: %v", name, limits)
		}
		return int(value)
	}
	sources := func(n int) string {
		var b strings.Builder
		b.WriteString("from:\n  database: alpha\n  name: orders\n  alias: s0\n  joins:\n")
		for i := 1; i < n; i++ {
			fmt.Fprintf(&b, "    - from: {database: alpha, name: customers, alias: s%d}\n      on: [{left: {field: id, source: s0}, op: '==', right: {field: id, source: s%d}}]\n", i, i)
		}
		return b.String()
	}
	depth := func(levels int) string {
		doc := "from: {database: alpha, name: orders, alias: l}\n"
		for i := 0; i < levels; i++ {
			doc = fmt.Sprintf("from:\n  query:\n    as: q%d\n%s", i, discoveryIndent(doc, "    "))
		}
		return doc
	}
	const one = "from: {database: alpha, name: orders}\n"
	for _, tc := range []struct {
		name          string
		atBound, over string
	}{
		{"sources", sources(bound("maxSources")), sources(bound("maxSources") + 1)},
		{"subquery depth", depth(bound("maxSubqueryDepth")), depth(bound("maxSubqueryDepth") + 1)},
		{"limit", fmt.Sprintf("%slimit: %d\n", one, bound("maxLimit")), fmt.Sprintf("%slimit: %d\n", one, bound("maxLimit")+1)},
		{"offset", fmt.Sprintf("%soffset: %d\n", one, bound("maxOffset")), fmt.Sprintf("%soffset: %d\n", one, bound("maxOffset")+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", tc.atBound, nil); resp.status != http.StatusOK {
				t.Errorf("at the bound: status %d: %s", resp.status, resp.raw)
			}
			if resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", tc.over, nil); resp.status != http.StatusBadRequest || resp.code() != "invalid_dtql" {
				t.Errorf("over the bound: status %d: %s", resp.status, resp.raw)
			}
		})
	}
}

func discoveryIndent(text, prefix string) string {
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "")
}

// features.joins is the list of join types the classifier takes: one document per
// advertised type is answered, and a right join is refused.
func TestAdvertisedJoinTypesAreTheOnesTheClassifierTakes(t *testing.T) {
	_, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())
	features := discoveryObject(t, discoveryObject(t, discoveryGet(t, host, "/.well-known/openvaultdb"), "query"), "features")
	advertised, _ := features["joins"].([]any)
	if len(advertised) == 0 {
		t.Fatalf("no advertised join types: %v", features)
	}
	doc := func(joinType any) string {
		return fmt.Sprintf("from:\n  database: alpha\n  name: orders\n  alias: o\n  joins:\n    - type: %v\n      from: {database: alpha, name: customers, alias: c}\n      on:\n        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}\n", joinType)
	}
	for _, joinType := range advertised {
		if resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", doc(joinType), nil); resp.status != http.StatusOK {
			t.Errorf("the advertised %v join: status %d: %s", joinType, resp.status, resp.raw)
		}
	}
	if resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", doc("right"), nil); resp.status != http.StatusBadRequest || resp.code() != "invalid_dtql" {
		t.Errorf("a right join: status %d: %s", resp.status, resp.raw)
	}
	if slices.Contains(advertised, any("right")) {
		t.Errorf("right is advertised: %v", advertised)
	}
}
