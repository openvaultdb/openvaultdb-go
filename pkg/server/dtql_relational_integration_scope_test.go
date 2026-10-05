package server_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// Which source an unqualified field belongs to, over HTTP, on real mounts: two
// SQLite files, which supply the columns of their tables, and a local inGitDB
// directory in partial mode, which declares only some of its fields and so supplies
// none. The documents carry an EXISTS test, which puts them on the in-memory
// evaluation that binds unqualified fields (a join document without a subquery is
// refused by the DTQL parser before it reaches the engine, for the same reason).
// The helpers of this file all start with relInt so they cannot clash with the others
// of the package.

const (
	relIntScopeMarker = "needle-7731"

	// relIntScopeJoin joins the people of crm to the orders of shop on the person, with
	// an EXISTS test and a value that must never come back in an answer. A test adds the
	// column list.
	relIntScopeJoin = "from: {database: crm, name: Person, alias: p, joins: [{type: inner, from: {database: shop, name: Orders, alias: o}, " +
		"on: [{left: {field: id, source: p}, op: '==', right: {field: person_id, source: o}}]}]}\n" +
		"where:\n  and:\n" +
		"    - {op: '>', left: {field: id, source: p}, right: {value: " + relIntScopeMarker + "}}\n" +
		"    - exists: {query: {from: {database: crm, name: Person, alias: x}}}\n" +
		"orderBy: [{field: id, source: o}]\n"
)

// relIntScopeServer serves the three mounts of the tests below.
func relIntScopeServer(t *testing.T) string {
	t.Helper()
	crm := relHTTPMount(t, "crm", "", map[string][]string{"Person": {"id", "name", "city"}},
		`CREATE TABLE "Person" ("id" TEXT PRIMARY KEY, "name" TEXT, "city" TEXT)`,
		`INSERT INTO "Person" VALUES ('p1', 'Ada', 'Leeds'), ('p2', 'Bea', 'Cork')`)
	shop := relHTTPMount(t, "shop", "", map[string][]string{"Orders": {"id", "person_id", "city", "total"}},
		`CREATE TABLE "Orders" ("id" TEXT PRIMARY KEY, "person_id" TEXT, "city" TEXT, "total" TEXT)`,
		`INSERT INTO "Orders" VALUES ('o1', 'p1', 'York', '10'), ('o2', 'p2', 'Cork', '20')`)

	// A partial database declares some of its fields: the record below holds one more,
	// which the manifest does not list, so the mount cannot tell the engine its fields.
	dir := t.TempDir()
	notes := relIntOpen(t, dir, "database: {id: notes, schema_mode: partial}\nstorage: {engine: ingitdb, path: data}\n"+
		"schemas:\n  collections:\n    Note:\n      fields:\n        person_id: {type: string}\n")
	for _, note := range []struct {
		id, person, mood string
	}{{"n1", "p1", "calm"}, {"n2", "p2", "busy"}} {
		key, err := core.ParseKey("Note", note.id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := notes.Apply(context.Background(), []core.Op{{Op: "insert", Key: key, Data: map[string]any{"person_id": note.person, "mood": note.mood}}}, "seed"); err != nil {
			t.Fatal(err)
		}
	}
	return relIntServe(t, map[string]*core.Database{"crm": crm, "shop": shop, "notes": notes})
}

func relIntScopeColumns(columns string) string { return relIntScopeJoin + "columns: " + columns + "\n" }

// relIntScopeRefused requires the 400 invalid_dtql of a scope refusal that names the
// field, says what the caller can do about it and repeats nothing else of the document.
func relIntScopeRefused(t *testing.T, resp relHTTPResponse, field string, says ...string) {
	t.Helper()
	message := resp.errorField("message")
	if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || resp.body["records"] != nil || resp.body["execution"] != nil {
		t.Fatalf("status %d, want a 400 invalid_dtql with no rows: %s", resp.status, resp.raw)
	}
	if !strings.HasPrefix(message, "scope at ") || !strings.Contains(message, "unqualified field "+field) {
		t.Fatalf("the message does not name the field %s as a scope refusal: %s", field, resp.raw)
	}
	for _, want := range says {
		if !strings.Contains(message, want) {
			t.Fatalf("the message does not say %q: %s", want, resp.raw)
		}
	}
	for _, other := range []string{relIntScopeMarker, "Orders", "Person", "shop", "crm"} {
		if strings.Contains(resp.raw, other) {
			t.Fatalf("the refusal repeats %q of the document: %s", other, resp.raw)
		}
	}
}

// A name both tables carry is refused, naming it; the same document with the name
// qualified is answered, and so is one with a name only one table carries.
func TestAnUnqualifiedFieldTwoSQLiteMountsCarryIsRefusedAndTheQualifiedOneIsAnswered(t *testing.T) {
	base := relIntScopeServer(t)
	refused := relHTTPPost(t, base, "/v1/dtql", "", relIntScopeColumns("[{field: city}]"))
	relIntScopeRefused(t, refused, "city", "ambiguous")

	answered := relHTTPPost(t, base, "/v1/dtql", "", relIntScopeColumns("[{field: city, source: p, as: home}, {field: city, source: o, as: shipped}]"))
	relIntRowsAre(t, answered, []map[string]any{{"home": "Leeds", "shipped": "York"}, {"home": "Cork", "shipped": "Cork"}})
	if answered.execution(t)["route"] != "in-memory" {
		t.Fatalf("execution = %v", answered.execution(t))
	}

	// The field only the orders carry, and the field only the people carry, are bound to
	// their own source.
	unique := relHTTPPost(t, base, "/v1/dtql", "", relIntScopeColumns("[{field: total}, {field: name}]"))
	relIntRowsAre(t, unique, []map[string]any{{"total": "10", "name": "Ada"}, {"total": "20", "name": "Bea"}})

	// The key column is a column of each table, so the name of the key is ambiguous too.
	key := relHTTPPost(t, base, "/v1/dtql", "", relIntScopeColumns("[{field: id}]"))
	relIntScopeRefused(t, key, "id", "ambiguous")
}

// The ambiguity is found wherever the field stands, and the refusal is the same in
// each place.
func TestAnAmbiguousUnqualifiedFieldIsRefusedInEveryClause(t *testing.T) {
	base := relIntScopeServer(t)
	const ok = "columns: [{field: id, source: o}]\n"
	for name, doc := range map[string]string{
		"a column": relIntScopeColumns("[{field: city}]"),
		"WHERE":    strings.Replace(relIntScopeJoin, "right: {value: "+relIntScopeMarker+"}}", "right: {field: city}}", 1) + ok,
		"ORDER BY": strings.Replace(relIntScopeJoin, "orderBy: [{field: id, source: o}]", "orderBy: [{field: city}]", 1) + ok,
	} {
		t.Run(name, func(t *testing.T) {
			relIntScopeRefused(t, relHTTPPost(t, base, "/v1/dtql", "", doc), "city", "ambiguous")
		})
	}
}

// A wildcard stands for the columns of its table, in memory too.
func TestAWildcardIsExpandedInMemoryFromTheColumnsOfTheTable(t *testing.T) {
	base := relIntScopeServer(t)
	resp := relHTTPPost(t, base, "/v1/dtql", "", relIntScopeColumns("[{wildcard: {source: p, exclude: [city]}}, {field: total, source: o}]"))
	relIntRowsAre(t, resp, []map[string]any{
		{"id": "p1", "name": "Ada", "total": "10"},
		{"id": "p2", "name": "Bea", "total": "20"},
	})
	if resp.execution(t)["route"] != "in-memory" {
		t.Fatalf("execution = %v", resp.execution(t))
	}
	excluded := relHTTPPost(t, base, "/v1/dtql", "", relIntScopeColumns("[{wildcard: {source: o, exclude: [id, person_id]}}]"))
	relIntRowsAre(t, excluded, []map[string]any{{"city": "York", "total": "10"}, {"city": "Cork", "total": "20"}})
}

// A mount that cannot say which fields its records hold is not guessed at: an
// unqualified field of a query that reads it is refused, with the way out, and the
// qualified field is read, undeclared ones included.
func TestAnUnqualifiedFieldOfAQueryThatReadsAMountWithoutFieldListsIsRefused(t *testing.T) {
	base := relIntScopeServer(t)
	const join = "from: {database: crm, name: Person, alias: p, joins: [{type: inner, from: {database: notes, name: Note, alias: n}, " +
		"on: [{left: {field: id, source: p}, op: '==', right: {field: person_id, source: n}}]}]}\n" +
		"where:\n  and:\n" +
		"    - {op: '>', left: {field: id, source: p}, right: {value: " + relIntScopeMarker + "}}\n" +
		"    - exists: {query: {from: {database: crm, name: Person, alias: x}}}\n" +
		"orderBy: [{field: id, source: p}]\n"
	refused := relHTTPPost(t, base, "/v1/dtql", "", join+"columns: [{field: mood}]\n")
	relIntScopeRefused(t, refused, "mood", "qualify")
	if strings.Contains(refused.errorField("message"), "ambiguous") {
		t.Fatalf("a mount without field lists is not an ambiguity: %s", refused.raw)
	}

	// The name only crm carries is refused as well: without the list of notes the
	// engine cannot tell it is not one of its fields.
	if name := relHTTPPost(t, base, "/v1/dtql", "", join+"columns: [{field: name}]\n"); name.status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", name.status, name.raw)
	}

	answered := relHTTPPost(t, base, "/v1/dtql", "", join+"columns: [{field: name, source: p}, {field: mood, source: n}]\n")
	relIntRowsAre(t, answered, []map[string]any{{"name": "Ada", "mood": "calm"}, {"name": "Bea", "mood": "busy"}})
}

// A document of one source is read as it always was, whichever fields it names and
// whether or not its mount can list its fields.
func TestTheDocumentsOfASingleSourceAreUntouched(t *testing.T) {
	base := relIntScopeServer(t)
	for path, doc := range map[string]string{
		"/v1/databases/shop/dtql": "from: {name: Orders}\norderBy: [{field: id}]\ncolumns: [{field: total}, {field: city}]\n",
		"/v1/dtql":                "from: {database: notes, name: Note}\norderBy: [{field: person_id}]\ncolumns: [{field: mood}]\n",
	} {
		resp := relHTTPPost(t, base, path, "", doc)
		if resp.status != http.StatusOK || len(resp.rows(t)) != 2 {
			t.Fatalf("%s: status %d: %s", path, resp.status, resp.raw)
		}
	}
}

// The fixture of DALgo that asks for a scope error names the field the error is
// about: the mounts supply their fields, so the answer is the ambiguity DALgo
// reports, on both engines and both endpoints, and not a refusal for want of lists.
func TestTheAmbiguousFieldOfTheScopeFixtureIsNamedOnBothEngines(t *testing.T) {
	dalgo := relIntDalgoDir(t)
	dir := filepath.Join(dalgo, "dtql", "testdata", "subqueries")
	set := relIntLoadSet(t, filepath.Join(dir, "schema.json"), filepath.Join(dir, "dataset.json"))
	mounts := map[string]*core.Database{}
	for _, engine := range relIntEngines {
		mounts[engine.id] = engine.mount(t, engine.id, "", set)
	}
	base := relIntServe(t, mounts)
	var doc []byte
	for _, c := range relIntLoadCases(t, "subqueries", dir) {
		if c.id == "subqueries/scope-ambiguous" {
			doc = c.doc
		}
	}
	if doc == nil {
		t.Fatal("the scope fixture is not in the corpus")
	}
	for _, engine := range relIntEngines {
		for _, endpoint := range []struct{ path, database string }{
			{"/v1/databases/" + engine.id + "/dtql", ""},
			{"/v1/dtql", engine.id},
		} {
			resp := relHTTPPost(t, base, endpoint.path, "", string(relIntRewrite(t, doc, endpoint.database)))
			if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || !strings.Contains(resp.errorField("message"), "scope at columns[0]: ambiguous unqualified field CustomerId") {
				t.Fatalf("%s on %s: %s", endpoint.path, engine.name, relIntDescribe(resp))
			}
		}
	}
}

// A field name as long as a name may be is clipped in the refusal.
func TestAnAmbiguousFieldNameIsClippedInTheRefusal(t *testing.T) {
	long := strings.Repeat("f", 250)
	crm := relHTTPMount(t, "crm", "", map[string][]string{"Person": {"id", long}},
		`CREATE TABLE "Person" ("id" TEXT PRIMARY KEY, "`+long+`" TEXT)`, `INSERT INTO "Person" VALUES ('p1', 'a')`)
	shop := relHTTPMount(t, "shop", "", map[string][]string{"Orders": {"id", "person_id", long}},
		`CREATE TABLE "Orders" ("id" TEXT PRIMARY KEY, "person_id" TEXT, "`+long+`" TEXT)`, `INSERT INTO "Orders" VALUES ('o1', 'p1', 'b')`)
	base := relIntServe(t, map[string]*core.Database{"crm": crm, "shop": shop})
	resp := relHTTPPost(t, base, "/v1/dtql", "", relIntScopeColumns("[{field: "+long+"}]"))
	message := resp.errorField("message")
	if resp.status != http.StatusBadRequest || !strings.Contains(message, "ambiguous unqualified field "+strings.Repeat("f", 50)) || len(message) > 256+len("...") || !strings.HasSuffix(message, "...") {
		t.Fatalf("status %d, message length %d: %s", resp.status, len(message), resp.raw)
	}
}
