package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// Queries on a SQL mount read only declared collections. The tests below use
// the counting fakes of write_guard_test.go and join_source_test.go: a refused
// query must make zero adapter calls.

var srcGuardSQLEngines = []string{"sqlite", "postgres", "mysql"}

// srcGuardCase is one query with a source named x in one position; every other
// source it reads is customers or orders.
type srcGuardCase struct {
	name  string
	query dal.StructuredQuery
	// single is true when the single-collection entry points (ExecuteDTQLQuery,
	// StreamDTQLSnapshot, SelectAccessSample) pass the query on to the guard;
	// they refuse a join, a derived root, a group, a column list and a scan
	// before it, for their own reasons.
	single bool
}

func srcGuardInner(collection string) dal.StructuredQuery {
	return selectQuery(fromTree(rootRef(collection)).NewQuery())
}

func srcGuardJoinOn(left, right string) dal.Condition {
	return eqCondition(dal.NewFieldRef(left, "id"), dal.NewFieldRef(right, "id"))
}

// srcGuardCases covers every position a source can be in. With x undeclared
// each is refused; with x declared each reaches the adapter.
func srcGuardCases(x string) []srcGuardCase {
	customers := func() dal.IQueryBuilder { return fromTree(rootRef("customers")).NewQuery() }
	base := func() dal.StructuredQuery { return selectQuery(customers()) }
	scalar := func(collection string) dal.Expression {
		return dal.NewQueryExpression(srcGuardInner(collection), "s")
	}
	joined := func(source dal.RecordsetSource, on ...dal.Condition) dal.StructuredQuery {
		if len(on) == 0 {
			on = []dal.Condition{srcGuardJoinOn("customers", "orders")}
		}
		return selectQuery(fromTree(rootRef("customers"), dal.NewJoinedSource(source, dal.JoinInner, on...)).NewQuery())
	}
	return []srcGuardCase{
		{"root", srcGuardInner(x), true},
		{"root of a join", selectQuery(fromTree(rootRef(x), dal.NewJoinedSource(rootRef("orders"), dal.JoinInner, srcGuardJoinOn(x, "orders"))).NewQuery()), false},
		{"join source", joined(rootRef(x)), false},
		{"second join source", selectQuery(fromTree(rootRef("customers"),
			dal.NewJoinedSource(rootRef("orders"), dal.JoinLeft, srcGuardJoinOn("customers", "orders")),
			dal.NewJoinedSource(rootRef(x), dal.JoinInner, srcGuardJoinOn("customers", x))).NewQuery()), false},
		{"nested join tree", selectQuery(fromTree(rootRef("customers"), dal.NewJoinedFrom(
			fromTree(rootRef("orders"), dal.NewJoinedSource(rootRef(x), dal.JoinInner, srcGuardJoinOn("orders", x))),
			dal.JoinInner, srcGuardJoinOn("customers", "orders"))).NewQuery()), false},
		{"derived root", selectQuery(fromTree(dal.NewQuerySource(srcGuardInner(x), "d")).NewQuery()), false},
		{"derived join source", joined(dal.NewQuerySource(srcGuardInner(x), "d")), false},
		{"join inside a derived source", selectQuery(fromTree(dal.NewQuerySource(
			selectQuery(fromTree(rootRef("customers"), dal.NewJoinedSource(rootRef(x), dal.JoinInner, srcGuardJoinOn("customers", x))).NewQuery()), "d")).NewQuery()), false},
		{"derived inside a derived source", selectQuery(fromTree(dal.NewQuerySource(
			selectQuery(fromTree(dal.NewQuerySource(srcGuardInner(x), "inner")).NewQuery()), "d")).NewQuery()), false},
		{"exists", withExists("customers", srcGuardInner(x)), true},
		{"not exists", selectQuery(customers().Where(dal.NewNotExistsCondition(srcGuardInner(x)))), true},
		{"exists in a group", selectQuery(customers().Where(dal.NewGroupCondition(dal.Or,
			dal.WhereField("a", dal.Equal, 1), dal.NewExistsCondition(srcGuardInner(x))))), true},
		{"scalar in where", selectQuery(customers().Where(eqCondition(dal.Field("a"), scalar(x)))), true},
		{"scalar in an arithmetic operand", selectQuery(customers().Where(eqCondition(dal.Binary(dal.Field("a"), dal.Add, scalar(x)), dal.String("y")))), true},
		{"scalar in a column", customers().SelectColumns(dal.Column{Expression: scalar(x)}), false},
		{"scalar in an aggregate argument", customers().SelectColumns(dal.Column{Expression: dal.NewAggregate("sum", false, scalar(x))}), false},
		{"scalar in group by", shapeQuery{StructuredQuery: base(), groupBy: []dal.Expression{scalar(x)}}, false},
		{"scalar in order by", shapeQuery{StructuredQuery: base(), orderBy: []dal.OrderExpression{dal.Ascending(scalar(x))}}, true},
		{"exists in having", shapeQuery{StructuredQuery: base(), having: dal.NewExistsCondition(srcGuardInner(x))}, false},
		{"exists in a join on", joined(rootRef("orders"), dal.NewExistsCondition(srcGuardInner(x))), false},
		{"scalar in a join on", joined(rootRef("orders"), eqCondition(dal.Field("a"), scalar(x))), false},
		{"null test of a scalar", selectQuery(customers().Where(dal.NewIsNullCondition(scalar(x)))), false},
		{"scalar in a scan order", selectQuery(fromTree(rootRef("customers").WithScan(1, dal.Ascending(scalar(x)))).NewQuery()), false},
		{"scalar in the scan order of a join source", joined(rootRef("orders").WithScan(1, dal.Ascending(scalar(x)))), false},
		{"subquery of a subquery", withExists("customers", withExists("orders", srcGuardInner(x))), true},
		{"join source of a subquery", withExists("customers", selectQuery(fromTree(rootRef("orders"),
			dal.NewJoinedSource(rootRef(x), dal.JoinInner, srcGuardJoinOn("orders", x))).NewQuery())), true},
		{"derived source of a subquery", withExists("customers", selectQuery(fromTree(dal.NewQuerySource(srcGuardInner(x), "d")).NewQuery())), true},
	}
}

// srcGuardSingleEntries are the entry points that take one single-collection
// query and read it through the engine guard.
var srcGuardSingleEntries = map[string]func(*Database, dal.StructuredQuery) error{
	"ExecuteDTQLQuery": func(db *Database, q dal.StructuredQuery) error {
		_, err := db.ExecuteDTQLQuery(context.Background(), q)
		return err
	},
	"StreamDTQLSnapshot": func(db *Database, q dal.StructuredQuery) error {
		return db.StreamDTQLSnapshot(context.Background(), q, func(Record) error { return nil })
	},
	"SelectAccessSample": func(db *Database, q dal.StructuredQuery) error {
		_, _, err := db.SelectAccessSample(context.Background(), q, 1, access.Principal{})
		return err
	},
}

// srcGuardSampleRefusal is the refusal SelectAccessSample gives before it looks
// at the sources of a query, or "" when there is none. It computes the order it
// reports before the source check, so that the order it returns with the refusal
// of a source is the same whichever collections the query names: an engine it
// cannot order (every engine but sqlite and ingitdb) and an order it cannot take
// (a subquery in the order by) are refused first, with their own text.
func srcGuardSampleRefusal(entry, engine, name string) string {
	switch {
	case entry != "SelectAccessSample":
		return ""
	case engine != "sqlite":
		return "sample ordering unsupported"
	case name == "scalar in order by":
		return "sample requires field ordering"
	}
	return ""
}

func srcGuardJoinOpen(t *testing.T, engine string, declared ...string) (*Database, *joinSrcRecorder) {
	t.Helper()
	calls := &joinSrcRecorder{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "joinsrc", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	for _, name := range declared {
		m.Schemas.Collections[name] = schema.Collection{Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}
	}
	db, err := Open(m, joinSrcFieldsDB{joinSrcDB{calls: calls, tx: joinSrcTxFields{joinSrcTx{calls: calls}}}}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db, calls
}

// srcGuardJoinEntries are the join source's query surfaces: Executor and the
// executor a read transaction passes to its callback.
var srcGuardJoinEntries = map[string]func(*Database, dal.StructuredQuery) error{
	"Executor records reader": func(db *Database, q dal.StructuredQuery) error {
		_, err := db.Executor().ExecuteQueryToRecordsReader(context.Background(), q)
		return err
	},
	"Executor recordset reader": func(db *Database, q dal.StructuredQuery) error {
		_, err := db.Executor().ExecuteQueryToRecordsetReader(context.Background(), q)
		return err
	},
	"ReadTx records reader": func(db *Database, q dal.StructuredQuery) error {
		var readErr error
		if err := db.ReadTx(context.Background(), func(tx dal.QueryExecutor) error {
			_, readErr = tx.ExecuteQueryToRecordsReader(context.Background(), q)
			return nil
		}); err != nil {
			return err
		}
		return readErr
	},
	"ReadTx recordset reader": func(db *Database, q dal.StructuredQuery) error {
		var readErr error
		if err := db.ReadTx(context.Background(), func(tx dal.QueryExecutor) error {
			_, readErr = tx.ExecuteQueryToRecordsetReader(context.Background(), q)
			return nil
		}); err != nil {
			return err
		}
		return readErr
	},
}

// TestUndeclaredSourceRefusedBeforeAdapterOnSQLEngines: a query whose root,
// joined, derived or subquery source is not declared makes zero adapter calls
// on sqlite, postgres and mysql, and is answered as an undeclared key read is.
func TestUndeclaredSourceRefusedBeforeAdapterOnSQLEngines(t *testing.T) {
	ctx := context.Background()
	for _, engine := range srcGuardSQLEngines {
		for _, tc := range srcGuardCases("ghost") {
			// The key read of the same name is the reference answer.
			db, fake := writeGuardOpen(t, engine, "customers", "orders")
			_, keyErr := db.Get(ctx, record.NewKeyWithID("ghost", "1"))
			if !errors.Is(keyErr, ErrNotFound) {
				t.Fatalf("key read: %v", keyErr)
			}
			check := func(entry string, err error) {
				t.Helper()
				if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidDTQL) || errors.Is(err, ErrQueryUnsupported) {
					t.Errorf("%s/%s/%s: want ErrNotFound, got %v", engine, tc.name, entry, err)
					return
				}
				if err.Error() != keyErr.Error() {
					t.Errorf("%s/%s/%s: answered %q, an undeclared key read is answered %q", engine, tc.name, entry, err, keyErr)
				}
			}
			if tc.single {
				for entry, run := range srcGuardSingleEntries {
					err := run(db, tc.query)
					if want := srcGuardSampleRefusal(entry, engine, tc.name); want != "" {
						if err == nil || err.Error() != want {
							t.Errorf("%s/%s/%s: want %q, got %v", engine, tc.name, entry, want, err)
						}
						continue
					}
					check(entry, err)
				}
			}
			if fake.reached() != 0 {
				t.Errorf("%s/%s: the adapter was reached %d times", engine, tc.name, fake.reached())
			}
			joinDB, calls := srcGuardJoinOpen(t, engine, "customers", "orders")
			for entry, run := range srcGuardJoinEntries {
				err := run(joinDB, tc.query)
				if engine != "sqlite" && strings.HasPrefix(entry, "ReadTx") {
					// ReadTx refuses an engine that cannot be queried before it
					// starts a transaction, so no query reaches the source check.
					if !errors.Is(err, ErrQueryUnsupported) {
						t.Errorf("%s/%s/%s: want ErrQueryUnsupported, got %v", engine, tc.name, entry, err)
					}
					continue
				}
				check(entry, err)
			}
			// A read transaction is started before the callback sees any query, so
			// only the reads and the schema calls are counted.
			if calls.readers != 0 || calls.recordsets != 0 || calls.fields != 0 {
				t.Errorf("%s/%s: the driver was reached through the join source: %+v", engine, tc.name, *calls)
			}
		}
	}
}

// TestDeclaredSourcesReachTheAdapter is the positive control of the test
// above: with every source declared the same queries get past the guard, to the
// adapter on sqlite and to the engine guard on postgres and mysql.
func TestDeclaredSourcesReachTheAdapter(t *testing.T) {
	for _, engine := range srcGuardSQLEngines {
		for _, tc := range srcGuardCases("ghost") {
			t.Run(engine+"/"+tc.name, func(t *testing.T) {
				db, fake := writeGuardOpen(t, engine, "customers", "orders", "ghost")
				reached := 0
				if tc.single {
					for entry, run := range srcGuardSingleEntries {
						err := run(db, tc.query)
						switch {
						case entry == "SelectAccessSample" && engine != "sqlite":
							// refused by its own engine-specific ordering rule
							if err == nil || !strings.Contains(err.Error(), "sample ordering unsupported") {
								t.Errorf("%s: %v", entry, err)
							}
						case entry == "SelectAccessSample" && tc.name == "scalar in order by":
							// the sample orders by fields only
							if err == nil || !strings.Contains(err.Error(), "sample requires field ordering") {
								t.Errorf("%s: %v", entry, err)
							}
						case engine == "sqlite":
							if !errors.Is(err, errFakeReached) {
								t.Errorf("%s: %v", entry, err)
							}
							reached++
						default:
							if !errors.Is(err, ErrQueryUnsupported) {
								t.Errorf("%s: %v", entry, err)
							}
						}
					}
				}
				if fake.queries != reached {
					t.Errorf("the adapter was reached %d times, want %d", fake.queries, reached)
				}
				joinDB, calls := srcGuardJoinOpen(t, engine, "customers", "orders", "ghost")
				for entry, run := range srcGuardJoinEntries {
					err := run(joinDB, tc.query)
					if engine == "sqlite" && !errors.Is(err, errJoinSrcReader) {
						t.Errorf("%s: want the driver's error, got %v", entry, err)
					}
					if engine != "sqlite" && !errors.Is(err, ErrQueryUnsupported) {
						t.Errorf("%s: want ErrQueryUnsupported, got %v", entry, err)
					}
				}
				if engine != "sqlite" && *calls != (joinSrcRecorder{}) {
					t.Errorf("an engine that is not cleared for queries reached the driver: %+v", *calls)
				}
			})
		}
	}
}

// TestWireQueryRefusesAnUndeclaredCollection: the wire query names one
// collection, and a SQL mount has no subcollections.
func TestWireQueryRefusesAnUndeclaredCollection(t *testing.T) {
	ctx := context.Background()
	customer := record.NewKeyWithID("customers", "c1")
	for _, engine := range srcGuardSQLEngines {
		for label, q := range map[string]Query{
			"unknown":             {Collection: "ghost"},
			"unknown keys":        {Collection: "ghost", KeysOnly: true, Limit: 3},
			"quote and semicolon": {Collection: `customers"; --`},
			"case differs":        {Collection: "Customers"},
			"with a filter":       {Collection: "ghost", Where: []Filter{{Field: "name", Op: "==", Value: "Ada"}}, OrderBy: []OrderBy{{Field: "name"}}},
			"nested declared":     {Collection: "orders", Parent: customer.String()},
			"nested undeclared":   {Collection: "ghost", Parent: customer.String()},
		} {
			t.Run(engine+"/"+label, func(t *testing.T) {
				db, fake := writeGuardOpen(t, engine, "customers", "orders")
				if _, err := db.Execute(ctx, q); !errors.Is(err, ErrNotFound) {
					t.Errorf("want ErrNotFound, got %v", err)
				}
				if fake.reached() != 0 {
					t.Errorf("the adapter was reached %d times", fake.reached())
				}
			})
		}
	}
	// A declared collection reaches the adapter on sqlite, the engine guard elsewhere.
	db, fake := writeGuardOpen(t, "sqlite", "customers")
	if _, err := db.Execute(ctx, Query{Collection: "customers"}); !errors.Is(err, errFakeReached) || fake.queries != 1 {
		t.Errorf("declared collection: %v, queries %d", err, fake.queries)
	}
	db, fake = writeGuardOpen(t, "postgres", "customers")
	if _, err := db.Execute(ctx, Query{Collection: "customers"}); !errors.Is(err, ErrQueryUnsupported) || fake.reached() != 0 {
		t.Errorf("declared collection on postgres: %v, reached %d", err, fake.reached())
	}
}

// TestExecuteDTQLRefusesAnUndeclaredCollection runs the same through the
// document form of the entry point that parses its own text.
func TestExecuteDTQLRefusesAnUndeclaredCollection(t *testing.T) {
	docs := map[string]string{
		"root":       "from: {name: ghost}\n",
		"exists":     "from: {name: customers}\nwhere: {exists: {query: {from: {name: ghost}}}}\n",
		"not exists": "from: {name: customers}\nwhere: {notExists: {query: {from: {name: orders, joins: [{from: {name: ghost}, on: [{op: '==', left: {field: id, source: orders}, right: {field: id, source: ghost}}]}]}}}}\n",
	}
	for _, engine := range srcGuardSQLEngines {
		for label, doc := range docs {
			db, fake := writeGuardOpen(t, engine, "customers", "orders")
			if _, err := db.ExecuteDTQL(context.Background(), []byte(doc)); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s/%s: want ErrNotFound, got %v", engine, label, err)
			}
			if fake.reached() != 0 {
				t.Errorf("%s/%s: the adapter was reached", engine, label)
			}
		}
	}
	db, fake := writeGuardOpen(t, "sqlite", "customers", "orders", "ghost")
	for label, doc := range docs {
		if _, err := db.ExecuteDTQL(context.Background(), []byte(doc)); !errors.Is(err, errFakeReached) {
			t.Errorf("declared %s: %v", label, err)
		}
	}
	if fake.queries != len(docs) {
		t.Errorf("queries = %d", fake.queries)
	}
}

// TestDocumentEnginesKeepTheirCollectionRuleForQueries: ingitdb and firestore
// build no SQL, so any collection that passes ValidateCollectionName is read,
// at any depth of the query, and a subcollection query is still served.
func TestDocumentEnginesKeepTheirCollectionRuleForQueries(t *testing.T) {
	ctx := context.Background()
	customer := record.NewKeyWithID("customers", "c1")
	for _, engine := range writeGuardDocumentEngines {
		t.Run(engine, func(t *testing.T) {
			db, fake := writeGuardOpen(t, engine) // nothing declared
			reached := 0
			for _, tc := range srcGuardCases("ghost") {
				if !tc.single {
					continue
				}
				for entry, run := range srcGuardSingleEntries {
					if entry == "SelectAccessSample" && (engine == "firestore" || tc.name == "scalar in order by") {
						continue // refused by its own ordering rules
					}
					if err := run(db, tc.query); !errors.Is(err, errFakeReached) {
						t.Errorf("%s/%s: %v", tc.name, entry, err)
					}
					reached++
				}
			}
			for _, q := range []Query{
				{Collection: "ghost"},
				{Collection: "ghost", Parent: customer.String()},
			} {
				if _, err := db.Execute(ctx, q); !errors.Is(err, errFakeReached) {
					t.Errorf("%+v: %v", q, err)
				}
				reached++
			}
			if fake.queries != reached {
				t.Errorf("the adapter was reached %d times, want %d", fake.queries, reached)
			}
		})
	}
	// And through the join source, with shapes only a join source accepts.
	for _, engine := range writeGuardDocumentEngines {
		db, calls := srcGuardJoinOpen(t, engine)
		for _, tc := range srcGuardCases("ghost") {
			if err := srcGuardJoinEntries["Executor records reader"](db, tc.query); !errors.Is(err, errJoinSrcReader) {
				t.Errorf("%s/%s: %v", engine, tc.name, err)
			}
		}
		if calls.readers != len(srcGuardCases("ghost")) {
			t.Errorf("%s: driver reads = %d", engine, calls.readers)
		}
	}
}

// TestDeclaredNamesMatchExactlyInQueries: a name that differs by case, by a
// space or by a quote is not the declared one. A quoted SQLite key is declared
// under its public name as well, and a query names the collection by that
// canonical name only: the quoted spelling is not the name the adapter is given.
func TestDeclaredNamesMatchExactlyInQueries(t *testing.T) {
	db, fake := writeGuardOpen(t, "sqlite", "Order Details", `"Quoted Name"`)
	for _, name := range []string{"order details", "Order  Details", "Order Details ", `Order Details"`, `"Order Details"`, "Quoted", `"Quoted Name"x`, `"Quoted Name"`} {
		if _, err := db.Execute(context.Background(), Query{Collection: name}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: %v", name, err)
		}
	}
	if fake.reached() != 0 {
		t.Fatalf("the adapter was reached %d times", fake.reached())
	}
	for _, name := range []string{"Order Details", "Quoted Name"} {
		if _, err := db.Execute(context.Background(), Query{Collection: name}); !errors.Is(err, errFakeReached) {
			t.Errorf("%q: %v", name, err)
		}
	}
	// Postgres declares the key as written only.
	pg, _ := writeGuardOpen(t, "postgres", `"Quoted Name"`)
	if err := pg.guardSources(srcGuardInner("Quoted Name")); !errors.Is(err, ErrNotFound) {
		t.Errorf("postgres, logical name of a quoted key: %v", err)
	}
}

// TestSQLiteInternalTablesAreUndeclared: the catalogue tables are refused on a
// SQLite mount unless the manifest declares them.
func TestSQLiteInternalTablesAreUndeclared(t *testing.T) {
	for _, name := range []string{"sqlite_master", "sqlite_schema", "sqlite_temp_master", "sqlite_sequence", "SQLITE_MASTER", "Sqlite_Schema"} {
		db, fake := writeGuardOpen(t, "sqlite", "customers")
		doc := fmt.Sprintf("from: {name: %s}\n", name)
		if _, err := db.ExecuteDTQL(context.Background(), []byte(doc)); !errors.Is(err, ErrNotFound) {
			t.Errorf("dtql %s: %v", name, err)
		}
		if _, err := db.Execute(context.Background(), Query{Collection: name}); !errors.Is(err, ErrNotFound) {
			t.Errorf("wire %s: %v", name, err)
		}
		inSubquery := withExists("customers", srcGuardInner(name))
		if _, err := db.ExecuteDTQLQuery(context.Background(), inSubquery); !errors.Is(err, ErrNotFound) {
			t.Errorf("subquery %s: %v", name, err)
		}
		if fake.reached() != 0 {
			t.Errorf("%s: the adapter was reached", name)
		}
	}
	// Declared, the name is a collection like any other.
	db, _ := writeGuardOpen(t, "sqlite", "sqlite_master")
	if _, err := db.Execute(context.Background(), Query{Collection: "sqlite_master"}); !errors.Is(err, errFakeReached) {
		t.Errorf("declared sqlite_master: %v", err)
	}
}

// srcGuardWalkerCases are shapes the walker refuses on a SQL engine whatever
// the names: a source it does not know is not read.
func srcGuardWalkerCases() map[string]dal.StructuredQuery {
	valid := func() dal.StructuredQuery { return srcGuardInner("customers") }
	cyclic := fromTree(rootRef("customers"))
	cyclic.Join(dal.NewJoinedFrom(cyclic, dal.JoinInner, srcGuardJoinOn("customers", "customers")))
	deepSource := valid()
	for i := 0; i <= maxQueryTreeDepth+1; i++ {
		deepSource = selectQuery(fromTree(dal.NewQuerySource(deepSource, "d")).NewQuery())
	}
	var deepExpression dal.Expression = dal.Field("a")
	for i := 0; i <= maxQueryTreeDepth+1; i++ {
		deepExpression = dal.Binary(deepExpression, dal.Add, dal.String("x"))
	}
	deepCondition := dal.WhereField("a", dal.Equal, 1)
	for i := 0; i <= maxQueryTreeDepth+1; i++ {
		deepCondition = dal.NewGroupCondition(dal.And, deepCondition)
	}
	return map[string]dal.StructuredQuery{
		"nil query":                     nil,
		"missing from":                  noFromQuery{valid()},
		"missing base":                  shapeQuery{StructuredQuery: valid(), from: nilFrom{dal.From(rootRef("customers"))}, hasFrom: true},
		"collection group":              selectQuery(fromTree(dal.NewCollectionGroupRef("customers", "")).NewQuery()),
		"pointer to a collection":       selectQuery(fromTree(&dal.CollectionRef{}).NewQuery()),
		"unknown source":                selectQuery(fromTree(unknownSource{rootRef("customers")}).NewQuery()),
		"unknown join source":           selectQuery(fromTree(rootRef("customers"), dal.NewJoinedSource(unknownSource{rootRef("orders")}, dal.JoinInner)).NewQuery()),
		"join without a source":         selectQuery(fromTree(rootRef("customers"), dal.NewNestedJoinedSource(nil, dal.JoinInner)).NewQuery()),
		"derived source without query":  selectQuery(fromTree(dal.NewQuerySource(nil, "d")).NewQuery()),
		"subquery without query":        selectQuery(fromTree(rootRef("customers")).NewQuery().Where(dal.NewExistsCondition(nil))),
		"unknown condition":             selectQuery(fromTree(rootRef("customers")).NewQuery().Where(unknownCondition{})),
		"unknown condition in a group":  selectQuery(fromTree(rootRef("customers")).NewQuery().Where(dal.NewGroupCondition(dal.And, unknownCondition{}))),
		"unknown condition in a join":   selectQuery(fromTree(rootRef("customers"), dal.NewJoinedSource(rootRef("orders"), dal.JoinInner, unknownCondition{})).NewQuery()),
		"unknown expression":            selectQuery(fromTree(rootRef("customers")).NewQuery().Where(eqCondition(unknownExpression{}, dal.String("y")))),
		"unknown expression operand":    selectQuery(fromTree(rootRef("customers")).NewQuery().Where(eqCondition(dal.Binary(dal.Field("a"), dal.Add, unknownExpression{}), dal.String("y")))),
		"unknown aggregate argument":    fromTree(rootRef("customers")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewAggregate("sum", false, unknownExpression{})}),
		"unknown column":                fromTree(rootRef("customers")).NewQuery().SelectColumns(dal.Column{Expression: unknownExpression{}}),
		"unknown null operand":          selectQuery(fromTree(rootRef("customers")).NewQuery().Where(dal.NewIsNullCondition(unknownExpression{}))),
		"unknown scan order":            selectQuery(fromTree(rootRef("customers").WithScan(1, dal.Ascending(unknownExpression{}))).NewQuery()),
		"nil ordering":                  shapeQuery{StructuredQuery: valid(), orderBy: []dal.OrderExpression{nil}},
		"unknown ordering expression":   shapeQuery{StructuredQuery: valid(), orderBy: []dal.OrderExpression{dal.Ascending(unknownExpression{})}},
		"unknown group by":              shapeQuery{StructuredQuery: valid(), groupBy: []dal.Expression{unknownExpression{}}},
		"unknown having":                shapeQuery{StructuredQuery: valid(), having: unknownCondition{}},
		"cyclic join tree":              selectQuery(cyclic.NewQuery()),
		"derived sources nested deeper": deepSource,
		"expression nested deeper":      selectQuery(fromTree(rootRef("customers")).NewQuery().Where(eqCondition(deepExpression, dal.String("y")))),
		"condition nested deeper":       selectQuery(fromTree(rootRef("customers")).NewQuery().Where(deepCondition)),
	}
}

func TestSourceGuardFailsClosedOnShapesItDoesNotKnow(t *testing.T) {
	db, fake := writeGuardOpen(t, "sqlite", "customers", "orders")
	for label, query := range srcGuardWalkerCases() {
		err := db.guardSources(query)
		if !errors.Is(err, ErrInvalidDTQL) || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want ErrInvalidDTQL, got %v", label, err)
		}
	}
	if fake.reached() != 0 {
		t.Fatal("the guard called the adapter")
	}
	// A document engine is not walked at all.
	doc, _ := writeGuardOpen(t, "ingitdb")
	for label, query := range srcGuardWalkerCases() {
		if err := doc.guardSources(query); err != nil {
			t.Errorf("document engine, %s: %v", label, err)
		}
	}
}

// TestSourceGuardRefusesQualifiedSources: a source qualified by a schema, a
// parent record or another database is not the declared collection of this one,
// whatever its name, and the adapter ignores or rewrites the qualifier.
func TestSourceGuardRefusesQualifiedSources(t *testing.T) {
	db, fake := writeGuardOpen(t, "sqlite", "customers")
	parent := record.NewKeyWithID("customers", "c1")
	for label, ref := range map[string]dal.CollectionRef{
		"schema":                dal.NewQualifiedRootCollectionRef("temp", "customers", ""),
		"schema main":           dal.NewQualifiedRootCollectionRef("main", "customers", ""),
		"parent":                dal.NewCollectionRef("customers", "", parent),
		"other database":        dal.NewDatabaseCollectionRef("elsewhere", "", "customers", ""),
		"other database+schema": dal.NewDatabaseCollectionRef("elsewhere", "main", "customers", ""),
	} {
		for where, query := range map[string]dal.StructuredQuery{
			"root":     selectQuery(fromTree(ref).NewQuery()),
			"join":     selectQuery(fromTree(rootRef("customers"), dal.NewJoinedSource(ref, dal.JoinInner, srcGuardJoinOn("customers", "customers"))).NewQuery()),
			"subquery": withExists("customers", selectQuery(fromTree(ref).NewQuery())),
		} {
			if err := db.guardSources(query); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s in %s: %v", label, where, err)
			}
		}
	}
	// The mount's own id is the same database, and an alias or a scan on a declared collection is not a different one.
	for label, ref := range map[string]dal.CollectionRef{
		"own database":       dal.NewDatabaseCollectionRef("guarded", "", "customers", ""),
		"own database alias": dal.NewDatabaseCollectionRef("guarded", "", "customers", "c"),
		"alias":              dal.NewRootCollectionRef("customers", "c"),
		"scan":               rootRef("customers").WithScan(10, dal.AscendingField("name")),
	} {
		if err := db.guardSources(selectQuery(fromTree(ref).NewQuery())); err != nil {
			t.Errorf("%s: %v", label, err)
		}
	}
	// A database without a manifest has no id to match.
	bare := &Database{}
	if err := bare.guardSources(selectQuery(fromTree(dal.NewDatabaseCollectionRef("guarded", "", "customers", "")).NewQuery())); !errors.Is(err, ErrNotFound) {
		t.Errorf("database without a manifest: %v", err)
	}
	if fake.reached() != 0 {
		t.Fatal("the guard called the adapter")
	}
}

// TestSourceGuardWalksScanOrders: a scan order is an expression, and an
// expression can hold a subquery.
func TestSourceGuardWalksScanOrders(t *testing.T) {
	db, _ := writeGuardOpen(t, "sqlite", "customers", "orders")
	order := func(collection string) dal.OrderExpression {
		return dal.Ascending(dal.NewQueryExpression(srcGuardInner(collection), "s"))
	}
	if err := db.guardSources(selectQuery(fromTree(rootRef("customers").WithScan(1, order("orders"))).NewQuery())); err != nil {
		t.Errorf("declared subquery in a scan order: %v", err)
	}
	if err := db.guardSources(selectQuery(fromTree(rootRef("customers").WithScan(1, order("ghost"))).NewQuery())); !errors.Is(err, ErrNotFound) {
		t.Errorf("undeclared subquery in a scan order: %v", err)
	}
}

// TestSourceGuardAcceptsConstantsParamsAggregatesNullTestsAndLeftJoins: the
// guard refuses none of these shapes, which the name walk accepts: constants and
// arrays, a parameter, a count of everything, a wildcard with an exclude, an
// aggregate with group by and having, a null test and a left join.
func TestSourceGuardAcceptsConstantsParamsAggregatesNullTestsAndLeftJoins(t *testing.T) {
	db, _ := writeGuardOpen(t, "sqlite", "customers", "orders")
	for label, query := range map[string]dal.StructuredQuery{
		"constants and arrays": selectQuery(fromTree(rootRef("customers")).NewQuery().WhereField("a", dal.In, []string{"x", "y"}).WhereField("b", dal.Equal, 1)),
		"params":               selectQuery(fromTree(rootRef("customers")).NewQuery().Where(eqCondition(dal.Field("a"), dal.NewParam("currentUser")))),
		"count star":           fromTree(rootRef("customers")).NewQuery().SelectColumns(dal.Count()),
		"wildcard":             fromTree(rootRef("customers")).NewQuery().SelectColumns(dal.AllColumnsExcept("x")),
		"aggregate and group": fromTree(rootRef("customers")).NewQuery().GroupBy(dal.Field("a")).Having(dal.NewComparison(dal.Field("a"), dal.Equal, dal.String("x"))).
			SelectColumns(dal.CountAs(dal.Field("a"), "n")),
		"null test": selectQuery(fromTree(rootRef("customers")).NewQuery().Where(dal.NewIsNotNullCondition(dal.Field("a")))),
		"left join": selectQuery(fromTree(rootRef("customers"), dal.NewJoinedSource(dal.NewRootCollectionRef("orders", "o"), dal.JoinLeft, srcGuardJoinOn("customers", "o"))).NewQuery()),
	} {
		if err := db.guardSources(query); err != nil {
			t.Errorf("%s: %v", label, err)
		}
		if err := db.checkRelationalNames(query); err != nil {
			t.Errorf("%s (names): %v", label, err)
		}
	}
}

// TestSourceGuardCountsANestedJoinTreeOneLevelPerNesting: the name walk checks
// a derived source in a nested join tree at the depth of its query, and the
// guard adds a level for each nesting of the tree, so it is never laxer than the
// name walk. A derived chain that both accept as the source of a plain query is
// accepted by the name walk and refused by the guard when two nested join trees
// hold it.
func TestSourceGuardCountsANestedJoinTreeOneLevelPerNesting(t *testing.T) {
	db, _ := writeGuardOpen(t, "sqlite", "customers", "orders")
	chain := srcGuardInner("customers")
	for i := 0; i < maxQueryTreeDepth-2; i++ {
		chain = selectQuery(fromTree(dal.NewQuerySource(chain, "d")).NewQuery())
	}
	derived := dal.NewQuerySource(chain, "x")
	plain := selectQuery(fromTree(derived).NewQuery())
	nested := selectQuery(fromTree(rootRef("customers"), dal.NewJoinedFrom(
		fromTree(rootRef("orders"), dal.NewJoinedFrom(fromTree(derived), dal.JoinInner, srcGuardJoinOn("orders", "orders"))),
		dal.JoinInner, srcGuardJoinOn("customers", "orders"))).NewQuery())
	for label, query := range map[string]dal.StructuredQuery{"plain": plain, "nested": nested} {
		if err := db.checkRelationalNames(query); err != nil {
			t.Errorf("%s: the name walk: %v", label, err)
		}
	}
	if err := db.guardSources(plain); err != nil {
		t.Errorf("plain: the guard: %v", err)
	}
	if err := db.guardSources(nested); !errors.Is(err, errSourcesTooDeep) {
		t.Errorf("nested: the guard: %v", err)
	}
}

// TestJoinFieldsRefusesAnUndeclaredCollection: the schema of a table is read
// from the table, so an undeclared one is refused before the driver is asked.
func TestJoinFieldsRefusesAnUndeclaredCollection(t *testing.T) {
	ctx := context.Background()
	for _, engine := range srcGuardSQLEngines {
		t.Run(engine, func(t *testing.T) {
			db, calls := srcGuardJoinOpen(t, engine, "customers")
			provider := db.Executor().(dal.JoinFieldsProvider)
			for label, source := range map[string]dal.RecordsetSource{
				"unknown":         rootRef("ghost"),
				"catalogue":       rootRef("sqlite_master"),
				"schema":          dal.NewQualifiedRootCollectionRef("main", "customers", ""),
				"derived unknown": dal.NewQuerySource(srcGuardInner("ghost"), "d"),
				"missing":         nil,
			} {
				fields, err := provider.JoinFields(ctx, source)
				if err == nil || errors.Is(err, ErrQueryUnsupported) || fields != nil {
					t.Errorf("%s: fields %v, err %v", label, fields, err)
				}
			}
			if _, err := provider.JoinFields(ctx, rootRef("ghost")); !errors.Is(err, ErrNotFound) {
				t.Errorf("an undeclared collection: %v", err)
			}
			if calls.fields != 0 {
				t.Fatalf("the driver was asked for the fields of an undeclared collection %d times", calls.fields)
			}
			// A declared collection gets as far as the engine guard or the driver.
			_, err := provider.JoinFields(ctx, rootRef("customers"))
			if engine == "sqlite" && (calls.fields != 1 || !errors.Is(err, errJoinSrcFields)) {
				t.Errorf("declared: fields calls %d, err %v", calls.fields, err)
			}
			if engine != "sqlite" && !errors.Is(err, ErrQueryUnsupported) {
				t.Errorf("declared on an engine that cannot be queried: %v", err)
			}
		})
	}
	for _, engine := range writeGuardDocumentEngines {
		db, calls := srcGuardJoinOpen(t, engine)
		if _, err := db.Executor().(dal.JoinFieldsProvider).JoinFields(ctx, rootRef("ghost")); !errors.Is(err, errJoinSrcFields) || calls.fields != 1 {
			t.Errorf("%s: %v, fields calls %d", engine, err, calls.fields)
		}
	}
}

// TestJoinSourceHoldsEnginesOutsideQuotedNamesToTheStrictRule: the join
// source's guard runs the relational name walk with the field-name rule of the
// mount's engine, in code, so an engine that is not in quotedNameEngines is
// held to the strict rule even if an operator lists it as a join engine.
func TestJoinSourceHoldsEnginesOutsideQuotedNamesToTheStrictRule(t *testing.T) {
	named := func(field string) dal.StructuredQuery {
		return selectQuery(fromTree(rootRef("customers")).NewQuery().WhereField(field, dal.Equal, 1))
	}
	cases := []struct {
		engine string
		field  string
		want   bool // the name reaches the engine guard
	}{
		{"sqlite", "zip code", true},
		{"ingitdb", "zip code", true},
		{"firestore", "zip code", false},
		{"postgres", "zip code", false},
		{"mysql", "zip code", false},
		{"oracle", "zip code", false},
		{"sqlite", "zipcode", true},
		{"firestore", "zipcode", true},
		{"postgres", "zipcode", true},
		{"sqlite", "a;b", false},
		{"ingitdb", `a"b`, false},
	}
	for _, tc := range cases {
		t.Run(tc.engine+"/"+tc.field, func(t *testing.T) {
			db, calls := srcGuardJoinOpen(t, tc.engine, "customers")
			err := srcGuardJoinEntries["Executor records reader"](db, named(tc.field))
			switch {
			case !tc.want && !errors.Is(err, ErrInvalidDTQL):
				t.Errorf("want ErrInvalidDTQL, got %v", err)
			case tc.want && errors.Is(err, ErrInvalidDTQL):
				t.Errorf("a name of the engine's rule was refused: %v", err)
			}
			if !tc.want && *calls != (joinSrcRecorder{}) {
				t.Errorf("the driver was reached: %+v", *calls)
			}
		})
	}
	// The Database method is the same walk, with the engine's rule.
	for _, tc := range cases {
		db, _ := srcGuardJoinOpen(t, tc.engine, "customers")
		if err := db.checkRelationalNames(named(tc.field)); (err == nil) != tc.want {
			t.Errorf("%s %q: checkRelationalNames = %v, want accepted %v", tc.engine, tc.field, err, tc.want)
		}
	}
	// ReadTx's executor and JoinFields go through the same guard.
	db, calls := srcGuardJoinOpen(t, "sqlite", "customers")
	if err := srcGuardJoinEntries["ReadTx records reader"](db, named("a;b")); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("ReadTx: %v", err)
	}
	if calls.readers != 0 {
		t.Errorf("the driver was reached")
	}
}

func TestCheckRelationalNamesKeepsTheRelationalVariant(t *testing.T) {
	db, _ := srcGuardJoinOpen(t, "sqlite", "customers")
	onDatabase := selectQuery(fromTree(dal.NewDatabaseCollectionRef("chinook", "", "Customer", "")).NewQuery())
	if err := db.checkRelationalNames(onDatabase); err != nil {
		t.Errorf("a source that names a database: %v", err)
	}
	nullTest := selectQuery(fromTree(rootRef("customers")).NewQuery().Where(dal.NewIsNullCondition(dal.Field("a"))))
	if err := db.checkRelationalNames(nullTest); err != nil {
		t.Errorf("a null test: %v", err)
	}
	if err := db.checkRelationalNames(selectQuery(fromTree(dal.NewCollectionRef("c", "", record.NewKeyWithID("p", "1"))).NewQuery())); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("a parent: %v", err)
	}
}

// TestClipNameBoundsWhatAMessageRepeats: a collection name or a database id
// longer than the bound is cut in the messages that repeat it.
func TestClipNameBoundsWhatAMessageRepeats(t *testing.T) {
	if got := clipName("customers"); got != "customers" {
		t.Errorf("short name: %q", got)
	}
	exact := strings.Repeat("a", maxEchoedNameLen)
	if got := clipName(exact); got != exact {
		t.Errorf("a name at the bound was changed")
	}
	long := strings.Repeat("a", maxEchoedNameLen+1)
	got := clipName(long)
	if !strings.HasPrefix(got, exact) || len(got) > maxEchoedNameLen+32 || strings.Contains(got, long) {
		t.Errorf("over-long name: %q", got)
	}
	if !strings.Contains(got, fmt.Sprint(len(long))) {
		t.Errorf("the clipped text must say how long the name was: %q", got)
	}
	// The cut never splits a multi-byte character: with a one-byte prefix the
	// bound falls inside a two-byte letter, and the cut moves back before it.
	wide := "a" + strings.Repeat("\u00e9", maxEchoedNameLen) // two bytes each
	clipped := clipName(wide)
	if !utf8.ValidString(clipped) {
		t.Fatalf("a character was split: %q", clipped)
	}
	if want := "a" + strings.Repeat("\u00e9", maxEchoedNameLen/2-1); !strings.HasPrefix(clipped, want) || strings.HasPrefix(clipped, want+"\u00e9") {
		t.Errorf("clipped = %q", clipped)
	}
	// When the bound falls between two characters the cut stays there.
	evenly := strings.Repeat("\u00e9", maxEchoedNameLen)
	if got := clipName(evenly); !strings.HasPrefix(got, strings.Repeat("\u00e9", maxEchoedNameLen/2)) || strings.HasPrefix(got, strings.Repeat("\u00e9", maxEchoedNameLen/2)+"\u00e9") {
		t.Errorf("clipped = %q", got)
	}
}

func TestUndeclaredCollectionMessagesClipTheName(t *testing.T) {
	hugeName := strings.Repeat("x", 1<<16)
	db, _ := writeGuardOpen(t, "sqlite", "customers")
	for label, err := range map[string]error{
		"wire":     func() error { _, err := db.Execute(context.Background(), Query{Collection: hugeName}); return err }(),
		"dtql":     func() error { _, err := db.ExecuteDTQLQuery(context.Background(), srcGuardInner(hugeName)); return err }(),
		"join":     srcGuardJoinEntries["Executor records reader"](srcGuardJoinDB(t), srcGuardInner(hugeName)),
		"derived":  db.guardSources(selectQuery(fromTree(dal.NewQuerySource(srcGuardInner(hugeName), "d")).NewQuery())),
		"parent":   db.guardSources(selectQuery(fromTree(dal.NewCollectionRef(hugeName, "", record.NewKeyWithID("customers", "1"))).NewQuery())),
		"schema":   db.guardSources(selectQuery(fromTree(dal.NewQualifiedRootCollectionRef(hugeName, "customers", "")).NewQuery())),
		"database": db.guardSources(selectQuery(fromTree(dal.NewDatabaseCollectionRef(hugeName, "", "customers", "")).NewQuery())),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", label, err)
			continue
		}
		if len(err.Error()) > 4*maxEchoedNameLen {
			t.Errorf("%s: the message is %d bytes", label, len(err.Error()))
		}
	}
}

func srcGuardJoinDB(t *testing.T) *Database {
	t.Helper()
	db, _ := srcGuardJoinOpen(t, "sqlite", "customers")
	return db
}

// TestOverLongCollectionNameIsClippedInQueryErrors: the messages a query builds
// around a collection name that the driver then fails on.
func TestOverLongCollectionNameIsClippedInQueryErrors(t *testing.T) {
	huge := strings.Repeat("c", 1<<16)
	db, _ := writeGuardOpen(t, "ingitdb")
	for label, err := range map[string]error{
		"wire":   func() error { _, err := db.Execute(context.Background(), Query{Collection: huge}); return err }(),
		"dtql":   func() error { _, err := db.ExecuteDTQLQuery(context.Background(), srcGuardInner(huge)); return err }(),
		"stream": db.StreamDTQLSnapshot(context.Background(), srcGuardInner(huge), func(Record) error { return nil }),
	} {
		if err == nil || len(err.Error()) > 4*maxEchoedNameLen {
			t.Errorf("%s: %d bytes: %.80v", label, len(fmt.Sprint(err)), err)
		}
	}
}

// srcGuardReadFailsDB is a driver whose reader fails on the first record.
type srcGuardReadFailsDB struct{ dal.DB }

var errSrcGuardRead = errors.New("source guard test: read failed")

type srcGuardFailingReader struct{}

func (srcGuardFailingReader) Cursor() (string, error)      { return "", nil }
func (srcGuardFailingReader) Close() error                 { return nil }
func (srcGuardFailingReader) Next() (record.Record, error) { return nil, errSrcGuardRead }

func (srcGuardReadFailsDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return srcGuardFailingReader{}, nil
}

// TestFailedReadMessagesClipTheCollectionName: the messages of a read that
// fails after the query was handed to the driver repeat the collection name
// clipped, on every route.
func TestFailedReadMessagesClipTheCollectionName(t *testing.T) {
	huge := strings.Repeat("c", 1<<16)
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "readfails", SchemaMode: schema.ModeSchemaless},
		Storage:  manifest.Storage{Engine: "ingitdb"},
	}
	db, err := Open(m, srcGuardReadFailsDB{}, []schema.Mode{schema.ModeSchemaless}, filepath.Join(t.TempDir(), "inferred.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for label, err := range map[string]error{
		"wire":   func() error { _, err := db.Execute(ctx, Query{Collection: huge}); return err }(),
		"dtql":   func() error { _, err := db.ExecuteDTQLQuery(ctx, srcGuardInner(huge)); return err }(),
		"stream": db.StreamDTQLSnapshot(ctx, srcGuardInner(huge), func(Record) error { return nil }),
	} {
		if !errors.Is(err, errSrcGuardRead) || !strings.Contains(err.Error(), "failed reading query results") || len(err.Error()) > 4*maxEchoedNameLen {
			t.Errorf("%s: %d bytes: %.100v", label, len(fmt.Sprint(err)), err)
		}
	}
}
