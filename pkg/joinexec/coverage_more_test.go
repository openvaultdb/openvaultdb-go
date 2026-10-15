package joinexec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

type dummyReader struct {
	closed bool
	nextFn func() (record.Record, error)
}

func (d *dummyReader) Next() (record.Record, error) {
	if d.nextFn != nil {
		return d.nextFn()
	}
	return nil, dal.ErrNoMoreRecords
}

func (d *dummyReader) Cursor() (string, error) {
	return "", nil
}

func (d *dummyReader) Close() error {
	d.closed = true
	return nil
}

type queryWithMoney struct {
	dal.StructuredQuery
	money *dal.MoneyConfig
}

func (q queryWithMoney) Money() *dal.MoneyConfig {
	return q.money
}

func TestWithNativePostgresFieldsOption(t *testing.T) {
	t.Parallel()
	opt := WithNativePostgresFields()
	var cfg config
	opt(&cfg)
	if !cfg.allowNativePGFields {
		t.Error("expected allowNativePGFields to be true")
	}
}

func TestExecuteStreamErrorsAndMoneyRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := exPlain("crm", "customers")

	// emit == nil
	if _, err := ExecuteStream(ctx, q, exProfile(t, q), "crm", newExRegistry(), exAllow, Limits{}, nil); err == nil {
		t.Fatal("expected error when emit is nil")
	}

	// query with money routes in memory
	moneyQ := queryWithMoney{
		StructuredQuery: exPlain("crm", "customers"),
		money:           &dal.MoneyConfig{},
	}
	mount := exMount("crm", "sqlite", false, nil)
	reg := newExRegistry(mount)
	// ExecuteStream with money exercises Money() check and withDefaultDatabase
	_, _ = ExecuteStream(ctx, moneyQ, exProfile(t, moneyQ), "crm", reg, exAllow, Limits{}, func(_ record.Record) error { return nil })
}

func TestConsumeEdgeCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rn := &run{guard: NewGuard(exAllow, Limits{}), money: true}

	// openErr != nil and reader != nil -> closes reader
	rdr := &dummyReader{}
	_, err := rn.consume(ctx, rdr, errors.New("open failed"), RouteDatabase, func(_ record.Record) error { return nil })
	if err == nil || !rdr.closed {
		t.Errorf("expected reader to be closed on openErr, closed=%v, err=%v", rdr.closed, err)
	}

	// metadata limits exceeded
	bigData := map[string]any{}
	for i := 0; i < maxColumnMetadataKeys+5; i++ {
		bigData[string(rune('a'+(i%26)))+string(rune('A'+(i/26)))] = i
	}
	rec := record.NewRecordWithData(record.NewKeyWithID("c", 1), bigData)
	rdrData := &dummyReader{
		nextFn: func() (record.Record, error) {
			return rec, nil
		},
	}
	_, err = rn.consume(ctx, rdrData, nil, RouteInMemory, func(_ record.Record) error { return nil })
	var bErr *BudgetError
	if !errors.As(err, &bErr) {
		t.Fatalf("expected BudgetError, got: %v", err)
	}
}

func TestWithDefaultDatabaseAndBind(t *testing.T) {
	t.Parallel()
	// from is nil
	if bindFromDatabase(nil, "db") != nil {
		t.Error("expected nil for nil from")
	}

	a := exRef("", "A", "a")
	b := exRef("", "B", "b")
	c := exRef("", "C", "c")

	// base is CollectionRef with no database, with scan
	refWithScan := dal.NewQualifiedRootCollectionRef("public", "orders", "o").WithScan(10, dal.Ascending(dal.NewFieldRef("o", "id")))
	from := dal.From(refWithScan)
	bound := bindFromDatabase(from, "db")
	if bound == nil || bound.Base().(dal.CollectionRef).Database() != "db" {
		t.Errorf("expected database db, got: %#v", bound)
	}

	// joins with child (nested join via NewJoinedFrom)
	joinedNested := dal.NewJoinedFrom(dal.From(c), dal.JoinLeft, exKeyEquals(a, c))
	fromWithNested := dal.From(a).Join(joinedNested)
	boundNested := bindFromDatabase(fromWithNested, "db")
	if boundNested == nil {
		t.Fatal("expected non-nil bound nested")
	}

	// joins without child, collection without database, with scan and algorithms
	joinRefWithScan := dal.NewRootCollectionRef("items", "i").WithScan(5, dal.Ascending(dal.NewFieldRef("i", "id")))
	joinedDirect := dal.NewJoinedSource(joinRefWithScan, dal.JoinInner, exKeyEquals(a, b)).WithAlgorithms(dal.JoinAlgorithmHash)
	fromDirect := dal.From(dal.NewDatabaseCollectionRef("db", "", "orders", "a")).Join(joinedDirect)
	boundDirect := bindFromDatabase(fromDirect, "db")
	if boundDirect == nil {
		t.Fatal("expected non-nil bound direct")
	}

	// withDefaultDatabase wrapper
	q := exPlain("", "users")
	qBound := withDefaultDatabase(q, "mydb")
	if qBound.From().Base().(dal.CollectionRef).Database() != "mydb" {
		t.Errorf("expected mydb, got %q", qBound.From().Base().(dal.CollectionRef).Database())
	}
}

func TestMoneyEvaluationErrors(t *testing.T) {
	t.Parallel()
	if isMoneyEvaluationError(nil) {
		t.Error("expected false for nil error")
	}
	if isMoneyEvaluationError(errors.New("other error")) {
		t.Error("expected false for generic error")
	}
	moneyErr := errors.New("money division by zero")
	if !isMoneyEvaluationError(moneyErr) {
		t.Error("expected true for money error")
	}

	// classify with money error
	rn := &run{guard: NewGuard(exAllow, Limits{}), money: true}
	cErr := rn.classify(moneyErr, RouteInMemory)
	if !errors.Is(cErr, ErrInvalidDocument) {
		t.Errorf("expected ErrInvalidDocument, got: %v", cErr)
	}

	// moneyErrorReader
	rdr := &dummyReader{
		nextFn: func() (record.Record, error) {
			return nil, moneyErr
		},
	}
	mr := &moneyErrorReader{RecordsReader: rdr, guard: NewGuard(exAllow, Limits{})}
	if _, err := mr.Next(); !errors.Is(err, ErrInvalidDocument) {
		t.Errorf("expected ErrInvalidDocument from moneyErrorReader, got: %v", err)
	}
}

type fakeNativePostgresSource struct {
	fakeSource
}

func (f *fakeNativePostgresSource) NativePostgresReadOnly() bool          { return true }
func (f *fakeNativePostgresSource) CheckRead(_ string, _ ...string) error { return nil }

func TestLeafInvalidSchemaQualifiedRelation(t *testing.T) {
	t.Parallel()
	src := &fakeNativePostgresSource{fakeSource: fakeSource{id: "pg", engine: "postgres"}}
	g := NewGuard(exAllow, Limits{})

	// Invalid schema/name containing > 63 chars triggers schema.NativePostgresCollectionID error
	badRef := dal.NewDatabaseCollectionRef("pg", strings.Repeat("x", 64), "table", "")
	badQ := dal.From(badRef).NewQuery().SelectColumns(dal.Column{Expression: dal.Star()})
	if _, err := g.Leaf(src).ExecuteQueryToRecordsReader(context.Background(), badQ); !errors.Is(err, ErrUnsupportedSource) {
		t.Errorf("expected ErrUnsupportedSource for invalid schema, got: %v", err)
	}
}

func TestWalkInvalidSchemaQualifiedCollectionAndValidField(t *testing.T) {
	t.Parallel()
	// Schema-qualified collection with bad schema triggers refuse in walker
	badRef := dal.NewDatabaseCollectionRef("pg", strings.Repeat("x", 64), "table", "")
	badQ := dal.From(badRef).NewQuery().SelectColumns(dal.Column{Expression: dal.Star()})
	if _, err := inspectWithNativePostgresFields(badQ, false); err == nil {
		t.Error("expected inspection error for bad schema-qualified collection")
	}

	// validNativePostgresFieldName with empty segment
	if validNativePostgresFieldName("a..b") {
		t.Error("expected false for empty segment in field name")
	}
	if validNativePostgresFieldName(".a") {
		t.Error("expected false for leading dot")
	}
	if validNativePostgresFieldName("a.") {
		t.Error("expected false for trailing dot")
	}
}
