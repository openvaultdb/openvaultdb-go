package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// A mount that reaches a database server holds the tables it provisioned from its
// manifest, so a name the tables do not have is a mistake of the caller that is
// refused before the adapter, which could only fail it as a fault of the server.

// fieldsDB is the fake adapter of these tests: it counts the queries it is handed, in
// a read transaction too.
type fieldsDB struct{ queryCountingDB }

func (f *fieldsDB) RunReadonlyTransaction(ctx context.Context, worker dal.ROTxWorker, _ ...dal.TransactionOption) error {
	return worker(ctx, &fieldsTx{db: &f.queryCountingDB})
}

type fieldsTx struct {
	dal.ReadTransaction
	db *queryCountingDB
}

func (t *fieldsTx) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	return t.db.ExecuteQueryToRecordsReader(ctx, query)
}

// reached reports whether a query was handed to the adapter: it answers every query
// with an error, which a server engine turns into a built one, so the count is what
// says it.
func (f *fieldsDB) reached(err error) bool {
	return f.queries == 1 && err != nil && !errors.Is(err, ErrInvalidDTQL)
}

// openFields opens a database of the engine and schema mode over a fake adapter that
// counts the queries it is handed. customers and orders declare fields (one of them
// in mixed case, which a PostgreSQL server holds folded), and bare declares none.
func openFields(t *testing.T, engine string, mode schema.Mode) (*Database, *fieldsDB) {
	t.Helper()
	fake := &fieldsDB{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "fields", SchemaMode: mode},
		Storage:  manifest.Storage{Engine: engine},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"customers": {Fields: map[string]schema.Field{
				"name":      {Type: schema.TypeString},
				"country":   {Type: schema.TypeString},
				"FirstName": {Type: schema.TypeString},
			}},
			"orders": {Fields: map[string]schema.Field{
				"customer_id": {Type: schema.TypeString},
				"total":       {Type: schema.TypeInteger},
			}},
			"bare": {},
		}},
	}
	db, err := Open(m, fake, []schema.Mode{schema.ModeStrict, schema.ModePartial, schema.ModeSchemaless}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db, fake
}

// fieldsCase is a query and what the check of its fields says: want is "" when the
// query passes, else a text of the refusal.
type fieldsCase struct {
	name  string
	query dal.StructuredQuery
	want  string
}

func fieldsOf(from dal.FromSource) *dal.QueryBuilder { return from.NewQuery() }

func ref(source, name string) dal.FieldRef { return dal.NewFieldRef(source, name) }

func equalTo(left dal.Expression, value any) dal.Condition {
	return dal.NewComparison(left, dal.Equal, dal.Constant{Value: value})
}

// fieldsCases are the queries of the check, over customers, orders and bare.
func fieldsCases() []fieldsCase {
	customers := func() *dal.QueryBuilder { return fieldsOf(fromTree(rootRef("customers"))) }
	aliased := func(name, alias string) dal.CollectionRef { return dal.NewRootCollectionRef(name, alias) }
	joined := func(on ...dal.Condition) *dal.QueryBuilder {
		if len(on) == 0 {
			on = []dal.Condition{eqCondition(ref("o", "customer_id"), ref("c", "id"))}
		}
		return fieldsOf(fromTree(aliased("orders", "o"), dal.NewJoinedSource(aliased("customers", "c"), dal.JoinInner, on...)))
	}
	hasNoField := func(collection, field string) string {
		return `collection "` + collection + `" has no field "` + field + `"`
	}
	noSource := func(qualifier string) string { return `no source of the query is named "` + qualifier + `"` }
	derived := func(inner dal.StructuredQuery, alias string) dal.FromSource {
		return fromTree(dal.NewQuerySource(inner, alias))
	}
	innerOrders := func() dal.StructuredQuery { return selectQuery(fieldsOf(fromTree(rootRef("orders")))) }

	return []fieldsCase{
		// What the table holds passes, under any case of the name.
		{"a column the manifest declares", selectQuery(customers().Where(equalTo(dal.Field("name"), "Ada"))), ""},
		{"the key column", selectQuery(customers().Where(equalTo(dal.Field("id"), "c1"))), ""},
		{"a declared name in mixed case, as declared", selectQuery(customers().Where(equalTo(dal.Field("FirstName"), "Ada"))), ""},
		{"a declared name in mixed case, as the server holds it", selectQuery(customers().Where(equalTo(dal.Field("firstname"), "Ada"))), ""},
		{"a name in another case than the declared one", selectQuery(customers().Where(equalTo(dal.Field("COUNTRY"), "IE"))), ""},
		{"order by a declared column", selectQuery(customers().OrderBy(dal.AscendingField("name"))), ""},
		{"the key of DALgo, which is the adapter's to refuse", selectQuery(customers().Where(equalTo(dal.DocumentID(), "c1"))), ""},
		{"a qualified column of an aliased source", selectQuery(fieldsOf(fromTree(aliased("orders", "o"))).Where(equalTo(ref("o", "total"), 5))), ""},
		{"a qualified column of an unaliased source", selectQuery(customers().Where(equalTo(ref("customers", "name"), "Ada"))), ""},
		{"a join on and the columns of both sources", selectQuery(joined().
			Where(equalTo(ref("c", "name"), "Ada")).OrderBy(dal.Ascending(ref("o", "total")))), ""},
		{"an unqualified field of a join, which the compiler refuses as it is", selectQuery(joined().Where(equalTo(dal.Field("nosuch"), 1))), ""},
		{"a name in order by that no column has and no alias gives", selectQuery(customers().OrderBy(dal.AscendingField("n"))), hasNoField("customers", "n")},
		{"an alias of a column in order by, declared by the query", shapeQuery{StructuredQuery: customers().SelectColumns(
			dal.Column{Alias: "Revenue", Expression: dal.NewAggregate("sum", false, dal.Field("country"))}),
			orderBy: []dal.OrderExpression{dal.AscendingField("revenue")}}, ""},
		{"an alias of a column in having", shapeQuery{StructuredQuery: customers().SelectColumns(
			dal.Column{Alias: "n", Expression: dal.NewAggregate("count", false, dal.Star())}),
			having: dal.NewComparison(dal.Field("n"), dal.GreaterThen, dal.Constant{Value: 1})}, ""},
		{"a column of a derived source, which the query of it decides", selectQuery(fieldsOf(derived(innerOrders(), "d")).
			Where(equalTo(ref("d", "anything"), 1))), ""},
		{"an unqualified field over a derived source", selectQuery(fieldsOf(derived(innerOrders(), "d")).
			Where(equalTo(dal.Field("anything"), 1))), ""},
		{"a source qualified by the alias of an enclosing query", withExists("customers", selectQuery(fieldsOf(fromTree(aliased("orders", "e"))).
			Where(eqCondition(ref("e", "customer_id"), ref("customers", "id"))))), ""},
		{"a collection that declares nothing has its key", selectQuery(fieldsOf(fromTree(rootRef("bare"))).Where(equalTo(dal.Field("id"), 1))), ""},

		// What it does not hold is refused, wherever the name stands.
		{"a field no column has, in where", selectQuery(customers().Where(equalTo(dal.Field("nosuch"), 1))), hasNoField("customers", "nosuch")},
		{"a field no column has, in order by", selectQuery(customers().OrderBy(dal.AscendingField("nosuch"))), hasNoField("customers", "nosuch")},
		{"a field no column has, in a column", customers().SelectColumns(dal.Column{Expression: dal.Field("nosuch")}), hasNoField("customers", "nosuch")},
		{"a dotted name, which no column of the table has", selectQuery(customers().Where(equalTo(dal.Field("name.first"), 1))), hasNoField("customers", "name.first")},
		{"a field of a collection that declares nothing", selectQuery(fieldsOf(fromTree(rootRef("bare"))).Where(equalTo(dal.Field("name"), 1))), hasNoField("bare", "name")},
		{"a field of the other source, qualified by this one", selectQuery(joined().Where(equalTo(ref("c", "total"), 1))), hasNoField("customers", "total")},
		{"a field of a source, in a join on", selectQuery(joined(eqCondition(ref("o", "nosuch"), ref("c", "id")))), hasNoField("orders", "nosuch")},
		{"a field of a source, on the right of a join on", selectQuery(joined(eqCondition(ref("o", "customer_id"), ref("c", "nosuch")))), hasNoField("customers", "nosuch")},
		{"a field in an aggregate argument", customers().SelectColumns(dal.Column{Alias: "s", Expression: dal.NewAggregate("sum", false, dal.Field("nosuch"))}), hasNoField("customers", "nosuch")},
		{"a field in arithmetic", selectQuery(customers().Where(equalTo(dal.Binary(dal.Field("name"), dal.Add, dal.Field("nosuch")), 1))), hasNoField("customers", "nosuch")},
		{"a field on the left of arithmetic", selectQuery(customers().Where(equalTo(dal.Binary(dal.Field("nosuch"), dal.Add, dal.Field("name")), 1))), hasNoField("customers", "nosuch")},
		{"a field in a group", selectQuery(customers().Where(dal.NewGroupCondition(dal.Or, equalTo(dal.Field("name"), 1), equalTo(dal.Field("nosuch"), 1)))), hasNoField("customers", "nosuch")},
		{"a field in a null test", selectQuery(customers().Where(dal.NewIsNullCondition(dal.Field("nosuch")))), hasNoField("customers", "nosuch")},
		{"a field in group by", shapeQuery{StructuredQuery: customers().SelectColumns(dal.Column{Expression: dal.Field("name")}), groupBy: []dal.Expression{dal.Field("nosuch")}}, hasNoField("customers", "nosuch")},
		{"a field in having", shapeQuery{StructuredQuery: customers().SelectColumns(dal.Column{Expression: dal.Field("name")}), having: equalTo(dal.Field("nosuch"), 1)}, hasNoField("customers", "nosuch")},
		{"a field in a scan order", selectQuery(fieldsOf(fromTree(rootRef("customers").WithScan(1, dal.AscendingField("nosuch"))))), hasNoField("customers", "nosuch")},
		{"a field of the source of a subquery", withExists("customers", selectQuery(fieldsOf(fromTree(rootRef("orders"))).Where(equalTo(dal.Field("nosuch"), 1)))), hasNoField("orders", "nosuch")},
		{"a field in a scalar subquery", selectQuery(customers().Where(eqCondition(dal.Field("name"),
			dal.NewQueryExpression(selectQuery(fieldsOf(fromTree(rootRef("orders"))).Where(equalTo(dal.Field("nosuch"), 1))), "s")))), hasNoField("orders", "nosuch")},
		{"a field in a derived source", selectQuery(fieldsOf(derived(selectQuery(fieldsOf(fromTree(rootRef("orders"))).Where(equalTo(dal.Field("nosuch"), 1))), "d"))), hasNoField("orders", "nosuch")},

		// A qualifier that no source has is refused.
		{"a qualifier no source has", selectQuery(customers().Where(equalTo(ref("x", "name"), 1))), noSource("x")},
		{"the name of an aliased source's collection, which the alias hides", selectQuery(fieldsOf(fromTree(aliased("orders", "o"))).Where(equalTo(ref("orders", "total"), 1))), noSource("orders")},
		{"a qualifier of a join no source has", selectQuery(joined(eqCondition(ref("o", "customer_id"), ref("z", "id")))), noSource("z")},
		{"a qualifier no source has, in order by", selectQuery(customers().OrderBy(dal.Ascending(ref("x", "name")))), noSource("x")},
		{"a wildcard qualifier no source has", customers().SelectColumns(dal.Column{Wildcard: &dal.WildcardProjection{Source: "x"}}), noSource("x")},
		{"a wildcard of a source", customers().SelectColumns(dal.Column{Wildcard: &dal.WildcardProjection{Source: "customers"}}), ""},
		{"a wildcard of the query", customers().SelectColumns(dal.Column{Wildcard: &dal.WildcardProjection{}}), ""},
		{"a qualifier a derived source hides from its sibling", selectQuery(fieldsOf(derived(
			selectQuery(fieldsOf(fromTree(aliased("orders", "o"))).Where(equalTo(ref("customers", "name"), 1))), "d"))), noSource("customers")},
	}
}

// TestTheFieldsAndQualifiersOfAQueryAreCheckedBeforeTheAdapterOnAServerEngine: on a
// strict PostgreSQL mount a field that no column of the table has, and a qualifier
// that no source has, are refused (ErrInvalidDTQL) before the adapter is reached, by
// every route that hands a query to it; what the tables hold passes.
func TestTheFieldsAndQualifiersOfAQueryAreCheckedBeforeTheAdapterOnAServerEngine(t *testing.T) {
	setPreview(t, true, "1")
	for _, c := range fieldsCases() {
		t.Run(c.name, func(t *testing.T) {
			db, fake := openFields(t, "postgres", schema.ModeStrict)
			_, err := db.Executor().ExecuteQueryToRecordsReader(context.Background(), c.query)
			if c.want == "" {
				if !fake.reached(err) {
					t.Fatalf("%v with %d adapter calls, want the query to reach the adapter", err, fake.queries)
				}
				return
			}
			if !errors.Is(err, ErrInvalidDTQL) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want an invalid query that says %q", err, c.want)
			}
			if fake.queries != 0 {
				t.Fatalf("the adapter was reached %d times by a refused query", fake.queries)
			}
		})
	}
}

// TestEveryRouteThatReadsRefusesAFieldNoColumnHasBeforeTheAdapter: the wire query, the
// DTQL document, the snapshot of it and a read transaction of the join source refuse
// a field the table does not have, and reach the adapter with one it has.
func TestEveryRouteThatReadsRefusesAFieldNoColumnHasBeforeTheAdapter(t *testing.T) {
	setPreview(t, true, "1")
	ctx := context.Background()
	parse := func(doc string) dal.StructuredQuery {
		query, _, err := ParseDTQL([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		return query
	}
	routes := map[string]func(*Database, string, string) error{
		"Execute, where": func(db *Database, field, _ string) error {
			_, err := db.Execute(ctx, Query{Collection: "customers", Where: []Filter{{Field: field, Op: "==", Value: "x"}}})
			return err
		},
		"Execute, order by": func(db *Database, field, _ string) error {
			_, err := db.Execute(ctx, Query{Collection: "customers", OrderBy: []OrderBy{{Field: field}}})
			return err
		},
		"Execute, keys only": func(db *Database, field, _ string) error {
			_, err := db.Execute(ctx, Query{Collection: "customers", KeysOnly: true, Where: []Filter{{Field: field, Op: "==", Value: "x"}}})
			return err
		},
		"ExecuteDTQL": func(db *Database, field, _ string) error {
			_, err := db.ExecuteDTQL(ctx, []byte("from: {name: customers}\nwhere: {op: '==', left: {field: "+field+"}, right: {value: x}}\n"))
			return err
		},
		"ExecuteDTQLQuery": func(db *Database, field, _ string) error {
			_, err := db.ExecuteDTQLQuery(ctx, parse("from: {name: customers}\nwhere: {op: '==', left: {field: "+field+"}, right: {value: x}}\n"))
			return err
		},
		"StreamDTQLSnapshot": func(db *Database, field, _ string) error {
			return db.StreamDTQLSnapshot(ctx, parse("from: {name: customers}\ncolumns: [{field: "+field+"}]\n"), func(Record) error { return nil })
		},
		"ReadTx": func(db *Database, field, _ string) error {
			return db.ReadTx(ctx, func(tx dal.QueryExecutor) error {
				_, err := tx.ExecuteQueryToRecordsReader(ctx, parse("from: {name: customers}\norderBy: [{field: "+field+"}]\n"))
				return err
			})
		},
	}
	for name, route := range routes {
		t.Run(name, func(t *testing.T) {
			db, fake := openFields(t, "postgres", schema.ModeStrict)
			if err := route(db, "nosuch", ""); !errors.Is(err, ErrInvalidDTQL) || !strings.Contains(err.Error(), `has no field "nosuch"`) {
				t.Fatalf("a field no column has: %v, want an invalid query", err)
			}
			if fake.queries != 0 {
				t.Fatalf("the adapter was reached %d times by a refused query", fake.queries)
			}
			if err := route(db, "name", ""); !fake.reached(err) {
				t.Fatalf("a column the table has: %v with %d adapter calls, want the adapter reached once", err, fake.queries)
			}
		})
	}
}

// TestTheFieldCheckLooksAtNothingOnAnEngineThatIsNotAServerOrNotCleared: a SQLite mount
// answers a name its tables do not have in its own way, a partial or schemaless mount
// has tables that may hold more than it declares (so only a qualifier is checked), and a
// PostgreSQL mount that read the switch off answers 501 for every query, whatever it
// names.
func TestTheFieldCheckLooksAtNothingOnAnEngineThatIsNotAServerOrNotCleared(t *testing.T) {
	unknownField := selectQuery(fieldsOf(fromTree(rootRef("customers"))).Where(equalTo(dal.Field("nosuch"), 1)))
	unknownQualifier := selectQuery(fieldsOf(fromTree(rootRef("customers"))).Where(equalTo(ref("x", "name"), 1)))

	t.Run("sqlite", func(t *testing.T) {
		db, fake := openFields(t, "sqlite", schema.ModeStrict)
		for name, query := range map[string]dal.StructuredQuery{"a field": unknownField, "a qualifier": unknownQualifier} {
			if _, err := db.Executor().ExecuteQueryToRecordsReader(context.Background(), query); !errors.Is(err, errFakeReached) {
				t.Errorf("%s: %v, want the query to reach the adapter", name, err)
			}
		}
		if fake.queries != 2 {
			t.Errorf("the adapter was reached %d times, want 2", fake.queries)
		}
	})
	for _, mode := range []schema.Mode{schema.ModePartial, schema.ModeSchemaless} {
		t.Run(string(mode), func(t *testing.T) {
			setPreview(t, true, "1")
			db, fake := openFields(t, "postgres", mode)
			_, err := db.Executor().ExecuteQueryToRecordsReader(context.Background(), unknownField)
			if !fake.reached(err) {
				t.Errorf("a field: %v with %d adapter calls, want the query to reach the adapter", err, fake.queries)
			}
			if _, err := db.Executor().ExecuteQueryToRecordsReader(context.Background(), unknownQualifier); !errors.Is(err, ErrInvalidDTQL) || fake.queries != 1 {
				t.Errorf("a qualifier: %v with %d adapter calls, want it refused", err, fake.queries)
			}
		})
	}
	t.Run("the switch is off", func(t *testing.T) {
		setPreview(t, false, "")
		db, fake := openFields(t, "postgres", schema.ModeStrict)
		for name, query := range map[string]dal.StructuredQuery{"a field": unknownField, "a qualifier": unknownQualifier} {
			var refusal *QueryUnsupportedError
			if _, err := db.Executor().ExecuteQueryToRecordsReader(context.Background(), query); !errors.As(err, &refusal) {
				t.Errorf("%s: %v, want the refusal of an engine that cannot be queried", name, err)
			}
		}
		if _, err := db.Execute(context.Background(), Query{Collection: "customers", Where: []Filter{{Field: "nosuch", Op: "==", Value: 1}}}); !errors.Is(err, ErrQueryUnsupported) {
			t.Errorf("Execute: %v, want the refusal of an engine that cannot be queried", err)
		}
		if fake.queries != 0 {
			t.Errorf("the adapter was reached %d times with the switch off", fake.queries)
		}
	})
}

// TestTheFieldCheckOfADatabaseThatDeclaresACollectionNobodyKnowsAndTheFoldOfAName: the
// check holds no column for a source the manifest does not give a schema (the source
// guard refuses such a source first), a name is folded only on an engine that folds
// names, and the check is as deep as the guard of the sources and refuses what is deeper.
func TestTheFieldCheckOfADatabaseThatDeclaresACollectionNobodyKnowsAndTheFoldOfAName(t *testing.T) {
	setPreview(t, true, "1")
	db, _ := openFields(t, "postgres", schema.ModeStrict)
	if err := db.guardFields(selectQuery(fieldsOf(fromTree(rootRef("unknown"))).Where(equalTo(dal.Field("nosuch"), 1)))); err != nil {
		t.Errorf("a source with no schema: %v", err)
	}
	if got := (fieldGuard{db: db}).fold("MiXed"); got != "mixed" {
		t.Errorf("fold on an engine that folds = %q", got)
	}
	if got := (fieldGuard{db: &Database{}}).fold("MiXed"); got != "MiXed" {
		t.Errorf("fold on an engine that does not = %q", got)
	}

	deepQuery := func(depth int) dal.StructuredQuery {
		query := selectQuery(fieldsOf(fromTree(rootRef("customers"))))
		for range depth {
			query = withExists("customers", query)
		}
		return query
	}
	if err := db.guardFields(deepQuery(maxQueryTreeDepth / 2)); err != nil {
		t.Errorf("a query nested within the limit: %v", err)
	}
	if err := db.guardFields(deepQuery(maxQueryTreeDepth * 2)); !errors.Is(err, errSourcesTooDeep) {
		t.Errorf("a query of nested queries too deep: %v", err)
	}
	deepDerived := selectQuery(fieldsOf(fromTree(rootRef("customers"))))
	for range maxQueryTreeDepth * 2 {
		deepDerived = selectQuery(fieldsOf(fromTree(dal.NewQuerySource(deepDerived, "d"))))
	}
	if err := db.guardFields(deepDerived); !errors.Is(err, errSourcesTooDeep) {
		t.Errorf("a query of derived sources too deep: %v", err)
	}
	var deepCondition dal.Condition = equalTo(dal.Field("name"), 1)
	for range maxQueryTreeDepth * 2 {
		deepCondition = dal.NewGroupCondition(dal.And, deepCondition)
	}
	if err := db.guardFields(selectQuery(fieldsOf(fromTree(rootRef("customers"))).Where(deepCondition))); !errors.Is(err, errSourcesTooDeep) {
		t.Errorf("a condition too deep: %v", err)
	}
	var deepExpression dal.Expression = dal.Field("name")
	for range maxQueryTreeDepth * 2 {
		deepExpression = dal.Binary(deepExpression, dal.Add, dal.Constant{Value: 1})
	}
	if err := db.guardFields(selectQuery(fieldsOf(fromTree(rootRef("customers"))).Where(equalTo(deepExpression, 1)))); !errors.Is(err, errSourcesTooDeep) {
		t.Errorf("an expression too deep: %v", err)
	}
	if err := db.guardFields(noFromQuery{selectQuery(fieldsOf(fromTree(rootRef("customers"))))}); !errors.Is(err, ErrInvalidDTQL) {
		t.Errorf("a query with no from clause: %v", err)
	}
}
