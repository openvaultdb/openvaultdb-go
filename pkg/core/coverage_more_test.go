package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/record"
	"github.com/dal-go/record/update"
	"github.com/openvaultdb/openvaultdb-go/pkg/inferred"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/policystore"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func TestModeCompatibilityError(t *testing.T) {
	t.Parallel()
	err := &ModeCompatibilityError{
		Engine:    "sqlite",
		Requested: schema.ModeSchemaless,
		Supported: []schema.Mode{schema.ModeStrict},
	}
	msg := err.Error()
	if !strings.Contains(msg, "sqlite") || !strings.Contains(msg, "schemaless") {
		t.Fatalf("unexpected ModeCompatibilityError message: %s", msg)
	}
}

func TestCoreHTTPAndReadOnlyMethods(t *testing.T) {
	t.Parallel()
	d := &Database{
		readOnlyHTTP: true,
		httpProfile:  "ecb-daily/1",
	}
	if !d.ReadOnlyHTTP() {
		t.Fatal("expected ReadOnlyHTTP true")
	}
	if d.HTTPProfile() != "ecb-daily/1" {
		t.Fatalf("unexpected profile: %s", d.HTTPProfile())
	}
	if !d.ReadOnly() {
		t.Fatal("expected ReadOnly true for HTTP")
	}
	err := d.readOnlyOperationError()
	if !errors.Is(err, ErrHTTPOperationUnsupported) {
		t.Fatalf("expected ErrHTTPOperationUnsupported, got %v", err)
	}

	dPostgres := &Database{nativePostgres: true}
	if !dPostgres.ReadOnly() {
		t.Fatal("expected ReadOnly true for nativePostgres")
	}
	errPG := dPostgres.readOnlyOperationError()
	if !errors.Is(errPG, ErrNativePostgresOperationUnsupported) {
		t.Fatalf("expected ErrNativePostgresOperationUnsupported, got %v", errPG)
	}
}

func TestRetentionMethods(t *testing.T) {
	t.Parallel()
	// None retention
	dNone := &Database{retention: manifest.RetentionNone}
	if dNone.Retention() != manifest.RetentionNone || !dNone.NoRetention() {
		t.Fatal("expected RetentionNone")
	}
	// db nil fails closed to RetentionNone
	dNilDB := &Database{retention: "other", db: nil}
	if dNilDB.Retention() != manifest.RetentionNone {
		t.Fatal("expected RetentionNone for nil db")
	}
	// db not nil and not RetentionNone
	dNormal := &Database{retention: "other", db: admitOnlyDB{}}
	if dNormal.Retention() != "" || dNormal.NoRetention() {
		t.Fatal("expected empty retention for normal db")
	}

	// ValidateRetentionCache
	// Case 1: Manifest is nil
	dNoManifest := &Database{retention: manifest.RetentionNone}
	if err := dNoManifest.ValidateRetentionCache(); err != nil {
		t.Fatalf("unexpected error for nil manifest: %v", err)
	}
	// Case 2: CacheTTL is "0s"
	mZeroTTL := &manifest.Manifest{Database: manifest.Database{CacheTTL: "0s"}}
	dZeroTTL := &Database{retention: manifest.RetentionNone, Manifest: mZeroTTL}
	if err := dZeroTTL.ValidateRetentionCache(); err != nil {
		t.Fatalf("unexpected error for 0s cache TTL: %v", err)
	}
	// Case 3: CacheTTL is invalid
	mInvalidTTL := &manifest.Manifest{Database: manifest.Database{CacheTTL: "invalid"}}
	dInvalidTTL := &Database{retention: manifest.RetentionNone, Manifest: mInvalidTTL}
	if err := dInvalidTTL.ValidateRetentionCache(); err == nil {
		t.Fatal("expected error for invalid cache TTL")
	}
	// Case 4: CacheTTL is positive
	mPosTTL := &manifest.Manifest{Database: manifest.Database{CacheTTL: "5m"}}
	dPosTTL := &Database{retention: manifest.RetentionNone, Manifest: mPosTTL}
	if err := dPosTTL.ValidateRetentionCache(); err == nil {
		t.Fatal("expected error for positive cache TTL with retention none")
	}
}

func TestInferFunctions(t *testing.T) {
	t.Parallel()
	if deepCopy(nil) != nil {
		t.Fatal("expected nil for deepCopy(nil)")
	}
	src := map[string]any{"a": "hello", "b": 123}
	cp := deepCopy(src)
	if cp["a"] != "hello" || cp["b"] != float64(123) {
		t.Fatalf("unexpected deepCopy: %v", cp)
	}
	// Test marshal error fallback in deepCopy
	unmarshalable := map[string]any{"bad": func() {}}
	cpFallback := deepCopy(unmarshalable)
	if _, ok := cpFallback["bad"]; !ok {
		t.Fatal("expected fallback deepCopy to copy keys directly")
	}

	// jsonFieldType tests
	tests := []struct {
		val  any
		want schema.FieldType
	}{
		{"text", schema.TypeString},
		{true, schema.TypeBoolean},
		{123, schema.TypeNumber},
		{int64(456), schema.TypeNumber},
		{3.14, schema.TypeNumber},
		{float32(2.5), schema.TypeNumber},
		{json.Number("42"), schema.TypeNumber},
		{map[string]any{"k": "v"}, schema.TypeObject},
		{[]any{1, 2}, schema.TypeArray},
		{struct{}{}, schema.TypeAny},
	}
	for _, tc := range tests {
		if got := jsonFieldType(tc.val); got != tc.want {
			t.Errorf("jsonFieldType(%T) = %v, want %v", tc.val, got, tc.want)
		}
	}

	// inferFields
	inferred := inferFields(map[string]any{
		"name": "Alex",
		"flag": true,
	})
	if inferred["name"].Type != schema.TypeString || inferred["flag"].Type != schema.TypeBoolean {
		t.Fatalf("unexpected inferFields result: %v", inferred)
	}
}

func TestNativePostgresMappingsAndHelpers(t *testing.T) {
	t.Parallel()
	// Nil or empty schemas
	if _, _, err := nativePostgresMappings(nil); err == nil {
		t.Fatal("expected error for nil schemas")
	}
	if _, _, err := nativePostgresMappings(&schema.Schemas{}); err == nil {
		t.Fatal("expected error for empty schemas")
	}
	// Missing source
	missingSrc := &schema.Schemas{
		Collections: map[string]schema.Collection{
			"orders": {},
		},
	}
	if _, _, err := nativePostgresMappings(missingSrc); err == nil {
		t.Fatal("expected error for missing source")
	}
	// Canonical ID mismatch
	idMismatch := &schema.Schemas{
		Collections: map[string]schema.Collection{
			"orders": {Source: &schema.NativeCollectionSource{Schema: "public", Name: "orders"}},
		},
	}
	if _, _, err := nativePostgresMappings(idMismatch); err == nil {
		t.Fatal("expected error for ID mismatch")
	}
	// Duplicate physical source mapping
	cid, err := schema.NativePostgresCollectionID("public", "orders")
	if err != nil {
		t.Fatal(err)
	}
	// Invalid column name in contract (>63 bytes or NUL)
	invalidCol := &schema.Schemas{
		Collections: map[string]schema.Collection{
			cid: {
				Source: &schema.NativeCollectionSource{Schema: "public", Name: "orders"},
				Fields: map[string]schema.Field{strings.Repeat("x", 64): {Type: schema.TypeString}},
			},
		},
	}
	if _, _, err := nativePostgresMappings(invalidCol); err == nil {
		t.Fatal("expected error for column name > 63 bytes")
	}
	// Valid mappings
	validSchemas := &schema.Schemas{
		Collections: map[string]schema.Collection{
			cid: {
				Source: &schema.NativeCollectionSource{Schema: "public", Name: "orders"},
				Fields: map[string]schema.Field{"id": {Type: schema.TypeString}},
			},
		},
	}
	byID, bySource, err := nativePostgresMappings(validSchemas)
	if err != nil {
		t.Fatalf("nativePostgresMappings valid: %v", err)
	}
	if len(byID) != 1 || len(bySource) != 1 {
		t.Fatalf("unexpected counts: %v, %v", byID, bySource)
	}

	// Methods on *Database
	dNotPG := &Database{nativePostgres: false}
	if _, ok := dNotPG.ResolveNativePostgresCollection("public", "orders"); ok {
		t.Fatal("expected false for ResolveNativePostgresCollection when not nativePostgres")
	}
	if _, ok := dNotPG.NativePostgresSource(cid); ok {
		t.Fatal("expected false for NativePostgresSource when not nativePostgres")
	}
	if dNotPG.NativePostgresReadOnly() {
		t.Fatal("expected false for NativePostgresReadOnly when not nativePostgres")
	}

	dPG := &Database{
		nativePostgres: true,
		nativeSources:  byID,
		nativeIDs:      bySource,
	}
	if gotID, ok := dPG.ResolveNativePostgresCollection("public", "orders"); !ok || gotID != cid {
		t.Fatalf("ResolveNativePostgresCollection: got %q, %v", gotID, ok)
	}
	if gotSrc, ok := dPG.NativePostgresSource(cid); !ok || gotSrc.Schema != "public" || gotSrc.Name != "orders" {
		t.Fatalf("NativePostgresSource: got %+v, %v", gotSrc, ok)
	}
	if !dPG.NativePostgresReadOnly() {
		t.Fatal("expected true for NativePostgresReadOnly")
	}

	// validateNativeIdentifier tests
	if err := validateNativeIdentifier(""); err == nil {
		t.Fatal("expected error for empty identifier")
	}
	if err := validateNativeIdentifier(strings.Repeat("a", 64)); err == nil {
		t.Fatal("expected error for >63 bytes identifier")
	}
	if err := validateNativeIdentifier("bad\x00ident"); err == nil {
		t.Fatal("expected error for NUL byte in identifier")
	}
	if err := validateNativeIdentifier("valid_name"); err != nil {
		t.Fatalf("unexpected error for valid identifier: %v", err)
	}
}

func TestDBSchemaType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   schema.FieldType
		want dbschema.Type
	}{
		{schema.TypeString, dbschema.String},
		{schema.TypeNumber, dbschema.Float},
		{schema.TypeDecimal, dbschema.String},
		{schema.TypeInteger, dbschema.Int},
		{schema.TypeBoolean, dbschema.Bool},
		{schema.TypeObject, dbschema.String},
		{schema.TypeArray, dbschema.String},
		{schema.TypeAny, dbschema.String},
	}
	for _, tc := range cases {
		if got := dbschemaType(tc.in); got != tc.want {
			t.Errorf("dbschemaType(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestLicensesCore(t *testing.T) {
	t.Parallel()
	// HasLicenseDeclarations on empty
	dEmpty := &Database{}
	if dEmpty.HasLicenseDeclarations() {
		t.Fatal("expected false for empty database")
	}

	// snapshotLicenses with database license
	m := &manifest.Manifest{
		Database: manifest.Database{
			ID:      "testdb",
			License: &license.Declaration{SPDX: "MIT"},
		},
		RecordsetLicenses: map[string]license.Declaration{
			"orders": {SPDX: "Apache-2.0"},
		},
		Storage: manifest.Storage{Engine: "sqlite"},
	}
	// Case 1: Recordset license references an undeclared collection in strict mode
	m.Database.SchemaMode = schema.ModeStrict
	dStrict := &Database{Manifest: m}
	if err := dStrict.snapshotLicenses(); err == nil || !strings.Contains(err.Error(), "undeclared collection") {
		t.Fatalf("expected undeclared collection error, got %v", err)
	}

	// Case 2: Recordset license collection name fails validation
	mInvalid := &manifest.Manifest{
		Database: manifest.Database{ID: "testdb"},
		RecordsetLicenses: map[string]license.Declaration{
			"": {SPDX: "MIT"},
		},
	}
	dInvalid := &Database{Manifest: mInvalid}
	if err := dInvalid.snapshotLicenses(); err == nil {
		t.Fatal("expected validation error for empty collection name in RecordsetLicenses")
	}

	// Case 3: Recordset license collection name is not canonical
	mNonCanonical := &manifest.Manifest{
		Database: manifest.Database{ID: "testdb"},
		RecordsetLicenses: map[string]license.Declaration{
			"`orders`": {SPDX: "MIT"},
		},
		Storage: manifest.Storage{Engine: "sqlite"},
	}
	dNonCanonical := &Database{
		Manifest: mNonCanonical,
		names: collectionNames{
			canonical: map[string]string{"`orders`": "orders"},
		},
	}
	if err := dNonCanonical.snapshotLicenses(); err == nil || !strings.Contains(err.Error(), "canonical collection names") {
		t.Fatalf("expected non-canonical error, got %v", err)
	}

	// Case 4: Valid snapshot licenses
	mValid := &manifest.Manifest{
		Database: manifest.Database{
			ID:         "testdb",
			SchemaMode: schema.ModeSchemaless,
			License:    &license.Declaration{SPDX: "MIT"},
		},
		RecordsetLicenses: map[string]license.Declaration{
			"orders": {SPDX: "Apache-2.0"},
		},
		Storage: manifest.Storage{Engine: "ingitdb"},
	}
	dValid := &Database{
		Manifest:       mValid,
		documentEngine: true,
		names: collectionNames{
			canonical: map[string]string{"orders": "orders"},
		},
	}
	if err := dValid.snapshotLicenses(); err != nil {
		t.Fatalf("snapshotLicenses failed: %v", err)
	}
	if !dValid.HasLicenseDeclarations() {
		t.Fatal("expected HasLicenseDeclarations true")
	}

	// SourceRight
	sr, err := dValid.SourceRight("srv1", &license.Declaration{SPDX: "BSD-3-Clause"}, "orders")
	if err != nil {
		t.Fatalf("SourceRight: %v", err)
	}
	if sr == nil {
		t.Fatal("expected non-nil SourceRight")
	}
}

func TestAccessLayersCore(t *testing.T) {
	t.Parallel()
	d := &Database{}
	if d.Coordinator() != nil {
		t.Fatal("expected nil coordinator")
	}

	// fixedOwnerLease
	lease := fixedOwnerLease{policies: []access.Policy{}}
	if len(lease.Policies()) != 0 {
		t.Fatal("unexpected policies length")
	}
	if lease.Revision() != "fixed" {
		t.Fatalf("unexpected revision: %s", lease.Revision())
	}
	lease.Release()

	// fixedPolicyLease error
	dErr := &Database{
		ownerPolicies: func(context.Context) ([]access.Policy, error) {
			return nil, errors.New("policy err")
		},
	}
	if _, err := dErr.fixedPolicyLease(context.Background()); err == nil {
		t.Fatal("expected error from fixedPolicyLease")
	}

	// PolicyLayers: test upper enabled with error and lower enabled with error
	dLayers := &Database{
		Manifest: &manifest.Manifest{Storage: manifest.Storage{Engine: "ingitdb"}},
		ownerPolicies: func(context.Context) ([]access.Policy, error) {
			return nil, errors.New("upper err")
		},
		lowerPolicies: func(context.Context) ([]access.Policy, error) {
			return nil, errors.New("lower err")
		},
	}
	layers := dLayers.PolicyLayers(context.Background())
	if len(layers) != 2 || layers[0].Err == nil || layers[1].Err == nil {
		t.Fatalf("unexpected layers: %+v", layers)
	}

	// validateProtectedCandidate
	ctxCancelled, cancel := context.WithCancel(context.Background())
	cancel()
	dCand := &Database{Manifest: &manifest.Manifest{Database: manifest.Database{SchemaMode: schema.ModeSchemaless}}}
	op, err := access.NewProtectedInsert("op1", record.NewKeyWithID("users", "u1"), map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := dCand.validateProtectedCandidate(ctxCancelled, op, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if err := dCand.validateProtectedCandidate(context.Background(), op, map[string]any{"x": 1}); err != nil {
		t.Fatalf("unexpected error for schemaless: %v", err)
	}
}

func TestAccessSampleCore(t *testing.T) {
	t.Parallel()
	rp := requesterPolicy{
		Policy:    joinSrcAllowPolicy{},
		principal: access.Principal{},
	}
	if err := rp.Authorize(context.Background(), access.Request{}); err != nil {
		t.Fatalf("requesterPolicy.Authorize: %v", err)
	}
	if dec := rp.Decide(context.Background(), access.Request{}); !dec.Allowed {
		t.Fatalf("requesterPolicy.Decide not allowed: %v", dec)
	}

	q, _, err := ParseTugQL([]byte("from: {name: users}\n"))
	if err != nil {
		t.Fatal(err)
	}

	sq := sampleQuery{StructuredQuery: q, count: 5, order: []dal.OrderExpression{dal.AscendingField("id")}}
	if sq.Limit() != 5 || sq.Offset() != 0 || len(sq.OrderBy()) != 1 {
		t.Fatalf("unexpected sampleQuery: %v", sq)
	}
	_ = sq.String()

	// SelectAccessSample validation paths
	d := &Database{
		Manifest: &manifest.Manifest{Storage: manifest.Storage{Engine: "sqlite"}},
	}
	// Invalid bounds: n < 1
	if _, _, err := d.SelectAccessSample(context.Background(), q, 0, access.Principal{}); err == nil {
		t.Fatal("expected error for n < 1")
	}
	// Unsupported engine for sample
	dUnknown := &Database{Manifest: &manifest.Manifest{Storage: manifest.Storage{Engine: "unknown"}}}
	if _, _, err := dUnknown.SelectAccessSample(context.Background(), q, 10, access.Principal{}); err == nil {
		t.Fatal("expected error for unknown engine")
	}

	// sample ordering with key not at end
	qBadOrder, _, err := ParseTugQL([]byte("from: {name: users}\norderBy: [{field: id}, {field: name}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.SelectAccessSample(context.Background(), qBadOrder, 10, access.Principal{}); err == nil {
		t.Fatal("expected error for key not last in ordering")
	}

	// sample order too large (>32 fields)
	var orders []dal.OrderExpression
	for i := 0; i < 33; i++ {
		orders = append(orders, dal.AscendingField(fmt.Sprintf("f%d", i)))
	}
	qTooManyOrders := sampleQuery{
		StructuredQuery: q,
		count:           5,
		order:           orders,
	}
	if _, _, err := d.SelectAccessSample(context.Background(), qTooManyOrders, 10, access.Principal{}); err == nil {
		t.Fatal("expected error for >32 ordering fields")
	}

	// sample requires field ordering (non-field ref order expression)
	qBadOrderExpr := sampleQuery{
		StructuredQuery: q,
		count:           5,
		order:           []dal.OrderExpression{dal.Ascending(dal.Constant{Value: 1})},
	}
	if _, _, err := d.SelectAccessSample(context.Background(), qBadOrderExpr, 10, access.Principal{}); err == nil {
		t.Fatal("expected error for non-field ordering expression")
	}
}

func TestUpdatesCore(t *testing.T) {
	t.Parallel()
	now := time.Now()

	// applyUpdates: both fieldName and fieldPath set
	err1 := applyUpdates(map[string]any{}, []UpdateOp{{FieldName: "a", FieldPath: []string{"b"}}}, now)
	if err1 == nil || !strings.Contains(err1.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err1)
	}

	// applyUpdates: neither set
	err2 := applyUpdates(map[string]any{}, []UpdateOp{{}}, now)
	if err2 == nil || !strings.Contains(err2.Error(), "one of fieldName or fieldPath is required") {
		t.Fatalf("expected required error, got %v", err2)
	}

	// applyUpdates: empty segment
	err3 := applyUpdates(map[string]any{}, []UpdateOp{{FieldPath: []string{"a", ""}}}, now)
	if err3 == nil || !strings.Contains(err3.Error(), "empty field path segment") {
		t.Fatalf("expected empty segment error, got %v", err3)
	}

	// applyUpdates: Delete
	data := map[string]any{"a": map[string]any{"b": 1, "c": 2}}
	if err := applyUpdates(data, []UpdateOp{{FieldPath: []string{"a", "b"}, Delete: true}}, now); err != nil {
		t.Fatalf("applyUpdates delete: %v", err)
	}
	if _, ok := data["a"].(map[string]any)["b"]; ok {
		t.Fatal("expected b to be deleted")
	}

	// applyUpdates: ServerTimestamp error (non-object intermediate)
	dataNonObj := map[string]any{"a": "scalar"}
	if err := applyUpdates(dataNonObj, []UpdateOp{{FieldPath: []string{"a", "b"}, ServerTimestamp: true}}, now); err == nil {
		t.Fatal("expected error for non-object intermediate on ServerTimestamp")
	}

	// applyUpdates: ServerTimestamp success
	dataObj := map[string]any{}
	if err := applyUpdates(dataObj, []UpdateOp{{FieldName: "created_at", ServerTimestamp: true}}, now); err != nil {
		t.Fatalf("applyUpdates ServerTimestamp: %v", err)
	}
	if dataObj["created_at"] == nil {
		t.Fatal("expected created_at timestamp")
	}

	// applyUpdates: increment error (non-numeric delta)
	if err := applyUpdates(map[string]any{}, []UpdateOp{{FieldName: "count", Transform: "increment", Value: "not_a_number"}}, now); err == nil {
		t.Fatal("expected error for non-numeric increment delta")
	}

	// applyUpdates: increment error (non-numeric existing field)
	if err := applyUpdates(map[string]any{"count": "str"}, []UpdateOp{{FieldName: "count", Transform: "increment", Value: 5}}, now); err == nil {
		t.Fatal("expected error for increment on non-numeric existing field")
	}

	// applyUpdates: increment success
	dataInc := map[string]any{"count": 10}
	if err := applyUpdates(dataInc, []UpdateOp{{FieldName: "count", Transform: "increment", Value: 5}}, now); err != nil {
		t.Fatalf("applyUpdates increment: %v", err)
	}
	if dataInc["count"] != 15.0 {
		t.Fatalf("unexpected increment result: %v", dataInc["count"])
	}

	// applyUpdates: unknown transform
	if err := applyUpdates(map[string]any{}, []UpdateOp{{FieldName: "x", Transform: "unknown"}}, now); err == nil {
		t.Fatal("expected error for unknown transform")
	}

	// applyUpdates: default setAtPath error
	if err := applyUpdates(map[string]any{"a": 123}, []UpdateOp{{FieldPath: []string{"a", "b"}, Value: "v"}}, now); err == nil {
		t.Fatal("expected error setting child on scalar")
	}

	// parentAtPath: non-map when create is false
	parent, err := parentAtPath(map[string]any{"a": "scalar"}, []string{"a", "b"}, false)
	if err != nil || parent != nil {
		t.Fatalf("expected nil, nil for parentAtPath create=false on non-map, got %v, %v", parent, err)
	}

	// toFloat tests for all types
	if f, ok := toFloat(float32(1.5)); !ok || f != 1.5 {
		t.Fatalf("toFloat(float32): %v, %v", f, ok)
	}
	if f, ok := toFloat(int64(42)); !ok || f != 42.0 {
		t.Fatalf("toFloat(int64): %v, %v", f, ok)
	}
	if f, ok := toFloat(uint64(99)); !ok || f != 99.0 {
		t.Fatalf("toFloat(uint64): %v, %v", f, ok)
	}
	if f, ok := toFloat(json.Number("3.14")); !ok || math.Abs(f-3.14) > 1e-6 {
		t.Fatalf("toFloat(json.Number): %v, %v", f, ok)
	}
	if _, ok := toFloat(json.Number("invalid")); ok {
		t.Fatal("expected false for invalid json.Number")
	}
	if _, ok := toFloat("string"); ok {
		t.Fatal("expected false for string")
	}
}

func TestTugQLProfileCore(t *testing.T) {
	t.Parallel()
	// unsupportedMoneyJoinShape
	if msg := unsupportedMoneyJoinShape(nil); msg != "money mode requires a root collection" {
		t.Fatalf("unexpected msg: %s", msg)
	}
	// non inner/left join
	crossJoin := dal.NewJoinedSource(dal.NewRootCollectionRef("items", ""), dal.JoinCross, nil)
	fromCross := dal.From(dal.NewRootCollectionRef("orders", "")).Join(crossJoin)
	if msg := unsupportedMoneyJoinShape(fromCross); msg != "money mode supports only flat inner or left hash joins" {
		t.Fatalf("unexpected msg: %s", msg)
	}

	// hash algorithm in join
	hashJoin := dal.NewJoinedSource(dal.NewRootCollectionRef("items", ""), dal.JoinInner, dal.NewComparison(dal.Field("orders.id"), dal.Equal, dal.Field("items.id"))).WithAlgorithms(dal.JoinAlgorithmHash)
	fromHash := dal.From(dal.NewRootCollectionRef("orders", "")).Join(hashJoin)
	_ = unsupportedMoneyJoinShape(fromHash)

	// expressionNode with float constant in money mode
	w := &profileWalk{money: true}
	err := w.expressionNode(dal.Constant{Value: 3.14}, 0)
	if err == nil || !strings.Contains(err.Error(), "money mode does not accept binary floating-point") {
		t.Fatalf("expected money float refusal, got %v", err)
	}

	// collection-name invalid schema in profileWalk.collection
	badSchemaRef := dal.NewQualifiedRootCollectionRef(strings.Repeat("x", 64), "items", "")
	errSchema := w.collection(badSchemaRef)
	if errSchema == nil || !strings.Contains(errSchema.Error(), "outside the PostgreSQL identifier contract") {
		t.Fatalf("expected schema identifier contract refusal, got %v", errSchema)
	}
}

func TestCoreGuardAndQuerySources(t *testing.T) {
	t.Parallel()
	// GuardCanonicalCollection when isDocumentEngine is true
	dDoc := &Database{documentEngine: true}
	if err := dDoc.GuardCanonicalCollection("any_name"); err != nil {
		t.Fatalf("GuardCanonicalCollection on documentEngine: %v", err)
	}

	// checkSourceCollection on nativePostgres with invalid schema or name
	dPG := &Database{
		nativePostgres: true,
		Manifest:       &manifest.Manifest{Database: manifest.Database{ID: "pgdb"}},
	}
	// Missing schema
	if err := dPG.checkSourceCollection(dal.NewRootCollectionRef("orders", "")); err == nil {
		t.Fatal("expected error for missing schema on nativePostgres")
	}
	// Invalid schema
	if err := dPG.checkSourceCollection(dal.NewQualifiedRootCollectionRef(strings.Repeat("x", 64), "orders", "")); err == nil {
		t.Fatal("expected error for invalid schema name on nativePostgres")
	}
	// Invalid relation name
	if err := dPG.checkSourceCollection(dal.NewQualifiedRootCollectionRef("public", strings.Repeat("x", 64), "")); err == nil {
		t.Fatal("expected error for invalid relation name on nativePostgres")
	}

	// query_guard.go nameWalker.sources with invalid native identifier in Name and Schema
	nw := nameWalker{relational: true}
	badSource := dal.NewQualifiedRootCollectionRef("public", strings.Repeat("x", 64), "")
	if _, err := nw.sources([]dal.RecordsetSource{badSource}, 0, nil); err == nil {
		t.Fatal("expected error for bad relation name in nameWalker.sources")
	}
	badSourceSchema := dal.NewQualifiedRootCollectionRef(strings.Repeat("x", 64), "orders", "")
	if _, err := nw.sources([]dal.RecordsetSource{badSourceSchema}, 0, nil); err == nil {
		t.Fatal("expected error for bad schema name in nameWalker.sources")
	}

	// queryError on readOnlyHTTP
	dHTTP := &Database{readOnlyHTTP: true}
	if err := dHTTP.queryError(context.Background(), "prefix", dal.ErrNotSupported); !errors.Is(err, ErrHTTPOperationUnsupported) {
		t.Fatalf("expected ErrHTTPOperationUnsupported, got %v", err)
	}
	ctxCancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := dHTTP.queryError(ctxCancelled, "prefix", errors.New("other")); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if err := dHTTP.queryError(context.Background(), "prefix", errors.New("other")); !errors.Is(err, ErrDatabaseUnreachable) {
		t.Fatalf("expected ErrDatabaseUnreachable, got %v", err)
	}
}

func TestCoreServingKeysAndPolicyControllerMethods(t *testing.T) {
	t.Parallel()
	d := &Database{
		Manifest: &manifest.Manifest{Database: manifest.Database{ID: "db1"}, Storage: manifest.Storage{Engine: "sqlite"}},
		names: collectionNames{
			canonical: map[string]string{"orders": "orders"},
			spellings: map[string][]string{"orders": {"orders"}},
		},
		servingKeys: map[string]string{
			"orders": "key123",
		},
		sqliteLockWait: 0,
	}
	// ServingKey
	if k, ok := d.ServingKey("orders"); !ok || k != "key123" {
		t.Fatalf("ServingKey orders: got %q, %v", k, ok)
	}
	if _, ok := d.ServingKey("unknown"); ok {
		t.Fatal("expected false for unknown serving key")
	}
	// HasImmutableSQLiteKeys true
	if !d.HasImmutableSQLiteKeys() {
		t.Fatal("expected HasImmutableSQLiteKeys true")
	}
	// HasImmutableSQLiteKeys false when lock wait != 0
	dLock := &Database{
		Manifest:       d.Manifest,
		names:          d.names,
		servingKeys:    d.servingKeys,
		sqliteLockWait: 5 * time.Second,
	}
	if dLock.HasImmutableSQLiteKeys() {
		t.Fatal("expected HasImmutableSQLiteKeys false when sqliteLockWait != 0")
	}

	// ReloadPolicies & PublishPolicies with policyController == nil
	dNoCtrl := &Database{}
	if _, err := dNoCtrl.ReloadPolicies(context.Background()); err == nil {
		t.Fatal("expected error from ReloadPolicies when policyController is nil")
	}
	if _, err := dNoCtrl.PublishPolicies(context.Background(), "", nil); err == nil {
		t.Fatal("expected error from PublishPolicies when policyController is nil")
	}
}

func TestCoreOpenValidations(t *testing.T) {
	t.Parallel()

	// 1. OpenWithPolicyController nil controller
	if _, err := OpenWithPolicyController(&manifest.Manifest{}, nil, nil, "", nil); err == nil {
		t.Fatal("expected error for nil controller")
	}

	// 2. OpenWithPolicyController owner mismatch
	storeDir := t.TempDir()
	owner := policystore.Owner{Database: "db1", Realm: "realm1", Enabled: true}
	st, err := policystore.Open(storeDir, owner)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := access.ParseTugQLPolicy([]byte(`apiVersion: tugql.org/access/v1
kind: AccessPolicy
metadata: {name: customers}
target: {database: db1}
composition: dalgo-hierarchical-v1
default: deny
scopes:
  - path: /customers
    rules: [{id: read, effect: allow, operations: [query]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Activate(context.Background(), "", []access.TugQLDocument{doc}); err != nil {
		t.Fatal(err)
	}
	ctrl, err := policystore.NewController(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	mOwnerMismatch := &manifest.Manifest{
		Database: manifest.Database{ID: "different_db"},
		ACL:      &access.FilePolicyConfig{Enabled: true, Realm: "realm1"},
	}
	if _, err := OpenWithPolicyController(mOwnerMismatch, nil, nil, "", ctrl); err == nil {
		t.Fatal("expected owner mismatch error")
	}

	// 3. open: enabled ACL without policies or controller
	mACLLacking := &manifest.Manifest{
		Database: manifest.Database{ID: "db1"},
		ACL:      &access.FilePolicyConfig{Enabled: true},
	}
	if _, err := open(mACLLacking, nil, nil, "", nil); err == nil || !strings.Contains(err.Error(), "requires loaded policies") {
		t.Fatalf("expected requires loaded policies error, got %v", err)
	}

	// 4. open: ValidateRetention error
	mRetentionErr := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", Retention: "invalid_retention"},
	}
	if _, err := open(mRetentionErr, nil, nil, "", nil); err == nil {
		t.Fatal("expected ValidateRetention error")
	}

	// 5. open: ValidateHTTP error
	mHTTPErr := &manifest.Manifest{
		Database: manifest.Database{ID: "db1"},
		Storage:  manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: "invalid"}},
	}
	if _, err := open(mHTTPErr, nil, nil, "", nil); err == nil {
		t.Fatal("expected ValidateHTTP error")
	}

	// 6. open: ValidateLicenses error
	mLicErr := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", License: &license.Declaration{SPDX: "invalid spdx"}},
	}
	if _, err := open(mLicErr, nil, nil, "", nil); err == nil {
		t.Fatal("expected ValidateLicenses error")
	}

	// 7. open: unsupported mode
	mMode := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeSchemaless},
		Storage:  manifest.Storage{Engine: "sqlite"},
	}
	if _, err := open(mMode, nil, []schema.Mode{schema.ModeStrict}, "", nil); err == nil {
		t.Fatal("expected ModeCompatibilityError")
	}

	// 8. open: checkFieldNames error (field named ID on SQL engine)
	mBadField := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				"users": {Fields: map[string]schema.Field{"ID": {Type: schema.TypeString}}},
			},
		},
	}
	if _, err := open(mBadField, nil, []schema.Mode{schema.ModeStrict}, "", nil); err == nil {
		t.Fatal("expected checkFieldNames error")
	}

	// 9. open: snapshotLicenses error
	mBadLic := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
			},
		},
		RecordsetLicenses: map[string]license.Declaration{
			"undeclared": {SPDX: "MIT"},
		},
	}
	if _, err := open(mBadLic, nil, []schema.Mode{schema.ModeStrict}, "", nil); err == nil {
		t.Fatal("expected snapshotLicenses error")
	}
}

func TestCoreLifecycleAndAccessInterfaces(t *testing.T) {
	t.Parallel()

	// io.Closer closing and error collection in Close()
	closedCall := false
	d := &Database{}
	d.OnClose(nil) // nil is ignored
	d.OnClose(func() error {
		closedCall = true
		return errors.New("err2")
	})
	d.OnClose(func() error {
		return errors.New("err1")
	})
	err := d.Close()
	if err == nil {
		t.Fatal("expected errors from Close()")
	}
	if !closedCall {
		t.Fatal("expected closer callback to run")
	}
	// Idempotent Close()
	if err2 := d.Close(); !errors.Is(err2, err) {
		t.Fatalf("expected same error on second Close(), got %v", err2)
	}

	// DB() and InferredSnapshot()
	if d.DB() != nil {
		t.Fatal("expected nil DB")
	}
	if d.InferredSnapshot() != nil {
		t.Fatal("expected nil InferredSnapshot for nil cat")
	}
	cat, err := inferred.Load(filepath.Join(t.TempDir(), "inferred.json"))
	if err != nil {
		t.Fatal(err)
	}
	d.cat = cat
	if d.InferredSnapshot() == nil {
		t.Fatal("expected non-nil InferredSnapshot")
	}
}

func TestFixedPolicyLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Error path
	dErr := &Database{ownerPolicies: func(context.Context) ([]access.Policy, error) {
		return nil, errors.New("policy err")
	}}
	if _, err := dErr.fixedPolicyLease(ctx); err == nil {
		t.Fatal("expected error from fixedPolicyLease")
	}

	// Success path
	d := &Database{ownerPolicies: func(context.Context) ([]access.Policy, error) {
		return []access.Policy{}, nil
	}}
	lease, err := d.fixedPolicyLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(lease.Policies()) != 0 {
		t.Fatal("expected empty policies")
	}
	if lease.Revision() != "fixed" {
		t.Fatalf("expected revision fixed, got %q", lease.Revision())
	}
	lease.Release()
}

func TestAccessSampleMoreBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 1. hasKey true (key is last in ordering)
	qWithKey, _, err := ParseTugQL([]byte("from: {name: customers}\norderBy: [{field: id}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := &Database{
		Manifest: &manifest.Manifest{
			Database: manifest.Database{ID: "db1"},
			Storage:  manifest.Storage{Engine: "sqlite"},
			Schemas: &schema.Schemas{
				Collections: map[string]schema.Collection{
					"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
				},
			},
		},
		names: collectionNames{
			canonical: map[string]string{"customers": "customers"},
			spellings: map[string][]string{"customers": {"customers"}},
		},
		db: &scriptedQueryDB{reader: &scriptedReader{}},
	}
	records, order, err := d.SelectAccessSample(ctx, qWithKey, 10, access.Principal{})
	if err != nil {
		t.Fatalf("SelectAccessSample with key: %v", err)
	}
	if len(order) != 1 || order[0].Expression().(dal.FieldRef).Name() != "id" {
		t.Fatalf("unexpected order: %v", order)
	}
	_ = records

	// 2. admitSample denial
	pureDeny, err := access.DeclareInspectionPure(joinSrcDenyPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	dDeny := &Database{
		Manifest: d.Manifest,
		names:    d.names,
		ownerPolicies: func(context.Context) ([]access.Policy, error) {
			return []access.Policy{pureDeny}, nil
		},
	}
	if _, _, err := dDeny.SelectAccessSample(ctx, qWithKey, 10, access.Principal{}); err == nil {
		t.Fatal("expected policy denial error from admitSample")
	}

	// 3. secureWithPolicies error (when d.db is nil)
	pureAllow, err := access.DeclareInspectionPure(joinSrcAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	dNilDB := &Database{
		Manifest: d.Manifest,
		names:    d.names,
		db:       nil,
		ownerPolicies: func(context.Context) ([]access.Policy, error) {
			return []access.Policy{pureAllow}, nil
		},
	}
	if _, _, err := dNilDB.SelectAccessSample(ctx, qWithKey, 10, access.Principal{}); err == nil {
		t.Fatal("expected secureWithPolicies error on nil db")
	}
}

type dummyProtectedFactoryDB struct {
	dal.DB
	retDB    dal.DB
	retCoord *access.EnforcementCoordinator
	retErr   error
}

func (f *dummyProtectedFactoryDB) ConfigureProtectedAccess(participants ...access.MandatoryParticipant) (dal.DB, *access.EnforcementCoordinator, error) {
	for _, p := range participants {
		if p.Provider != nil {
			_, _ = p.Provider(context.Background())
		}
	}
	return f.retDB, f.retCoord, f.retErr
}

type dummyLowerPoliciesDB struct {
	dal.DB
	policies []access.Policy
	err      error
}

func (d *dummyLowerPoliciesDB) AccessPolicies(context.Context) ([]access.Policy, error) {
	return d.policies, d.err
}

type dummyProtectedAndLowerDB struct {
	dal.DB
	retDB    dal.DB
	retCoord *access.EnforcementCoordinator
	retErr   error
	policies []access.Policy
}

func (f *dummyProtectedAndLowerDB) ConfigureProtectedAccess(participants ...access.MandatoryParticipant) (dal.DB, *access.EnforcementCoordinator, error) {
	return f.retDB, f.retCoord, f.retErr
}

func (f *dummyProtectedAndLowerDB) AccessPolicies(context.Context) ([]access.Policy, error) {
	return f.policies, nil
}

func TestOpenProtectedFactoryAndMore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// OpenWithPolicyController snapshot error
	emptyCtrl := &policystore.Controller{}
	if _, err := OpenWithPolicyController(&manifest.Manifest{}, nil, nil, "", emptyCtrl); err == nil {
		t.Fatal("expected snapshot error for empty controller")
	}

	// OpenWithPolicyController success path
	storeDir := t.TempDir()
	owner := policystore.Owner{Database: "db1", Realm: "realm1", Enabled: true}
	st, err := policystore.Open(storeDir, owner)
	if err != nil {
		t.Fatal(err)
	}
	pDoc, err := access.ParseTugQLPolicy([]byte(`apiVersion: tugql.org/access/v1
kind: AccessPolicy
metadata: {name: customers}
target: {database: db1}
composition: dalgo-hierarchical-v1
default: deny
scopes: [{path: /customers, rules: [{id: r1, effect: allow, operations: [query]}]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Activate(ctx, "", []access.TugQLDocument{pDoc}); err != nil {
		t.Fatal(err)
	}
	ctrl, err := policystore.NewController(ctx, st)
	if err != nil {
		t.Fatal(err)
	}

	mSuccess := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		ACL:      &access.FilePolicyConfig{Enabled: true, Realm: "realm1"},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
			},
		},
	}
	dbSuccess, err := OpenWithPolicyController(mSuccess, &writeGuardDB{}, []schema.Mode{schema.ModeStrict}, "", ctrl)
	if err != nil {
		t.Fatalf("OpenWithPolicyController: %v", err)
	}
	if dbSuccess == nil {
		t.Fatal("expected non-nil Database")
	}

	// ReloadPolicies & PublishPolicies success
	snap, err := dbSuccess.ReloadPolicies(ctx)
	if err != nil {
		t.Fatalf("ReloadPolicies: %v", err)
	}
	if _, err := dbSuccess.PublishPolicies(ctx, snap.Revision, []access.TugQLDocument{pDoc}); err != nil {
		t.Fatalf("PublishPolicies: %v", err)
	}

	// open with HTTP profile
	mHTTP := &manifest.Manifest{
		Database: manifest.Database{ID: "httpdb", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileECBDaily, Collection: "rates"}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"rates": {
				Fields: map[string]schema.Field{
					"time":     {Type: schema.TypeString},
					"currency": {Type: schema.TypeString},
					"rate":     {Type: schema.TypeString},
				},
			},
		}},
	}
	dbHTTP, err := open(mHTTP, nil, []schema.Mode{schema.ModeStrict}, "", nil)
	if err != nil {
		t.Fatalf("open HTTP: %v", err)
	}
	if dbHTTP.HTTPProfile() != manifest.HTTPProfileECBDaily {
		t.Fatalf("unexpected HTTP profile: %s", dbHTTP.HTTPProfile())
	}

	// open with nativePostgres error (long column name)
	pgID, _ := schema.NativePostgresCollectionID("public", "orders")
	mBadPG := &manifest.Manifest{
		Database: manifest.Database{ID: "pgdb", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true}},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				pgID: {
					Source: &schema.NativeCollectionSource{Schema: "public", Name: "orders"},
					Fields: map[string]schema.Field{strings.Repeat("x", 64): {Type: schema.TypeString}},
				},
			},
		},
	}
	if _, err := open(mBadPG, nil, []schema.Mode{schema.ModeStrict}, "", nil); err == nil {
		t.Fatal("expected nativePostgresMappings error in open")
	}

	// open with lowerPolicies (db implements AccessPolicies)
	mLower := &manifest.Manifest{
		Database: manifest.Database{ID: "lowerdb", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	dbWithLower, err := open(mLower, &dummyLowerPoliciesDB{}, []schema.Mode{schema.ModeStrict}, "", nil)
	if err != nil {
		t.Fatalf("open lowerPolicies: %v", err)
	}
	if dbWithLower.lowerPolicies == nil {
		t.Fatal("expected lowerPolicies to be set")
	}

	// open with ConfigureProtectedAccess
	coord := &access.EnforcementCoordinator{}
	allowPol := joinSrcAllowPolicy{}
	mProt := &manifest.Manifest{
		Database: manifest.Database{ID: "protdb", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		ACL:      &access.FilePolicyConfig{Enabled: true, Realm: "realm1"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	// Case 1: Incomplete boundary error
	factoryIncomplete := &dummyProtectedFactoryDB{retDB: nil, retCoord: nil}
	if _, err := open(mProt, factoryIncomplete, []schema.Mode{schema.ModeStrict}, "", nil, allowPol); err == nil {
		t.Fatal("expected incomplete enforcement boundary error")
	}
	// Case 2: Factory error
	factoryErr := &dummyProtectedFactoryDB{retErr: errors.New("factory failed")}
	if _, err := open(mProt, factoryErr, []schema.Mode{schema.ModeStrict}, "", nil, allowPol); err == nil {
		t.Fatal("expected factory error")
	}
	// Case 3: Success with fixed policy lease
	factoryOK := &dummyProtectedFactoryDB{retDB: &writeGuardDB{}, retCoord: coord}
	if _, err := open(mProt, factoryOK, []schema.Mode{schema.ModeStrict}, "", nil, allowPol); err != nil {
		t.Fatalf("expected success with fixedPolicyLease: %v", err)
	}
	// Case 4: Success with controller
	mProtCtrl := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		ACL:      &access.FilePolicyConfig{Enabled: true, Realm: "realm1"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	if _, err := open(mProtCtrl, factoryOK, []schema.Mode{schema.ModeStrict}, "", ctrl); err != nil {
		t.Fatalf("expected success with controller: %v", err)
	}
	// Case 5: HasAccessPolicies without ownerPolicies (only lowerPolicies)
	factoryLowerOnly := &dummyProtectedAndLowerDB{
		retDB:    &writeGuardDB{},
		retCoord: coord,
		policies: []access.Policy{allowPol},
	}
	mProtLower := &manifest.Manifest{
		Database: manifest.Database{ID: "loweronly", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	if _, err := open(mProtLower, factoryLowerOnly, []schema.Mode{schema.ModeStrict}, "", nil); err != nil {
		t.Fatalf("expected success for lowerPolicies only in ConfigureProtectedAccess: %v", err)
	}

	// open partial mode with invalid catalogue path
	mPartial := &manifest.Manifest{
		Database: manifest.Database{ID: "part", SchemaMode: schema.ModePartial},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	if _, err := open(mPartial, nil, []schema.Mode{schema.ModePartial}, filepath.Join("\x00", "invalid"), nil); err == nil {
		t.Fatal("expected error loading inferred catalogue on invalid path")
	}

	// Case 6: Storage engine http with empty target and policies -> SecureDB(nil) fails
	// Case 6: Storage engine http with empty target and policies -> SecureDB(nil) fails
	mHTTPEmpty := &manifest.Manifest{
		Database: manifest.Database{ID: "httpdb", SchemaMode: schema.ModeStrict},
		Storage: manifest.Storage{
			Engine: "http",
			HTTP: &manifest.HTTPOptions{
				Profile:    manifest.HTTPProfileECBDaily,
				Collection: "rates",
			},
		},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"rates": {
				Fields: map[string]schema.Field{
					"time":     {Type: schema.TypeString},
					"currency": {Type: schema.TypeString},
					"rate":     {Type: schema.TypeString},
				},
			},
		}},
	}
	if _, err := open(mHTTPEmpty, nil, []schema.Mode{schema.ModeStrict}, "", nil, allowPol); err == nil {
		t.Fatal("expected error securing nil db in open")
	}
}

func TestCoerceToSchemaCases(t *testing.T) {
	t.Parallel()
	d := &Database{
		Manifest: &manifest.Manifest{
			Schemas: &schema.Schemas{
				Collections: map[string]schema.Collection{
					"customers": {
						Fields: map[string]schema.Field{
							"FirstName": {Type: schema.TypeString},
							"active":    {Type: schema.TypeBoolean},
						},
					},
				},
			},
		},
	}

	// Key casing mismatch & float64 to bool conversion
	input := map[string]any{
		"firstname": "Ada",
		"active":    float64(1.0),
	}
	out := d.coerceToSchema("customers", input)
	if out["FirstName"] != "Ada" {
		t.Fatalf("expected FirstName: Ada, got %v", out["FirstName"])
	}
	if _, hasOld := out["firstname"]; hasOld {
		t.Fatal("expected lowercase key to be deleted")
	}
	if out["active"] != true {
		t.Fatalf("expected active true, got %v", out["active"])
	}

	// False float64
	outFalse := d.coerceToSchema("customers", map[string]any{"active": float64(0.0)})
	if outFalse["active"] != false {
		t.Fatalf("expected active false, got %v", outFalse["active"])
	}
}

func TestCollectionsBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 1. nativePostgres
	dPG := &Database{
		nativePostgres: true,
		nativeSources: map[string]schema.NativeCollectionSource{
			"orders":    {},
			"customers": {},
		},
	}
	colsPG, err := dPG.Collections(ctx)
	if err != nil || len(colsPG) != 2 || colsPG[0] != "customers" || colsPG[1] != "orders" {
		t.Fatalf("unexpected Collections nativePostgres: %v, %v", colsPG, err)
	}

	// 2. readOnlyHTTP
	dHTTP := &Database{
		readOnlyHTTP: true,
		names: collectionNames{
			spellings: map[string][]string{
				"b": {"b"},
				"a": {"a"},
			},
		},
	}
	colsHTTP, err := dHTTP.Collections(ctx)
	if err != nil || len(colsHTTP) != 2 || colsHTTP[0] != "a" || colsHTTP[1] != "b" {
		t.Fatalf("unexpected Collections readOnlyHTTP: %v, %v", colsHTTP, err)
	}

	// 3. Reader not ok
	dNoReader := &Database{db: struct{ dal.DB }{}}
	colsNoReader, err := dNoReader.Collections(ctx)
	if err != nil || colsNoReader != nil {
		t.Fatalf("expected nil, nil for db without SchemaReader: %v, %v", colsNoReader, err)
	}

	// 4. Reader returns error
	dErrReader := &Database{db: &scriptedReaderDB{failure: errors.New("list failed")}}
	if _, err := dErrReader.Collections(ctx); err == nil || !strings.Contains(err.Error(), "failed to list collections") {
		t.Fatalf("expected failed to list collections error, got %v", err)
	}
}

type dummyDescribeDB struct {
	dal.DB
	def *dbschema.CollectionDef
	err error
}

func (d *dummyDescribeDB) DescribeCollection(context.Context, *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	return d.def, d.err
}
func (d *dummyDescribeDB) ListCollections(context.Context, *record.Key) ([]dal.CollectionRef, error) {
	return nil, nil
}
func (d *dummyDescribeDB) ListIndexes(context.Context, *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	return nil, nil
}
func (d *dummyDescribeDB) ListConstraints(context.Context, *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	return nil, nil
}
func (d *dummyDescribeDB) ListReferrers(context.Context, *dal.CollectionRef) ([]dbschema.Referrer, error) {
	return nil, nil
}

func TestCollectionForeignKeysBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 1. ServingKeys invalid collection
	dServing := &Database{
		servingKeys: map[string]string{"orders": "k1"},
		names: collectionNames{
			canonical: map[string]string{"orders": "orders"},
			spellings: map[string][]string{"orders": {"orders"}},
		},
	}
	if _, err := dServing.CollectionForeignKeys(ctx, "unknown"); err == nil {
		t.Fatal("expected GuardCollection error for unknown collection")
	}

	// 2. DB does not implement SchemaReader
	dNoReader := &Database{db: struct{ dal.DB }{}}
	if fks, err := dNoReader.CollectionForeignKeys(ctx, "orders"); err != nil || fks != nil {
		t.Fatalf("expected nil, nil for db without SchemaReader: %v, %v", fks, err)
	}

	// 3. DescribeCollection returns *dbschema.NotSupportedError
	dUnsupported := &Database{db: &dummyDescribeDB{err: &dbschema.NotSupportedError{}}}
	if fks, err := dUnsupported.CollectionForeignKeys(ctx, "orders"); err != nil || fks != nil {
		t.Fatalf("expected nil, nil on NotSupportedError: %v, %v", fks, err)
	}

	// 4. DescribeCollection returns def == nil
	dNilDef := &Database{db: &dummyDescribeDB{def: nil}}
	if fks, err := dNilDef.CollectionForeignKeys(ctx, "orders"); err != nil || fks != nil {
		t.Fatalf("expected nil, nil on def == nil: %v, %v", fks, err)
	}

	// 5. ServingKeys visible foreign keys filtering
	fkMatch := dbschema.ForeignKeyDef{ReferencedCollection: "customers"}
	fkNoMatch := dbschema.ForeignKeyDef{ReferencedCollection: "other"}
	fkNamespace := dbschema.ForeignKeyDef{ReferencedCollection: "customers", ReferencedNamespace: "remote"}
	dServingFilter := &Database{
		servingKeys: map[string]string{"orders": "k1"},
		names: collectionNames{
			canonical: map[string]string{"orders": "orders", "customers": "customers"},
			spellings: map[string][]string{"orders": {"orders"}, "customers": {"customers"}},
		},
		db: &dummyDescribeDB{def: &dbschema.CollectionDef{
			ForeignKeys: []dbschema.ForeignKeyDef{fkMatch, fkNoMatch, fkNamespace},
		}},
	}
	visible, err := dServingFilter.CollectionForeignKeys(ctx, "orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].ReferencedCollection != "customers" {
		t.Fatalf("unexpected visible foreign keys: %v", visible)
	}

	// 6. No serving keys - returns def.ForeignKeys directly & with nativeSources
	dNoServing := &Database{
		nativeSources: map[string]schema.NativeCollectionSource{
			"orders": {Schema: "public", Name: "orders"},
		},
		db: &dummyDescribeDB{def: &dbschema.CollectionDef{
			ForeignKeys: []dbschema.ForeignKeyDef{fkMatch},
		}},
	}
	fksDirect, err := dNoServing.CollectionForeignKeys(ctx, "orders")
	if err != nil || len(fksDirect) != 1 {
		t.Fatalf("unexpected foreign keys: %v, %v", fksDirect, err)
	}
}

type configurableTxDB struct {
	writeGuardDB
	insertErr error
	setErr    error
	updateErr error
	deleteErr error
}

func (c *configurableTxDB) RunReadwriteTransaction(ctx context.Context, worker dal.RWTxWorker, _ ...dal.TransactionOption) error {
	return worker(ctx, &configurableTx{db: c})
}

type configurableTx struct {
	dal.ReadwriteTransaction
	db *configurableTxDB
}

func (t *configurableTx) Set(context.Context, record.Record) error { return t.db.setErr }
func (t *configurableTx) Insert(context.Context, record.Record, ...dal.InsertOption) error {
	return t.db.insertErr
}
func (t *configurableTx) Update(context.Context, *record.Key, []update.Update, ...dal.Precondition) error {
	return t.db.updateErr
}
func (t *configurableTx) Delete(context.Context, *record.Key) error {
	return t.db.deleteErr
}

func TestApplyAndValidateOpsMore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 1. validateOps unknown op
	d := &Database{
		Manifest: &manifest.Manifest{
			Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
			Storage:  manifest.Storage{Engine: "sqlite"},
			Schemas: &schema.Schemas{
				Collections: map[string]schema.Collection{
					"orders": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}},
				},
			},
		},
		names: collectionNames{
			canonical: map[string]string{"orders": "orders"},
			spellings: map[string][]string{"orders": {"orders"}},
		},
		db: &writeGuardDB{},
	}
	if _, err := d.Apply(ctx, []Op{{Op: "unknown", Key: record.NewKeyWithID("orders", "1")}}, ""); err == nil {
		t.Fatal("expected error for unknown op in Apply")
	}

	// 2. Partial mode with 2 ops in same collection (ensured[chain] true -> continue)
	catDir := t.TempDir()
	catPath := filepath.Join(catDir, "cat.json")
	cat, err := inferred.Load(catPath)
	if err != nil {
		t.Fatal(err)
	}
	dPartial := &Database{
		Manifest: &manifest.Manifest{
			Database: manifest.Database{ID: "db1", SchemaMode: schema.ModePartial},
			Storage:  manifest.Storage{Engine: "sqlite"},
			Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
		},
		names: collectionNames{
			canonical: map[string]string{"orders": "orders"},
			spellings: map[string][]string{"orders": {"orders"}},
		},
		cat: cat,
		db:  &writeGuardDB{absent: true},
	}
	opsTwo := []Op{
		{Op: "insert", Key: record.NewKeyWithID("orders", "1"), Data: map[string]any{"v": "a"}},
		{Op: "insert", Key: record.NewKeyWithID("orders", "2"), Data: map[string]any{"v": "b"}},
	}
	n, err := dPartial.Apply(ctx, opsTwo, "")
	if err != nil || n != 2 {
		t.Fatalf("Apply 2 ops partial: %d, %v", n, err)
	}

	// 3. Partial mode ensureCollection error
	dEnsureErr := &Database{
		Manifest: dPartial.Manifest,
		names:    dPartial.names,
		cat:      cat,
		db:       provisionDB{DB: &writeGuardDB{absent: true}, err: errors.New("ddl failed")},
	}
	if _, err := dEnsureErr.Apply(ctx, []Op{{Op: "insert", Key: record.NewKeyWithID("orders", "1"), Data: map[string]any{"v": "a"}}}, ""); err == nil {
		t.Fatal("expected ensureCollection error in partial mode")
	}

	// 4. Tx operation errors (set, insert, update, delete, toDalUpdates)
	baseKey := record.NewKeyWithID("orders", "1")
	// Set error
	dbSetErr := &configurableTxDB{setErr: errors.New("set failed")}
	dTxSet := &Database{Manifest: d.Manifest, names: d.names, db: dbSetErr}
	if _, err := dTxSet.Apply(ctx, []Op{{Op: "set", Key: baseKey, Data: map[string]any{"id": "1"}}}, ""); err == nil {
		t.Fatal("expected set error")
	}
	// Insert error
	dbInsertErr := &configurableTxDB{insertErr: errors.New("insert failed")}
	dTxInsert := &Database{Manifest: d.Manifest, names: d.names, db: dbInsertErr}
	if _, err := dTxInsert.Apply(ctx, []Op{{Op: "insert", Key: baseKey, Data: map[string]any{"id": "1"}}}, ""); err == nil {
		t.Fatal("expected insert error")
	}
	// toDalUpdates error (e.g. invalid transform)
	dTxToDalErr := &Database{Manifest: d.Manifest, names: d.names, db: &configurableTxDB{}}
	if _, err := dTxToDalErr.Apply(ctx, []Op{{Op: "update", Key: baseKey, Updates: []UpdateOp{{FieldName: "x", Transform: "bad_transform"}}}}, ""); err == nil {
		t.Fatal("expected toDalUpdates error")
	}
	// Update error
	dbUpdateErr := &configurableTxDB{updateErr: errors.New("update failed")}
	dTxUpdate := &Database{Manifest: d.Manifest, names: d.names, db: dbUpdateErr}
	if _, err := dTxUpdate.Apply(ctx, []Op{{Op: "update", Key: baseKey, Updates: []UpdateOp{{FieldName: "id", Value: "1"}}}}, ""); err == nil {
		t.Fatal("expected update error")
	}
	// Delete error
	dbDeleteErr := &configurableTxDB{deleteErr: errors.New("delete failed")}
	dTxDelete := &Database{Manifest: d.Manifest, names: d.names, db: dbDeleteErr}
	if _, err := dTxDelete.Apply(ctx, []Op{{Op: "delete", Key: baseKey}}, ""); err == nil {
		t.Fatal("expected delete error")
	}

	// 5. afterWrite hook error
	dHookErr := &Database{Manifest: d.Manifest, names: d.names, db: &writeGuardDB{}}
	dHookErr.SetAfterWrite(func(context.Context) error { return errors.New("hook failed") })
	if _, err := dHookErr.Apply(ctx, []Op{{Op: "delete", Key: baseKey}}, ""); err == nil || !strings.Contains(err.Error(), "after-write hook failed") {
		t.Fatalf("expected after-write hook error, got %v", err)
	}

	// 6. cat.Observe error (blocking file prevents directory creation)
	tempParent := t.TempDir()
	catDirErr := filepath.Join(tempParent, "catdir")
	if err := os.Mkdir(catDirErr, 0o755); err != nil {
		t.Fatal(err)
	}
	badCatPath := filepath.Join(catDirErr, "cat.json")
	badCat, err := inferred.Load(badCatPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(catDirErr); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catDirErr, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	dObsErr := &Database{
		Manifest: dPartial.Manifest,
		names:    dPartial.names,
		cat:      badCat,
		db:       &writeGuardDB{absent: true},
	}
	if _, err := dObsErr.Apply(ctx, []Op{{Op: "insert", Key: baseKey, Data: map[string]any{"id": "1"}}}, ""); err == nil || !strings.Contains(err.Error(), "inferred-schema observation failed") {
		t.Fatalf("expected inferred-schema observation error, got %v", err)
	}
}

func TestQueryTargetAndStreamQueryBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 1. Query.Target errors and success
	if _, _, err := (Query{Collection: ".."}).Target(); err == nil {
		t.Fatal("expected error for bad collection in Target")
	}
	if _, _, err := (Query{Collection: "orders", Parent: "bad/.."}).Target(); err == nil {
		t.Fatal("expected error for bad parent in Target")
	}
	parentKey, root, err := (Query{Collection: "items", Parent: "customers/c1"}).Target()
	if err != nil || root != "customers" || parentKey == nil {
		t.Fatalf("unexpected Target result: %v, %s, %v", parentKey, root, err)
	}

	// 2. StreamQuery validations
	d := &Database{
		Manifest: &manifest.Manifest{
			Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
			Storage:  manifest.Storage{Engine: "sqlite"},
			Schemas: &schema.Schemas{
				Collections: map[string]schema.Collection{
					"orders": {Fields: map[string]schema.Field{"total": {Type: schema.TypeInteger}, "arr": {Type: schema.TypeArray}}},
				},
			},
		},
		names: collectionNames{
			canonical: map[string]string{"orders": "orders"},
			spellings: map[string][]string{"orders": {"orders"}},
		},
		db: &scriptedQueryDB{reader: &scriptedReader{}},
	}
	// emit == nil
	if err := d.StreamQuery(ctx, Query{Collection: "orders"}, nil); err == nil {
		t.Fatal("expected error for nil emit")
	}
	// Target error
	if err := d.StreamQuery(ctx, Query{Collection: ".."}, func(Record) error { return nil }); err == nil {
		t.Fatal("expected error for bad collection")
	}
	// Unknown where op
	qUnknownWhere := Query{
		Collection: "orders",
		Where:      []Filter{{Field: "total", Op: "unknown_op", Value: 10}},
	}
	if err := d.StreamQuery(ctx, qUnknownWhere, func(Record) error { return nil }); err == nil {
		t.Fatal("expected error for unknown filter op")
	}

	// Filter ops: <, <=, >=, in, array-contains, array-contains-any, OrderBy Desc, Limit > 0
	qFilters := Query{
		Collection: "orders",
		Where: []Filter{
			{Field: "total", Op: "<", Value: 100},
			{Field: "total", Op: "<=", Value: 99},
			{Field: "total", Op: ">=", Value: 10},
			{Field: "total", Op: "array-contains", Value: 5},
			{Field: "total", Op: "array-contains-any", Value: []any{1, 2}},
		},
		OrderBy: []OrderBy{{Field: "total", Desc: true}},
		Limit:   5,
	}
	if err := d.StreamQuery(ctx, qFilters, func(Record) error { return nil }); err != nil {
		t.Fatalf("StreamQuery filters: %v", err)
	}

	// StreamQuery with parentKey != nil and !sortKeys (lines 114, 164)
	dWithRec := &Database{
		Manifest: &manifest.Manifest{
			Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
			Storage:  manifest.Storage{Engine: "ingitdb"},
			Schemas: &schema.Schemas{
				Collections: map[string]schema.Collection{
					"orders": {Fields: map[string]schema.Field{"total": {Type: schema.TypeInteger}}},
				},
			},
		},
		names:          d.names,
		documentEngine: true,
		db:             &scriptedQueryDB{reader: &scriptedReader{rows: 1}},
	}
	var emittedWithParent Record
	qWithParent := Query{
		Collection: "orders",
		Parent:     "customers/c1",
		OrderBy:    []OrderBy{{Field: "total", Desc: false}},
	}
	err = dWithRec.StreamQuery(ctx, qWithParent, func(rec Record) error {
		emittedWithParent = rec
		return nil
	})
	if err != nil {
		t.Fatalf("StreamQuery with parent: %v", err)
	}
	if emittedWithParent.Key.Parent() == nil {
		t.Fatal("expected parent key to be attached to record")
	}

	// sortKeys branch: KeysOnly, no OrderBy, Limit > 0, parentKey != nil, emit error
	dSortKeys := &Database{
		Manifest:       dWithRec.Manifest,
		names:          d.names,
		documentEngine: true,
		db:             &scriptedQueryDB{reader: &scriptedReader{rows: 3}},
	}
	qSortKeys := Query{
		Collection: "orders",
		Parent:     "customers/c1",
		KeysOnly:   true,
		Limit:      1, // truncation path
	}
	errEmit := dSortKeys.StreamQuery(ctx, qSortKeys, func(rec Record) error {
		return errors.New("emit error")
	})
	if errEmit == nil || !strings.Contains(errEmit.Error(), "emit error") {
		t.Fatalf("expected emit error, got %v", errEmit)
	}

	// sortKeys success path (emit returns nil)
	dSortKeysOK := &Database{
		Manifest:       dWithRec.Manifest,
		names:          d.names,
		documentEngine: true,
		db:             &scriptedQueryDB{reader: &scriptedReader{rows: 1}},
	}
	if err := dSortKeysOK.StreamQuery(ctx, qSortKeys, func(rec Record) error { return nil }); err != nil {
		t.Fatalf("expected success for sortKeys, got %v", err)
	}
}

type dummyQueryNoFrom struct {
	dal.StructuredQuery
}

func (dummyQueryNoFrom) From() dal.FromSource { return nil }

func TestValidateTugQLForMore(t *testing.T) {
	t.Parallel()

	// 1. query == nil
	if _, err := validateTugQLFor(nil, quotedNames); err == nil {
		t.Fatal("expected error for nil query")
	}

	// 2. query.From() == nil
	if _, err := validateTugQLFor(dummyQueryNoFrom{}, quotedNames); err == nil {
		t.Fatal("expected error for nil From")
	}

	// 3. Joins != 0
	from := dal.From(dal.NewRootCollectionRef("orders", "o"))
	from.Join(dal.NewJoinedSource(dal.NewRootCollectionRef("customers", "c"), dal.JoinInner, dal.NewComparison(dal.NewFieldRef("o", "customer_id"), dal.Equal, dal.NewFieldRef("c", "id"))))
	qJoin := from.NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("o", "id")})
	if _, err := validateTugQLFor(qJoin, quotedNames); err == nil {
		t.Fatal("expected error for joins in single-collection profile")
	}

	// 4. Invalid collection name
	fromBad := dal.From(dal.NewRootCollectionRef("..", ""))
	qBadCol := fromBad.NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "id")})
	if _, err := validateTugQLFor(qBadCol, strictNames); err == nil {
		t.Fatal("expected error for bad collection name with strictNames")
	}
}

func TestBoundedTugQLAndStreamTugQLSnapshotMore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	q, collection, err := ParseTugQL([]byte("from: {name: customers}\n"))
	if err != nil {
		t.Fatal(err)
	}
	_ = collection

	// boundedTugQL.String()
	b := boundedTugQL{q}
	if b.String() == "" {
		t.Fatal("expected non-empty boundedTugQL String")
	}

	d := &Database{
		Manifest: &manifest.Manifest{
			Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
			Storage:  manifest.Storage{Engine: "sqlite"},
			Schemas: &schema.Schemas{
				Collections: map[string]schema.Collection{
					"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
				},
			},
		},
		names: collectionNames{
			canonical: map[string]string{"customers": "customers"},
			spellings: map[string][]string{"customers": {"customers"}},
		},
		db: &scriptedQueryDB{reader: &scriptedReader{rows: 1}},
	}

	// StreamTugQLSnapshot limit != 0 error
	qWithLimit := sampleQuery{StructuredQuery: q, count: 5}
	if err := d.StreamTugQLSnapshot(ctx, qWithLimit, func(Record) error { return nil }); err == nil {
		t.Fatal("expected error for non-zero limit in StreamTugQLSnapshot")
	}

	// StreamTugQLSnapshot ctx.Err() error
	cancCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := d.StreamTugQLSnapshot(cancCtx, q, func(Record) error { return nil }); err == nil {
		t.Fatal("expected error for cancelled ctx in StreamTugQLSnapshot")
	}

	// StreamTugQLSnapshot emit error
	if err := d.StreamTugQLSnapshot(ctx, q, func(Record) error { return errors.New("emit fail") }); err == nil {
		t.Fatal("expected emit error in StreamTugQLSnapshot")
	}

	// StreamTugQLQuery emit == nil
	if err := d.StreamTugQLQuery(ctx, q, nil); err == nil {
		t.Fatal("expected error for nil emit in StreamTugQLQuery")
	}

	// StreamTugQLSnapshot keyless record rebuild
	dKeyless := &Database{
		Manifest: d.Manifest,
		names:    d.names,
		db:       &scriptedQueryDB{reader: &scriptedKeylessReader{rows: 1}},
	}
	var rebuiltSnap Record
	if err := dKeyless.StreamTugQLSnapshot(ctx, q, func(rec Record) error {
		rebuiltSnap = rec
		return nil
	}); err != nil {
		t.Fatalf("StreamTugQLSnapshot keyless: %v", err)
	}
	if rebuiltSnap.Key == nil || rebuiltSnap.Key.ID != "rebuilt1" {
		t.Fatalf("expected rebuilt key in snapshot, got %v", rebuiltSnap.Key)
	}
}

type keylessRecord struct {
	data map[string]any
}

func (r keylessRecord) Key() *record.Key             { return nil }
func (r keylessRecord) Error() error                 { return nil }
func (r keylessRecord) Exists() bool                 { return true }
func (r keylessRecord) SetError(error) record.Record { return r }
func (r keylessRecord) Data() any                    { return r.data }
func (r keylessRecord) HasChanged() bool             { return false }
func (r keylessRecord) MarkAsChanged()               {}

type scriptedKeylessReader struct {
	rows int
}

func (r *scriptedKeylessReader) Next() (record.Record, error) {
	if r.rows > 0 {
		r.rows--
		return keylessRecord{data: map[string]any{"id": "rebuilt1", "name": "Ada"}}, nil
	}
	return nil, io.EOF
}
func (r *scriptedKeylessReader) Cursor() (string, error) { return "", nil }
func (r *scriptedKeylessReader) Close() error            { return nil }

func TestExecuteDalQueryStreamOnBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	q, _, err := ParseTugQL([]byte("from: {name: customers}\n"))
	if err != nil {
		t.Fatal(err)
	}

	d := &Database{
		Manifest: &manifest.Manifest{
			Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
			Storage:  manifest.Storage{Engine: "sqlite"},
			Schemas: &schema.Schemas{
				Collections: map[string]schema.Collection{
					"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
				},
			},
		},
		names: collectionNames{
			canonical: map[string]string{"customers": "customers"},
			spellings: map[string][]string{"customers": {"customers"}},
		},
	}

	// 1. reader == nil
	dNilReader := &Database{
		Manifest: d.Manifest,
		names:    d.names,
		db:       &scriptedQueryDB{reader: nil},
	}
	if err := dNilReader.executeDalQueryStreamOn(ctx, ctx, dNilReader.db, q, "customers", false, func(Record) error { return nil }); err == nil {
		t.Fatal("expected error for nil reader")
	}

	// 2. reader.Close() returns error
	dCloseErr := &Database{
		Manifest: d.Manifest,
		names:    d.names,
		db:       &scriptedQueryDB{reader: &scriptedReader{closeErr: errors.New("close fail")}},
	}
	if err := dCloseErr.executeDalQueryStreamOn(ctx, ctx, dCloseErr.db, q, "customers", false, func(Record) error { return nil }); err == nil {
		t.Fatal("expected error from reader.Close()")
	}

	// 3. Key rebuilt from data["id"]
	dKeyless := &Database{
		Manifest: d.Manifest,
		names:    d.names,
		db:       &scriptedQueryDB{reader: &scriptedKeylessReader{rows: 1}},
	}
	var rebuilt Record
	err = dKeyless.executeDalQueryStreamOn(ctx, ctx, dKeyless.db, q, "customers", false, func(rec Record) error {
		rebuilt = rec
		return nil
	})
	if err != nil {
		t.Fatalf("executeDalQueryStreamOn keyless: %v", err)
	}
	if rebuilt.Key == nil || rebuilt.Key.ID != "rebuilt1" {
		t.Fatalf("expected rebuilt key with id rebuilt1, got %v", rebuilt.Key)
	}

	// 4. json.Marshal error (unmarshalable value)
	dBadData := &Database{
		Manifest: d.Manifest,
		names:    d.names,
		db:       &scriptedQueryDB{reader: &scriptedReaderWithFunc{}},
	}
	if err := dBadData.executeDalQueryStreamOn(ctx, ctx, dBadData.db, q, "customers", false, func(Record) error { return nil }); err == nil {
		t.Fatal("expected json.Marshal error")
	}

	// 5. emit error
	dEmitErr := &Database{
		Manifest: d.Manifest,
		names:    d.names,
		db:       &scriptedQueryDB{reader: &scriptedReader{rows: 1}},
	}
	if err := dEmitErr.executeDalQueryStreamOn(ctx, ctx, dEmitErr.db, q, "customers", false, func(Record) error { return errors.New("emit fail") }); err == nil {
		t.Fatal("expected emit error")
	}

	// 6. Result too large
	bigData := map[string]any{"big": strings.Repeat("x", ResultBufferBytes+100)}
	dBig := &Database{
		Manifest: d.Manifest,
		names:    d.names,
		db:       &scriptedQueryDB{reader: &scriptedReaderBig{data: bigData}},
	}
	if err := dBig.executeDalQueryStreamOn(ctx, ctx, dBig.db, q, "customers", false, func(Record) error { return nil }); !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("expected ErrResultTooLarge, got %v", err)
	}

	// 7. Context cancelled error
	cancCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := d.executeDalQueryStreamOn(cancCtx, cancCtx, &scriptedQueryDB{reader: &scriptedReader{rows: 1}}, q, "customers", false, func(Record) error { return nil }); err == nil {
		t.Fatal("expected error for cancelled ctx in executeDalQueryStreamOn")
	}
}

type scriptedReaderWithFunc struct{}

func (scriptedReaderWithFunc) Next() (record.Record, error) {
	return record.NewRecordWithData(record.NewKeyWithID("customers", "c1"), map[string]any{"bad": func() {}}), nil
}
func (scriptedReaderWithFunc) Cursor() (string, error) { return "", nil }
func (scriptedReaderWithFunc) Close() error            { return nil }

type scriptedReaderBig struct {
	data map[string]any
	done bool
}

func (r *scriptedReaderBig) Next() (record.Record, error) {
	if !r.done {
		r.done = true
		return record.NewRecordWithData(record.NewKeyWithID("customers", "c1"), r.data), nil
	}
	return nil, io.EOF
}
func (r *scriptedReaderBig) Cursor() (string, error) { return "", nil }
func (r *scriptedReaderBig) Close() error            { return nil }

func TestIncrementAtPathIntermediateNonObject(t *testing.T) {
	t.Parallel()
	data := map[string]any{"a": "scalar"}
	if err := incrementAtPath(data, []string{"a", "b"}, 1); err == nil {
		t.Fatal("expected error for non-object intermediate on incrementAtPath")
	}
}
