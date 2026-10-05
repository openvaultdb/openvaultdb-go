package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// These tests run a join of one database through the planner of DALgo that stands
// between the guarded executor of a mount and its adapter (dal.NewDB), over an adapter
// that decides a join as the PostgreSQL adapter does: it runs one whose sources name no
// database as a single statement (dal.NativeJoinProvider), and declines one that does,
// which DALgo then reads table by table and joins itself, under its own bounds, in the
// transaction. The adapter of this file keeps what it was handed, so a test says which of
// the two ran and with what.

// joinBackend is a dal.Backend that answers like the adapter of a PostgreSQL mount for a
// join. native is what CanExecuteJoin says when no source names a database (nil accepts
// the join); a source that names one is declined, as dalgo2sql's compiler does.
type joinBackend struct {
	dal.Backend
	native error
	// handed is every query that reached a read of the adapter, in order.
	handed []dal.StructuredQuery
	// read answers a query. It is called for the statement of a native join and for the
	// scan of each table of the generic one.
	read func(dal.StructuredQuery) (dal.RecordsReader, error)
	// fields is what the catalog says a table holds, for the generic join.
	fields func(dal.RecordsetSource) ([]string, error)
}

func (b *joinBackend) ID() string { return "pg" }

func (b *joinBackend) CanExecuteJoin(_ context.Context, q dal.StructuredQuery) error {
	if joinSourceDatabases(q.From()) != "" {
		return fmt.Errorf("join_plan: PostgreSQL cannot natively compile query: %w", dal.ErrNotSupported)
	}
	return b.native
}

func (b *joinBackend) answer(query dal.Query) (dal.RecordsReader, error) {
	structured := query.(dal.StructuredQuery)
	b.handed = append(b.handed, structured)
	if b.read == nil {
		return &rowsReader{}, nil
	}
	return b.read(structured)
}

func (b *joinBackend) ExecuteQueryToRecordsReader(_ context.Context, query dal.Query) (dal.RecordsReader, error) {
	return b.answer(query)
}

func (b *joinBackend) RunReadonlyTransaction(ctx context.Context, worker dal.ROTxWorker, _ ...dal.TransactionOption) error {
	return worker(ctx, &joinTx{backend: b})
}

func (b *joinBackend) JoinFields(_ context.Context, source dal.RecordsetSource) ([]string, error) {
	if b.fields == nil {
		return nil, nil
	}
	return b.fields(source)
}

// joinTx is the read transaction of joinBackend.
type joinTx struct {
	dal.ReadTransaction
	backend *joinBackend
}

func (t *joinTx) ExecuteQueryToRecordsReader(_ context.Context, query dal.Query) (dal.RecordsReader, error) {
	return t.backend.answer(query)
}

func (t *joinTx) JoinFields(ctx context.Context, source dal.RecordsetSource) ([]string, error) {
	return t.backend.JoinFields(ctx, source)
}

// rowsReader reads the rows it holds, then fails with err or ends.
type rowsReader struct {
	rows []map[string]any
	next int
	err  error
}

func (r *rowsReader) Next() (record.Record, error) {
	if r.next < len(r.rows) {
		row := r.rows[r.next]
		r.next++
		return record.NewRecordWithData(record.NewKeyWithID("t", fmt.Sprint(row["id"])), row), nil
	}
	if r.err != nil {
		return nil, r.err
	}
	return nil, dal.ErrNoMoreRecords
}
func (r *rowsReader) Cursor() (string, error) { return "", nil }
func (r *rowsReader) Close() error            { return nil }

// joinSourceDatabases returns the first database a source of the relation tree names, or "".
func joinSourceDatabases(from dal.FromSource) string {
	for _, name := range joinSourceDatabaseList(from) {
		if name != "" {
			return name
		}
	}
	return ""
}

// joinSourceDatabaseList lists the database each collection source of a relation tree
// names, in order: the base, then each join's source or tree.
func joinSourceDatabaseList(from dal.FromSource) []string {
	var names []string
	if ref, ok := from.Base().(dal.CollectionRef); ok {
		names = append(names, ref.Database())
	}
	for _, join := range from.Joins() {
		if tree := join.From(); tree != nil {
			names = append(names, joinSourceDatabaseList(tree)...)
		} else if ref, ok := join.RecordsetSource.(dal.CollectionRef); ok {
			names = append(names, ref.Database())
		}
	}
	return names
}

// openJoinMount opens a mount over backend, wrapped as every adapter is by dal.NewDB, of
// the engine, with orders and customers declared. Its id is pg.
func openJoinMount(t *testing.T, engine string, backend *joinBackend) *Database {
	t.Helper()
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "pg", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"orders":    {Fields: map[string]schema.Field{"customer_id": {Type: schema.TypeString}, "total": {Type: schema.TypeInteger}}},
			"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}, "FirstName": {Type: schema.TypeString}}},
			"regions":   {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
		}},
	}
	db, err := Open(m, dal.NewDB(backend), []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// source is a collection of the database, or of the endpoint of a database when it names none.
func source(database, name, alias string) dal.CollectionRef {
	if database == "" {
		return dal.NewRootCollectionRef(name, alias)
	}
	return dal.NewDatabaseCollectionRef(database, "", name, alias)
}

// namedJoin is a join of orders and customers of the database, every source of which names
// it, as every source of a document of /v1/dtql does.
func namedJoin(database string) dal.StructuredQuery {
	orders := source(database, "orders", "o")
	customers := source(database, "customers", "c")
	from := dal.From(orders)
	from.Join(dal.NewJoinedSource(customers, dal.JoinInner, dal.NewComparison(dal.NewFieldRef("o", "customer_id"), dal.Equal, dal.NewFieldRef("c", "id"))))
	return from.NewQuery().SelectColumns(
		dal.Column{Expression: dal.NewFieldRef("o", "id")},
		dal.Column{Expression: dal.NewFieldRef("c", "name")},
	)
}

// readVia runs query through one route that hands a structured query to the adapter and
// returns what it ended with: the executor of the mount, or a read transaction of it.
func readVia(db *Database, route string, query dal.StructuredQuery) error {
	ctx := context.Background()
	drain := func(reader dal.RecordsReader, err error) error {
		if err != nil {
			return err
		}
		for {
			if _, err := reader.Next(); err != nil {
				closeErr := reader.Close()
				if !errors.Is(err, io.EOF) {
					return err
				}
				return closeErr
			}
		}
	}
	if route == "Executor" {
		return drain(db.Executor().ExecuteQueryToRecordsReader(ctx, query))
	}
	return db.ReadTx(ctx, func(tx dal.QueryExecutor) error {
		return drain(tx.ExecuteQueryToRecordsReader(ctx, query))
	})
}

var joinRoutes = []string{"Executor", "ReadTx"}

// describeTree writes a relation tree down without the databases its sources name: what
// each source reads, under which alias, how each join is made and on what, and its hints.
func describeTree(from dal.FromSource) string {
	var b strings.Builder
	b.WriteString(describeSource(from.Base()))
	for _, join := range from.Joins() {
		fmt.Fprintf(&b, " %s join ", join.JoinType())
		if tree := join.From(); tree != nil {
			b.WriteString("(" + describeTree(tree) + ")")
		} else {
			b.WriteString(describeSource(join.RecordsetSource))
		}
		for _, on := range join.On() {
			fmt.Fprintf(&b, " on %v", on)
		}
		fmt.Fprintf(&b, " hints %v", join.Algorithms())
	}
	return b.String()
}

func describeSource(source dal.RecordsetSource) string {
	ref, ok := source.(dal.CollectionRef)
	if !ok {
		return fmt.Sprintf("%T", source)
	}
	return fmt.Sprintf("%s as %q scan %d %v", ref.Name(), ref.Alias(), ref.ScanLimit(), ref.ScanOrders())
}

// nestedJoin is a join whose second source is a relation tree of two, with a left join and a
// hint, every source of which names the database.
func nestedJoin(database string) dal.StructuredQuery {
	customers := source(database, "customers", "c")
	regions := source(database, "regions", "r")
	tree := dal.From(customers)
	tree.Join(dal.NewJoinedSource(regions, dal.JoinLeft, dal.NewComparison(dal.NewFieldRef("c", "name"), dal.Equal, dal.NewFieldRef("r", "name"))))
	from := dal.From(source(database, "orders", "o"))
	from.Join(dal.NewNestedJoinedSource(tree, dal.JoinInner,
		dal.NewComparison(dal.NewFieldRef("o", "customer_id"), dal.Equal, dal.NewFieldRef("c", "id"))).WithAlgorithms(dal.JoinAlgorithmHash))
	return from.NewQuery().Where(dal.NewComparison(dal.NewFieldRef("o", "total"), dal.GreaterThen, dal.Constant{Value: 5})).
		SelectColumns(dal.Column{Expression: dal.NewFieldRef("o", "id")})
}

// TestAJoinOfOneDatabaseIsHandedToAServerMountWithoutTheDatabaseNamesOfItsSources: every
// source of a document of /v1/dtql names its database, and the compiler of the adapter
// declines a join whose source does, so DALgo would read each table whole, with no WHERE,
// under a bound of its own, and join them in the transaction. The source guard has checked
// that the name is the mount's own, so the document is handed to the mount of a server
// engine without it: the adapter then runs the join as one statement. What else the
// document holds (the aliases, the kind of each join, its conditions, the nesting of the
// tree, the hints) is as it was written.
func TestAJoinOfOneDatabaseIsHandedToAServerMountWithoutTheDatabaseNamesOfItsSources(t *testing.T) {
	setPreview(t, true, "1")
	for _, c := range []struct {
		name  string
		query dal.StructuredQuery
	}{
		{"a join", namedJoin("pg")},
		{"a nested relation tree, a left join and a hint", nestedJoin("pg")},
	} {
		for _, route := range joinRoutes {
			t.Run(c.name+"/"+route, func(t *testing.T) {
				backend := &joinBackend{}
				if err := readVia(openJoinMount(t, "postgres", backend), route, c.query); err != nil {
					t.Fatal(err)
				}
				if len(backend.handed) != 1 {
					t.Fatalf("the adapter was reached %d times, want once with the whole join (a join it declined is read table by table)", len(backend.handed))
				}
				handed := backend.handed[0]
				if name := joinSourceDatabases(handed.From()); name != "" {
					t.Errorf("the adapter was handed a source that names the database %q", name)
				}
				if got, want := describeTree(handed.From()), describeTree(c.query.From()); got != want {
					t.Errorf("relation tree = %q, want %q", got, want)
				}
				if got, want := fmt.Sprint(handed.Where(), handed.Columns()), fmt.Sprint(c.query.Where(), c.query.Columns()); got != want {
					t.Errorf("clauses = %q, want %q", got, want)
				}
			})
		}
	}
}

// TestAQueryThatNamesNoDatabaseOrReachesAnEngineThatIsNotAServerIsHandedOnAsItWas: a
// document with no database named (a source of the endpoint of a database), a query of one
// collection (no join has a source the compiler declines for its database), and a mount that is
// not a server engine, are not rewritten.
func TestAQueryThatNamesNoDatabaseOrReachesAnEngineThatIsNotAServerIsHandedOnAsItWas(t *testing.T) {
	setPreview(t, true, "1")
	for _, c := range []struct {
		name   string
		engine string
		query  dal.StructuredQuery
		want   string // the first database the adapter is handed a source with
	}{
		{"a join that names no database", "postgres", namedJoin(""), ""},
		{"a query of one collection that names the database", "postgres", selectQuery(fieldsOf(fromTree(dal.NewDatabaseCollectionRef("pg", "", "orders", "")))), "pg"},
		{"a join on an engine that is not a server", "sqlite", namedJoin("pg"), "pg"},
	} {
		for _, route := range joinRoutes {
			t.Run(c.name+"/"+route, func(t *testing.T) {
				backend := &joinBackend{}
				if err := readVia(openJoinMount(t, c.engine, backend), route, c.query); err != nil {
					t.Fatal(err)
				}
				if len(backend.handed) == 0 {
					t.Fatal("the adapter was not reached")
				}
				if got := joinSourceDatabases(backend.handed[0].From()); got != c.want {
					t.Errorf("the adapter was handed a source of the database %q, want %q", got, c.want)
				}
			})
		}
	}
}

// genericJoin is a backend whose adapter declines every join, as the compiler of the PostgreSQL
// adapter does for what it cannot write as one statement (a wildcard, an aggregate it has no
// form for, two columns of types it cannot equate): DALgo then reads each table whole in the
// transaction and joins the rows itself.
func genericJoin() *joinBackend {
	return &joinBackend{native: fmt.Errorf("join_plan: PostgreSQL cannot natively compile query: %w", dal.ErrNotSupported)}
}

// orderRows and customerRows are what the scan of each table of the document gives.
func scanRows(count int) *rowsReader {
	rows := make([]map[string]any, count)
	for i := range rows {
		rows[i] = map[string]any{"id": fmt.Sprint(i), "customer_id": "0", "name": "n"}
	}
	return &rowsReader{rows: rows}
}

// joinOf is the join of orders and customers of namedJoin with a column, a source that names
// no database (the generic join is asked for one the document was written for).
func joinOf(column dal.Column) dal.StructuredQuery {
	from := dal.From(dal.NewRootCollectionRef("orders", "o"))
	from.Join(dal.NewJoinedSource(dal.NewRootCollectionRef("customers", "c"), dal.JoinInner,
		dal.NewComparison(dal.NewFieldRef("o", "customer_id"), dal.Equal, dal.NewFieldRef("c", "id"))))
	return from.NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("o", "id")}, column)
}

// TestWhatTheJoinOfDalgoRefusesBelowTheMountIsARefusalOnAServerEngine: a join the adapter
// declines is evaluated by DALgo in the transaction, under the mount, and what it refuses
// there is the caller's or the bound of the request, as it is anywhere else the same engine
// runs: the bound of its scan, and a field the table does not hold, stay the typed refusals
// DALgo raised, which the server answers 422 and 400. The error that is kept is rebuilt from
// the category, the path and the message, so nothing of the cause comes with it.
func TestWhatTheJoinOfDalgoRefusesBelowTheMountIsARefusalOnAServerEngine(t *testing.T) {
	setPreview(t, true, "1")
	known := func(source dal.RecordsetSource) ([]string, error) {
		if source.Name() == "orders" {
			return []string{"id", "customer_id", "total"}, nil
		}
		return []string{"id", "name", "firstname"}, nil
	}
	for _, route := range joinRoutes {
		t.Run(route+"/the bound of the scan", func(t *testing.T) {
			backend := genericJoin()
			backend.read = func(dal.StructuredQuery) (dal.RecordsReader, error) { return scanRows(10001), nil }
			err := readVia(openJoinMount(t, "postgres", backend), route, joinOf(dal.Column{Expression: dal.NewFieldRef("c", "name")}))
			var refusal *dal.JoinValidationError
			if !errors.As(err, &refusal) || refusal.Category != "join_plan" || refusal.Message != "relation scan exceeds row or byte bound" {
				t.Fatalf("%v, want the scan bound DALgo raised", err)
			}
			if errors.Is(err, ErrQueryNotRunnable) {
				t.Errorf("a bound is not a query nothing can run: %v", err)
			}
		})
		t.Run(route+"/a field the table does not hold", func(t *testing.T) {
			backend := genericJoin()
			backend.fields = known
			err := readVia(openJoinMount(t, "postgres", backend), route, joinOf(dal.Column{Expression: dal.NewFieldRef("c", "FirstName")}))
			var refusal *dal.JoinValidationError
			if !errors.As(err, &refusal) || refusal.Category != "join_field" || !strings.Contains(refusal.Message, `"FirstName"`) {
				t.Fatalf("%v, want the refusal of a field DALgo raised", err)
			}
		})
		t.Run(route+"/an alias used twice, which the planner refuses before it chooses an engine", func(t *testing.T) {
			from := dal.From(dal.NewRootCollectionRef("orders", "o"))
			from.Join(dal.NewJoinedSource(dal.NewRootCollectionRef("customers", "o"), dal.JoinInner,
				dal.NewComparison(dal.NewFieldRef("o", "id"), dal.Equal, dal.NewFieldRef("o", "id"))))
			err := readVia(openJoinMount(t, "postgres", &joinBackend{}), route, from.NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("o", "id")}))
			var refusal *dal.JoinValidationError
			if !errors.As(err, &refusal) || refusal.Category != "join_scope" || !strings.Contains(refusal.Message, `duplicate alias "o"`) {
				t.Fatalf("%v, want the refusal of an alias used twice DALgo raised", err)
			}
		})
		for name, failure := range map[string]func(*joinBackend){
			"a scan that fails": func(b *joinBackend) {
				b.read = func(dal.StructuredQuery) (dal.RecordsReader, error) {
					return &rowsReader{err: errors.New("connection to server " + failureMarker + " lost")}, nil
				}
			},
			"a scan that cannot start": func(b *joinBackend) {
				b.read = func(dal.StructuredQuery) (dal.RecordsReader, error) {
					return nil, errors.New("connection to server " + failureMarker + " refused")
				}
			},
			"a field list that cannot be read": func(b *joinBackend) {
				b.fields = func(dal.RecordsetSource) ([]string, error) {
					return nil, errors.New("relation " + failureMarker + " does not exist")
				}
			},
		} {
			t.Run(route+"/"+name, func(t *testing.T) {
				backend := genericJoin()
				failure(backend)
				err := readVia(openJoinMount(t, "postgres", backend), route, joinOf(dal.Column{Expression: dal.NewFieldRef("c", "name")}))
				if err == nil {
					t.Fatal("no error")
				}
				var refusal *dal.JoinValidationError
				if errors.As(err, &refusal) || strings.Contains(err.Error(), failureMarker) {
					t.Errorf("%v: the text of the failed read is kept", err)
				}
				if errors.Is(err, ErrQueryNotRunnable) || errors.Is(err, ErrQueryDoesNotFit) {
					t.Errorf("a failure of the server is not a refusal: %v", err)
				}
			})
		}
		t.Run(route+"/a scan the server refuses for a value", func(t *testing.T) {
			backend := genericJoin()
			backend.read = func(dal.StructuredQuery) (dal.RecordsReader, error) {
				return &rowsReader{err: &sqlStateError{code: "22P02"}}, nil
			}
			err := readVia(openJoinMount(t, "postgres", backend), route, joinOf(dal.Column{Expression: dal.NewFieldRef("c", "name")}))
			if !errors.Is(err, ErrQueryDoesNotFit) || strings.Contains(err.Error(), failureMarker) {
				t.Errorf("%v, want the refusal of a value the server could not read", err)
			}
		})
		t.Run(route+"/an engine that is not a server keeps what DALgo said", func(t *testing.T) {
			backend := genericJoin()
			cause := errors.New("sqlite says " + failureMarker)
			backend.read = func(dal.StructuredQuery) (dal.RecordsReader, error) { return &rowsReader{err: cause}, nil }
			err := readVia(openJoinMount(t, "sqlite", backend), route, joinOf(dal.Column{Expression: dal.NewFieldRef("c", "name")}))
			if !errors.Is(err, cause) {
				t.Errorf("%v, want the adapter's error in the chain", err)
			}
		})
	}
}
