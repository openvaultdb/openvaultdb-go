package core

import (
	"context"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
)

// handedExecutor is a query executor that keeps the query it is handed.
type handedExecutor struct {
	dal.QueryExecutor
	records, recordset dal.Query
}

func (e *handedExecutor) ExecuteQueryToRecordsReader(_ context.Context, query dal.Query) (dal.RecordsReader, error) {
	e.records = query
	return nil, nil
}

func (e *handedExecutor) ExecuteQueryToRecordsetReader(_ context.Context, query dal.Query, _ ...recordset.Option) (dal.RecordsetReader, error) {
	e.recordset = query
	return nil, nil
}

// declaredMoneyQuery is a query that declares a money configuration, as DALgo's federated executor
// reads it from a query by type assertion.
type declaredMoneyQuery struct {
	dal.StructuredQuery
	config *dal.MoneyConfig
}

func (q declaredMoneyQuery) Money() *dal.MoneyConfig { return q.config }

// TestAScanBoundAndASourceTheMountCannotNameAreKeptWhenTheDatabaseNamesGo: the sources of a
// join lose the database they name and nothing else: a scan bound stays (its limit and
// order), a source that a schema qualifies, which the source guard refuses before the adapter
// is reached, and a derived source are not touched, and a join with no source that names a
// database is the query it was.
func TestAScanBoundAndASourceTheMountCannotNameAreKeptWhenTheDatabaseNamesGo(t *testing.T) {
	on := dal.NewComparison(dal.NewFieldRef("o", "customer_id"), dal.Equal, dal.NewFieldRef("c", "id"))
	joined := func(base, other dal.RecordsetSource) dal.StructuredQuery {
		from := dal.From(base)
		from.Join(dal.NewJoinedSource(other, dal.JoinInner, on))
		return from.NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("o", "id")})
	}
	derived := dal.NewQuerySource(selectQuery(fieldsOf(fromTree(source("pg", "customers", "")))), "c")

	t.Run("a scan bound", func(t *testing.T) {
		scanned := source("pg", "orders", "o").WithScan(5, dal.AscendingField("id"), dal.DescendingField("total"))
		query := joined(scanned, source("pg", "customers", "c"))
		got := withoutDatabaseNames(query)
		if name := joinSourceDatabases(got.From()); name != "" {
			t.Errorf("a source still names %q", name)
		}
		ref := got.From().Base().(dal.CollectionRef)
		if ref.ScanLimit() != 5 || len(ref.ScanOrders()) != 2 {
			t.Errorf("scan = %d %v, want the bound the source had", ref.ScanLimit(), ref.ScanOrders())
		}
		if describeTree(got.From()) != describeTree(query.From()) {
			t.Errorf("tree = %q, want %q", describeTree(got.From()), describeTree(query.From()))
		}
	})
	t.Run("a source a schema qualifies is left to the guard", func(t *testing.T) {
		qualified := dal.NewDatabaseCollectionRef("pg", "audit", "orders", "o")
		query := joined(qualified, dal.NewRootCollectionRef("customers", "c"))
		if got := joinSourceDatabases(withoutDatabaseNames(query).From()); got != "pg" {
			t.Errorf("the source names %q, want it left as it was", got)
		}
	})
	t.Run("a derived source", func(t *testing.T) {
		query := joined(source("pg", "orders", "o"), derived)
		got := withoutDatabaseNames(query)
		if name := joinSourceDatabases(got.From()); name != "" {
			t.Errorf("a source still names %q", name)
		}
		if _, ok := got.From().Joins()[0].RecordsetSource.(dal.QuerySource); !ok {
			t.Errorf("the derived source became %T", got.From().Joins()[0].RecordsetSource)
		}
	})
	t.Run("a join that names no database is the query it was", func(t *testing.T) {
		query := joined(source("", "orders", "o"), source("", "customers", "c"))
		if _, rebuilt := withoutDatabaseNames(query).(unnamedQuery); rebuilt {
			t.Error("a query with nothing to remove was rebuilt")
		}
	})
	t.Run("a query that is no join is the query it was", func(t *testing.T) {
		query := selectQuery(fieldsOf(fromTree(source("pg", "orders", ""))))
		if _, rebuilt := withoutDatabaseNames(query).(unnamedQuery); rebuilt {
			t.Error("a query with no join was rebuilt")
		}
	})
	t.Run("a query with no from clause is the query it was", func(t *testing.T) {
		if got := withoutDatabaseNames(noFromQuery{selectQuery(fieldsOf(fromTree(source("pg", "orders", ""))))}); got.From() != nil {
			t.Errorf("from = %v", got.From())
		}
	})
}

// TestTheQueryWithoutTheDatabaseNamesIsHandedToAnExecutorAsItself: the rebuilt query hands
// itself, and not the query it wraps, to the executor that reads it, prints the clauses it
// holds, and passes on the money configuration of the query it wraps.
func TestTheQueryWithoutTheDatabaseNamesIsHandedToAnExecutorAsItself(t *testing.T) {
	ctx := context.Background()
	config := &dal.MoneyConfig{}
	for name, query := range map[string]dal.StructuredQuery{"a query with no money": namedJoin("pg"), "a query that declares money": declaredMoneyQuery{namedJoin("pg"), config}} {
		t.Run(name, func(t *testing.T) {
			got, ok := withoutDatabaseNames(query).(unnamedQuery)
			if !ok {
				t.Fatalf("not rebuilt: %T", withoutDatabaseNames(query))
			}
			want := (*dal.MoneyConfig)(nil)
			if _, declares := query.(declaredMoneyQuery); declares {
				want = config
			}
			if got.Money() != want {
				t.Errorf("money = %v, want %v", got.Money(), want)
			}
			if text := got.String(); !strings.Contains(text, "orders") || !strings.Contains(text, "c.name") {
				t.Errorf("String = %q, want the source and the columns it holds", text)
			}
			executor := &handedExecutor{}
			if _, err := got.GetRecordsReader(ctx, executor); err != nil {
				t.Fatal(err)
			}
			if _, err := got.GetRecordsetReader(ctx, executor); err != nil {
				t.Fatal(err)
			}
			if _, ok := executor.records.(unnamedQuery); !ok {
				t.Errorf("the records reader was handed a %T", executor.records)
			}
			if _, ok := executor.recordset.(unnamedQuery); !ok {
				t.Errorf("the recordset reader was handed a %T", executor.recordset)
			}
		})
	}
}
