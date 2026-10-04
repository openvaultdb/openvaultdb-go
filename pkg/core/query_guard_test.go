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

func openEngine(t *testing.T, engine string) (*Database, *queryCountingDB) {
	t.Helper()
	fake := &queryCountingDB{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "guarded", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
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
					continue // refused earlier by its own ordering rule; asserted below
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

func TestQueryUnsupportedErrorIsNotInvalidQuery(t *testing.T) {
	err := error(&QueryUnsupportedError{Engine: "postgres"})
	if errors.Is(err, ErrInvalidQuery) || errors.Is(err, ErrInvalidDTQL) {
		t.Fatal("a refused engine is not a malformed query")
	}
}

// unsafeFieldNames are names that must never reach an adapter on any engine.
var unsafeFieldNames = []string{
	`na"me`, `na'me`, "na me", "name;", "name; DROP TABLE x", "name--", "na--me",
	"name/*", "na/**/me", "name#", "", ".", "a..b", ".name", "name.", "1name",
	"name\x00", "na\nme", "na\tme", "naïve", `[name]`, "`name`", "name)", "(name",
	"name=1", "name OR 1=1", strings.Repeat("a", maxFieldNameLen+1),
}

var safeFieldNames = []string{"name", "_id", "Name2", "a_b", "address.city", "a.b.c", strings.Repeat("a", maxFieldNameLen)}

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

func TestWireQueryFieldNamesValidatedBeforeAdapter(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb", "postgres"} {
		db, fake := openEngine(t, engine)
		for _, name := range unsafeFieldNames {
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
	for _, engine := range []string{"sqlite", "ingitdb", "postgres"} {
		db, fake := openEngine(t, engine)
		for _, name := range unsafeFieldNames {
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
		"from: {name: customers}\nwhere: {op: '==', left: {field: \"na me\"}, right: {value: 1}}\n",
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
	if err := validateCondition(otherCondition{}, 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("unknown condition: %v", err)
	}
	if err := validateExpression(otherExpression{}, 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("unknown expression: %v", err)
	}
	if err := validateExpression(dal.NewComparison(nil, dal.Equal, nil), 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("comparison used as expression: %v", err)
	}
	if err := validateExpression(dal.Param{Name: "bad name"}, 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("bad param: %v", err)
	}
	if err := validateExpression(dal.NewParam("currentUser"), 0); err != nil {
		t.Errorf("good param: %v", err)
	}
	if err := validateCondition(dal.NewComparison(dal.Field("a"), dal.In, dal.Array{Value: []string{"x"}}), 0); err != nil {
		t.Errorf("array: %v", err)
	}
	if err := validateCondition(nil, 0); err != nil {
		t.Errorf("nil condition: %v", err)
	}
	if err := validateExpression(nil, 0); err != nil {
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
	if err := validateExpression(dal.NewFieldRef("c", "x"), 0); err != nil {
		t.Errorf("qualified field: %v", err)
	}
	if err := validateExpression(dal.NewFieldRef("c;", "x"), 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("qualified field source: %v", err)
	}
	// Deeply nested conditions are refused rather than walked without bound.
	var deep dal.Condition = dal.WhereField("a", dal.Equal, 1)
	for i := 0; i <= maxQueryTreeDepth+1; i++ {
		deep = dal.NewGroupCondition(dal.And, deep)
	}
	if err := validateCondition(deep, 0); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("deep condition: %v", err)
	}
	var deepExpr dal.Expression = dal.Field("a")
	for i := 0; i <= maxQueryTreeDepth+1; i++ {
		deepExpr = dal.Binary(deepExpr, dal.Add, dal.String("x"))
	}
	if err := validateExpression(deepExpr, 0); !errors.Is(err, ErrInvalidDTQL) {
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
