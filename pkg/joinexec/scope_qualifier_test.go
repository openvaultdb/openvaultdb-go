package joinexec

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// The helpers of this file start with "sq" so that they cannot clash with the helpers of
// the tests around them.
//
// The rule of these tests is the one of the whole change: the server sorts a document, binds
// it or refuses it, and never ignores part of it. A document of one source that names its
// database is handed to the mount whole, and the executor of a mount reads the name of a
// field and nothing else of it: a qualifier that no source of the query has was ignored (a
// status 200), where DALgo, which evaluates every other document, refuses it. Likewise a
// name inside the arithmetic of an ORDER BY that a column also calls its own and that no
// field list can place, and the key pseudo-field where DALgo, which does not know it, sorts.

// sqHanded is a document of one source that names its database, over the mount alMount:
// the shape DALgo's federated executor hands to the mount whole.
func sqHanded(build func(b dal.IQueryBuilder) dal.StructuredQuery) dal.StructuredQuery {
	return build(dal.From(exRef("one", "A", "a")).NewQuery())
}

// sqNamed selects the name of the one source, which every document below does.
var sqNamed = dal.Column{Expression: dal.NewFieldRef("a", "name")}

// A qualifier that names no source of the query a field stands in is refused before a
// mount is asked for anything, with the words DALgo itself uses on the documents it
// evaluates (a query_scope error: the unknown alias, at the path of the field). The same
// document on the per-database endpoint, which DALgo evaluates, is refused in the same words.
func TestAQualifierThatNamesNoSourceOfTheQueryIsRefusedBeforeAMountIsAsked(t *testing.T) {
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		path  string
		alias string
	}{
		"ORDER BY a field the source does not have": {sqHanded(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("zzz", "nope"))).SelectColumns(sqNamed)
		}), "orderBy[0].source", "zzz"},
		"ORDER BY a field the source has": {sqHanded(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Descending(dal.NewFieldRef("zzz", "name"))).SelectColumns(sqNamed)
		}), "orderBy[0].source", "zzz"},
		"ORDER BY the collection of a source that has an alias": {sqHanded(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Descending(dal.NewFieldRef("A", "name"))).SelectColumns(sqNamed)
		}), "orderBy[0].source", "A"},
		"the second ordering": {sqHanded(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("a", "name")), dal.Ascending(dal.NewFieldRef("zzz", "k"))).SelectColumns(sqNamed)
		}), "orderBy[1].source", "zzz"},
		"an operand of arithmetic in ORDER BY": {sqHanded(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.Binary(dal.NewFieldRef("zzz", "k"), dal.Multiply, srOne))).SelectColumns(sqNamed)
		}), "orderBy[0].left.source", "zzz"},
		// The same hole, in the clauses that a mount reads by the name of the field too.
		"WHERE": {sqHanded(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(dal.NewFieldRef("zzz", "name"), dal.Equal, dal.NewConstant("a1"))).SelectColumns(sqNamed)
		}), "where.left.source", "zzz"},
		"a column": {sqHanded(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(dal.Column{Expression: dal.NewFieldRef("zzz", "name")})
		}), "columns[0].source", "zzz"},
	} {
		t.Run(name, func(t *testing.T) {
			mount := alMount()
			_, err := exRun(t, tc.query, "", newExRegistry(mount), exAllow, Limits{})
			var refused *dal.QueryValidationError
			if !errors.As(err, &refused) || refused.Category != "query_scope" || refused.Path != tc.path || refused.Message != `unknown alias "`+tc.alias+`"` {
				t.Fatalf("got %T %v, want query_scope at %s: unknown alias %q", err, err, tc.path, tc.alias)
			}
			if executorCalls, txCalls := mount.counts(); executorCalls != 0 || txCalls != 0 || len(mount.exec.seen()) != 0 {
				t.Fatalf("the mount was used (%d executors, %d transactions, %d reads) for a refused document", executorCalls, txCalls, len(mount.exec.seen()))
			}
		})
	}
}

// The qualifier of a source of the query, by its alias or, with none, by its collection,
// and the qualifier of a query around the one that names it, are not refused.
func TestAQualifierThatNamesASourceOfTheQueryOrOfAQueryAroundItIsNotRefused(t *testing.T) {
	t.Run("the alias of the source, handed whole", func(t *testing.T) {
		mount := alMount()
		res, err := exRun(t, sqHanded(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Descending(dal.NewFieldRef("a", "name"))).SelectColumns(sqNamed)
		}), "", newExRegistry(mount), exAllow, Limits{})
		if err != nil || len(res.Records) != 3 {
			t.Fatalf("Execute: %v (%d rows)", err, len(res.Records))
		}
	})
	t.Run("the collection of a source with no alias", func(t *testing.T) {
		mount := alMount()
		query := dal.From(exRef("one", "A", "")).NewQuery().OrderBy(dal.Descending(dal.NewFieldRef("A", "name"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("A", "name")})
		if _, err := exRun(t, query, "", newExRegistry(mount), exAllow, Limits{}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	t.Run("the source of a query around a subquery", func(t *testing.T) {
		mount := alMount()
		inner := dal.From(exRef("one", "A", "x")).NewQuery().Where(dal.NewComparison(dal.NewFieldRef("x", "k"), dal.Equal, dal.NewFieldRef("a", "k"))).
			OrderBy(dal.Ascending(dal.NewFieldRef("a", "name"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("x", "k")})
		query := sqHanded(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewExistsCondition(inner)).SelectColumns(sqNamed)
		})
		if _, err := exRun(t, query, "", newExRegistry(mount), exAllow, Limits{}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
}

// A one-source document with a mount that holds access policies is not handed whole when it
// orders by an expression: it is read as a plain scan through the policy-checked executor
// and sorted above it, the path a null test already took. (The server refuses a document
// over such a database before Execute: a relational document is not run on one, so this is
// not an answer the HTTP endpoints give.)
func TestAnOrderingExpressionOverAMountWithAccessPoliciesIsReadAsAPlainScanAndSortedAbove(t *testing.T) {
	protected := exMount("hr", "sqlite", true, map[string][]record.Record{"A": exRows("A", "a", 3)})
	protected.exec.fields = []string{"id", "k", "name"}
	query := dal.From(exRef("hr", "A", "a")).NewQuery().
		OrderBy(dal.Ascending(dal.Binary(dal.NewFieldRef("a", "k"), dal.Multiply, dal.NewConstant(-1)))).SelectColumns(sqNamed)
	res, err := exRun(t, query, "", newExRegistry(protected), exAllow, Limits{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	seen := protected.exec.seen()
	if len(seen) != 1 || len(seen[0].(dal.StructuredQuery).OrderBy()) != 0 {
		t.Fatalf("the protected mount was asked %v, want one read with no ordering", seen)
	}
	if _, txCalls := protected.counts(); txCalls != 0 {
		t.Fatalf("a protected mount runs no transaction of its own: %d", txCalls)
	}
	var names []any
	for _, rec := range res.Records {
		names = append(names, rec.Data().(map[string]any)["name"])
	}
	if want := []any{"a3", "a2", "a1"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

// sqAliased selects a field of the one source under the name T, and orders by an
// expression that reads T inside arithmetic.
func sqAliased(column dal.Column, order dal.Expression) dal.StructuredQuery {
	return srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
		return b.OrderBy(dal.Ascending(order)).SelectColumns(column)
	})
}

// A SQL database reads a name inside the arithmetic of an ORDER BY as the alias of a column
// when its table has no column of that name, and as the column of the table when it has one.
// Where the one source of the query supplies no field list nothing can say which, and a mount
// would read the name as a field: a record that has none sorted nothing, with a status 200.
// The name is refused, with the way out. (Where the list is supplied the name is a field of
// the source, or unknown, and that is decided and said already.)
func TestAnAliasInsideOrderByArithmeticOverASourceWithNoFieldListIsRefused(t *testing.T) {
	plus := func(name string) dal.Expression {
		return dal.Binary(dal.NewFieldRef("", name), dal.Add, srOne)
	}
	field := dal.Column{Expression: dal.NewFieldRef("a", "x"), Alias: "T"}
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		path  string
	}{
		"the alias of a field": {sqAliased(field, plus("T")), "orderBy[0].left"},
		"the alias of an expression": {sqAliased(dal.Column{Expression: dal.Binary(dal.NewFieldRef("a", "x"), dal.Add, srOne), Alias: "T"}, plus("T")),
			"orderBy[0].left"},
		"the result name of a scalar subquery": {sqAliased(dal.Column{Expression: dal.NewQueryExpression(srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), "T")}, plus("T")), "orderBy[0].left"},
		"the right operand": {sqAliased(field, dal.Binary(srOne, dal.Add, dal.NewFieldRef("", "T"))), "orderBy[0].right"},
		"the second ordering": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("a", "id")), dal.Ascending(plus("T"))).SelectColumns(field)
		}), "orderBy[1].left"},
		"a derived source, whose columns the walk does not know": {
			dal.From(dal.NewQuerySource(srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
				return b.SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
			}), "d")).NewQuery().OrderBy(dal.Ascending(plus("T"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "x"), Alias: "T"}),
			"orderBy[0].left"},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{} // no source supplies a list
			scope := scAsScope(t, checkScopes(context.Background(), tc.query, supplier.fields))
			if scope.Path != tc.path {
				t.Errorf("path = %q, want %q", scope.Path, tc.path)
			}
			for _, want := range []string{"T", "inside arithmetic", "no field list", "qualify the field with its source"} {
				if !strings.Contains(scope.Message, want) {
					t.Errorf("message %q does not say %q", scope.Message, want)
				}
			}
		})
	}
}

// What a refusal of the name would take away from a document that is read right stays: a
// name no column carries is the mount's to read, a source that supplies its list says what the
// name is, and a name that is the whole of the expression is the alias and is replaced.
func TestAnAliasInsideOrderByArithmeticIsLeftAloneWhenTheDocumentIsNotAmbiguous(t *testing.T) {
	plus := func(ref dal.FieldRef) dal.Expression { return dal.Binary(ref, dal.Add, srOne) }
	field := dal.Column{Expression: dal.NewFieldRef("a", "x"), Alias: "T"}
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		lists map[string][]string
	}{
		"no column carries the name":  {sqAliased(field, plus(dal.NewFieldRef("", "zz"))), nil},
		"the name is qualified":       {sqAliased(field, plus(dal.NewFieldRef("a", "T"))), nil},
		"the column carries no alias": {sqAliased(dal.Column{Expression: dal.NewFieldRef("a", "x")}, plus(dal.NewFieldRef("", "x"))), nil},
		// A column named as the field it selects is that field under its own name: both readings
		// of the name are one, so nothing is ambiguous.
		"the column selects the field of the same name, qualified":   {sqAliased(dal.Column{Expression: dal.NewFieldRef("a", "x"), Alias: "x"}, plus(dal.NewFieldRef("", "x"))), nil},
		"the column selects the field of the same name, unqualified": {sqAliased(dal.Column{Expression: dal.NewFieldRef("", "x"), Alias: "x"}, plus(dal.NewFieldRef("", "x"))), nil},
		"the source supplies its list and carries the name": {sqAliased(field, plus(dal.NewFieldRef("", "T"))),
			map[string][]string{"A": {"T", "x"}}},
		"the name is the whole of the expression, which is the alias of a field": {sqAliased(field, dal.NewFieldRef("", "T")), nil},
		"a query that aggregates reads the alias as the column": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(dal.NewFieldRef("a", "x")).OrderBy(dal.Ascending(plus(dal.NewFieldRef("", "n")))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")}, dal.CountAs(dal.Star(), "n"))
		}), nil},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: tc.lists}
			if err := checkScopes(context.Background(), tc.query, supplier.fields); err != nil {
				t.Fatalf("checkScopes: %v", err)
			}
		})
	}
}

// DALgo does not know the key pseudo-field of the document engines: over a source with a
// field list it refuses it as unavailable, and over one with none it reads it as a null, so
// an ordering by it did nothing, with a status 200. Only a mount that is handed the document
// whole sorts by it, and a document DALgo evaluates is refused when it orders by it.
func TestTheKeyIsRefusedInOrderByWhereDALgoEvaluatesTheDocument(t *testing.T) {
	key := dal.NewFieldRef("", "$id")
	qualifiedKey := dal.NewFieldRef("a", "$id")
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		path  string
	}{
		"the key":            {sqAliased(sqNamed, key), "orderBy[0]"},
		"the key, qualified": {sqAliased(sqNamed, qualifiedKey), "orderBy[0]"},
		// An alias that stands for the key is replaced by the key before DALgo sees the
		// document, which reads it as a null: it is refused as the key itself is.
		"an alias of the key":              {sqAliased(dal.Column{Expression: dal.NewFieldRef("a", "$id"), Alias: "k"}, dal.NewFieldRef("", "k")), "orderBy[0]"},
		"an alias of the key, unqualified": {sqAliased(dal.Column{Expression: dal.NewFieldRef("", "$id"), Alias: "k"}, dal.NewFieldRef("", "k")), "orderBy[0]"},
		"an alias of the key beside an ordering by arithmetic": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("", "k")), dal.Ascending(dal.Binary(dal.NewFieldRef("a", "x"), dal.Multiply, srOne))).
				SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "$id"), Alias: "k"})
		}), "orderBy[0]"},
		"an alias of the key in a derived source": {dal.From(dal.NewQuerySource(sqAliased(dal.Column{Expression: dal.NewFieldRef("a", "$id"), Alias: "k"}, dal.NewFieldRef("", "k")), "d")).NewQuery().
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "k")}), "from.query.orderBy[0]"},
		"an operand of arithmetic": {sqAliased(sqNamed, dal.Binary(key, dal.Add, srOne)), "orderBy[0].left"},
		"beside an ordering by arithmetic": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(key), dal.Ascending(dal.Binary(dal.NewFieldRef("a", "x"), dal.Multiply, srOne))).SelectColumns(sqNamed)
		}), "orderBy[0]"},
		"in a query nested in a clause": {func() dal.StructuredQuery {
			inner := sqAliased(sqNamed, key)
			return dal.From(exRef("", "C", "c")).NewQuery().Where(dal.NewExistsCondition(inner)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("c", "k")})
		}(), "where.query.orderBy[0]"},
		"in a derived source": {dal.From(dal.NewQuerySource(sqAliased(sqNamed, key), "d")).NewQuery().
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "name")}), "from.query.orderBy[0]"},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{}
			scope := scAsScope(t, checkScopes(context.Background(), tc.query, supplier.fields))
			if scope.Path != tc.path {
				t.Errorf("path = %q, want %q", scope.Path, tc.path)
			}
			for _, want := range []string{"$id", "order by a field"} {
				if !strings.Contains(scope.Message, want) {
					t.Errorf("message %q does not say %q", scope.Message, want)
				}
			}
			if len(supplier.asked) != 0 {
				t.Errorf("a source was asked for its fields: %v", supplier.asked)
			}
		})
	}

	t.Run("the mount that is handed the document whole sorts by it", func(t *testing.T) {
		supplier := &scSupplier{}
		query := sqAliased(sqNamed, qualifiedKey)
		if err := checkScopes(context.Background(), query, supplier.fields, keyOrderedByTheMount); err != nil {
			t.Fatalf("checkScopes: %v", err)
		}
		// So does it for an alias of the key, which is replaced by the key first.
		aliased := sqAliased(dal.Column{Expression: dal.NewFieldRef("a", "$id"), Alias: "k"}, dal.NewFieldRef("", "k"))
		if err := checkScopes(context.Background(), aliased, supplier.fields, keyOrderedByTheMount); err != nil {
			t.Fatalf("an alias of the key: %v", err)
		}
		// A nested query is always evaluated by DALgo, whatever the root is handed.
		inner := dal.From(exRef("", "C", "c")).NewQuery().Where(dal.NewExistsCondition(sqAliased(sqNamed, key))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("c", "k")})
		scope := scAsScope(t, checkScopes(context.Background(), inner, supplier.fields, keyOrderedByTheMount))
		if scope.Path != "where.query.orderBy[0]" {
			t.Fatalf("path = %q", scope.Path)
		}
	})
}

// Through Execute: the document a mount is handed whole is sorted by the key by the mount,
// and each shape DALgo evaluates instead is refused before the mount is read.
func TestTheKeyIsSortedByAMountThatIsHandedTheDocumentWholeAndRefusedElsewhere(t *testing.T) {
	key := dal.NewFieldRef("", "$id")
	named := dal.Column{Expression: dal.NewFieldRef("a", "name")}
	whole := func(build func(b dal.IQueryBuilder) dal.StructuredQuery) dal.StructuredQuery {
		return build(dal.From(exRef("one", "A", "a")).NewQuery())
	}
	t.Run("a plain document of one source that names its database", func(t *testing.T) {
		mount := alMount()
		if _, err := exRun(t, whole(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Descending(key)).SelectColumns(named)
		}), "", newExRegistry(mount), exAllow, Limits{}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		seen := mount.exec.seen()
		if len(seen) != 1 || len(seen[0].(dal.StructuredQuery).OrderBy()) != 1 {
			t.Fatalf("the mount was asked %v, want one read with the ordering by the key", seen)
		}
	})
	for name, tc := range map[string]dal.StructuredQuery{
		"beside an ordering by arithmetic": whole(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(key), dal.Ascending(dal.Binary(dal.NewFieldRef("a", "k"), dal.Multiply, srOne))).SelectColumns(named)
		}),
		"with a null test": whole(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewIsNullCondition(dal.NewFieldRef("a", "name"))).OrderBy(dal.Ascending(key)).SelectColumns(named)
		}),
		"with a subquery": whole(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewExistsCondition(exPlain("one", "A"))).OrderBy(dal.Ascending(key)).SelectColumns(named)
		}),
		"with a scan clause":              dal.From(exRef("one", "A", "a").WithScan(5)).NewQuery().OrderBy(dal.Ascending(key)).SelectColumns(named),
		"a source that names no database": dal.From(exRef("", "A", "a")).NewQuery().OrderBy(dal.Ascending(key)).SelectColumns(named),
	} {
		t.Run(name, func(t *testing.T) {
			mount := alMount()
			_, err := exRun(t, tc, "one", newExRegistry(mount), exAllow, Limits{})
			scope := scAsScope(t, err)
			if scope.Path != "orderBy[0]" || !strings.Contains(scope.Message, "$id") {
				t.Fatalf("got %+v", scope)
			}
			if len(mount.exec.seen()) != 0 {
				t.Fatalf("the mount was read for a refused document: %v", mount.exec.seen())
			}
		})
	}
}
