package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// The journey of the server-side joins feature (spec/features/server-side-joins in
// openvaultdb/openvaultdb), steps 1 to 7, walked over HTTP in one test with no manual
// step: real SQLite files, a real local inGitDB directory, the mounts of
// examples/layered-acl, the real executor. Each step names the acceptance criteria of
// the feature it proves and asserts how the answer was reached, not only what it
// holds: the database computes a join of 20,000 rows that the in-memory engine
// refuses, a budget refusal carries the limit and no row, a policy-protected
// database is not read by a relational document at all.
//
// Where this server answers differently from the criterion as the feature words it,
// the step says so in its own comment.

// relIntJourneyCriteria are the acceptance criteria each step proves, in the words of
// the feature's journey table.
var relIntJourneyCriteria = map[int][]string{
	1: {"discovery-advertises-query", "capabilities-per-database"},
	2: {"single-database-join-pushdown", "relational-profile-accepted", "response-shape", "result-row-cap", "result-byte-limit", "consistency-documented", "single-source-with-database-unchanged", "parameters-and-names-cannot-change-query"},
	3: {"cross-database-join", "cross-database-endpoint-single-source", "response-shape", "mount-lease-drains-on-unmount", "consistency-documented"},
	4: {"route-label-follows-routing", "ingitdb-route-label", "engine-outside-join-set-refused", "lone-source-outside-join-set-by-endpoint"},
	5: {"budget-exceeded-is-422-never-partial", "timeout-is-504", "budget-errors-report-limit-only", "paging-headers-refused"},
	6: {"grant-checked-for-every-source", "policy-applied-per-leaf", "count-equals-readable-rows", "no-row-count-for-protected-source", "nested-join-authorised", "profile-refusals", "budget-errors-report-limit-only", "per-database-endpoint-refuses-foreign-source", "subquery-source-authorised", "collection-scoped-grant-checked", "hidden-field-not-reachable"},
	7: {"identical-gets-cacheable", "capacity-gate-503", "database-route-capacity-gate"},
}

// relIntJourneyCountries is the countries database of the journey: the four countries
// of the customers, in a local inGitDB directory.
func relIntJourneyCountries() relIntSet {
	set := relIntSet{tables: map[string][]string{"Country": {"code", "name", "region"}}, rows: map[string][]map[string]any{"Country": {}}}
	for _, code := range relIntCountries {
		region := map[bool]string{true: "Europe", false: "Americas"}[code != "US"]
		set.rows["Country"] = append(set.rows["Country"], map[string]any{"code": code, "name": relIntCountryNames[code], "region": region})
	}
	return set
}

// Documents of the journey, over the mounts chinook (the fact tables of relIntShop),
// people (its customers alone), countries (an inGitDB mount) and the protected mounts
// of the example. The per-database documents name no database.
const (
	relIntOnCustomer = "on: [{left: {field: customer_id, source: i}, op: '==', right: {field: id, source: c}}]"

	relIntInvoiceOne = "from: {name: Invoice, alias: i, joins: [{type: inner, from: {name: Customer, alias: c}, " + relIntOnCustomer + "}]}\n" +
		"where: {op: '==', left: {field: id, source: i}, right: {value: '00001'}}\n" +
		"columns: [{field: id, source: i}, {field: name, source: c, as: customer}, {field: country, source: c}]\n"
	relIntInvoiceCount = "from: {name: Customer, alias: c, joins: [{type: left, from: {name: Invoice, alias: i}, " +
		"on: [{left: {field: id, source: c}, op: '==', right: {field: customer_id, source: i}}]}]}\n" +
		"groupBy: [{field: id, source: c}]\norderBy: [{field: id, source: c}]\n" +
		"columns: [{field: id, source: c}, {aggregate: {function: count, args: [{field: id, source: i}]}, as: invoices}]\n"
	relIntNestedJoin = "from: {name: Invoice, alias: i, joins: [{type: inner, from: {name: Customer, alias: c, joins: [{type: inner, from: {name: Customer, alias: d}, " +
		"on: [{left: {field: country, source: c}, op: '==', right: {field: country, source: d}}]}]}, " + relIntOnCustomer + "}]}\n" +
		"where: {op: '==', left: {field: id, source: i}, right: {value: '00001'}}\n" +
		"orderBy: [{field: id, source: d}]\ncolumns: [{field: id, source: i}, {field: id, source: d, as: same_country}]\n"
	relIntSubquery = "from: {name: Customer, alias: c}\n" +
		"where: {op: In, left: {field: country, source: c}, right: {query: {from: {name: Customer, alias: d}, where: {op: '==', left: {field: id, source: d}, right: {value: c1}}, columns: [{field: country, source: d}]}}}\n" +
		"orderBy: [{field: id, source: c}]\ncolumns: [{field: id, source: c}]\n"
	relIntAllInvoices = "from: {name: Invoice, alias: i, joins: [{type: inner, from: {name: Customer, alias: c}, " + relIntOnCustomer + "}]}\n" +
		"columns: [{field: id, source: i}, {field: name, source: c}]\n"
	relIntWideRows = "from: {name: Doc, alias: d}\ncolumns: [{field: id, source: d}, {field: body, source: d, as: text}]\n"
	// relIntCustomerCountry joins a customer of chinook to its country in the inGitDB
	// mount countries: the cross-database join of step 3.
	relIntCustomerCountry = "from: {database: chinook, name: Customer, alias: c, joins: [{type: inner, from: {database: countries, name: Country, alias: k}, " +
		"on: [{left: {field: country, source: c}, op: '==', right: {field: code, source: k}}]}]}\n" +
		"orderBy: [{field: id, source: c}]\ncolumns: [{field: id, source: c}, {field: name, source: k, as: country}]\n"
)

// relIntSubqueryOnCountries is a document over chinook whose subquery reads another
// database.
const relIntSubqueryOnCountries = "from: {database: chinook, name: Customer, alias: c}\n" +
	"where: {exists: {query: {from: {database: countries, name: Country, alias: k}}}}\ncolumns: [{field: id, source: c}]\n"

// relIntQualify names chinook on every source of a per-database document.
func relIntQualify(doc string) string {
	return strings.NewReplacer("{name: ", "{database: chinook, name: ").Replace(doc)
}

// relIntCustomerCountryRows are the rows of relIntCustomerCountry.
func relIntCustomerCountryRows() []map[string]any {
	rows := make([]map[string]any, 10)
	for n := range rows {
		rows[n] = map[string]any{"id": fmt.Sprintf("c%d", n), "country": relIntCountryNames[relIntCountries[n%4]]}
	}
	return rows
}

func relIntRowsAre(t *testing.T, resp relHTTPResponse, want []map[string]any) {
	t.Helper()
	if resp.status != http.StatusOK {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
	if got := resp.rows(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

// relIntWideMount holds 950 rows of about 10 KB each: fewer than the 1000 rows of a
// response and more than the 8 MiB of its bytes.
func relIntWideMount(t *testing.T) *core.Database {
	return relHTTPMount(t, "wide", "", map[string][]string{"Doc": {"id", "body"}},
		`CREATE TABLE "Doc" ("id" TEXT PRIMARY KEY, "body" TEXT)`,
		`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 950) `+
			`INSERT INTO "Doc" SELECT printf('%04d', n), replace(hex(zeroblob(5000)), '00', 'xx') FROM seq`)
}

// Steps 1 to 7 of the journey.
func TestTheJourneyOfAJoinOverHTTP(t *testing.T) {
	chinook := relIntShop(t, "chinook", ", cache_ttl: 120s")
	people := relHTTPMount(t, "people", "", relIntCustomerFields, relIntCustomerStatements()...)
	countries := relIntMountInGitDB(t, "countries", ", cache_ttl: 60s", relIntJourneyCountries())
	wide := relIntWideMount(t)
	slow := relIntParkMount(t, "slow")
	mounts := relIntLayeredMounts(t)
	mounts["chinook"], mounts["people"], mounts["countries"], mounts["wide"], mounts["slow"] = chinook, people, countries, wide, slow

	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	readOf := func(collection string) []auth.Capability {
		return []auth.Capability{{Action: auth.CapRecordsRead, Collection: collection}}
	}
	for token, grant := range map[string]*auth.Grant{
		"token-for-chinook": {PrincipalID: "app-chinook", DatabaseID: "chinook", Capabilities: readOf("")},
		"token-for-invoice": {PrincipalID: "app-invoice", DatabaseID: "chinook", Capabilities: readOf("Invoice")},
		relIntAlice:         {Subject: &relIntAliceRef, Actor: &relIntApplication, Capabilities: readOf("")},
	} {
		if err := store.CreateGrant(grant, token); err != nil {
			t.Fatal(err)
		}
	}
	// One query at a time on each route, and a query that finds the route full is
	// refused at once: the capacity of step 7 is reached without a sleep.
	main := relIntServe(t, mounts,
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}),
		server.WithGrantIdentity(server.GrantIdentityConfig{
			Bootstrap: access.PrincipalRef{Realm: "local-demo", Kind: access.PrincipalKindService, ID: "demo-bootstrap"},
			Resolve: func(_ context.Context, ref access.PrincipalRef) (server.Membership, error) {
				if ref == relIntAliceRef {
					return server.Membership{Roles: []string{"reader"}, Revision: "fixture-1"}, nil
				}
				return server.Membership{}, nil
			},
		}),
		server.WithQueryLimits(server.QueryLimits{InMemory: 1, Database: 1, QueueWait: -1}))
	// A read-only server with authentication off over the two databases that carry a
	// cache lifetime; one with a source budget of 100 rows; one with a time limit
	// that is over before the first read.
	open := relIntServe(t, map[string]*core.Database{"chinook": chinook, "countries": countries}, server.WithReadOnly(true))
	shared := map[string]*core.Database{"chinook": chinook, "people": people, "countries": countries}
	small := relIntServe(t, shared, server.WithQueryLimits(server.QueryLimits{MaxSourceRows: 100}))
	late := relIntServe(t, shared, server.WithQueryLimits(server.QueryLimits{Timeout: 1}))

	post := func(path, token, doc string) relHTTPResponse { return relHTTPPost(t, main, path, token, doc) }
	owner := func(path, doc string) relHTTPResponse { return post(path, ownerToken, doc) }
	step := func(n int, title string, body func(t *testing.T)) {
		t.Run(fmt.Sprintf("step %d, %s", n, title), func(t *testing.T) {
			t.Logf("proves %s", strings.Join(relIntJourneyCriteria[n], ", "))
			body(t)
		})
	}

	// Step 1: the discovery document. discovery-advertises-query and
	// capabilities-per-database ask for a `query` block and for the flags `joins` and
	// `aggregation` on every database. Those belong to the discovery change, which is not
	// part of this server yet; this step asserts what holds here: the document is served
	// in both authentication modes, the protocol string is unchanged and, with
	// authentication off, every database that can be queried says so. It is to be
	// extended with the block when that change lands.
	step(1, "discovery", func(t *testing.T) {
		secured := relHTTPDo(t, main, http.MethodGet, "/.well-known/openvaultdb", "", "", nil)
		if secured.status != http.StatusOK || secured.body["protocol"] != "openvaultdb/0.1" || secured.body["authEnabled"] != true {
			t.Fatalf("authentication on: status %d: %s", secured.status, secured.raw)
		}
		public := relHTTPDo(t, open, http.MethodGet, "/.well-known/openvaultdb", "", "", nil)
		if public.status != http.StatusOK || public.body["protocol"] != "openvaultdb/0.1" || public.body["authEnabled"] != false {
			t.Fatalf("authentication off: status %d: %s", public.status, public.raw)
		}
		listed := map[string]map[string]any{}
		databases, _ := public.body["databases"].([]any)
		for _, entry := range databases {
			database := entry.(map[string]any)
			listed[database["id"].(string)], _ = database["capabilities"].(map[string]any)
		}
		for _, id := range []string{"chinook", "countries"} {
			if listed[id] == nil || listed[id]["dtql"] != true || listed[id]["query"] != true {
				t.Fatalf("the capabilities of %s = %v", id, listed[id])
			}
		}
	})

	// Step 2: one document on the per-database endpoint. single-source-with-database-
	// unchanged says that a one-source document naming another database is answered
	// with `database` ignored; this server refuses it with a 400, as it did before the
	// feature (the criterion marks that as open), and reads a document that names the
	// database of the endpoint exactly as one that names none.
	step(2, "a join in one database", func(t *testing.T) {
		// single-database-join-pushdown: the negative control first. The same document
		// with its tables in two mounts runs in memory, where DALgo's join holds 10,000
		// rows, and is refused; the document in one mount reads 20,000 invoices and is
		// answered by the database, with the right sums.
		control := owner("/v1/dtql", relIntRevenue("chinook", "people", true))
		detail, _ := control.body["error"].(map[string]any)
		budget, _ := detail["budget"].(map[string]any)
		if control.status != http.StatusUnprocessableEntity || control.errorField("code") != "query_budget_exceeded" || budget["limit"] != float64(10000) || budget["route"] != "in-memory" {
			t.Fatalf("the control: status %d, want a 422 on the 10,000-row bound of the in-memory route: %s", control.status, control.raw)
		}
		pushed := owner("/v1/databases/chinook/dtql", relIntRevenue("", "", true))
		relIntRowsAre(t, pushed, relIntExpectedRevenue())
		if pushed.execution(t)["route"] != "database" {
			t.Fatalf("execution = %v", pushed.execution(t))
		}

		// relational-profile-accepted and response-shape: an inner join, a left join with
		// a grouping, a nested join, GROUP BY with HAVING and an alias, and a subquery
		// each return their rows, with ordered columns and an execution block and no key.
		for _, tc := range []struct {
			name, doc string
			columns   []string
			rows      []map[string]any
		}{
			{"an inner join", relIntInvoiceOne, []string{"id", "customer", "country"}, []map[string]any{{"id": "00001", "customer": "Customer 1", "country": "US"}}},
			{"a nested join", relIntNestedJoin, []string{"id", "same_country"}, []map[string]any{
				{"id": "00001", "same_country": "c1"}, {"id": "00001", "same_country": "c5"}, {"id": "00001", "same_country": "c9"}}},
			{"a subquery", relIntSubquery, []string{"id"}, []map[string]any{{"id": "c1"}, {"id": "c5"}, {"id": "c9"}}},
			{"GROUP BY, HAVING and an alias", relIntRevenue("", "", true) + "having: {op: '>', left: {aggregate: {function: sum, args: [{field: total, source: i}]}}, right: {value: 20000}}\n",
				[]string{"country", "revenue", "invoices"}, []map[string]any{
					{"country": "UK", "revenue": float64(24001), "invoices": float64(6000)}, {"country": "US", "revenue": float64(23994), "invoices": float64(6000)}}},
		} {
			resp := owner("/v1/databases/chinook/dtql", tc.doc)
			relIntRowsAre(t, resp, tc.rows)
			if !reflect.DeepEqual(resp.columns(), tc.columns) {
				t.Fatalf("%s: columns = %v, want %v", tc.name, resp.columns(), tc.columns)
			}
			for _, record := range resp.body["records"].([]any) {
				if _, hasKey := record.(map[string]any)["key"]; hasKey {
					t.Fatalf("%s: a relational record carries a key: %s", tc.name, resp.raw)
				}
			}
			for _, field := range []string{"route", "elapsedMs", "rowsReturned", "sources"} {
				if _, ok := resp.execution(t)[field]; !ok {
					t.Fatalf("%s: the execution block has no %s: %s", tc.name, field, resp.raw)
				}
			}
		}
		counts := owner("/v1/databases/chinook/dtql", relIntInvoiceCount)
		if counts.status != http.StatusOK || len(counts.rows(t)) != 10 || counts.rows(t)[0]["invoices"] != float64(2000) {
			t.Fatalf("a left join with a grouping: status %d: %s", counts.status, counts.raw)
		}
		// The single-collection request is what it always returned: records with keys,
		// and no other member.
		plain := owner("/v1/databases/chinook/dtql", "from: {name: Customer}\norderBy: [{field: id}]\n")
		first, _ := plain.body["records"].([]any)[0].(map[string]any)
		if plain.status != http.StatusOK || len(plain.body) != 1 || first["key"] != "Customer/c0" {
			t.Fatalf("single-collection request: status %d: %s", plain.status, plain.raw)
		}
		bare := "from: {name: Customer}\norderBy: [{field: id}]\ncolumns: [{field: name}]\n"
		own := "from: {database: chinook, name: Customer}\norderBy: [{field: id}]\ncolumns: [{field: name}]\n"
		if a, b := owner("/v1/databases/chinook/dtql", bare), owner("/v1/databases/chinook/dtql", own); a.status != http.StatusOK || a.raw != b.raw {
			t.Fatalf("a root that names the database of the endpoint is read as one that names none: %s\n%s", a.raw, b.raw)
		}
		if other := owner("/v1/databases/chinook/dtql", strings.Replace(own, "chinook", "people", 1)); other.status != http.StatusBadRequest {
			t.Fatalf("a root that names another database: status %d: %s", other.status, other.raw)
		}

		// result-row-cap: a join of 20,000 rows posted without a limit is refused, with
		// the limit, and no rows.
		capped := owner("/v1/databases/chinook/dtql", relIntAllInvoices)
		detail, _ = capped.body["error"].(map[string]any)
		budget, _ = detail["budget"].(map[string]any)
		if capped.status != http.StatusUnprocessableEntity || budget["name"] != "response_rows" || budget["limit"] != float64(1000) || capped.body["records"] != nil {
			t.Fatalf("result-row-cap: status %d: %s", capped.status, capped.raw)
		}
		// result-byte-limit: 950 rows of 10 KB are fewer than the row cap and more than
		// the byte cap.
		heavy := owner("/v1/databases/wide/dtql", relIntWideRows)
		detail, _ = heavy.body["error"].(map[string]any)
		budget, _ = detail["budget"].(map[string]any)
		if heavy.status != http.StatusUnprocessableEntity || budget["name"] != "response_bytes" || heavy.body["records"] != nil {
			t.Fatalf("result-byte-limit: status %d: %.300s", heavy.status, heavy.raw)
		}
		// consistency-documented asks for a sentence of docs/api.md and is read from there.

		// parameters-and-names-cannot-change-query: a parameter value that holds SQL is
		// data (no row has that name); an alias or a field name that holds a quote is a
		// 400 invalid_dtql with no text of a database in it, on an unprotected mount and
		// on a policy-protected one.
		query := "from: {name: Customer, alias: c}\nwhere: {op: '==', left: {field: name, source: c}, right: {param: n}}\ncolumns: [{field: id, source: c}]\n"
		body := fmt.Sprintf(`{"query": %q, "parameters": {"n": "x' OR '1'='1"}}`, query)
		bound := relHTTPDo(t, main, http.MethodPost, "/v1/databases/chinook/dtql", ownerToken, body, map[string]string{"Content-Type": "application/json"})
		if bound.status != http.StatusOK || len(bound.rows(t)) != 0 {
			t.Fatalf("a parameter that holds SQL: status %d: %s", bound.status, bound.raw)
		}
		for _, database := range []string{"chinook", "sqlite"} {
			collection := map[string]string{"chinook": "Customer", "sqlite": "customers"}[database]
			for name, columns := range map[string]string{
				"an alias with a quote":      "columns: [{field: id, source: c, as: \"x'y\"}]\n",
				"a field name with a quote":  "columns: [{field: \"id'--\", source: c}]\n",
				"a source alias with a dash": "columns: [{field: id, source: \"c-c\"}]\n",
			} {
				resp := owner("/v1/databases/"+database+"/dtql", "from: {name: "+collection+", alias: c}\n"+columns)
				text := strings.ToLower(resp.raw)
				if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || strings.Contains(text, "sqlite") || strings.Contains(text, "syntax") {
					t.Fatalf("%s on %s: status %d: %s", name, database, resp.status, resp.raw)
				}
			}
		}
	})

	// Step 3: a document across two databases on /v1/dtql. The schema, scan, cursor and
	// money of a single-source document are refused with a 400 invalid_dtql here, not
	// the 422 that cross-database-endpoint-single-source words; the paging headers are a
	// 422. mount-lease-drains-on-unmount is proved by
	// TestUnmountingAMountWhileAQueryReadsItWaitsForTheQuery; consistency-documented is
	// read from docs/api.md.
	step(3, "a join across two databases", func(t *testing.T) {
		resp := owner("/v1/dtql", relIntCustomerCountry)
		relIntRowsAre(t, resp, relIntCustomerCountryRows())
		if got := resp.columns(); !reflect.DeepEqual(got, []string{"id", "country"}) {
			t.Fatalf("columns = %v", got)
		}
		sources, _ := resp.execution(t)["sources"].([]any)
		reads := map[string]float64{}
		for _, source := range sources {
			entry := source.(map[string]any)
			if _, ok := entry["elapsedMs"].(float64); !ok {
				t.Fatalf("a source has no elapsedMs: %v", entry)
			}
			reads[entry["database"].(string)+"."+entry["collection"].(string)], _ = entry["rows"].(float64)
		}
		if !reflect.DeepEqual(reads, map[string]float64{"chinook.Customer": 10, "countries.Country": 4}) {
			t.Fatalf("sources = %v", sources)
		}
		for name, tc := range map[string]struct {
			doc    string
			status int
		}{
			"a source without a database":    {strings.Replace(relIntCustomerCountry, "database: countries, ", "", 1), http.StatusBadRequest},
			"an id that is not valid":        {strings.Replace(relIntCustomerCountry, "database: countries", "database: 'a b'", 1), http.StatusBadRequest},
			"a database that is not mounted": {strings.Replace(relIntCustomerCountry, "database: countries", "database: nowhere", 1), http.StatusNotFound},
		} {
			if refused := owner("/v1/dtql", tc.doc); refused.status != tc.status || refused.body["records"] != nil {
				t.Fatalf("%s: status %d: %s", name, refused.status, refused.raw)
			}
		}
		// A document of one source on /v1/dtql has columns and execution and no key.
		single := owner("/v1/dtql", "from: {database: chinook, name: Customer}\norderBy: [{field: id}]\ncolumns: [{field: id}]\n")
		if single.status != http.StatusOK || single.columns() == nil || single.execution(t)["route"] != "database" || len(single.rows(t)) != 10 {
			t.Fatalf("a single source on /v1/dtql: status %d: %s", single.status, single.raw)
		}
		for name, doc := range map[string]string{
			"a schema": "from: {database: chinook, schema: main, name: Customer}\n",
			"a scan":   "from: {database: chinook, name: Customer, scan: {limit: 5, orderBy: [{field: id}]}}\n",
			"a cursor": "from: {database: chinook, name: Customer}\nstartFrom: x\n",
			"money":    "from: {database: chinook, name: Customer}\nmoney: {minorUnitScale: 2, divisionScale: 2, rounding: halfEven}\n",
		} {
			if refused := owner("/v1/dtql", doc); refused.status != http.StatusBadRequest || refused.errorField("code") != "invalid_dtql" {
				t.Fatalf("%s: status %d: %s", name, refused.status, refused.raw)
			}
		}
		paged := relHTTPDo(t, main, http.MethodPost, "/v1/dtql", ownerToken, "from: {database: chinook, name: Customer}\n", map[string]string{"OVDB-Page-Size": "5"})
		if paged.status != http.StatusUnprocessableEntity || paged.errorField("code") != "snapshot_unsupported" {
			t.Fatalf("a single source with a paging header: status %d: %s", paged.status, paged.raw)
		}
	})

	// Step 4: the route label. A policy-protected database is not read by a relational
	// document, so the label of route-label-follows-routing for it is a 422
	// authorization_unsupported. engine-outside-join-set-refused and
	// lone-source-outside-join-set-by-endpoint need a PostgreSQL or Firestore mount and
	// are proved with fake drivers in TestRelationalHandlerRefusesEnginesTheGuardOrTheListLeavesOut.
	step(4, "the route", func(t *testing.T) {
		route := func(resp relHTTPResponse) string {
			t.Helper()
			if resp.status != http.StatusOK {
				t.Fatalf("status %d: %s", resp.status, resp.raw)
			}
			label, _ := resp.execution(t)["route"].(string)
			return label
		}
		if got := route(owner("/v1/databases/chinook/dtql", relIntRevenue("", "", true))); got != "database" {
			t.Fatalf("one unprotected SQLite mount: %s", got)
		}
		if got := route(owner("/v1/dtql", relIntCustomerCountry)); got != "in-memory" {
			t.Fatalf("two mounts: %s", got)
		}
		if got := route(owner("/v1/databases/chinook/dtql", relIntSubquery)); got != "in-memory" {
			t.Fatalf("a subquery: %s", got)
		}
		const gitJoin = "from: {database: countries, name: Country, alias: a, joins: [{type: inner, from: {database: countries, name: Country, alias: b}, " +
			"on: [{left: {field: region, source: a}, op: '==', right: {field: region, source: b}}]}]}\ncolumns: [{field: code, source: a, as: first}, {field: code, source: b, as: second}]\n"
		if got := route(owner("/v1/dtql", gitJoin)); got != "in-memory" {
			t.Fatalf("one local inGitDB mount: %s", got)
		}
		protected := owner("/v1/dtql", "from: {database: sqlite, name: customers, alias: c}\ncolumns: [{field: id, source: c}]\n")
		if protected.status != http.StatusUnprocessableEntity || protected.errorField("code") != "authorization_unsupported" {
			t.Fatalf("a policy-protected mount: status %d: %s", protected.status, protected.raw)
		}
	})

	// Step 5: a query that is too big or too slow. The limit is told and no row is
	// returned; the figure the query reached is not told.
	step(5, "too big or too slow", func(t *testing.T) {
		const invoicesAcross = "from: {database: chinook, name: Invoice, alias: i, joins: [{type: inner, from: {database: people, name: Customer, alias: c}, " + relIntOnCustomer + "}]}\n" +
			"columns: [{field: id, source: i}, {field: name, source: c}]\n"
		refused := relHTTPPost(t, small, "/v1/dtql", "", invoicesAcross)
		detail, _ := refused.body["error"].(map[string]any)
		budget, _ := detail["budget"].(map[string]any)
		if refused.status != http.StatusUnprocessableEntity || refused.errorField("code") != "query_budget_exceeded" || budget["name"] != "source_rows" || budget["limit"] != float64(100) ||
			detail["hint"] == "" || refused.body["records"] != nil || refused.body["execution"] != nil {
			t.Fatalf("status %d: %s", refused.status, refused.raw)
		}
		for _, observed := range []string{"101", "20000", "20,000"} {
			if strings.Contains(refused.raw, observed) {
				t.Fatalf("the refusal reports a figure the query reached (%s): %s", observed, refused.raw)
			}
		}
		// The time limit is a 504 on both routes, and the server answers the next request.
		for path, doc := range map[string]string{"/v1/databases/chinook/dtql": relIntRevenue("", "", true), "/v1/dtql": relIntCustomerCountry} {
			if slowed := relHTTPPost(t, late, path, "", doc); slowed.status != http.StatusGatewayTimeout || slowed.errorField("code") != "query_timeout" {
				t.Fatalf("%s: status %d: %s", path, slowed.status, slowed.raw)
			}
		}
		if next := relHTTPPost(t, late, "/v1/databases/chinook/dtql", "", "from: {name: Customer}\n"); next.status != http.StatusOK {
			t.Fatalf("the next request: status %d: %s", next.status, next.raw)
		}
		// The paging headers are refused on a relational document and on a single source
		// of /v1/dtql, and no snapshot is taken.
		for _, header := range []string{"OVDB-Page-Size", "OVDB-Page-Token", "OVDB-Page-Close"} {
			for path, doc := range map[string]string{"/v1/databases/chinook/dtql": relIntInvoiceOne, "/v1/dtql": relIntCustomerCountry} {
				resp := relHTTPDo(t, main, http.MethodPost, path, ownerToken, doc, map[string]string{header: "10"})
				if resp.status != http.StatusUnprocessableEntity || resp.errorField("code") != "snapshot_unsupported" {
					t.Fatalf("%s on %s: status %d: %s", header, path, resp.status, resp.raw)
				}
			}
		}
	})

	// Step 6: a scoped token, and a policy-protected database. The refusals of
	// profile-refusals are 400 invalid_dtql answers that give the classifier's reason,
	// where the criterion words a 422. policy-applied-per-leaf, count-equals-readable-
	// rows, no-row-count-for-protected-source, nested-join-authorised and
	// hidden-field-not-reachable are, on this server, one answer: a relational document
	// is not run on a database with access policies, so no join, count or nested join
	// over one returns a row, and what the policy lets the principal read is read one
	// collection at a time.
	step(6, "a scoped token and a protected database", func(t *testing.T) {
		forbidden := func(resp relHTTPResponse, names string) {
			t.Helper()
			if resp.status != http.StatusForbidden || resp.errorField("code") != "forbidden" || !strings.Contains(resp.errorField("message"), names) || resp.body["records"] != nil {
				t.Fatalf("status %d, want a 403 that names %s: %s", resp.status, names, resp.raw)
			}
		}
		// grant-checked-for-every-source: a token for one database is refused the join of
		// two, naming the other, before the second is looked up (403 before 404).
		forbidden(post("/v1/dtql", "token-for-chinook", relIntCustomerCountry), "countries")
		forbidden(post("/v1/dtql", "token-for-chinook", strings.Replace(relIntCustomerCountry, "database: countries", "database: nowhere", 1)), "nowhere")
		// per-database-endpoint-refuses-foreign-source.
		if foreign := owner("/v1/databases/chinook/dtql", relIntCustomerCountry); foreign.status != http.StatusBadRequest || !strings.Contains(foreign.errorField("message"), "countries") {
			t.Fatalf("a foreign source on the per-database endpoint: status %d: %s", foreign.status, foreign.raw)
		}
		// subquery-source-authorised: a subquery that reads another database is refused
		// like a join, one that reads the token's own database is answered.
		forbidden(post("/v1/dtql", "token-for-chinook", relIntSubqueryOnCountries), "countries")
		relIntRowsAre(t, post("/v1/dtql", "token-for-chinook", relIntQualify(relIntSubquery)), []map[string]any{{"id": "c1"}, {"id": "c5"}, {"id": "c9"}})
		// collection-scoped-grant-checked: a token for the collection Invoice is refused
		// a join that reads Customer, and an EXISTS subquery that reads it.
		forbidden(post("/v1/databases/chinook/dtql", "token-for-invoice", relIntInvoiceOne), "Customer")
		forbidden(post("/v1/databases/chinook/dtql", "token-for-invoice",
			"from: {name: Invoice, alias: i}\nwhere: {exists: {query: {from: {name: Customer, alias: c}, where: {op: '==', left: {field: id, source: c}, right: {field: customer_id, source: i}}}}}\ncolumns: [{field: id, source: i}]\n"), "Customer")

		// A principal who reads every database and is a member of the role the policy of
		// the mount sqlite names.
		const (
			joined = "from: {database: sqlite, name: customers, alias: c, joins: [{from: {database: chinook, name: Customer, alias: k}, " +
				"on: [{left: {field: country, source: c}, op: '==', right: {field: country, source: k}}]}]}\ncolumns: [{field: name, source: k}]\n"
			reversed = "from: {database: chinook, name: Customer, alias: k, joins: [{from: {database: sqlite, name: customers, alias: c}, " +
				"on: [{left: {field: country, source: k}, op: '==', right: {field: country, source: c}}]}]}\ncolumns: [{field: name, source: c}]\n"
			count  = "from: {database: sqlite, name: customers, alias: c}\ncolumns: [{aggregate: {function: count, args: [{star: true}]}, as: n}]\n"
			nested = "from: {database: chinook, name: Customer, alias: a, joins: [{from: {database: chinook, name: Customer, alias: b, joins: [{from: {database: sqlite, name: customers, alias: c}, " +
				"on: [{left: {field: country, source: b}, op: '==', right: {field: country, source: c}}]}]}, on: [{left: {field: id, source: a}, op: '==', right: {field: id, source: b}}]}]}\ncolumns: [{field: name, source: c}]\n"
			hidden = "from: {database: sqlite, name: customers, alias: c}\ncolumns: [{aggregate: {function: max, args: [{field: secret, source: c}]}, as: s}]\n"
		)
		var want string
		for name, doc := range map[string]string{"a join to a public mount": joined, "the same join, reversed": reversed, "a count": count, "a nested join": nested, "an aggregate over a hidden field": hidden} {
			resp := post("/v1/dtql", relIntAlice, doc)
			if resp.status != http.StatusUnprocessableEntity || resp.errorField("code") != "authorization_unsupported" || resp.body["records"] != nil || resp.body["execution"] != nil {
				t.Fatalf("%s: status %d, want a 422 authorization_unsupported with no rows and no execution: %s", name, resp.status, resp.raw)
			}
			if want == "" {
				want = resp.raw
			} else if resp.raw != want {
				t.Fatalf("%s: %s\nwant the answer every shape gets: %s", name, resp.raw, want)
			}
		}
		read := post("/v1/databases/sqlite/dtql", relIntAlice, "from: {name: customers}\norderBy: [{field: name}]\n")
		var keys []string
		for _, record := range read.body["records"].([]any) {
			keys = append(keys, record.(map[string]any)["key"].(string))
		}
		if read.status != http.StatusOK || !reflect.DeepEqual(keys, []string{"customers/01", "customers/03"}) || strings.Contains(read.raw, "hidden") {
			t.Fatalf("a single-collection read of the protected mount: status %d: %s", read.status, read.raw)
		}

		// profile-refusals, on the per-database endpoint: each refused document is a 400
		// invalid_dtql that names its reason and reads nothing; the documents the profile
		// accepts are answered. A DTQL document has no key for a cursor, so the refusal of
		// a cursor is the parser's, which names the key it does not know.
		nest := func(levels int) string {
			doc := "from: {name: Customer}\n"
			for i := 0; i < levels; i++ {
				doc = "from: {name: Customer}\nwhere:\n  exists:\n    query:\n" + indentRelHTTP(doc, 6)
			}
			return doc
		}
		var nine strings.Builder
		nine.WriteString("from:\n  name: Customer\n  alias: s0\n  joins:\n")
		for i := 1; i <= 8; i++ {
			fmt.Fprintf(&nine, "    - from: {name: Customer, alias: s%d}\n      on: [{left: {field: id, source: s0}, op: '==', right: {field: id, source: s%d}}]\n", i, i)
		}
		deep := "from: {name: Customer, alias: s0, joins: [{from: {name: Customer, alias: s1, joins: [{from: {name: Customer, alias: s2, joins: [{from: {name: Customer, alias: s3, joins: [{from: {name: Customer, alias: s4, " +
			"joins: [{from: {name: Customer, alias: s5}, on: [{left: {field: id, source: s4}, op: '==', right: {field: id, source: s5}}]}]}, on: [{left: {field: id, source: s3}, op: '==', right: {field: id, source: s4}}]}]}, " +
			"on: [{left: {field: id, source: s2}, op: '==', right: {field: id, source: s3}}]}]}, on: [{left: {field: id, source: s1}, op: '==', right: {field: id, source: s2}}]}]}, on: [{left: {field: id, source: s0}, op: '==', right: {field: id, source: s1}}]}]}\n"
		join := func(extraJoin, tail string) string {
			return "from: {name: Customer, alias: a, joins: [{type: inner, from: {name: Customer, alias: b}, on: [{left: {field: id, source: a}, op: '==', right: {field: id, source: b}}]" + extraJoin + "}]}\n" + tail
		}
		for name, tc := range map[string]struct{ doc, reason string }{
			"a cursor":                     {join("", "startFrom: x\n"), "startFrom"},
			"a schema":                     {strings.Replace(join("", ""), "{name: Customer, alias: b}", "{schema: other, name: Customer, alias: b}", 1), "schema"},
			"money":                        {join("", "money: {minorUnitScale: 2, divisionScale: 2, rounding: halfEven}\n"), "money"},
			"nine sources":                 {nine.String(), "sources"},
			"subqueries nested five deep":  {nest(5), "subqueries nest"},
			"limit 1001":                   {join("", "limit: 1001\n"), "limit"},
			"offset 10001":                 {join("", "limit: 5\noffset: 10001\n"), "offset"},
			"a scan on an unprotected one": {"from: {name: Customer, alias: a, scan: {limit: 5, orderBy: [{field: id}]}}\n", "scan"},
			"a scan on a protected one":    {"from: {name: customers, alias: a, scan: {limit: 5, orderBy: [{field: id}]}}\n", "scan"},
			"a right join":                 {strings.Replace(join("", ""), "type: inner", "type: right", 1), "right"},
		} {
			database := map[bool]string{true: "sqlite", false: "chinook"}[name == "a scan on a protected one"]
			resp := owner("/v1/databases/"+database+"/dtql", tc.doc)
			if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || !strings.Contains(resp.errorField("message"), tc.reason) || resp.body["records"] != nil {
				t.Fatalf("%s: status %d, want a 400 invalid_dtql naming %q: %s", name, resp.status, tc.reason, resp.raw)
			}
		}
		for name, doc := range map[string]string{
			"a cursor on a single collection":   "from: {name: Customer}\nstartFrom: x\n",
			"limit 1001 on a single collection": "from: {name: Customer}\nlimit: 1001\n",
		} {
			if resp := owner("/v1/databases/chinook/dtql", doc); resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" {
				t.Fatalf("%s: status %d: %s", name, resp.status, resp.raw)
			}
		}
		// The classifier bounds the number of sources (eight) and the depth of subqueries
		// (four), and not the depth of a join tree: a tree five levels deep, of six
		// sources, is answered where profile-refusals words a refusal of a join depth
		// above four.
		for name, tc := range map[string]struct {
			doc  string
			rows int
		}{
			"a join tree five levels deep": {deep, 10},
			"a join algorithm hint":        {join(", hints: {algorithms: [hash, nestedLoop]}", "orderBy: [{field: id, source: a}]\ncolumns: [{field: id, source: a}]\n"), 10},
			"offset 10000":                 {join("", "orderBy: [{field: id, source: a}]\nlimit: 5\noffset: 10000\ncolumns: [{field: id, source: a}]\n"), 0},
			"limit 1000":                   {join("", "orderBy: [{field: id, source: a}]\nlimit: 1000\ncolumns: [{field: id, source: a}]\n"), 10},
		} {
			if resp := owner("/v1/databases/chinook/dtql", tc.doc); resp.status != http.StatusOK || len(resp.rows(t)) != tc.rows {
				t.Fatalf("%s: status %d: %s", name, resp.status, resp.raw)
			}
		}
	})

	// Step 7: the same question asked again and again. A GET of a joined query on a
	// read-only server with authentication off carries a public cache lifetime of the
	// smallest cache_ttl of the databases it reads (60 s and 120 s), the same for the
	// same query; with authentication on it is not public. A query that finds its route
	// full is a 503 with a Retry-After on both routes, and the instance goes on.
	step(7, "the same question many times", func(t *testing.T) {
		get := func(base string, token string) relHTTPResponse {
			return relHTTPDo(t, base, http.MethodGet, "/v1/dtql?q="+url.QueryEscape(relIntCustomerCountry), token, "", nil)
		}
		first, second := get(open, ""), get(open, "")
		const public = "public, max-age=60, s-maxage=60"
		for _, resp := range []relHTTPResponse{first, second} {
			if resp.status != http.StatusOK || resp.header.Get("Cache-Control") != public || !strings.Contains(resp.header.Get("Vary"), "OVDB-Page-Size") {
				t.Fatalf("status %d, Cache-Control %q, Vary %q: %s", resp.status, resp.header.Get("Cache-Control"), resp.header.Get("Vary"), resp.raw)
			}
		}
		relIntRowsAre(t, first, relIntCustomerCountryRows())
		if !reflect.DeepEqual(first.rows(t), second.rows(t)) || !reflect.DeepEqual(first.columns(), second.columns()) {
			t.Fatalf("the same query gave two answers:\n%s\n%s", first.raw, second.raw)
		}
		if secured := get(main, ownerToken); secured.status != http.StatusOK || secured.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("authentication on: status %d, Cache-Control %q", secured.status, secured.header.Get("Cache-Control"))
		}
		relIntCheckCapacity(t, main, ownerToken)
	})
}
