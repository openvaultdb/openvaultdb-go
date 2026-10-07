package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

var errFakeReached = errors.New("fake adapter query path reached")

// queryCountingDB is a dal.DB whose only behaviour is to count how often the
// query path is entered. Every other method panics (nil embedded DB), so a
// guard that lets a refused request touch any other adapter path fails loudly.
type queryCountingDB struct {
	dal.DB
	queries int
}

func (f *queryCountingDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	f.queries++
	return nil, errFakeReached
}

func (f *queryCountingDB) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	f.queries++
	return nil, errFakeReached
}

// openEngine opens a fake-backed database that declares the collections the
// tests of this file query: customers, and other for their subqueries.
func openEngine(t *testing.T, engine string) (*Database, *queryCountingDB) {
	t.Helper()
	fake := &queryCountingDB{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "guarded", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
			"other":     {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
		}},
	}
	db, err := Open(m, fake, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db, fake
}

const guardDTQL = "from: {name: customers}\n"

// refusedEntryPoints runs every core entry point that hands a structured query
// to the driver.
func refusedEntryPoints(t *testing.T, db *Database) map[string]error {
	t.Helper()
	ctx := context.Background()
	parsed, _, err := ParseDTQL([]byte(guardDTQL))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]error{}
	_, out["Execute"] = db.Execute(ctx, Query{Collection: "customers"})
	_, out["ExecuteKeysOnly"] = db.Execute(ctx, Query{Collection: "customers", KeysOnly: true})
	_, out["ExecuteDTQL"] = db.ExecuteDTQL(ctx, []byte(guardDTQL))
	_, out["ExecuteDTQLQuery"] = db.ExecuteDTQLQuery(ctx, parsed)
	out["StreamDTQLSnapshot"] = db.StreamDTQLSnapshot(ctx, parsed, func(Record) error { return nil })
	_, _, out["SelectAccessSample"] = db.SelectAccessSample(ctx, parsed, 1, access.Principal{})
	return out
}

func TestStructuredQueryRefusedOnPostgresAndMySQL(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			db, fake := openEngine(t, engine)
			for name, err := range refusedEntryPoints(t, db) {
				if name == "SelectAccessSample" {
					// Refused earlier, by its own engine-specific ordering rule,
					// so it carries no typed refusal; it must still fail.
					if err == nil || !strings.Contains(err.Error(), "sample ordering unsupported") {
						t.Errorf("%s: want its ordering refusal, got %v", name, err)
					}
					continue
				}
				var unsupported *QueryUnsupportedError
				if !errors.Is(err, ErrQueryUnsupported) || !errors.As(err, &unsupported) || unsupported.Engine != engine {
					t.Errorf("%s: want typed refusal naming %q, got %v", name, engine, err)
				}
				if !strings.Contains(err.Error(), engine) || !strings.Contains(err.Error(), "not yet supported") {
					t.Errorf("%s: message must name the engine and say it is not yet supported: %q", name, err)
				}
			}
			if fake.queries != 0 {
				t.Fatalf("adapter query path called %d times", fake.queries)
			}
		})
	}
}

func TestStructuredQueryRefusedOnUnknownEngine(t *testing.T) {
	for _, engine := range []string{"", "oracle"} {
		db, fake := openEngine(t, engine)
		_, err := db.Execute(context.Background(), Query{Collection: "customers"})
		if !errors.Is(err, ErrQueryUnsupported) || fake.queries != 0 {
			t.Errorf("engine %q: err=%v queries=%d", engine, err, fake.queries)
		}
	}
	if err := (&Database{}).guardQuery(); !errors.Is(err, ErrQueryUnsupported) {
		t.Fatalf("database without manifest must be refused: %v", err)
	}
}

func TestStructuredQueryReachesQueryableEngines(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb", "firestore"} {
		t.Run(engine, func(t *testing.T) {
			db, fake := openEngine(t, engine)
			ctx := context.Background()
			parsed, _, err := ParseDTQL([]byte(guardDTQL))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Execute(ctx, Query{Collection: "customers"}); !errors.Is(err, errFakeReached) {
				t.Errorf("Execute: %v", err)
			}
			if _, err := db.ExecuteDTQLQuery(ctx, parsed); !errors.Is(err, errFakeReached) {
				t.Errorf("ExecuteDTQLQuery: %v", err)
			}
			if err := db.StreamDTQLSnapshot(ctx, parsed, func(Record) error { return nil }); !errors.Is(err, errFakeReached) {
				t.Errorf("StreamDTQLSnapshot: %v", err)
			}
			if fake.queries != 3 {
				t.Fatalf("queries = %d, want 3", fake.queries)
			}
		})
	}
}

func TestInGitDBMembershipFiltersRefusedBeforeAdapter(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []string{"ingitdb", "sqlite"} {
		t.Run(engine, func(t *testing.T) {
			db, fake := openEngine(t, engine)
			query := dal.From(rootRef("customers")).NewQuery().
				WhereField("name", dal.In, []string{"Alice", "Bob"}).SelectIntoRecordset()

			if engine == "ingitdb" {
				for label, run := range map[string]func() error{
					"wire query": func() error {
						_, err := db.Execute(ctx, Query{Collection: "customers", Where: []Filter{{Field: "name", Op: "in", Value: []string{"Alice", "Bob"}}}})
						return err
					},
					"DTQL query": func() error {
						_, err := db.ExecuteDTQLQuery(ctx, query)
						return err
					},
					"snapshot": func() error {
						return db.StreamDTQLSnapshot(ctx, query, func(Record) error { return nil })
					},
				} {
					if err := run(); !errors.Is(err, ErrQueryNotRunnable) {
						t.Errorf("%s error = %v, want ErrQueryNotRunnable", label, err)
					}
				}
				if fake.queries != 0 {
					t.Fatalf("adapter query path called %d times for refused IN queries", fake.queries)
				}
				return
			}

			_, err := db.Execute(ctx, Query{Collection: "customers", Where: []Filter{{Field: "name", Op: "in", Value: []string{"Alice", "Bob"}}}})
			if !errors.Is(err, errFakeReached) || fake.queries != 1 {
				t.Fatalf("SQLite membership query: err=%v adapter calls=%d, want supported query to reach adapter", err, fake.queries)
			}
		})
	}
}

func TestQueryUnsupportedErrorIsNotInvalidQuery(t *testing.T) {
	err := error(&QueryUnsupportedError{Engine: "postgres"})
	if errors.Is(err, ErrInvalidQuery) || errors.Is(err, ErrInvalidDTQL) {
		t.Fatal("a refused engine is not a malformed query")
	}
}

// unsafeFieldNames are names that must never reach an adapter on any engine.
var unsafeFieldNames = []string{
	`na"me`, `na'me`, "na me", "name;", "name; DROP TABLE x", "name--", "na--me",
	"name/*", "na/**/me", "name#", "", ".", "a..b", ".name", "name.",
	"name\x00", "na\nme", "na\tme", `[name]`, "`name`", "name)", "(name",
	"name=1", "name OR 1=1", strings.Repeat("a", maxFieldNameLen+1),
	"$", "$$id", "a$$", "-name", "a.-b", "a--b", "a---b", `na\me`, "na\u200bme", "na\u2028me",
	"a/b", "a*b", "a%b", "a,b", "a|b", "a:b", "a<b", "a+b", "a@b", "a!b", "a?b", "a~b", "a^b", "a&b", "a{b", "a}b", "a]b",
	"$id.", "$1", "a.$1", "$1a", "a$b", "a.$b$", "$.a", "\u00a0name", "name\u00a0",
	// A combining mark continues a segment but never starts one.
	"\u0308name", "a.\u0308b", "$\u0308a", "\u093e", "\u0e48a",
}

// safeFieldNames include the key pseudo-field of the document engines ($id),
// hyphenated names and non-ASCII letters, none of which are SQL syntax.
var safeFieldNames = []string{
	"name", "_id", "Name2", "a_b", "address.city", "a.b.c", strings.Repeat("a", maxFieldNameLen),
	"$id", "$id.x", "a.$id", "first-name", "a-b-c", "naïve", "名前", "address.zip-code",
	"1name", "byYear.2024", "1st_line", "2024", "a.1.b",
	// Scripts that need combining marks: Devanagari (U+093E is Mc), Thai
	// (U+0E37 and U+0E48 are Mn) and a decomposed Latin letter (U+0308 is Mn).
	"\u0928\u093e\u092e", "\u0e0a\u0e37\u0e48\u0e2d", "nai\u0308ve", "a.\u0928\u093e\u092e", "$\u0e0a\u0e37\u0e48\u0e2d",
}

// quotedUnsafeFieldNames are the names the quoted-name rule (sqlite, ingitdb,
// firestore) refuses as well: they could end a quoted identifier or a statement,
// are empty or have an empty segment, hold a control character, start or end a
// segment with a space, or are not text.
var quotedUnsafeFieldNames = []string{
	`na"me`, `na'me`, "na`me", `na\me`, "name;", "name; DROP TABLE x", `"; DROP TABLE x; --`,
	"", ".", "a..b", ".name", "name.", "$id.", "a. b", "a .b",
	"name\x00", "na\nme", "na\tme", "na\rme", "na\x7fme", "na\u0085me",
	" name", "name ", "\u00a0name", "name\u00a0", "\u3000name",
	strings.Repeat("a", maxFieldNameLen+1), "a\xffb",
}

// quotedOnlyFieldNames are accepted by the quoted-name rule and refused by the
// strict one. Each class is also queried against a real SQLite file (see
// field_names_sqlite_test.go).
var quotedOnlyFieldNames = []string{
	"zip code", "na me", "a  b", "name--", "na--me", "a---b", "name/*", "na/**/me", "name#", "(name", "name)", "name=1",
	"name OR 1=1", "[name]", "a/b", "a*b", "a%b", "a,b", "a|b", "a:b", "a<b", "a+b", "a@b", "a!b", "a?b", "a~b", "a^b",
	"a&b", "a{b", "a}b", "a]b", "-name", "a.-b", "$", "$$id", "a$$", "$1", "a.$1", "$1a", "a$b", "a.$b$", "$.a",
	"na\u00a0me", "na\u200bme", "na\u2028me", "temp \u00b0C", "emoji \U0001F600", "address.zip code", "a b.c d",
}

func TestValidateFieldName(t *testing.T) {
	for _, name := range unsafeFieldNames {
		if err := ValidateFieldName(name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
	for _, name := range safeFieldNames {
		if err := ValidateFieldName(name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}
}

// unsafeFieldNamesFor returns the names an engine must refuse before any adapter
// call: the strict list, or the shorter list of an engine with the quoted rule.
func unsafeFieldNamesFor(engine string) []string {
	if quotedNameEngines[engine] {
		return quotedUnsafeFieldNames
	}
	return unsafeFieldNames
}

func TestWireQueryFieldNamesValidatedBeforeAdapter(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb", "firestore", "postgres", "mysql", "oracle"} {
		db, fake := openEngine(t, engine)
		for _, name := range unsafeFieldNamesFor(engine) {
			for label, q := range map[string]Query{
				"where":   {Collection: "customers", Where: []Filter{{Field: name, Op: "==", Value: 1}}},
				"orderBy": {Collection: "customers", OrderBy: []OrderBy{{Field: name}}},
			} {
				if _, err := db.Execute(context.Background(), q); !errors.Is(err, ErrInvalidQuery) {
					t.Errorf("%s %s %.30q: %v", engine, label, name, err)
				}
			}
		}
		if fake.queries != 0 {
			t.Fatalf("%s: adapter reached %d times", engine, fake.queries)
		}
	}
}

func TestWireQuerySafeFieldNamesPass(t *testing.T) {
	db, fake := openEngine(t, "sqlite")
	for _, name := range safeFieldNames {
		q := Query{Collection: "customers", Where: []Filter{{Field: name, Op: "==", Value: 1}}, OrderBy: []OrderBy{{Field: name}}}
		if _, err := db.Execute(context.Background(), q); !errors.Is(err, errFakeReached) {
			t.Errorf("%q: %v", name, err)
		}
	}
	if fake.queries != len(safeFieldNames) {
		t.Fatalf("queries = %d", fake.queries)
	}
}

// dtqlWith places one field name in every expression position the walker
// covers, built with the dal builders so the name needs no YAML escaping.
func dtqlWith(name string) map[string]dal.StructuredQuery {
	base := func() dal.IQueryBuilder {
		return dal.From(dal.NewRootCollectionRef("customers", "")).NewQuery()
	}
	build := func(b dal.IQueryBuilder) dal.StructuredQuery {
		return b.SelectIntoRecord(func() record.Record {
			return record.NewRecordWithIncompleteKey("customers", 0, map[string]any{})
		})
	}
	sub := dal.From(dal.NewRootCollectionRef("other", "")).NewQuery().WhereField(name, dal.Equal, 1)
	return map[string]dal.StructuredQuery{
		"where":     build(base().WhereField(name, dal.Equal, 1)),
		"whereIn":   build(base().WhereField(name, dal.In, []string{"a"})),
		"whereRHS":  build(base().Where(dal.NewComparison(dal.Field("ok"), dal.Equal, dal.NewFieldRef("", name)))),
		"orderBy":   build(base().OrderBy(dal.AscendingField(name))),
		"orderDesc": build(base().OrderBy(dal.DescendingField(name))),
		"column":    base().SelectColumns(dal.Column{Expression: dal.Field(name)}),
		"group": build(base().Where(dal.NewGroupCondition(dal.And,
			dal.WhereField("ok", dal.Equal, 1), dal.WhereField(name, dal.Equal, 2)))),
		"binary": build(base().Where(dal.NewComparison(dal.Binary(dal.Field(name), dal.Add, dal.String("x")), dal.Equal, dal.String("y")))),
		"exists": build(base().Where(dal.NewExistsCondition(build(sub)))),
		"scalar": build(base().Where(dal.NewComparison(dal.Field("ok"), dal.Equal, dal.NewQueryExpression(build(sub), "s")))),
	}
}

func TestDTQLFieldNamesValidatedBeforeAdapter(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb", "firestore", "postgres", "mysql", "oracle"} {
		db, fake := openEngine(t, engine)
		for _, name := range unsafeFieldNamesFor(engine) {
			for label, query := range dtqlWith(name) {
				if _, err := db.ExecuteDTQLQuery(context.Background(), query); !errors.Is(err, ErrInvalidDTQL) {
					t.Errorf("%s %s %.30q: %v", engine, label, name, err)
				}
				err := db.StreamDTQLSnapshot(context.Background(), query, func(Record) error { return nil })
				if !errors.Is(err, ErrInvalidDTQL) {
					t.Errorf("stream %s %s %.30q: %v", engine, label, name, err)
				}
			}
		}
		if fake.queries != 0 {
			t.Fatalf("%s: adapter reached %d times", engine, fake.queries)
		}
	}
}

func TestDTQLSafeFieldNamesPass(t *testing.T) {
	db, _ := openEngine(t, "sqlite")
	for _, name := range safeFieldNames {
		for label, query := range dtqlWith(name) {
			if _, err := db.ExecuteDTQLQuery(context.Background(), query); !errors.Is(err, errFakeReached) {
				t.Errorf("%s %q: %v", label, name, err)
			}
		}
	}
}

func TestParseDTQLRefusesUnsafeFieldNames(t *testing.T) {
	for _, doc := range []string{
		"from: {name: customers}\nwhere: {op: '==', left: {field: \"na;me\"}, right: {value: 1}}\n",
		"from: {name: customers}\norderBy: [{field: \"a;b\"}]\n",
		"from: {name: customers}\ncolumns: [{field: \"a'b\"}]\n",
	} {
		if _, _, err := ParseDTQL([]byte(doc)); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("%q: %v", doc, err)
		}
	}
	if _, _, err := ParseDTQL([]byte("from: {name: customers}\nwhere: {op: '==', left: {field: name}, right: {value: 1}}\norderBy: [{field: address.city}]\n")); err != nil {
		t.Fatal(err)
	}
}

type otherCondition struct{}

func (otherCondition) String() string { return "other" }

type otherExpression struct{}

func (otherExpression) String() string { return "other" }

func TestValidateDTQLFieldsFailsClosedOnUnknownShapes(t *testing.T) {
	if err := (nameWalker{}).condition(otherCondition{}, 0, nil); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("unknown condition: %v", err)
	}
	if err := (nameWalker{}).expression(otherExpression{}, 0, nil); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("unknown expression: %v", err)
	}
	if err := (nameWalker{}).expression(dal.NewComparison(nil, dal.Equal, nil), 0, nil); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("comparison used as expression: %v", err)
	}
	if err := (nameWalker{}).expression(dal.Param{Name: "bad name"}, 0, nil); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("bad param: %v", err)
	}
	if err := (nameWalker{}).expression(dal.NewParam("currentUser"), 0, nil); err != nil {
		t.Errorf("good param: %v", err)
	}
	if err := (nameWalker{}).condition(dal.NewComparison(dal.Field("a"), dal.In, dal.Array{Value: []string{"x"}}), 0, nil); err != nil {
		t.Errorf("array: %v", err)
	}
	if err := (nameWalker{}).condition(nil, 0, nil); err != nil {
		t.Errorf("nil condition: %v", err)
	}
	if err := (nameWalker{}).expression(nil, 0, nil); err != nil {
		t.Errorf("nil expression: %v", err)
	}
}

func TestValidateDTQLFieldsAggregatesAliasesSourcesAndDepth(t *testing.T) {
	root := dal.From(dal.NewRootCollectionRef("customers", "")).NewQuery()
	build := func(b dal.IQueryBuilder) dal.StructuredQuery {
		return b.SelectIntoRecord(func() record.Record {
			return record.NewRecordWithIncompleteKey("customers", 0, map[string]any{})
		})
	}
	if err := validateDTQLFields(root.SelectColumns(dal.CountAs(dal.Field("ok"), "")), 0); err != nil {
		t.Errorf("aggregate: %v", err)
	}
	if err := validateDTQLFields(root.SelectColumns(dal.CountAs(dal.Field("a b"), "")), 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("aggregate argument: %v", err)
	}
	if err := validateDTQLFields(root.SelectColumns(dal.Count()), 0); err != nil {
		t.Errorf("count star: %v", err)
	}
	if err := validateDTQLFields(root.SelectColumns(dal.Column{Expression: dal.Field("ok"), Alias: "ok_alias"}), 0); err != nil {
		t.Errorf("alias: %v", err)
	}
	for _, alias := range []string{"a b", "a.b", "a;"} {
		if err := validateDTQLFields(root.SelectColumns(dal.Column{Expression: dal.Field("ok"), Alias: alias}), 0); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("alias %q: %v", alias, err)
		}
	}
	if err := validateDTQLFields(root.SelectColumns(dal.AllColumnsExceptFrom("c", "x")), 0); err != nil {
		t.Errorf("wildcard source: %v", err)
	}
	if err := validateDTQLFields(root.SelectColumns(dal.AllColumnsExceptFrom("c d", "x")), 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("wildcard source: %v", err)
	}
	if err := (nameWalker{}).expression(dal.NewFieldRef("c", "x"), 0, nil); err != nil {
		t.Errorf("qualified field: %v", err)
	}
	if err := (nameWalker{}).expression(dal.NewFieldRef("c;", "x"), 0, nil); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("qualified field source: %v", err)
	}
	// Deeply nested conditions are refused rather than walked without bound.
	deep := dal.WhereField("a", dal.Equal, 1)
	for i := 0; i <= maxQueryTreeDepth+1; i++ {
		deep = dal.NewGroupCondition(dal.And, deep)
	}
	if err := (nameWalker{}).condition(deep, 0, nil); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("deep condition: %v", err)
	}
	var deepExpr dal.Expression = dal.Field("a")
	for i := 0; i <= maxQueryTreeDepth+1; i++ {
		deepExpr = dal.Binary(deepExpr, dal.Add, dal.String("x"))
	}
	if err := (nameWalker{}).expression(deepExpr, 0, nil); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("deep expression: %v", err)
	}
	if err := validateDTQLFields(build(root), maxQueryTreeDepth+1); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("deep query: %v", err)
	}
}

func TestValidateDTQLFieldsCoversEveryClause(t *testing.T) {
	root := func() dal.IQueryBuilder { return dal.From(dal.NewRootCollectionRef("customers", "")).NewQuery() }
	bad := dal.Field("a b")
	for label, query := range map[string]dal.StructuredQuery{
		"having":          root().Having(dal.NewComparison(bad, dal.Equal, dal.String("x"))).SelectColumns(dal.Count()),
		"groupBy":         root().GroupBy(bad).SelectColumns(dal.Count()),
		"wildcardExclude": root().SelectColumns(dal.AllColumnsExcept("ok", "a b")),
		"columnShape":     root().SelectColumns(dal.Column{Expression: otherExpression{}}),
	} {
		if err := validateDTQLFields(query, 0); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("%s: %v", label, err)
		}
	}
	good := root().GroupBy(dal.Field("ok")).Having(dal.NewComparison(dal.Field("ok"), dal.Equal, dal.String("x"))).SelectColumns(dal.AllColumnsExcept("x"))
	if err := validateDTQLFields(good, 0); err != nil {
		t.Errorf("good: %v", err)
	}
}

// TestDocumentEngineKeyPseudoFieldReachesAdapter is the regression test for
// the over-strict identifier rule: ordering or filtering by the inGitDB $id
// key pseudo-field, and firestore names that are not ASCII identifiers, were
// valid before the guard and must still reach the adapter.
func TestDocumentEngineKeyPseudoFieldReachesAdapter(t *testing.T) {
	for _, engine := range []string{"ingitdb", "firestore", "sqlite"} {
		t.Run(engine, func(t *testing.T) {
			db, fake := openEngine(t, engine)
			ctx := context.Background()
			for _, name := range []string{"$id", "first-name", "naïve"} {
				q := Query{Collection: "customers", Where: []Filter{{Field: name, Op: ">", Value: "a"}}, OrderBy: []OrderBy{{Field: name}}}
				if _, err := db.Execute(ctx, q); !errors.Is(err, errFakeReached) {
					t.Errorf("wire %q: %v", name, err)
				}
				root := dal.From(dal.NewRootCollectionRef("customers", "")).NewQuery()
				dq := root.WhereField(name, dal.Equal, "x").OrderBy(dal.AscendingField(name)).SelectColumns(dal.Column{Expression: dal.Field(name)})
				if _, err := db.ExecuteDTQLQuery(ctx, dq); !errors.Is(err, errFakeReached) {
					t.Errorf("dtql %q: %v", name, err)
				}
			}
			if fake.queries != 6 {
				t.Fatalf("queries = %d, want 6", fake.queries)
			}
		})
	}
}

func TestCanQueryFollowsTheGuardAllowList(t *testing.T) {
	for engine, want := range map[string]bool{
		"sqlite": true, "ingitdb": true, "firestore": true,
		"postgres": false, "mysql": false, "oracle": false, "": false,
	} {
		db, _ := openEngine(t, engine)
		if got := db.CanQuery(); got != want {
			t.Errorf("%q: CanQuery = %v, want %v", engine, got, want)
		}
		if got := db.guardQuery() == nil; got != want {
			t.Errorf("%q: guardQuery allows = %v, want %v", engine, got, want)
		}
	}
	if (&Database{}).CanQuery() {
		t.Error("database without manifest must not advertise query")
	}
}

// selectQuery finishes a builder into a structured query.
func selectQuery(b dal.IQueryBuilder) dal.StructuredQuery {
	return b.SelectIntoRecord(func() record.Record {
		return record.NewRecordWithIncompleteKey("customers", 0, map[string]any{})
	})
}

// fromTree builds a from clause with the given joins.
func fromTree(base dal.RecordsetSource, joins ...dal.JoinedSource) dal.FromSource {
	f := dal.From(base)
	for _, j := range joins {
		f.Join(j)
	}
	return f
}

func rootRef(name string) dal.CollectionRef { return dal.NewRootCollectionRef(name, "") }

func eqCondition(left, right dal.Expression) dal.Condition {
	return dal.NewComparison(left, dal.Equal, right)
}

// withExists wraps inner in an EXISTS predicate of an outer query over
// outerCollection, so inner is walked as a subquery.
func withExists(outerCollection string, inner dal.StructuredQuery) dal.StructuredQuery {
	return selectQuery(fromTree(rootRef(outerCollection)).NewQuery().Where(dal.NewExistsCondition(inner)))
}

// noFromQuery is a structured query whose From() is nil.
type noFromQuery struct{ dal.StructuredQuery }

func (noFromQuery) From() dal.FromSource { return nil }

// TestValidateDTQLFieldsWalksFromSources is the regression test for the
// unwalked from clause: every name a subquery's source carries (collection,
// alias, schema, scan order, join source, join ON operands, derived query)
// is validated, and an unknown source shape is refused.
func TestValidateDTQLFieldsWalksFromSources(t *testing.T) {
	const bad = "na'me; DROP TABLE x --"
	badOrder := dal.AscendingField(bad)
	valid := func() dal.StructuredQuery { return selectQuery(fromTree(rootRef("other")).NewQuery()) }
	badJoinOn := func(left, right dal.Expression) dal.JoinedSource {
		return dal.NewJoinedSource(rootRef("o2"), dal.JoinInner, eqCondition(left, right))
	}
	cyclic := fromTree(rootRef("other"))
	cyclic.Join(dal.NewJoinedFrom(cyclic, dal.JoinInner, eqCondition(dal.Field("a"), dal.Field("b"))))

	cases := map[string]dal.StructuredQuery{
		"base scan order":        selectQuery(fromTree(rootRef("other").WithScan(1, badOrder)).NewQuery()),
		"base alias":             selectQuery(fromTree(dal.NewRootCollectionRef("other", "a b")).NewQuery()),
		"base name":              selectQuery(fromTree(rootRef("ot\x00her")).NewQuery()),
		"base schema":            selectQuery(fromTree(dal.NewQualifiedRootCollectionRef("s", "other", "")).NewQuery()),
		"base database":          selectQuery(fromTree(dal.NewDatabaseCollectionRef("d", "", "other", "")).NewQuery()),
		"base parent":            selectQuery(fromTree(dal.NewCollectionRef("other", "", record.NewKeyWithID("p", "1"))).NewQuery()),
		"join on left":           selectQuery(fromTree(rootRef("other"), badJoinOn(dal.Field(bad), dal.Field("b"))).NewQuery()),
		"join on right":          selectQuery(fromTree(rootRef("other"), badJoinOn(dal.Field("a"), dal.NewFieldRef("", bad))).NewQuery()),
		"join on qualifier":      selectQuery(fromTree(rootRef("other"), badJoinOn(dal.NewFieldRef("x y", "a"), dal.Field("b"))).NewQuery()),
		"join on unknown":        selectQuery(fromTree(rootRef("other"), dal.NewJoinedSource(rootRef("o2"), dal.JoinInner, otherCondition{})).NewQuery()),
		"join source scan order": selectQuery(fromTree(rootRef("other"), dal.NewJoinedSource(rootRef("o2").WithScan(1, badOrder), dal.JoinInner)).NewQuery()),
		"join source alias":      selectQuery(fromTree(rootRef("other"), dal.NewJoinedSource(dal.NewRootCollectionRef("o2", "a;b"), dal.JoinInner)).NewQuery()),
		"join source name":       selectQuery(fromTree(rootRef("other"), dal.NewJoinedSource(rootRef("o\x012"), dal.JoinInner)).NewQuery()),
		"join source missing":    selectQuery(fromTree(rootRef("other"), dal.NewNestedJoinedSource(nil, dal.JoinInner)).NewQuery()),
		"nested join on": selectQuery(fromTree(rootRef("other"), dal.NewJoinedFrom(
			fromTree(rootRef("o2"), badJoinOn(dal.Field(bad), dal.Field("b"))), dal.JoinInner, eqCondition(dal.Field("a"), dal.Field("b")))).NewQuery()),
		"nested join cycle": selectQuery(cyclic.NewQuery()),
		"derived where": selectQuery(fromTree(dal.NewQuerySource(
			selectQuery(fromTree(rootRef("o2")).NewQuery().WhereField(bad, dal.Equal, 1)), "d")).NewQuery()),
		"derived from":  selectQuery(fromTree(dal.NewQuerySource(selectQuery(fromTree(rootRef("o2").WithScan(1, badOrder)).NewQuery()), "d")).NewQuery()),
		"derived alias": selectQuery(fromTree(dal.NewQuerySource(valid(), "d e")).NewQuery()),
		"derived nil":   selectQuery(fromTree(dal.NewQuerySource(nil, "d")).NewQuery()),
		"joined derived": selectQuery(fromTree(rootRef("other"), dal.NewJoinedSource(dal.NewQuerySource(
			selectQuery(fromTree(rootRef("o2")).NewQuery().WhereField(bad, dal.Equal, 1)), "d"), dal.JoinInner)).NewQuery()),
		"unknown source": selectQuery(fromTree(dal.NewCollectionGroupRef("g", "")).NewQuery()),
		"pointer source": selectQuery(fromTree(&dal.CollectionRef{}).NewQuery()),
		"missing source": selectQuery(fromTree(nil).NewQuery()),
		"missing from":   noFromQuery{valid()},
		"missing query":  nil,
		"nested missing": selectQuery(fromTree(dal.NewQuerySource(noFromQuery{valid()}, "d")).NewQuery()),
		"scalar from":    selectQuery(fromTree(rootRef("customers")).NewQuery().Where(eqCondition(dal.Field("a"), dal.NewQueryExpression(selectQuery(fromTree(rootRef("o2").WithScan(1, badOrder)).NewQuery()), "s")))),
	}
	for label, inner := range cases {
		if err := validateDTQLFields(withExists("customers", inner), 0); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("%s: %v", label, err)
		}
		if label != "nested join cycle" {
			// The subquery is also refused when it is the query itself.
			if err := validateDTQLFields(inner, 0); !errors.Is(err, ErrInvalidDTQL) {
				t.Errorf("%s (top): %v", label, err)
			}
		}
	}
}

func TestValidateDTQLFieldsAcceptsValidFromSources(t *testing.T) {
	good := map[string]dal.StructuredQuery{
		"scan order":     selectQuery(fromTree(rootRef("other").WithScan(5, dal.AscendingField("name"))).NewQuery()),
		"alias":          selectQuery(fromTree(dal.NewRootCollectionRef("other", "o")).NewQuery()),
		"join":           selectQuery(fromTree(rootRef("a"), dal.NewJoinedSource(dal.NewRootCollectionRef("b", "bb"), dal.JoinLeft, eqCondition(dal.NewFieldRef("a", "id"), dal.NewFieldRef("bb", "a_id")))).NewQuery()),
		"nested join":    selectQuery(fromTree(rootRef("a"), dal.NewJoinedFrom(fromTree(rootRef("b"), dal.NewJoinedSource(rootRef("c"), dal.JoinInner, eqCondition(dal.NewFieldRef("b", "id"), dal.NewFieldRef("c", "b_id")))), dal.JoinInner, eqCondition(dal.NewFieldRef("a", "id"), dal.NewFieldRef("c", "a_id")))).NewQuery()),
		"derived source": selectQuery(fromTree(dal.NewQuerySource(selectQuery(fromTree(rootRef("o2")).NewQuery().WhereField("x", dal.Equal, 1)), "d")).NewQuery()),
	}
	for label, inner := range good {
		if err := validateDTQLFields(inner, 0); err != nil {
			t.Errorf("%s: %v", label, err)
		}
		if err := validateDTQLFields(withExists("customers", inner), 0); err != nil {
			t.Errorf("%s in EXISTS: %v", label, err)
		}
	}
}

// TestSourceQualifierMayBeAnInScopeCollectionName covers the second round
// regression: a field qualified with the name of a collection that is not an
// ASCII identifier ("Order Details", "order-items") worked before the guard.
func TestSourceQualifierMayBeAnInScopeCollectionName(t *testing.T) {
	const spaced, hyphen = "Order Details", "order-items"
	qualified := func(source string) dal.StructuredQuery {
		return selectQuery(fromTree(rootRef(spaced)).NewQuery().Where(eqCondition(dal.NewFieldRef(source, "Quantity"), dal.String("x"))))
	}
	if err := validateDTQLFields(qualified(spaced), 0); err != nil {
		t.Errorf("own collection name: %v", err)
	}
	if err := validateDTQLFields(qualified("Other Details"), 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("name of no source in scope: %v", err)
	}
	if err := validateDTQLFields(qualified("a'b"), 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("unsafe qualifier: %v", err)
	}
	// A column wildcard qualified by the collection name.
	if err := validateDTQLFields(fromTree(rootRef(spaced)).NewQuery().SelectColumns(dal.AllColumnsExceptFrom(spaced, "x")), 0); err != nil {
		t.Errorf("wildcard qualified by own collection name: %v", err)
	}
	if err := validateDTQLFields(fromTree(rootRef(spaced)).NewQuery().SelectColumns(dal.AllColumnsExceptFrom("Other Details", "x")), 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("wildcard qualified by a name not in scope: %v", err)
	}
	// A join source and an outer collection are in scope inside a subquery.
	joined := selectQuery(fromTree(rootRef("customers"), dal.NewJoinedSource(rootRef(hyphen), dal.JoinInner,
		eqCondition(dal.NewFieldRef("customers", "id"), dal.NewFieldRef(hyphen, "customer_id")))).NewQuery())
	if err := validateDTQLFields(joined, 0); err != nil {
		t.Errorf("join source qualifier: %v", err)
	}
	inner := selectQuery(fromTree(rootRef("other")).NewQuery().Where(eqCondition(dal.NewFieldRef(spaced, "id"), dal.NewFieldRef("other", "id"))))
	if err := validateDTQLFields(withExists(spaced, inner), 0); err != nil {
		t.Errorf("outer collection qualifier inside a subquery: %v", err)
	}
	if err := validateDTQLFields(withExists("customers", inner), 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("qualifier of a collection that is not in scope: %v", err)
	}
}

func TestParseDTQLRefusesQualifiedAndScannedTopLevelSource(t *testing.T) {
	for label, doc := range map[string]string{
		"scan":          "from: {name: customers, scan: {limit: 1, orderBy: [{field: \"na'me; DROP TABLE x --\"}]}}\n",
		"safe scan":     "from: {name: customers, scan: {limit: 1, orderBy: [{field: name}]}}\n",
		"schema":        "from: {name: customers, schema: s}\n",
		"database":      "from: {name: customers, database: d}\n",
		"database+schm": "from: {name: customers, database: d, schema: s}\n",
	} {
		if _, _, err := ParseDTQL([]byte(doc)); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("%s: %v", label, err)
		}
	}
}

// TestNameWalkerVariantsDifferInTwoPlaces pins what the relational variant
// changes and what it leaves alone: a database on a source is accepted, an IS
// NULL test is walked, and every name rule still applies.
func TestNameWalkerVariantsDifferInTwoPlaces(t *testing.T) {
	onDatabase := selectQuery(fromTree(dal.NewDatabaseCollectionRef("chinook", "", "Customer", "")).NewQuery())
	if err := validateDTQLFields(onDatabase, 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("single-collection variant, source with a database: %v", err)
	}
	if err := validateRelationalNames(onDatabase); err != nil {
		t.Errorf("relational variant, source with a database: %v", err)
	}

	isNull := func(operand dal.Expression, negated bool) dal.StructuredQuery {
		condition := dal.NewIsNullCondition(operand)
		if negated {
			condition = dal.NewIsNotNullCondition(operand)
		}
		return selectQuery(fromTree(rootRef("customers")).NewQuery().Where(condition))
	}
	for _, negated := range []bool{false, true} {
		if err := validateDTQLFields(isNull(dal.Field("a"), negated), 0); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("single-collection variant, null test (negated %v): %v", negated, err)
		}
		if err := validateRelationalNames(isNull(dal.Field("a"), negated)); err != nil {
			t.Errorf("relational variant, null test (negated %v): %v", negated, err)
		}
		if err := validateRelationalNames(isNull(dal.Field("a;b"), negated)); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("relational variant, null test of an unsafe field (negated %v): %v", negated, err)
		}
	}
	// A null test nested in a group and in a subquery is walked too.
	nested := selectQuery(fromTree(rootRef("customers")).NewQuery().Where(dal.NewGroupCondition(dal.And,
		dal.NewComparison(dal.Field("ok"), dal.Equal, dal.String("x")),
		dal.NewExistsCondition(selectQuery(fromTree(rootRef("other")).NewQuery().Where(dal.NewIsNotNullCondition(dal.Field("a;b"))))))))
	if err := validateRelationalNames(nested); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("relational variant, nested null test: %v", err)
	}

	// Everything else is shared: parent, invalid schema identifiers, names,
	// aliases and depth. A valid schema-qualified source is now accepted by the
	// relational classifier and checked against a native PostgreSQL catalog at
	// execution time.
	for label, query := range map[string]dal.StructuredQuery{
		"parent":       selectQuery(fromTree(dal.NewCollectionRef("other", "", record.NewKeyWithID("p", "1"))).NewQuery()),
		"schema":       selectQuery(fromTree(dal.NewQualifiedRootCollectionRef("s\x00", "other", "")).NewQuery()),
		"name":         selectQuery(fromTree(rootRef("ot\x00her")).NewQuery()),
		"alias":        selectQuery(fromTree(dal.NewDatabaseCollectionRef("d", "", "other", "a b")).NewQuery()),
		"unsafe field": selectQuery(fromTree(dal.NewDatabaseCollectionRef("d", "", "other", "")).NewQuery().WhereField("a;b", dal.Equal, 1)),
	} {
		if err := validateRelationalNames(query); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("relational variant, %s: %v", label, err)
		}
	}
	if err := (nameWalker{relational: true}).query(selectQuery(fromTree(rootRef("customers")).NewQuery()), maxQueryTreeDepth+1, nil); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("relational variant, deep query: %v", err)
	}
}

// TestParseDTQLRefusesNullTestsThatNoAdapterImplements pins what dalgo
// v0.89.1 (which parses isNull and isNotNull) does to today's endpoint: no
// adapter pinned here implements the node, so ParseDTQL refuses it with
// ErrInvalidDTQL, a 400, instead of handing it to an adapter.
func TestParseDTQLRefusesNullTestsThatNoAdapterImplements(t *testing.T) {
	for label, doc := range map[string]string{
		"isNull":     "from: {name: customers}\nwhere: {isNull: {field: x}}\n",
		"isNotNull":  "from: {name: customers}\nwhere: {isNotNull: {field: x}}\n",
		"in a group": "from: {name: customers}\nwhere:\n  and:\n    - op: '=='\n      left: {field: a}\n      right: {value: 1}\n    - isNull: {field: x}\n",
	} {
		if _, _, err := ParseDTQL([]byte(doc)); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("%s: %v", label, err)
		}
	}
}

// TestNameWalkerChecksTheStringsThatAreNotFieldNames covers three
// caller-supplied strings /dtql carries besides names: the operator of an
// arithmetic expression, the name of an aggregate function (one of the five of the profile), and the result name
// of a scalar subquery. None is exploitable today (the SQLite compiler
// allow-lists operators and function names, no adapter compiles a subquery),
// but the legacy text emitter would write all three into SQL.
func TestNameWalkerChecksTheStringsThatAreNotFieldNames(t *testing.T) {
	const injected = "x; --"
	where := func(left dal.Expression) dal.StructuredQuery {
		return selectQuery(fromTree(rootRef("customers")).NewQuery().Where(dal.NewComparison(left, dal.Equal, dal.String("y"))))
	}
	sub := selectQuery(fromTree(rootRef("other")).NewQuery())
	for label, query := range map[string]dal.StructuredQuery{
		"binary operator":           where(dal.Binary(dal.Field("a"), dal.ArithmeticOperator(injected), dal.String("1"))),
		"nested binary operator":    where(dal.Binary(dal.Binary(dal.Field("a"), dal.Add, dal.String("1")), dal.ArithmeticOperator("%"), dal.String("1"))),
		"empty binary operator":     where(dal.Binary(dal.Field("a"), dal.ArithmeticOperator(""), dal.String("1"))),
		"aggregate function":        where(dal.NewAggregate(injected, false, dal.Field("a"))),
		"unknown aggregate":         where(dal.NewAggregate("MEDIAN", false, dal.Field("a"))),
		"empty aggregate":           where(dal.NewAggregate("", false, dal.Field("a"))),
		"aggregate in a scan order": selectQuery(fromTree(rootRef("customers").WithScan(1, dal.Ascending(dal.NewAggregate(injected, false, dal.Field("a"))))).NewQuery()),
		"aggregate in a join on": selectQuery(fromTree(rootRef("customers"), dal.NewJoinedSource(rootRef("o2"), dal.JoinInner,
			eqCondition(dal.NewAggregate(injected, false, dal.Field("a")), dal.Field("b")))).NewQuery()),
		"scalar subquery name":     where(dal.NewQueryExpression(sub, injected)),
		"scalar subquery in a col": fromTree(rootRef("customers")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewQueryExpression(sub, "a b")}),
	} {
		if err := validateDTQLFields(query, 0); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("single-collection variant, %s: %v", label, err)
		}
		if err := validateRelationalNames(query); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("relational variant, %s: %v", label, err)
		}
	}
	for _, operator := range []dal.ArithmeticOperator{dal.Add, dal.Subtract, dal.Multiply, dal.Divide} {
		if err := validateDTQLFields(where(dal.Binary(dal.Field("a"), operator, dal.String("1"))), 0); err != nil {
			t.Errorf("operator %q: %v", operator, err)
		}
	}
	for _, name := range []string{"COUNT", "SUM", "AVG", "MIN", "MAX", "sum", "Avg", "max"} {
		if err := validateDTQLFields(where(dal.NewAggregate(name, false, dal.Field("a"))), 0); err != nil {
			t.Errorf("aggregate %q: %v", name, err)
		}
	}
	// first and last are not in the profile: the name walk refuses them as the
	// classifier does.
	for _, name := range []string{"FIRST", "LAST", "first", "Last"} {
		if err := validateDTQLFields(where(dal.NewAggregate(name, false, dal.Field("a"))), 0); !errors.Is(err, ErrInvalidDTQL) || !strings.Contains(err.Error(), strings.ToLower(name)+" is not in the relational profile") {
			t.Errorf("aggregate %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "s", "total_2"} {
		if err := validateDTQLFields(where(dal.NewQueryExpression(sub, name)), 0); err != nil {
			t.Errorf("scalar subquery name %q: %v", name, err)
		}
	}
}

// TestValidateIdentifierDoesNotEchoAnOverLongName: an alias or qualifier over
// the length limit is answered with the limit, not with the caller's text,
// which can be as large as the request body. ValidateFieldName already does
// the same.
func TestValidateIdentifierDoesNotEchoAnOverLongName(t *testing.T) {
	long := strings.Repeat("a", maxFieldNameLen+1)
	err := validateIdentifier(long)
	if !errors.Is(err, ErrInvalidDTQL) {
		t.Fatalf("err = %v, want ErrInvalidDTQL", err)
	}
	if strings.Contains(err.Error(), "aaaa") || !strings.Contains(err.Error(), "256") {
		t.Fatalf("message must state the limit and omit the name: %q", err)
	}
	// An over-long name that is not an identifier is still refused without the echo.
	err = validateIdentifier(strings.Repeat("a b", maxFieldNameLen))
	if !errors.Is(err, ErrInvalidDTQL) || strings.Contains(err.Error(), "a b") {
		t.Fatalf("err = %v", err)
	}
	// A short refused name is still quoted, so the caller can see which one it was.
	if err := validateIdentifier("a b"); !errors.Is(err, ErrInvalidDTQL) || !strings.Contains(err.Error(), `"a b"`) {
		t.Fatalf("err = %v", err)
	}
	if err := validateIdentifier(strings.Repeat("a", maxFieldNameLen)); err != nil {
		t.Fatalf("a name at the limit: %v", err)
	}
	// The same through a source qualifier that names no source in scope.
	if err := validateQualifier(long, nil); !errors.Is(err, ErrInvalidDTQL) || strings.Contains(err.Error(), "aaaa") {
		t.Fatalf("qualifier err = %v", err)
	}
}

func TestValidateQuotedFieldName(t *testing.T) {
	for _, name := range quotedUnsafeFieldNames {
		if err := validateQuotedFieldName(name); err == nil {
			t.Errorf("%.40q accepted", name)
		}
	}
	for _, name := range quotedOnlyFieldNames {
		if err := validateQuotedFieldName(name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
		if err := ValidateFieldName(name); err == nil {
			t.Errorf("%q is in quotedOnlyFieldNames but the strict rule accepts it", name)
		}
	}
	// The quoted rule is wider: whatever the strict rule accepts, it accepts.
	for _, name := range safeFieldNames {
		if err := validateQuotedFieldName(name); err != nil {
			t.Errorf("%q is plain but refused: %v", name, err)
		}
	}
	// And the strict rule refuses everything the quoted one refuses.
	for _, name := range quotedUnsafeFieldNames {
		if err := ValidateFieldName(name); err == nil {
			t.Errorf("%.40q is refused by the quoted rule but accepted by the strict one", name)
		}
	}
	// The limit is on bytes, and an over-long name is not echoed.
	if err := validateQuotedFieldName(strings.Repeat("a", maxFieldNameLen)); err != nil {
		t.Errorf("a name at the limit: %v", err)
	}
	if err := validateQuotedFieldName(strings.Repeat("a b", maxFieldNameLen)); err == nil || strings.Contains(err.Error(), "a ba b") {
		t.Errorf("over-long name: %v", err)
	}
	if err := validateQuotedFieldName(strings.Repeat("\u00e9", maxFieldNameLen/2+1)); err == nil {
		t.Error("257 bytes of two-byte letters accepted")
	}
}

func TestFieldRuleFollowsTheEngine(t *testing.T) {
	for engine, want := range map[string]fieldRule{
		"sqlite": quotedNames, "ingitdb": quotedNames,
		"firestore": strictNames, "postgres": strictNames, "mysql": strictNames, "oracle": strictNames, "": strictNames,
	} {
		db, _ := openEngine(t, engine)
		if got := db.fieldRule(); got != want {
			t.Errorf("%q: rule = %v, want %v", engine, got, want)
		}
	}
	if got := (&Database{}).fieldRule(); got != strictNames {
		t.Errorf("a database without a manifest must take the strict rule, got %v", got)
	}
	// An engine takes the wide rule only if it can be queried at all.
	for engine := range quotedNameEngines {
		if !queryEngines[engine] {
			t.Errorf("%q has the quoted-name rule but is not cleared for queries", engine)
		}
	}
	// The strict rule stays on the two engines the guard refuses for the legacy
	// text emitter, and on firestore until its own name rules are tested: its
	// client rejects the characters ~ * / [ ] in a field path, which the quoted
	// rule accepts.
	for _, engine := range []string{"postgres", "mysql", "firestore"} {
		if quotedNameEngines[engine] {
			t.Errorf("%q must keep the strict rule", engine)
		}
	}
}

// TestEnginesThatQuoteAcceptNamesTheStrictRuleRefuses: a name with a space or
// punctuation reaches the adapter on sqlite and ingitdb, from the wire query and
// from DTQL, and is refused with no adapter call on every other engine
// (firestore, postgres and mysql among them).
func TestEnginesThatQuoteAcceptNamesTheStrictRuleRefuses(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb", "firestore", "postgres", "mysql", "oracle", ""} {
		t.Run(engine, func(t *testing.T) {
			db, fake := openEngine(t, engine)
			ctx := context.Background()
			quoted := quotedNameEngines[engine]
			reached := 0
			for _, name := range quotedOnlyFieldNames {
				_, wireErr := db.Execute(ctx, Query{Collection: "customers", Where: []Filter{{Field: name, Op: "==", Value: 1}}, OrderBy: []OrderBy{{Field: name}}})
				root := dal.From(dal.NewRootCollectionRef("customers", "")).NewQuery()
				parsed := root.WhereField(name, dal.Equal, 1).OrderBy(dal.AscendingField(name)).SelectColumns(dal.Column{Expression: dal.Field(name)})
				_, dtqlErr := db.ExecuteDTQLQuery(ctx, parsed)
				streamErr := db.StreamDTQLSnapshot(ctx, parsed, func(Record) error { return nil })
				if quoted {
					for label, err := range map[string]error{"wire": wireErr, "dtql": dtqlErr, "stream": streamErr} {
						if !errors.Is(err, errFakeReached) {
							t.Errorf("%s %q: %v", label, name, err)
						}
					}
					reached += 3
					continue
				}
				_, _, sampleErr := db.SelectAccessSample(ctx, parsed, 1, access.Principal{})
				if !errors.Is(wireErr, ErrInvalidQuery) || !errors.Is(dtqlErr, ErrInvalidDTQL) || !errors.Is(streamErr, ErrInvalidDTQL) || !errors.Is(sampleErr, ErrInvalidDTQL) {
					t.Errorf("%q: wire %v, dtql %v, stream %v, sample %v", name, wireErr, dtqlErr, streamErr, sampleErr)
				}
			}
			if fake.queries != reached {
				t.Fatalf("adapter reached %d times, want %d", fake.queries, reached)
			}
		})
	}
}

// TestParseDTQLKnowsNoEngine: ParseDTQL applies the widest rule, because the
// handler parses before it looks at the engine; the Database applies its own.
func TestParseDTQLKnowsNoEngine(t *testing.T) {
	doc := "from: {name: customers}\nwhere: {op: '==', left: {field: \"zip code\"}, right: {value: 1}}\norderBy: [{field: \"zip code\"}]\n"
	query, collection, err := ParseDTQL([]byte(doc))
	if err != nil || collection != "customers" {
		t.Fatalf("ParseDTQL: %v", err)
	}
	ctx := context.Background()
	if _, err := openEngineDatabase(t, "sqlite").ExecuteDTQLQuery(ctx, query); !errors.Is(err, errFakeReached) {
		t.Errorf("sqlite: %v", err)
	}
	if _, err := openEngineDatabase(t, "postgres").ExecuteDTQLQuery(ctx, query); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("postgres: %v", err)
	}
	if _, err := openEngineDatabase(t, "postgres").ExecuteDTQL(ctx, []byte(doc)); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("postgres, from the document: %v", err)
	}
	// What no engine takes is refused at the parse.
	if _, _, err := ParseDTQL([]byte("from: {name: customers}\nwhere: {op: '==', left: {field: \"a;b\"}, right: {value: 1}}\n")); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("a semicolon: %v", err)
	}
}

func openEngineDatabase(t *testing.T, engine string) *Database {
	t.Helper()
	db, _ := openEngine(t, engine)
	return db
}

// TestWildcardExcludeNamesAFieldWithNoStarOrQuestionMark: DALgo reads an
// exclude of a wildcard column that holds * as a case-insensitive mask, and
// gives ? no meaning, so the name walk refuses either character in an exclude,
// whatever the field-name rule, and accepts any other name.
func TestWildcardExcludeNamesAFieldWithNoStarOrQuestionMark(t *testing.T) {
	root := func() dal.IQueryBuilder { return dal.From(rootRef("customers")).NewQuery() }
	db, _ := openEngine(t, "sqlite")
	walks := map[string]func(dal.StructuredQuery) error{
		"single-collection, quoted rule": func(q dal.StructuredQuery) error { return validateDTQLFieldsWith(q, 0, quotedNames) },
		"relational":                     validateRelationalNames,
		"relational, engine's rule":      db.checkRelationalNames,
	}
	for _, name := range []string{"*", "?", "a*", "*a", "a*b", "a?b", "zip code*", "a.b?", "**", "a\u00e9*"} {
		for label, walk := range walks {
			for shape, query := range map[string]dal.StructuredQuery{
				"unqualified":       root().SelectColumns(dal.AllColumnsExcept("ok", name)),
				"qualified":         root().SelectColumns(dal.AllColumnsExceptFrom("customers", name)),
				"after a good name": root().SelectColumns(dal.AllColumnsExcept("ok", "other", name)),
			} {
				err := walk(query)
				if !errors.Is(err, ErrInvalidDTQL) || !strings.Contains(err.Error(), "mask") {
					t.Errorf("%s, %s, %q: %v", label, shape, name, err)
				}
				// The message does not repeat the name (the characters of the
				// message itself are not the name).
				if strings.ContainsAny(name, "abcdefghijklmnopqrstuvwxyz") && strings.Contains(err.Error(), name) {
					t.Errorf("%s, %s, %q: the message repeats the name: %v", label, shape, name, err)
				}
			}
		}
	}
	for _, name := range []string{"ok", "zip code", "a.b", "a-b", "名前", "a%b", "a_b"} {
		for label, walk := range walks {
			if err := walk(root().SelectColumns(dal.AllColumnsExcept(name))); err != nil {
				t.Errorf("%s, %q: %v", label, name, err)
			}
		}
	}
	// The strict rule refuses them as names before the mask rule is reached.
	if err := validateDTQLFields(root().SelectColumns(dal.AllColumnsExcept("a*")), 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("strict rule: %v", err)
	}
	// A column that is not a wildcard may be named with these characters on an engine that quotes.
	if err := validateRelationalNames(root().SelectColumns(dal.Column{Expression: dal.Field("a*b")})); err != nil {
		t.Errorf("a column named a*b: %v", err)
	}
}

// EngineCanQuery is the allow-list CanQuery asks: the same answer for the engine of
// a database, and for an engine name no database holds.
func TestEngineCanQueryIsTheAllowListOfTheGuard(t *testing.T) {
	for engine, want := range map[string]bool{
		"sqlite": true, "ingitdb": true, "firestore": true,
		"postgres": false, "mysql": false, "oracle": false, "": false, EngineInGitDBGitHub: false,
	} {
		if got := EngineCanQuery(engine); got != want {
			t.Errorf("EngineCanQuery(%q) = %v, want %v", engine, got, want)
		}
		db := &Database{Manifest: &manifest.Manifest{Storage: manifest.Storage{Engine: engine}}}
		if db.CanQuery() != want {
			t.Errorf("CanQuery on %q = %v, want %v", engine, db.CanQuery(), want)
		}
	}
}
