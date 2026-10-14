package core

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dtql"
	"github.com/dal-go/dalgo/recordset"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// nativeLongNamesFixture is a native mount whose relations have the longest
// names PostgreSQL keeps, so that their collection IDs are far over 63 bytes.
func nativeLongNamesFixture(t *testing.T) (*Database, map[string]string) {
	t.Helper()
	long := strings.Repeat("s", 63)
	sources := map[string]schema.NativeCollectionSource{
		"short":        {Schema: "public", Name: "t"},
		"over 36":      {Schema: "HumanResources schema", Name: "EmployeeDepartmentHistory"},
		"63 and 63":    {Schema: long, Name: strings.Repeat("r", 63)},
		"spaces quote": {Schema: "sales data", Name: `Alphabetical list of "products"`},
	}
	ids := map[string]string{}
	collections := map[string]schema.Collection{}
	for label, source := range sources {
		id, err := schema.NativePostgresCollectionID(source.Schema, source.Name)
		if err != nil {
			t.Fatal(err)
		}
		ids[label] = id
		collections[id] = schema.Collection{Source: &schema.NativeCollectionSource{Schema: source.Schema, Name: source.Name}, Fields: map[string]schema.Field{
			"id":    {Type: schema.TypeInteger, NativeType: "bigint"},
			"small": {Type: schema.TypeInteger, NativeType: "smallint"},
			"count": {Type: schema.TypeInteger, NativeType: "integer"},
			"blob":  {Type: schema.TypeString, NativeType: "bytea"},
			"name":  {Type: schema.TypeString, NativeType: "text"},
		}}
	}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "samples", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true}},
		Schemas:  &schema.Schemas{Collections: collections},
	}
	db, err := Open(m, &namesRecordingDB{}, []schema.Mode{schema.ModeStrict}, t.TempDir()+"/inferred.json")
	if err != nil {
		t.Fatal(err)
	}
	return db, ids
}

func TestNativePostgresLongCollectionIDsAreReadable(t *testing.T) {
	db, ids := nativeLongNamesFixture(t)
	for label, id := range ids {
		t.Run(label, func(t *testing.T) {
			if label != "short" && len(id) <= 63 {
				t.Fatalf("collection ID %q is not over 63 bytes", id)
			}
			if err := db.GuardCollection(id); err != nil {
				t.Fatalf("GuardCollection: %v", err)
			}
			if err := db.GuardCanonicalCollection(id); err != nil {
				t.Fatalf("GuardCanonicalCollection: %v", err)
			}
		})
	}
	// An ID the mount did not discover is not found, however long, and not a bad key.
	unknown, _ := schema.NativePostgresCollectionID(strings.Repeat("x", 63), strings.Repeat("y", 63))
	for _, guard := range []func(string) error{db.GuardCollection, db.GuardCanonicalCollection} {
		if err := guard(unknown); !errors.Is(err, ErrNotFound) {
			t.Fatalf("undiscovered long ID: %v, want not found", err)
		}
	}
}

func TestWritablePostgresKeepsTheCollectionNameLimit(t *testing.T) {
	d := &Database{Manifest: &manifest.Manifest{Storage: manifest.Storage{Engine: "postgres"}}}
	if !d.collectionNameTooLong(strings.Repeat("n", 64)) || d.collectionNameTooLong(strings.Repeat("n", 63)) {
		t.Fatal("a writable PostgreSQL mount must refuse a collection name over 63 bytes")
	}
	if err := d.GuardCollection(strings.Repeat("n", 64)); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("GuardCollection: %v, want invalid key", err)
	}
}

func TestNativePostgresValuesAColumnCannotHoldAreRefused(t *testing.T) {
	db, _ := nativeLongNamesFixture(t)
	from := "from: {schema: 'HumanResources schema', name: EmployeeDepartmentHistory}\n"
	cases := map[string]struct {
		where string
		ok    bool
	}{
		"whole float":               {"{op: '==', left: {field: id}, right: {value: 1000.0}}", true},
		"integer":                   {"{op: '==', left: {field: id}, right: {value: 42}}", true},
		"text left to postgres":     {"{op: '==', left: {field: id}, right: {value: 'abc'}}", true},
		"field on the right":        {"{op: '==', left: {value: 7}, right: {field: id}}", true},
		"two fields":                {"{op: '==', left: {field: id}, right: {field: count}}", true},
		"parameter":                 {"{op: '==', left: {field: id}, right: {param: p}}", true},
		"text column":               {"{op: '==', left: {field: name}, right: {value: 1.5}}", true},
		"in of integers":            {"{op: In, left: {field: id}, right: {values: [1, 2, 3]}}", true},
		"fraction":                  {"{op: '==', left: {field: id}, right: {value: 1.5}}", false},
		"fraction on the left":      {"{op: '==', left: {value: 1.5}, right: {field: id}}", false},
		"order by a fraction":       {"{op: '<', left: {value: 1.5}, right: {field: id}}", true},
		"order beyond the range":    {"{op: '<', left: {field: small}, right: {value: 40000}}", true},
		"order with a boolean":      {"{op: '>=', left: {field: id}, right: {value: false}}", false},
		"not in with a fraction":    {"{op: NotIn, left: {field: id}, right: {values: [1, 2.5]}}", false},
		"beyond int64":              {"{op: '==', left: {field: id}, right: {value: 1180591620717411303424}}", false},
		"boolean":                   {"{op: '==', left: {field: id}, right: {value: true}}", false},
		"smallint overflow":         {"{op: '==', left: {field: small}, right: {value: 40000}}", false},
		"integer overflow":          {"{op: '==', left: {field: count}, right: {value: 3000000000}}", false},
		"in with a fraction":        {"{op: In, left: {field: id}, right: {values: [1, 2.5]}}", false},
		"bytea":                     {"{op: '==', left: {field: blob}, right: {value: 'AP8Q'}}", false},
		"bytea in an and":           {"{and: [{op: '==', left: {field: name}, right: {value: x}}, {op: '>', left: {field: blob}, right: {value: 'AA=='}}]}", false},
		"qualified fraction":        {"{op: '==', left: {field: id, source: e}, right: {value: 1.5}}", false},
		"unknown qualifier skipped": {"{op: '==', left: {field: id, source: nobody}, right: {value: 1.5}}", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			text := from + "where: " + tc.where + "\n"
			if strings.Contains(tc.where, "source: e") || strings.Contains(tc.where, "source: nobody") {
				text = "from: {schema: 'HumanResources schema', name: EmployeeDepartmentHistory, alias: e}\nwhere: " + tc.where + "\n"
			}
			query, err := dtql.Deserialize([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			err = db.guardSources(query)
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidDTQL) {
				t.Fatalf("error = %v, want invalid DTQL", err)
			}
		})
	}
}

func TestNativeIntegerValueBounds(t *testing.T) {
	bigint := nativeIntegerRanges["bigint"]
	for _, tc := range []struct {
		value any
		want  bool
	}{
		{nil, true},
		{int64(math.MaxInt64), true},
		{int64(math.MinInt64), true},
		{uint64(math.MaxInt64), true},
		{uint64(math.MaxInt64) + 1, false},
		{float64(-9223372036854775808), true},
		{float64(9223372036854775807), false}, // 2^63 as a float64
		{float32(2.5), false},
		{float32(2), true},
		{math.NaN(), false},
		{math.Inf(1), false},
		{struct{}{}, true}, // left to the driver
	} {
		if got := nativeIntegerValue(tc.value, bigint.low, bigint.high); got != tc.want {
			t.Errorf("nativeIntegerValue(%#v) = %v, want %v", tc.value, got, tc.want)
		}
	}
	small := nativeIntegerRanges["smallint"]
	if nativeIntegerValue(uint8(200), small.low, small.high) != true || nativeIntegerValue(uint32(40000), small.low, small.high) != false {
		t.Error("unsigned values are held to the range of the column")
	}
	if _, _, ok := fieldAndValue(dal.Field("x"), dal.Array{Value: 3}); ok {
		t.Error("an array that is not a slice is not a list of values")
	}
}

func TestNativeValueIsNotCheckedOffNativeMounts(t *testing.T) {
	w := fieldWalk{fieldGuard: fieldGuard{db: &Database{}}}
	if err := w.nativeValue(dal.Comparison{Operator: dal.Equal, Left: dal.Field("id"), Right: dal.Constant{Value: 1.5}}); err != nil {
		t.Fatal(err)
	}
	native := fieldWalk{fieldGuard: fieldGuard{db: &Database{nativePostgres: true, Manifest: &manifest.Manifest{}}}}
	if got := native.nativeType(dal.Field("id")); got != "" {
		t.Fatalf("a field of no source has type %q", got)
	}
	if got := native.nativeType(dal.NewFieldRef("nobody", "id")); got != "" {
		t.Fatalf("a field of an unknown qualifier has type %q", got)
	}
	native.single = &fieldSource{collection: "missing"}
	if got := native.nativeType(dal.Field("id")); got != "" {
		t.Fatalf("a field of an undeclared source has type %q", got)
	}
}

// Read-only transaction fakes.

type readTxDB struct {
	dal.DB
	beginErr   error
	commitErr  error
	openErr    error
	panicOpen  bool
	began      int
	ended      int
	closed     int
	readerErr  error
	recordsets int
}

func (f *readTxDB) RunReadonlyTransaction(ctx context.Context, worker dal.ROTxWorker, _ ...dal.TransactionOption) error {
	if f.beginErr != nil {
		return f.beginErr
	}
	f.began++
	err := worker(ctx, readTxTx{db: f})
	f.ended++
	if err != nil {
		return err
	}
	return f.commitErr
}

type readTxTx struct {
	dal.ReadTransaction
	db *readTxDB
}

func (tx readTxTx) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	if tx.db.panicOpen {
		panic("driver panic")
	}
	if tx.db.openErr != nil {
		return nil, tx.db.openErr
	}
	return readTxReader{db: tx.db}, nil
}

func (tx readTxTx) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	if tx.db.openErr != nil {
		return nil, tx.db.openErr
	}
	tx.db.recordsets++
	return readTxRecordset{db: tx.db}, nil
}

type readTxReader struct {
	dal.RecordsReader
	db *readTxDB
}

func (r readTxReader) Close() error {
	if r.db.ended != 0 && r.db.closed == 0 {
		panic("the transaction ended before its reader was closed")
	}
	r.db.closed++
	return r.db.readerErr
}

type readTxRecordset struct {
	dal.RecordsetReader
	db *readTxDB
}

func (r readTxRecordset) Close() error {
	r.db.closed++
	return r.db.readerErr
}

func TestNativeReadTxKeepsTheTransactionOpenUntilTheReaderCloses(t *testing.T) {
	fake := &readTxDB{}
	db := nativeReadTxDB{DB: fake}
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if fake.began != 1 || fake.ended != 0 {
		t.Fatalf("began %d ended %d before Close", fake.began, fake.ended)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if fake.closed != 1 || fake.ended != 1 {
		t.Fatalf("closed %d ended %d after Close", fake.closed, fake.ended)
	}
	if err := reader.Close(); err != nil || fake.ended != 1 {
		t.Fatalf("a second Close: %v, ended %d", err, fake.ended)
	}

	recordsets, err := db.ExecuteQueryToRecordsetReader(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordsets.Close(); err != nil || fake.recordsets != 1 || fake.ended != 2 {
		t.Fatalf("recordset reader: %v, recordsets %d ended %d", err, fake.recordsets, fake.ended)
	}
}

func TestNativeReadTxErrors(t *testing.T) {
	boom := errors.New("boom")
	for name, fake := range map[string]*readTxDB{
		"begin": {beginErr: boom},
		"open":  {openErr: boom},
	} {
		t.Run(name, func(t *testing.T) {
			db := nativeReadTxDB{DB: fake}
			if _, err := db.ExecuteQueryToRecordsReader(context.Background(), nil); !errors.Is(err, boom) {
				t.Fatalf("records: %v", err)
			}
			if _, err := db.ExecuteQueryToRecordsetReader(context.Background(), nil); !errors.Is(err, boom) {
				t.Fatalf("recordset: %v", err)
			}
		})
	}
	t.Run("panic", func(t *testing.T) {
		db := nativeReadTxDB{DB: &readTxDB{panicOpen: true}}
		if _, err := db.ExecuteQueryToRecordsReader(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "driver panic") {
			t.Fatalf("panic: %v", err)
		}
	})
	t.Run("commit", func(t *testing.T) {
		for _, open := range []func(nativeReadTxDB) (dal.Reader, error){
			func(db nativeReadTxDB) (dal.Reader, error) {
				return db.ExecuteQueryToRecordsReader(context.Background(), nil)
			},
			func(db nativeReadTxDB) (dal.Reader, error) {
				return db.ExecuteQueryToRecordsetReader(context.Background(), nil)
			},
		} {
			reader, err := open(nativeReadTxDB{DB: &readTxDB{commitErr: boom}})
			if err != nil {
				t.Fatal(err)
			}
			if err := reader.Close(); !errors.Is(err, boom) {
				t.Fatalf("commit failure: %v", err)
			}
			reader, err = open(nativeReadTxDB{DB: &readTxDB{commitErr: errors.New("commit"), readerErr: boom}})
			if err != nil {
				t.Fatal(err)
			}
			if err := reader.Close(); !errors.Is(err, boom) {
				t.Fatalf("the reader's own failure comes first: %v", err)
			}
		}
	})
	t.Run("context", func(t *testing.T) {
		fake := &readTxDB{}
		ctx, cancel := context.WithCancel(context.Background())
		reader, err := nativeReadTxDB{DB: fake}.ExecuteQueryToRecordsReader(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		// The transaction ends with the request even when the reader is never closed.
		hold := reader.(readTxRecordsReader).hold
		if err := <-hold.done; err != nil {
			t.Fatal(err)
		}
		hold.done <- nil // finish reads it once more
		_ = fake
	})
}

func TestNativeJoinFieldsComeFromTheDriverUnderTheReadTransaction(t *testing.T) {
	db, _ := nativeLongNamesFixture(t)
	if _, ok := db.db.(nativeReadTxDB); !ok {
		t.Fatalf("the driver of a native mount is %T, want the read-transaction driver", db.db)
	}
	provider := db.Executor().(dal.JoinFieldsProvider)
	fields, err := provider.JoinFields(context.Background(), dal.NewQualifiedRootCollectionRef("HumanResources schema", "EmployeeDepartmentHistory", ""))
	if err != nil || len(fields) != 5 {
		t.Fatalf("fields = %v, %v; want the five declared fields", fields, err)
	}
}
