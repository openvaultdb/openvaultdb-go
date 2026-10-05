package core

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// PostgreSQL keeps 63 bytes of a name and silently cuts the rest, so a name of 64 bytes
// or more would address the table or the column named by its first 63. On a PostgreSQL
// mount every route refuses such a name, a collection, a field or a key path, with the
// refusal of its kind (400 invalid_key for a collection, bad_request for a field of a
// write and invalid_dtql for a field of a query), before the driver is reached. A name
// of exactly 63 bytes is accepted, and no other engine is held to the limit.

// namedDB is a driver that counts every call that reaches it and fails each with
// errReached, so that a call that was not refused is told from one that was.
type namedDB struct {
	scriptedFieldsDB
	calls atomic.Int32
}

var errReached = errors.New("the driver was reached")

func (f *namedDB) reached() error {
	f.calls.Add(1)
	return errReached
}

func (f *namedDB) Get(context.Context, record.Record) error { return f.reached() }
func (f *namedDB) Exists(context.Context, *record.Key) (bool, error) {
	return false, f.reached()
}
func (f *namedDB) RunReadwriteTransaction(context.Context, dal.RWTxWorker, ...dal.TransactionOption) error {
	return f.reached()
}
func (f *namedDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return nil, f.reached()
}

func (f *namedDB) RunReadonlyTransaction(ctx context.Context, worker dal.ROTxWorker, _ ...dal.TransactionOption) error {
	return worker(ctx, &namedTx{db: f})
}

// namedTx is the read transaction of namedDB.
type namedTx struct {
	dal.ReadTransaction
	db *namedDB
}

func (t *namedTx) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	return t.db.ExecuteQueryToRecordsReader(ctx, query)
}

// pgName is a name of n bytes.
func pgName(n int) string { return strings.Repeat("n", n) }

// openNamed mounts, on engine, a database that declares the collection and the field
// by those names and a short collection "customers".
func openNamed(t *testing.T, engine, collection, field string) (*Database, *namedDB) {
	t.Helper()
	setPreview(t, true, "1")
	fake := &namedDB{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "scripted", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
			collection:  {Fields: map[string]schema.Field{field: {Type: schema.TypeString}}},
		}},
	}
	db, err := Open(m, fake, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db, fake
}

// nameRoute is one route a name can reach the driver by. collectionErr is the refusal
// of a collection that is too long, and fieldErr that of a field, which only a route that
// names a field has.
type nameRoute struct {
	run           func() error
	collectionErr error
	fieldErr      error
}

// nameRoutes are the routes a name can reach the driver by, built from the collection and
// the field a test names.
func nameRoutes(db *Database, collection, field string) map[string]nameRoute {
	ctx := context.Background()
	key := record.NewKeyWithID(collection, "r1")
	dtql := func(doc string) func() error {
		return func() error {
			query, _, err := ParseDTQL([]byte(doc))
			if err != nil {
				return err
			}
			_, err = db.ExecuteDTQLQuery(ctx, query)
			return err
		}
	}
	apply := func(op Op) func() error {
		return func() error { _, err := db.Apply(ctx, []Op{op}, ""); return err }
	}
	write := nameRoute{collectionErr: ErrInvalidKey, fieldErr: ErrInvalidFieldName}
	query := nameRoute{collectionErr: ErrInvalidKey, fieldErr: ErrInvalidDTQL}
	with := func(r nameRoute, run func() error) nameRoute { r.run = run; return r }
	return map[string]nameRoute{
		"get":                    {run: func() error { _, err := db.Get(ctx, key); return err }, collectionErr: ErrInvalidKey},
		"exists":                 {run: func() error { _, err := db.Exists(ctx, key); return err }, collectionErr: ErrInvalidKey},
		"delete":                 {run: apply(Op{Op: "delete", Key: key}), collectionErr: ErrInvalidKey},
		"set a field":            with(write, apply(Op{Op: "set", Key: key, Data: map[string]any{field: "x"}})),
		"insert a field":         with(write, apply(Op{Op: "insert", Key: key, Data: map[string]any{field: "x"}})),
		"update a field":         with(write, apply(Op{Op: "update", Key: key, Updates: []UpdateOp{{FieldName: field, Value: "x"}}})),
		"update a field path":    with(write, apply(Op{Op: "update", Key: key, Updates: []UpdateOp{{FieldPath: []string{field, "inner"}, Value: "x"}}})),
		"wire query, collection": {run: func() error { _, err := db.Execute(ctx, Query{Collection: collection}); return err }, collectionErr: ErrInvalidKey},
		"wire query, where": with(query, func() error {
			_, err := db.Execute(ctx, Query{Collection: collection, Where: []Filter{{Field: field, Op: "==", Value: "x"}}})
			return err
		}),
		"wire query, order by": with(query, func() error {
			_, err := db.Execute(ctx, Query{Collection: collection, OrderBy: []OrderBy{{Field: field}}})
			return err
		}),
		"DTQL, collection": {run: dtql("from: {name: " + collection + "}\n"), collectionErr: ErrInvalidKey},
		"DTQL, where":      with(query, dtql("from: {name: "+collection+"}\nwhere: {op: '==', left: {field: "+field+"}, right: {value: x}}\n")),
		"DTQL, a column":   with(query, dtql("from: {name: "+collection+"}\ncolumns: [{field: "+field+"}]\n")),
		"executor, where": with(query, func() error {
			parsed, err := DeserializeDTQL([]byte("from: {name: " + collection + ", alias: c}\nwhere: {op: '==', left: {field: " + field + ", source: c}, right: {value: x}}\n"))
			if err != nil {
				return err
			}
			_, err = db.Executor().ExecuteQueryToRecordsReader(ctx, parsed)
			return err
		}),
		"executor, a transaction": {run: func() error {
			parsed, _, err := ParseDTQL([]byte("from: {name: " + collection + "}\n"))
			if err != nil {
				return err
			}
			return db.ReadTx(ctx, func(tx dal.QueryExecutor) error {
				_, err := tx.ExecuteQueryToRecordsReader(ctx, parsed)
				return err
			})
		}, collectionErr: ErrInvalidKey},
	}
}

// A collection or a field of 64 bytes or more is refused before the driver is reached,
// by every route that names it, with the refusal of its kind and a message that gives
// the limit and not the name.
func TestAPostgresNameOver63BytesIsRefusedBeforeTheDriver(t *testing.T) {
	long := pgName(maxServerNameBytes + 1)
	for _, c := range []struct{ what, collection, field string }{
		{"a collection", long, "name"},
		{"a field", "customers", long},
	} {
		db, fake := openNamed(t, "postgres", "orders", "total")
		for name, route := range nameRoutes(db, c.collection, c.field) {
			want := route.collectionErr
			if c.what == "a field" {
				if route.fieldErr == nil {
					continue // the route names no field
				}
				want = route.fieldErr
			}
			t.Run(c.what+"/"+name, func(t *testing.T) {
				before := fake.calls.Load()
				err := route.run()
				if err == nil || errors.Is(err, errReached) || fake.calls.Load() != before {
					t.Fatalf("error = %v with %d driver calls: the name reached the driver", err, fake.calls.Load()-before)
				}
				if strings.Contains(err.Error(), long) {
					t.Errorf("the message repeats the name: %v", err)
				}
				if !strings.Contains(err.Error(), "63 bytes") {
					t.Errorf("the message does not give the limit: %v", err)
				}
				if !errors.Is(err, want) {
					t.Errorf("error = %v, want one that matches %v", err, want)
				}
			})
		}
	}
}

// A name of exactly 63 bytes is not refused: the call reaches the driver.
func TestAPostgresNameOf63BytesReachesTheDriver(t *testing.T) {
	exact := pgName(maxServerNameBytes)
	db, fake := openNamed(t, "postgres", exact, exact)
	for name, route := range nameRoutes(db, exact, exact) {
		t.Run(name, func(t *testing.T) {
			before := fake.calls.Load()
			err := route.run()
			if fake.calls.Load() == before {
				t.Fatalf("error = %v: the driver was not reached by a name of 63 bytes", err)
			}
		})
	}
}

// The limit is the engine's: a mount of another engine is not held to it, so a name
// of 64 bytes is the refusal it always was (an undeclared collection), or reaches the
// driver when it is declared.
func TestANameOver63BytesIsNotRefusedOnAnotherEngine(t *testing.T) {
	long := pgName(maxServerNameBytes + 1)
	db, fake := openNamed(t, "sqlite", long, long)
	for name, route := range nameRoutes(db, long, long) {
		t.Run(name, func(t *testing.T) {
			before := fake.calls.Load()
			err := route.run()
			if fake.calls.Load() == before {
				t.Fatalf("error = %v: the driver was not reached", err)
			}
		})
	}
	if err := db.GuardCollection(pgName(70)); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidKey) {
		t.Fatalf("an undeclared collection of 70 bytes on sqlite: %v, want the refusal of an undeclared collection", err)
	}
}

// The check comes before the question whether the collection is declared: a collection
// of 70 bytes that is not declared is the refusal of its length, not a 404, and the same
// name on a name that is declared is too.
func TestAnUndeclaredPostgresCollectionOver63BytesIsRefusedForItsLength(t *testing.T) {
	db, _ := openNamed(t, "postgres", "orders", "total")
	for _, name := range []string{pgName(64), pgName(70), strings.Repeat("é", 40)} {
		for _, guard := range map[string]func(string) error{"GuardCollection": db.GuardCollection, "GuardCanonicalCollection": db.GuardCanonicalCollection} {
			if err := guard(name); !errors.Is(err, ErrInvalidKey) || errors.Is(err, ErrNotFound) {
				t.Errorf("%q: %v, want a refusal of the length and not ErrNotFound", name, err)
			}
		}
	}
	if err := db.GuardCollection("nosuchcollection"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an undeclared short collection: %v, want ErrNotFound", err)
	}
}

// CheckRead gives, without the driver, the refusal the guarded executor gives inside a
// transaction: a field of 64 bytes is refused for its length, a declared name is not, and
// a query that is not a structured one is refused.
func TestCheckReadRefusesWhatTheGuardedExecutorRefusesAndReachesNoDriver(t *testing.T) {
	long := pgName(maxServerNameBytes + 1)
	db, fake := openNamed(t, "postgres", "orders", "total")
	document := func(field string) dal.Query {
		parsed, err := DeserializeDTQL([]byte("from: {name: customers, alias: c}\nwhere: {op: '==', left: {field: " + field + ", source: c}, right: {value: x}}\n"))
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	if err := db.CheckRead(document(long)); !errors.Is(err, ErrInvalidDTQL) || !strings.Contains(err.Error(), "63 bytes") || strings.Contains(err.Error(), long) {
		t.Errorf("a field of 64 bytes: %v, want ErrInvalidDTQL that gives the limit and not the name", err)
	}
	if err := db.CheckRead(document("name")); err != nil {
		t.Errorf("a declared field: %v, want it accepted", err)
	}
	if err := db.CheckRead(dal.NewTextQuery("select 1", nil)); err == nil {
		t.Error("a text query was accepted")
	}
	if fake.calls.Load() != 0 {
		t.Errorf("%d calls reached the driver", fake.calls.Load())
	}
}
