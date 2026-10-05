package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/dal-go/record"
	"gopkg.in/yaml.v3"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
	_ "modernc.org/sqlite"
)

// Integration tests of the relational DTQL path: real mounts (SQLite files and
// local inGitDB directories), the real handler and the real executor, reached over
// HTTP. They add no sleep and start no process of their own: every server is an
// httptest server and every mount lives in a temporary directory. The helpers of
// the files that start with dtql_relational_integration all start with relInt so
// they cannot clash with the helpers of the other test files of the package.

// relIntSet is the schema and the rows of one dataset, in the shape of the
// schema.json and dataset.json files of DALgo's DTQL fixtures: the ordered columns of
// every table, and its rows.
type relIntSet struct {
	tables map[string][]string
	rows   map[string][]map[string]any
}

func (s relIntSet) names() []string {
	names := make([]string, 0, len(s.tables))
	for name := range s.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// relIntLoadSet reads a schema file and a dataset file. Numbers stay json.Number so
// that an integer is stored as one.
func relIntLoadSet(t *testing.T, schemaPath, datasetPath string) relIntSet {
	t.Helper()
	var schemaFile struct {
		Tables map[string][]string `json:"tables"`
	}
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &schemaFile); err != nil {
		t.Fatalf("%s: %v", schemaPath, err)
	}
	var datasetFile struct {
		Tables map[string][]map[string]any `json:"tables"`
	}
	raw, err = os.ReadFile(datasetPath)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&datasetFile); err != nil {
		t.Fatalf("%s: %v", datasetPath, err)
	}
	return relIntSet{tables: schemaFile.Tables, rows: datasetFile.Tables}
}

// relIntValue turns the json.Number of a dataset into the integer or float it
// spells.
func relIntValue(v any) any {
	number, ok := v.(json.Number)
	if !ok {
		return v
	}
	if i, err := number.Int64(); err == nil {
		return i
	}
	f, _ := number.Float64()
	return f
}

// relIntFieldType is the declared type of a column: the type of its first value
// that is not null.
func relIntFieldType(rows []map[string]any, column string) string {
	for _, row := range rows {
		switch v := row[column].(type) {
		case json.Number:
			if _, err := v.Int64(); err == nil {
				return "integer"
			}
			return "number"
		case string:
			return "string"
		case bool:
			return "boolean"
		}
	}
	return "string"
}

// relIntManifest declares every table of the set as a collection of a strict
// database. The key column of a table is not a declared field.
func relIntManifest(id, engine, storage, extra string, set relIntSet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "database: {id: %s, schema_mode: strict%s}\nstorage: {engine: %s, path: %s}\nschemas:\n  collections:\n", id, extra, engine, storage)
	for _, name := range set.names() {
		fmt.Fprintf(&b, "    %s:\n      fields:\n", name)
		for _, column := range set.tables[name] {
			fmt.Fprintf(&b, "        %s: {type: %s}\n", column, relIntFieldType(set.rows[name], column))
		}
	}
	return b.String()
}

// relIntMountSQLite loads the set into a SQLite file. Every table has the integer
// primary key id that an OpenVaultDB SQLite mount keys its records by, so a table
// of the set that has no column of that name gets one (the number of the row).
func relIntMountSQLite(t *testing.T, id, extra string, set relIntSet) *core.Database {
	t.Helper()
	dir := t.TempDir()
	storage, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range set.names() {
		columns := `"id" INTEGER PRIMARY KEY`
		for _, column := range set.tables[name] {
			if column != "id" {
				columns += fmt.Sprintf(`, %q`, column)
			}
		}
		if _, err := storage.Exec(fmt.Sprintf(`CREATE TABLE %q (%s)`, name, columns)); err != nil {
			t.Fatal(err)
		}
		for i, row := range set.rows[name] {
			names, marks, args := `"id"`, "?", []any{i + 1}
			for _, column := range set.tables[name] {
				if column == "id" {
					args[0] = relIntValue(row[column])
					continue
				}
				names += fmt.Sprintf(`, %q`, column)
				marks += ", ?"
				args = append(args, relIntValue(row[column]))
			}
			if _, err := storage.Exec(fmt.Sprintf(`INSERT INTO %q (%s) VALUES (%s)`, name, names, marks), args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	return relIntOpen(t, dir, relIntManifest(id, "sqlite", "data.sqlite", extra, set))
}

// relIntMountInGitDB loads the set into a local inGitDB directory through the mount,
// one record per row, keyed by the number of the row.
func relIntMountInGitDB(t *testing.T, id, extra string, set relIntSet) *core.Database {
	t.Helper()
	db := relIntOpen(t, t.TempDir(), relIntManifest(id, "ingitdb", "data", extra, set))
	for _, name := range set.names() {
		for i, row := range set.rows[name] {
			data := map[string]any{}
			for _, column := range set.tables[name] {
				data[column] = relIntValue(row[column])
			}
			key := record.NewKeyWithID(name, fmt.Sprintf("r%03d", i+1))
			if _, err := db.Apply(context.Background(), []core.Op{{Op: "insert", Key: key, Data: data}}, "seed"); err != nil {
				t.Fatalf("%s row %d: %v", name, i+1, err)
			}
		}
	}
	return db
}

// relIntOpen writes the manifest into dir and mounts it.
func relIntOpen(t *testing.T, dir, manifest string) *core.Database {
	t.Helper()
	path := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// relIntServe serves the mounts and returns the base URL of the server.
func relIntServe(t *testing.T, mounts map[string]*core.Database, opts ...server.Option) string {
	t.Helper()
	service := server.New("test", mounts, opts...)
	t.Cleanup(service.CloseSnapshots)
	host := httptest.NewServer(service.Handler())
	t.Cleanup(host.Close)
	return host.URL
}

var (
	relIntDalgoOnce sync.Once
	relIntDalgoPath string
	relIntDalgoErr  error
)

// relIntDalgoDir is the directory of the DALgo module in the module cache, where
// the DTQL fixtures the tests run live. It asks the go command, so it finds the
// version this module is built with.
func relIntDalgoDir(t *testing.T) string {
	t.Helper()
	relIntDalgoOnce.Do(func() {
		goCommand := "go"
		if path, err := exec.LookPath("go"); err == nil {
			goCommand = path
		}
		out, err := exec.Command(goCommand, "list", "-m", "-f", "{{.Dir}}", "github.com/dal-go/dalgo").Output()
		relIntDalgoPath, relIntDalgoErr = strings.TrimSpace(string(out)), err
	})
	if relIntDalgoErr != nil || relIntDalgoPath == "" {
		t.Fatalf("cannot locate the DALgo module in the module cache: %v", relIntDalgoErr)
	}
	return relIntDalgoPath
}

// relIntRewrite returns doc with the spelling the tests need and nothing else changed:
// the default schema of a source is dropped, and when database is not empty every
// source names it.
func relIntRewrite(t *testing.T, doc []byte, database string) []byte {
	t.Helper()
	return relIntRewriteWith(t, doc, func(int) string { return database })
}

// relIntRewriteWith is relIntRewrite with a database chosen for each source by its
// place in the document: the sources are numbered in the order a person reads them,
// the root source first and then the sources of its joins.
func relIntRewriteWith(t *testing.T, doc []byte, database func(i int) string) []byte {
	t.Helper()
	var root any
	if err := yaml.Unmarshal(doc, &root); err != nil {
		t.Fatal(err)
	}
	i := 0
	relIntWalkSources(root, func(source map[string]any) {
		if source["schema"] == "main" {
			delete(source, "schema")
		}
		if name := database(i); name != "" {
			source["database"] = name
		}
		i++
	})
	out, err := yaml.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// relIntCountSources is how many sources a document names.
func relIntCountSources(t *testing.T, doc []byte) int {
	t.Helper()
	var root any
	if err := yaml.Unmarshal(doc, &root); err != nil {
		t.Fatal(err)
	}
	count := 0
	relIntWalkSources(root, func(map[string]any) { count++ })
	return count
}

// relIntWalkSources calls visit for every source of a document, depth first: at each
// mapping the keys that hold the sources of a query (from, then joins) come before
// the others, which follow in alphabetical order.
func relIntWalkSources(node any, visit func(source map[string]any)) {
	switch x := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for key := range x {
			keys = append(keys, key)
		}
		rank := func(key string) int {
			switch key {
			case "from":
				return 0
			case "joins":
				return 1
			}
			return 2
		}
		sort.Slice(keys, func(a, b int) bool {
			if rank(keys[a]) != rank(keys[b]) {
				return rank(keys[a]) < rank(keys[b])
			}
			return keys[a] < keys[b]
		})
		for _, key := range keys {
			if source, ok := x[key].(map[string]any); ok && key == "from" {
				if _, named := source["name"]; named {
					visit(source)
				}
			}
			relIntWalkSources(x[key], visit)
		}
	case []any:
		for _, item := range x {
			relIntWalkSources(item, visit)
		}
	}
}

// relIntJSONRows decodes the rows of an expectation file.
func relIntJSONRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return rows
}

// relIntCase is one document of a fixture directory and what its fixture says an
// engine returns for it: rows, or the category and path of a diagnostic.
type relIntCase struct {
	id   string
	doc  []byte
	rows []map[string]any
	err  *relIntDiagnostic
}

type relIntDiagnostic struct {
	Category string `json:"category"`
	Path     string `json:"path"`
}

// relIntExpectationElsewhere names the expectation of a document that has no
// companion file of its own. The fixtures of DALgo run chinook-hinted.dtql.yaml
// against the rows of chinook-nested.rows.json, the same query with hints added; the
// rows of the wildcard document are derived by hand from the dataset in testdata.
var relIntExpectationElsewhere = map[string]string{
	"joins/chinook-hinted":   "dalgo:dtql/testdata/joins/chinook-nested.rows.json",
	"joins/chinook-wildcard": "local:testdata/joins-chinook/chinook-wildcard.rows.json",
}

// relIntLoadCases reads every document of a fixture directory with its expectation.
// A document with no expectation fails the test, so a fixture added to the
// directory is never skipped without a word.
func relIntLoadCases(t *testing.T, corpus, dir string) []relIntCase {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var cases []relIntCase
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".dtql.yaml")
		if !ok {
			if name, ok = strings.CutSuffix(entry.Name(), ".dtql.json"); !ok {
				continue
			}
		}
		c := relIntCase{id: corpus + "/" + name}
		if c.doc, err = os.ReadFile(filepath.Join(dir, entry.Name())); err != nil {
			t.Fatal(err)
		}
		rowsPath, errorPath := filepath.Join(dir, name+".rows.json"), filepath.Join(dir, name+".error.json")
		if elsewhere, ok := relIntExpectationElsewhere[c.id]; ok {
			kind, path, _ := strings.Cut(elsewhere, ":")
			if kind == "dalgo" {
				path = filepath.Join(relIntDalgoDir(t), path)
			}
			rowsPath = path
		}
		switch {
		case relIntExists(rowsPath):
			c.rows = relIntJSONRows(t, rowsPath)
		case relIntExists(errorPath):
			raw, err := os.ReadFile(errorPath)
			if err != nil {
				t.Fatal(err)
			}
			c.err = &relIntDiagnostic{}
			if err := json.Unmarshal(raw, c.err); err != nil {
				t.Fatalf("%s: %v", errorPath, err)
			}
		default:
			t.Fatalf("%s has neither a .rows.json nor an .error.json: add an expectation to relIntExpectationElsewhere", c.id)
		}
		cases = append(cases, c)
	}
	return cases
}

func relIntExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// relIntDivergences is testdata/joins-divergences.json: every fixture case this
// server answers differently from its fixture or refuses.
type relIntDivergences struct {
	Note        string             `json:"note"`
	Divergences []relIntDivergence `json:"divergences"`
}

type relIntDivergence struct {
	Case    string   `json:"case"`
	Engines []string `json:"engines"`
	Status  int      `json:"status"`
	Code    string   `json:"code,omitempty"`
	// Message, when set, is a fragment the message of the error answer must hold, so
	// an entry that lists a refusal pins the reason of the refusal and not only its
	// status and code.
	Message string `json:"message,omitempty"`
	// Route, when set, is the route the execution block of the 200 answer must name
	// (database or in-memory), so an entry whose reason names a route pins it.
	Route  string           `json:"route,omitempty"`
	Rows   []map[string]any `json:"rows,omitempty"`
	Reason string           `json:"reason"`
}

func relIntLoadDivergences(t *testing.T) relIntDivergences {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "joins-divergences.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file relIntDivergences
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		t.Fatalf("joins-divergences.json: %v", err)
	}
	return file
}

// relIntEngines are the two engines every fixture runs on, with the database id
// each is mounted as.
var relIntEngines = []struct {
	name, id string
	mount    func(t *testing.T, id, extra string, set relIntSet) *core.Database
}{
	{"sqlite", "litedb", relIntMountSQLite},
	{"ingitdb", "gitdb", relIntMountInGitDB},
}

// relIntMatches reports whether an answer is the one a fixture asks for.
func relIntMatches(t *testing.T, c relIntCase, resp relHTTPResponse) bool {
	t.Helper()
	if c.err != nil {
		return resp.status == http.StatusBadRequest && resp.errorField("code") == "invalid_dtql" &&
			strings.Contains(resp.errorField("message"), c.err.Category+" at "+c.err.Path)
	}
	return resp.status == http.StatusOK && reflect.DeepEqual(resp.rows(t), c.rows)
}

// relIntDescribe is an answer in one line, for a failure message.
func relIntDescribe(resp relHTTPResponse) string {
	if resp.status == http.StatusOK {
		records, _ := resp.body["records"].([]any)
		rows, _ := json.Marshal(records)
		return fmt.Sprintf("status 200, records %s", rows)
	}
	return fmt.Sprintf("status %d %s: %s", resp.status, resp.errorField("code"), resp.errorField("message"))
}

// Every document of DALgo's join and subquery fixtures, posted over HTTP to a SQLite
// mount and to a local inGitDB mount that hold the fixture's dataset, on the
// per-database endpoint and on /v1/dtql. Each answer is the one the fixture asks for,
// or it is listed in testdata/joins-divergences.json with the status it gets; a case
// that is neither fails, and so does an entry that has become green.
func TestRelationalFixtureCorpusOverHTTP(t *testing.T) {
	dalgo := relIntDalgoDir(t)
	file := relIntLoadDivergences(t)
	known := map[string]bool{}
	byCase := map[string][]relIntDivergence{}
	used := map[string]bool{}
	entryFor := func(caseID, engine string) *relIntDivergence {
		for i, candidate := range byCase[caseID] {
			for _, name := range candidate.Engines {
				if name == engine {
					used[caseID+"|"+engine] = true
					return &byCase[caseID][i]
				}
			}
		}
		return nil
	}
	for _, entry := range file.Divergences {
		if strings.TrimSpace(entry.Reason) == "" || strings.ContainsAny(entry.Reason, "\r\n") || !strings.HasSuffix(entry.Reason, ".") {
			t.Errorf("%s: the reason is one sentence on one line that ends in a full stop: %q", entry.Case, entry.Reason)
		}
		if entry.Status < 100 || entry.Status > 599 {
			t.Errorf("%s: status %d", entry.Case, entry.Status)
		}
		if entry.Route != "" && entry.Status != http.StatusOK {
			t.Errorf("%s: only a 200 answer has a route to pin, not status %d", entry.Case, entry.Status)
		}
		if entry.Message != "" && entry.Status == http.StatusOK {
			t.Errorf("%s: only an error answer has a message to pin", entry.Case)
		}
		if len(entry.Engines) == 0 {
			t.Errorf("%s: no engine", entry.Case)
		}
		for _, engine := range entry.Engines {
			key := entry.Case + "|" + engine
			if known[key] {
				t.Errorf("%s on %s is listed twice", entry.Case, engine)
			}
			known[key] = true
		}
		byCase[entry.Case] = append(byCase[entry.Case], entry)
	}
	if strings.TrimSpace(file.Note) == "" {
		t.Error("joins-divergences.json has no header note")
	}

	corpora := []struct {
		name, dir, schema, dataset string
	}{
		{"joins", filepath.Join(dalgo, "dtql", "testdata", "joins"), filepath.Join("testdata", "joins-chinook", "schema.json"), filepath.Join("testdata", "joins-chinook", "dataset.json")},
		{"subqueries", filepath.Join(dalgo, "dtql", "testdata", "subqueries"), filepath.Join(dalgo, "dtql", "testdata", "subqueries", "schema.json"), filepath.Join(dalgo, "dtql", "testdata", "subqueries", "dataset.json")},
	}
	seen := map[string]bool{}
	for _, corpus := range corpora {
		set := relIntLoadSet(t, corpus.schema, corpus.dataset)
		cases := relIntLoadCases(t, corpus.name, corpus.dir)
		if len(cases) == 0 {
			t.Fatalf("no document in %s", corpus.dir)
		}
		mounts := map[string]*core.Database{}
		for _, engine := range relIntEngines {
			mounts[engine.id] = engine.mount(t, engine.id, "", set)
		}
		base := relIntServe(t, mounts)
		for _, c := range cases {
			seen[c.id] = true
			for _, engine := range relIntEngines {
				// The entry is looked up, and marked as used, outside the subtests: a run
				// that selects some of them (go test -run) would otherwise report every
				// entry of the others as one for an engine the fixtures do not run on.
				entry := entryFor(c.id, engine.name)
				for _, endpoint := range []struct{ name, path, database string }{
					{"per-database endpoint", "/v1/databases/" + engine.id + "/dtql", ""},
					{"/v1/dtql", "/v1/dtql", engine.id},
				} {
					t.Run(c.id+" on "+engine.name+", "+endpoint.name, func(t *testing.T) {
						doc := relIntRewrite(t, c.doc, endpoint.database)
						resp := relHTTPPost(t, base, endpoint.path, "", string(doc))
						green := relIntMatches(t, c, resp)
						switch {
						case entry == nil && !green:
							t.Fatalf("the answer is not the fixture's and the case is not in joins-divergences.json: %s", relIntDescribe(resp))
						case entry != nil && green:
							t.Fatalf("the case is in joins-divergences.json and now answers as its fixture says: remove the entry")
						case entry == nil:
							return
						}
						if resp.status != entry.Status || (entry.Code != "" && resp.errorField("code") != entry.Code) {
							t.Fatalf("joins-divergences.json says status %d %s: %s", entry.Status, entry.Code, relIntDescribe(resp))
						}
						if entry.Message != "" && !strings.Contains(resp.errorField("message"), entry.Message) {
							t.Fatalf("joins-divergences.json says the message holds %q: %s", entry.Message, relIntDescribe(resp))
						}
						if entry.Route != "" {
							if route, _ := resp.execution(t)["route"].(string); route != entry.Route {
								t.Fatalf("joins-divergences.json says the route is %q, the answer says %q: %s", entry.Route, route, relIntDescribe(resp))
							}
						}
						if entry.Rows != nil && !reflect.DeepEqual(resp.rows(t), entry.Rows) {
							t.Fatalf("joins-divergences.json lists other rows: %s", relIntDescribe(resp))
						}
					})
				}
			}
		}
	}
	for key := range known {
		caseID, _, _ := strings.Cut(key, "|")
		if !seen[caseID] {
			t.Errorf("joins-divergences.json lists %s, which is not a case of the fixtures", caseID)
		} else if !used[key] {
			t.Errorf("joins-divergences.json lists %s for an engine the fixtures do not run on", key)
		}
	}
}

// relIntInvoices is the size of the fact table of the pushdown proof: twice the
// 10,000 rows that DALgo's in-memory join holds.
const relIntInvoices = 20000

// relIntCountries are the countries of the ten customers of the fact tables; customer
// cN lives in relIntCountries[N%4].
var relIntCountries = []string{"UK", "US", "NL", "IE"}

// relIntCustomerStatements and relIntInvoiceStatements create and fill the two
// tables of the proof. The invoice n belongs to customer c(n%10) and is worth
// n%7+1.
func relIntCustomerStatements() []string {
	statements := []string{`CREATE TABLE "Customer" ("id" TEXT PRIMARY KEY, "name" TEXT, "country" TEXT)`}
	for i := 0; i < 10; i++ {
		statements = append(statements, fmt.Sprintf(`INSERT INTO "Customer" VALUES ('c%d', 'Customer %d', '%s')`, i, i, relIntCountries[i%4]))
	}
	return statements
}

func relIntInvoiceStatements() []string {
	return []string{
		`CREATE TABLE "Invoice" ("id" TEXT PRIMARY KEY, "customer_id" TEXT, "total" REAL)`,
		fmt.Sprintf(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < %d) `+
			`INSERT INTO "Invoice" SELECT printf('%%05d', n), 'c' || (n %% 10), n %% 7 + 1 FROM seq`, relIntInvoices),
	}
}

var (
	relIntCustomerFields = map[string][]string{"Customer": {"id", "name", "country"}}
	relIntInvoiceFields  = map[string][]string{"Invoice": {"id", "customer_id", "total"}}
)

// relIntShop is one SQLite mount that holds both tables.
func relIntShop(t *testing.T, id, extra string) *core.Database {
	t.Helper()
	return relHTTPMount(t, id, extra, map[string][]string{
		"Customer": relIntCustomerFields["Customer"], "Invoice": relIntInvoiceFields["Invoice"],
	}, append(relIntCustomerStatements(), relIntInvoiceStatements()...)...)
}

// relIntRevenue is the revenue and the invoice count per country: the invoices
// joined to their customers, grouped by country, in country order. invoiceDB and
// customerDB name the database of each source, or are empty on the per-database
// endpoint. orderBy adds the ORDER BY that keeps DALgo's streaming aggregate plan
// from serving the document.
func relIntRevenue(invoiceDB, customerDB string, orderBy bool) string {
	source := func(database string) string {
		if database == "" {
			return ""
		}
		return "database: " + database + ", "
	}
	doc := "from: {" + source(invoiceDB) + "name: Invoice, alias: i, joins: [{type: inner, from: {" + source(customerDB) + "name: Customer, alias: c}, " +
		"on: [{left: {field: customer_id, source: i}, op: '==', right: {field: id, source: c}}]}]}\n" +
		"groupBy: [{field: country, source: c}]\n"
	if orderBy {
		doc += "orderBy: [{field: country, source: c}]\n"
	}
	return doc + "columns:\n  - {field: country, source: c}\n" +
		"  - {aggregate: {function: sum, args: [{field: total, source: i}]}, as: revenue}\n" +
		"  - {aggregate: {function: count, args: [{star: true}]}, as: invoices}\n"
}

// relIntExpectedRevenue is what relIntRevenue returns for the fact tables,
// computed here from the formula that generated them.
func relIntExpectedRevenue() []map[string]any {
	revenue, invoices := map[string]float64{}, map[string]float64{}
	for n := 1; n <= relIntInvoices; n++ {
		country := relIntCountries[(n%10)%4]
		revenue[country] += float64(n%7 + 1)
		invoices[country]++
	}
	countries := append([]string(nil), relIntCountries...)
	sort.Strings(countries)
	rows := make([]map[string]any, len(countries))
	for i, country := range countries {
		rows[i] = map[string]any{"country": country, "revenue": revenue[country], "invoices": invoices[country]}
	}
	return rows
}

// A join with GROUP BY over 20,000 invoices is answered by the database (200,
// route database, the right sums). The same document with its two tables in two
// mounts runs in memory, where DALgo's join holds 10,000 rows, and is a 422
// query_budget_exceeded that names a join bound: it shows that the document is more
// than the in-memory engine can serve, so the first answer was computed by SQLite.
func TestAJoinOverTwentyThousandRowsIsPushedDownAndTheSplitQueryIsRefused(t *testing.T) {
	shop := relIntShop(t, "shop", "")
	sales := relHTTPMount(t, "sales", "", relIntInvoiceFields, relIntInvoiceStatements()...)
	people := relHTTPMount(t, "people", "", relIntCustomerFields, relIntCustomerStatements()...)
	base := relIntServe(t, map[string]*core.Database{"shop": shop, "sales": sales, "people": people})

	t.Run("the negative control: the split query is refused", func(t *testing.T) {
		resp := relHTTPPost(t, base, "/v1/dtql", "", relIntRevenue("sales", "people", true))
		detail, _ := resp.body["error"].(map[string]any)
		budget, _ := detail["budget"].(map[string]any)
		name, _ := budget["name"].(string)
		if resp.status != http.StatusUnprocessableEntity || resp.errorField("code") != "query_budget_exceeded" || !strings.HasPrefix(name, "join") ||
			budget["limit"] != float64(10000) || budget["route"] != "in-memory" {
			t.Fatalf("status %d, want a 422 query_budget_exceeded on the 10,000-row join bound of the in-memory route: %s", resp.status, resp.raw)
		}
		if resp.body["records"] != nil || resp.body["execution"] != nil {
			t.Fatalf("a refused query returned a result: %s", resp.raw)
		}
	})
	t.Run("without the ORDER BY the in-memory engine streams the same rows", func(t *testing.T) {
		// The refusal above comes from the ORDER BY, which keeps DALgo's streaming
		// aggregate plan from serving the document, and not from the split or from the
		// data: the document without it is answered in memory from the 20,000 invoices.
		resp := relHTTPPost(t, base, "/v1/dtql", "", relIntRevenue("sales", "people", false))
		if resp.status != http.StatusOK || resp.execution(t)["route"] != "in-memory" {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		rows := resp.rows(t)
		sort.Slice(rows, func(i, j int) bool { return rows[i]["country"].(string) < rows[j]["country"].(string) })
		if want := relIntExpectedRevenue(); !reflect.DeepEqual(rows, want) {
			t.Fatalf("rows = %v, want %v", rows, want)
		}
	})
	t.Run("the same document inside one mount is answered by the database", func(t *testing.T) {
		resp := relHTTPPost(t, base, "/v1/databases/shop/dtql", "", relIntRevenue("", "", true))
		if resp.status != http.StatusOK {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		if got, want := resp.rows(t), relIntExpectedRevenue(); !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
		if got := resp.columns(); !reflect.DeepEqual(got, []string{"country", "revenue", "invoices"}) {
			t.Fatalf("columns = %v", got)
		}
		execution := resp.execution(t)
		if execution["route"] != "database" || execution["rowsReturned"] != float64(len(relIntCountries)) {
			t.Fatalf("execution = %v", execution)
		}
	})
	t.Run("the same document, qualified, on /v1/dtql", func(t *testing.T) {
		resp := relHTTPPost(t, base, "/v1/dtql", "", relIntRevenue("shop", "shop", true))
		if resp.status != http.StatusOK || resp.execution(t)["route"] != "database" || !reflect.DeepEqual(resp.rows(t), relIntExpectedRevenue()) {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
}

// relIntRegions is the countries database of the cross-engine tests: the three
// countries of the customers of relHTTPChinook and one more, France, that has none.
func relIntRegions() relIntSet {
	return relIntSet{
		tables: map[string][]string{"Country": {"code", "name", "region"}},
		rows: map[string][]map[string]any{"Country": {
			{"code": "UK", "name": "United Kingdom", "region": "Europe"},
			{"code": "US", "name": "United States", "region": "Americas"},
			{"code": "NL", "name": "Netherlands", "region": "Europe"},
			{"code": "FR", "name": "France", "region": "Europe"},
		}},
	}
}

// A SQLite mount and a local inGitDB mount on one server, joined by one document
// over /v1/dtql: the rows come from both engines, in either order of the sources, and
// an aggregate over a join of the two engines and a third source is computed above
// both reads.
func TestASQLiteMountIsJoinedToAnInGitDBMountOverHTTP(t *testing.T) {
	chinook := relHTTPChinook(t, "")
	countries := relIntMountInGitDB(t, "countries", "", relIntRegions())
	if got := countries.Engine(); got != "ingitdb" {
		t.Fatalf("the countries mount is %q, want a local inGitDB mount", got)
	}
	base := relIntServe(t, map[string]*core.Database{"chinook": chinook, "countries": countries})

	sourceRows := func(t *testing.T, resp relHTTPResponse) map[string]float64 {
		t.Helper()
		out := map[string]float64{}
		sources, _ := resp.execution(t)["sources"].([]any)
		for _, source := range sources {
			entry := source.(map[string]any)
			out[entry["database"].(string)+"."+entry["collection"].(string)], _ = entry["rows"].(float64)
		}
		return out
	}

	t.Run("a SQLite customer joined to its inGitDB country", func(t *testing.T) {
		resp := relHTTPPost(t, base, "/v1/dtql", "", relHTTPCustomerRegions)
		if resp.status != http.StatusOK {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		want := []map[string]any{
			{"customer": "Ada", "country": "United Kingdom", "region": "Europe"},
			{"customer": "Edsger", "country": "Netherlands", "region": "Europe"},
			{"customer": "Grace", "country": "United States", "region": "Americas"},
		}
		if got := resp.rows(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
		if got := resp.columns(); !reflect.DeepEqual(got, []string{"customer", "country", "region"}) {
			t.Fatalf("columns = %v", got)
		}
		if reads := sourceRows(t, resp); !reflect.DeepEqual(reads, map[string]float64{"chinook.Customer": 3, "countries.Country": 4}) {
			t.Fatalf("source rows = %v", reads)
		}
		if resp.execution(t)["route"] != "in-memory" {
			t.Fatalf("execution = %v", resp.execution(t))
		}
	})
	t.Run("the inGitDB source first, with a left join that keeps the country no customer lives in", func(t *testing.T) {
		const doc = `from:
  database: countries
  name: Country
  alias: k
  joins:
    - type: left
      from: {database: chinook, name: Customer, alias: c}
      on:
        - {left: {field: code, source: k}, op: '==', right: {field: country, source: c}}
orderBy:
  - {field: code, source: k}
columns:
  - {field: code, source: k}
  - {field: name, source: c, as: customer}
`
		resp := relHTTPPost(t, base, "/v1/dtql", "", doc)
		if resp.status != http.StatusOK {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		want := []map[string]any{
			{"code": "FR", "customer": nil},
			{"code": "NL", "customer": "Edsger"},
			{"code": "UK", "customer": "Ada"},
			{"code": "US", "customer": "Grace"},
		}
		if got := resp.rows(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	})
	t.Run("revenue by region over two SQLite tables and an inGitDB one", func(t *testing.T) {
		const doc = `from:
  database: chinook
  name: Invoice
  alias: i
  joins:
    - type: inner
      from: {database: chinook, name: Customer, alias: c}
      on:
        - {left: {field: customer_id, source: i}, op: '==', right: {field: id, source: c}}
    - type: inner
      from: {database: countries, name: Country, alias: k}
      on:
        - {left: {field: country, source: c}, op: '==', right: {field: code, source: k}}
groupBy:
  - {field: region, source: k}
orderBy:
  - {field: region, source: k}
columns:
  - {field: region, source: k}
  - {aggregate: {function: sum, args: [{field: total, source: i}]}, as: revenue}
  - {aggregate: {function: count, args: [{star: true}]}, as: invoices}
`
		resp := relHTTPPost(t, base, "/v1/dtql", "", doc)
		if resp.status != http.StatusOK {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		want := []map[string]any{
			{"region": "Americas", "revenue": float64(5), "invoices": float64(1)},
			{"region": "Europe", "revenue": float64(45), "invoices": float64(4)},
		}
		if got := resp.rows(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
		if reads := sourceRows(t, resp); reads["chinook.Invoice"] != 5 || reads["countries.Country"] == 0 || resp.execution(t)["route"] != "in-memory" {
			t.Fatalf("execution = %v", resp.execution(t))
		}
	})
	t.Run("one inGitDB mount joined to itself runs in memory", func(t *testing.T) {
		const doc = `from:
  database: countries
  name: Country
  alias: a
  joins:
    - type: inner
      from: {database: countries, name: Country, alias: b}
      on:
        - {left: {field: region, source: a}, op: '==', right: {field: region, source: b}}
columns:
  - {field: code, source: a, as: first}
  - {field: code, source: b, as: second}
orderBy:
  - {field: code, source: a}
  - {field: code, source: b}
limit: 3
`
		resp := relHTTPPost(t, base, "/v1/dtql", "", doc)
		if resp.status != http.StatusOK || resp.execution(t)["route"] != "in-memory" {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		want := []map[string]any{{"first": "FR", "second": "FR"}, {"first": "FR", "second": "NL"}, {"first": "FR", "second": "UK"}}
		if got := resp.rows(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	})
}
