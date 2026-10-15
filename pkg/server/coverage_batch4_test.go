package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// --- 1. postgres_wire.go tests ---

func TestPostgresWireCoverage(t *testing.T) {
	t.Parallel()

	// 1. nativePostgresJSONSafeIntegers: nil map
	if nativePostgresJSONSafeIntegers(nil) != nil {
		t.Fatal("expected nil for nil input")
	}

	// 2. nativePostgresJSONSafeValue: int64, uint64, json.Number, slices
	vals := map[string]any{
		"safeInt":      int64(42),
		"unsafeIntPos": int64(9007199254740993),
		"unsafeIntNeg": int64(-9007199254740993),
		"safeUint":     uint64(42),
		"unsafeUint":   uint64(9007199254740993),
		"numSafe":      json.Number("42"),
		"numUnsafePos": json.Number("9007199254740993"),
		"numNonInt":    json.Number("3.14"),
		"numHuge":      json.Number("99999999999999999999999999999999999999999999"),
		"raw":          json.RawMessage(`{"nested": 1}`),
		"bytes":        []byte("hello"),
		"obj":          map[string]any{"nested": int64(100)},
		"sliceAny":     []any{int64(9007199254740993)},
		"sliceInt":     []int64{int64(9007199254740993)},
		"sliceUint8":   []uint8{1, 2, 3},
		"str":          "string",
	}
	res := nativePostgresJSONSafeIntegers(vals)
	if res["unsafeIntPos"] != "9007199254740993" || res["unsafeIntNeg"] != "-9007199254740993" {
		t.Fatalf("int64 unsafe formatting failed: %+v", res)
	}
	if res["unsafeUint"] != "9007199254740993" {
		t.Fatalf("uint64 unsafe formatting failed: %+v", res)
	}
	if res["numUnsafePos"] != "9007199254740993" {
		t.Fatalf("json.Number unsafe formatting failed: %+v", res)
	}

	// 3. nativePostgresIntegerLexeme
	for _, tc := range []struct {
		s    string
		want bool
	}{
		{"", false},
		{"-", false},
		{"123", true},
		{"-123", true},
		{"12a3", false},
	} {
		if got := nativePostgresIntegerLexeme(tc.s); got != tc.want {
			t.Fatalf("nativePostgresIntegerLexeme(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}

	// 4. nativePostgresJSONValues: temporal fields and json fields
	now := time.Date(2026, 10, 14, 15, 4, 5, 123456789, time.UTC)
	fields := map[string]any{
		"d":         now,
		"t_wo":      now,
		"t_w":       now,
		"ts_wo":     now,
		"ts_w":      now,
		"d_str":     now.Format(time.RFC3339Nano),
		"t_wo_str":  now.Format(time.RFC3339Nano),
		"t_w_str":   now.Format(time.RFC3339Nano),
		"ts_wo_str": now.Format(time.RFC3339Nano),
		"ts_w_str":  now.Format(time.RFC3339Nano),
		"json_str":  `{"key": "val"}`,
		"json_b":    []byte(`[1, 2, 3]`),
		"bad_time":  "not-a-time",
		"other":     123,
	}
	types := map[string]string{
		"d":         "date",
		"t_wo":      "time without time zone",
		"t_w":       "time with time zone",
		"ts_wo":     "timestamp without time zone",
		"ts_w":      "timestamp with time zone",
		"d_str":     "date",
		"t_wo_str":  "time without time zone",
		"t_w_str":   "time with time zone",
		"ts_wo_str": "timestamp without time zone",
		"ts_w_str":  "timestamp with time zone",
		"json_str":  "json",
		"json_b":    "jsonb",
		"bad_time":  "date",
		"other":     "int",
	}
	outVals := nativePostgresJSONValues(fields, types)
	if outVals["d"] != "2026-10-14" || outVals["d_str"] != "2026-10-14" {
		t.Fatalf("date formatting failed: %+v", outVals)
	}

	// 5. nativePostgresStringMap
	if _, ok := nativePostgresStringMap(123); ok {
		t.Fatal("expected false for int")
	}
	if _, ok := nativePostgresStringMap(map[int]string{1: "a"}); ok {
		t.Fatal("expected false for map with int key")
	}
	mStr, ok := nativePostgresStringMap(map[string]int{"a": 1})
	if !ok || mStr["a"] != 1 {
		t.Fatalf("expected map[string]any with a=1, got %+v", mStr)
	}

	// 6. nativePostgresJSONValue with map and non-map
	_ = nativePostgresJSONValue(map[string]any{"a": 1}, types)
	_ = nativePostgresJSONValue(123, types)

	// 7. nativePostgresFieldTypes with nil
	if nativePostgresFieldTypes(nil) != nil {
		t.Fatal("expected nil for nil collection")
	}

	// 8. nativePostgresProjectionTypes with nil query
	if nativePostgresProjectionTypes(nil, nil, nil) != nil {
		t.Fatal("expected nil for nil query")
	}

	// 9. nativePostgresExpressionName
	if nativePostgresExpressionName(dal.Field("id")) != "id" {
		t.Fatal("expected id")
	}
	if nativePostgresExpressionName(dal.Constant{Value: 1}) != "" {
		t.Fatal("expected empty for constant")
	}

	// 10. nativePostgresCollectionRefTypes
	if nativePostgresCollectionRefTypes(dal.CollectionRef{}, nil, nil) != nil {
		t.Fatal("expected nil for nil endpoint")
	}
}

// --- 2. dtql.go dtqlFromURL and bindDTQLParameters ---

func TestDTQLURLAndParameters(t *testing.T) {
	t.Parallel()

	// 1. dtqlFromURL: URL too long
	uLong, _ := url.Parse("/dtql?" + strings.Repeat("q=1&", 3000))
	if _, err := dtqlFromURL(uLong); !errors.Is(err, errDTQLURLTooLong) {
		t.Fatalf("expected errDTQLURLTooLong, got %v", err)
	}

	// 2. dtqlFromURL: unsupported param
	uBadParam, _ := url.Parse("/dtql?unknown=1")
	if _, err := dtqlFromURL(uBadParam); err == nil || !strings.Contains(err.Error(), "unsupported DTQL URL parameter") {
		t.Fatalf("expected unsupported parameter error, got %v", err)
	}

	// 3. dtqlFromURL: empty q
	uEmptyQ, _ := url.Parse("/dtql?q=")
	if _, err := dtqlFromURL(uEmptyQ); err == nil || !strings.Contains(err.Error(), "exactly one nonempty q parameter") {
		t.Fatalf("expected nonempty q error, got %v", err)
	}

	// 4. dtqlFromURL: invalid parameters
	uBadParams, _ := url.Parse("/dtql?q=from+orders&parameters=invalid-json")
	if _, err := dtqlFromURL(uBadParams); err == nil || !strings.Contains(err.Error(), "parameters must be a JSON object") {
		t.Fatalf("expected parameters must be JSON object error, got %v", err)
	}

	// 5. bindDTQLParameters branches
	// Empty query
	if _, err := bindDTQLParameters([]byte(`{"query": " "}`)); err == nil || !strings.Contains(err.Error(), "query must contain DTQL YAML") {
		t.Fatalf("expected empty query error, got %v", err)
	}
	// Extra JSON object
	if _, err := bindDTQLParameters([]byte(`{"query":"from orders"}{"extra":1}`)); err == nil || !strings.Contains(err.Error(), "must contain one object") {
		t.Fatalf("expected one object error, got %v", err)
	}
	// Too many parameters (> 64)
	manyParams := map[string]json.RawMessage{}
	for i := 0; i < 65; i++ {
		manyParams[fmt.Sprintf("p%d", i)] = json.RawMessage(`1`)
	}
	bodyMany, _ := json.Marshal(map[string]any{"query": "from orders", "parameters": manyParams})
	if _, err := bindDTQLParameters(bodyMany); err == nil || !strings.Contains(err.Error(), "too many DTQL parameters") {
		t.Fatalf("expected too many parameters error, got %v", err)
	}
	// Invalid YAML
	if _, err := bindDTQLParameters([]byte(`{"query":"\t\t[invalid: yaml:"}`)); err == nil || !strings.Contains(err.Error(), "invalid DTQL YAML") {
		t.Fatalf("expected invalid DTQL YAML error, got %v", err)
	}
	// Parameter not bound
	if _, err := bindDTQLParameters([]byte(`{"query":"where: {param: missing}"}`)); err == nil || !strings.Contains(err.Error(), "is not bound") {
		t.Fatalf("expected not bound error, got %v", err)
	}
	// Parameter null
	if _, err := bindDTQLParameters([]byte(`{"query":"where: {param: p1}", "parameters":{"p1": null}}`)); err == nil || !strings.Contains(err.Error(), "cannot be null") {
		t.Fatalf("expected cannot be null error, got %v", err)
	}
	// Parameter not scalar/array (e.g. object)
	if _, err := bindDTQLParameters([]byte(`{"query":"where: {param: p1}", "parameters":{"p1": {"nested": 1}}}`)); err == nil || !strings.Contains(err.Error(), "must be a scalar or array") {
		t.Fatalf("expected scalar or array error, got %v", err)
	}
	// Parameter array with null element
	if _, err := bindDTQLParameters([]byte(`{"query":"where: {param: p1}", "parameters":{"p1": [null]}}`)); err == nil || !strings.Contains(err.Error(), "must contain only non-null scalars") {
		t.Fatalf("expected non-null scalars error, got %v", err)
	}
	// Unused parameter
	if _, err := bindDTQLParameters([]byte(`{"query":"from: {name: orders}", "parameters":{"unused": "val"}}`)); err == nil || !strings.Contains(err.Error(), "is not used") {
		t.Fatalf("expected unused parameter error, got %v", err)
	}
	// Valid bound parameter
	validBody := []byte(`{"query":"where: {param: p1}", "parameters":{"p1": "val1"}}`)
	if bound, err := bindDTQLParameters(validBody); err != nil || !strings.Contains(string(bound), "value: val1") {
		t.Fatalf("expected successful bind, got %v (%s)", err, string(bound))
	}
}

// --- 3. dtql_pages.go snapshot tests ---

// Not parallel: mutates readSnapshotRand
func TestSnapshotIdentityRandError(t *testing.T) {
	oldRand := readSnapshotRand
	t.Cleanup(func() { readSnapshotRand = oldRand })
	readSnapshotRand = func(b []byte) (int, error) {
		return 0, errors.New("simulated entropy failure")
	}
	if _, err := newSnapshotIdentity(); err == nil {
		t.Fatal("expected error on entropy failure")
	}
}

func TestDTQLPagesBranches(t *testing.T) {
	t.Parallel()

	srv := New("test", nil)
	srv.snapshotLimits = SnapshotLimits{Slots: 1, Bytes: 1000}

	// 1. reserveSnapshotSlot and releaseSnapshotSlot
	if !srv.reserveSnapshotSlot() {
		t.Fatal("expected slot 1 to succeed")
	}
	if srv.reserveSnapshotSlot() {
		t.Fatal("expected slot 2 to fail due to limit")
	}
	srv.releaseSnapshotSlot()

	// 2. pageOffset with invalid tokens
	var qh, ah [32]byte
	if _, _, ok := srv.pageOffset("bad.token", "db1", qh, ah, 50); ok {
		t.Fatal("expected false for invalid token format")
	}
	if _, _, ok := srv.pageOffset("badid.0.sig", "db1", qh, ah, 50); ok {
		t.Fatal("expected false for non-base64 id")
	}
	if _, _, ok := srv.pageOffset(strings.Repeat("a", 43)+".-5.sig", "db1", qh, ah, 50); ok {
		t.Fatal("expected false for negative offset")
	}

	// 3. closeSnapshot with invalid token
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	recClose := httptest.NewRecorder()
	srv.closeSnapshot(recClose, "bad.token", db, qh, ah, 50)
	if recClose.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad token in closeSnapshot, got %d", recClose.Code)
	}

	// 4. expireSnapshot and expireSnapshotsForDB
	srv.snapshots = make(map[string]*querySnapshot)
	snap := &querySnapshot{id: "snap1", db: db}
	srv.snapshots["snap1"] = snap
	srv.expireSnapshot(snap)
	if srv.snapshots["snap1"] != nil {
		t.Fatal("expected snap1 expired")
	}

	// expireSnapshot with mismatched snap
	srv.expireSnapshot(&querySnapshot{id: "unregistered"})

	// expireSnapshotsForDB
	srv.snapshots["snap2"] = &querySnapshot{id: "snap2", db: db}
	srv.expireSnapshotsForDB(db)
	if srv.snapshots["snap2"] != nil {
		t.Fatal("expected snap2 expired for db")
	}
}

// --- 4. records.go parseRecordKey, handleBatch, decodeQuery ---

func TestRecordsCoverageBranches(t *testing.T) {
	t.Parallel()

	// 1. parseRecordKey: missing /records/ in path
	reqNoRecords := httptest.NewRequest(http.MethodGet, "/v1/databases/db1/query", nil)
	if _, err := parseRecordKey(reqNoRecords); err == nil || !errors.Is(err, core.ErrInvalidKey) {
		t.Fatalf("expected ErrInvalidKey, got %v", err)
	}

	// parseRecordKey: empty after /records/
	reqEmptyKey := httptest.NewRequest(http.MethodGet, "/v1/databases/db1/records/", nil)
	if _, err := parseRecordKey(reqEmptyKey); err == nil || !errors.Is(err, core.ErrInvalidKey) {
		t.Fatalf("expected ErrInvalidKey for empty key, got %v", err)
	}

	// 2. decodeQuery: GET empty q and GET huge q
	reqEmptyQ := httptest.NewRequest(http.MethodGet, "/v1/databases/db1/query?q=", nil)
	var q core.Query
	if err := decodeQuery(httptest.NewRecorder(), reqEmptyQ, &q); err == nil {
		t.Fatal("expected error on empty q")
	}
	reqHugeQ := httptest.NewRequest(http.MethodGet, "/v1/databases/db1/query?q="+strings.Repeat("x", maxQueryRequestBytes+10), nil)
	if err := decodeQuery(httptest.NewRecorder(), reqHugeQ, &q); err == nil {
		t.Fatal("expected error on huge q")
	}

	// 3. handleBatch: empty ops array
	recBatchEmpty := httptest.NewRecorder()
	reqBatchEmpty := httptest.NewRequest(http.MethodPost, "/v1/databases/db1/batch", strings.NewReader(`{"ops":[]}`))
	reqBatchEmpty.SetPathValue("db", "db1")
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvDB := New("test", map[string]*core.Database{"db1": db})
	srvDB.handleBatch(recBatchEmpty, reqBatchEmpty)
	if recBatchEmpty.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty ops, got %d", recBatchEmpty.Code)
	}

	// handleBatch: invalid key
	recBatchBadKey := httptest.NewRecorder()
	reqBatchBadKey := httptest.NewRequest(http.MethodPost, "/v1/databases/db1/batch", strings.NewReader(`{"ops":[{"op":"insert","key":"bad/key/format/missing/pairs"}]}`))
	reqBatchBadKey.SetPathValue("db", "db1")
	srvDB.handleBatch(recBatchBadKey, reqBatchBadKey)
	if recBatchBadKey.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad key, got %d", recBatchBadKey.Code)
	}
}

// --- 5. access.go and access_inspect.go branches ---

func TestAccessAndInspectBranches(t *testing.T) {
	t.Parallel()

	// 1. WithAccessInstanceID panics on invalid ID
	assertPanic := func(fn func()) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic")
			}
		}()
		fn()
	}
	s := &Server{}
	assertPanic(func() { WithAccessInstanceID("")(s) })
	assertPanic(func() { WithAccessInstanceID(strings.Repeat("a", 129))(s) })
	WithAccessInstanceID("instance-1")(s)
	if s.accessInstance != "instance-1" {
		t.Fatalf("expected instance-1, got %s", s.accessInstance)
	}

	// 2. conditionNodes: nil, Left, Right, And, Or
	if conditionNodes(nil) != 0 {
		t.Fatal("expected 0 for nil conditionNodes")
	}
	cond := &access.DocumentCondition{
		Left:  &access.DocumentExpression{Field: "x"},
		Right: &access.DocumentExpression{Field: "y"},
		And:   []access.DocumentCondition{{}},
		Or:    []access.DocumentCondition{{}},
	}
	if n := conditionNodes(cond); n < 5 {
		t.Fatalf("expected at least 5 nodes, got %d", n)
	}

	// 3. boundAuthorization: exceed 1000 blockers / restrictions / 256KB
	res := az.Result{
		Result: az.OutcomeDeny,
		Operations: []az.OperationResult{
			{ID: "op1", Result: az.OutcomeDeny},
			{ID: "op2", Result: az.OutcomeAllow},
		},
		Blockers: make([]az.Blocker, 1001),
	}
	bounded := boundAuthorization(res)
	if !bounded.Coverage.Truncated {
		t.Fatal("expected truncated result")
	}

	// 4. writeProtectedFailure
	for _, tc := range []struct {
		err  error
		code int
	}{
		{access.ErrDataRevisionConflict, 409},
		{context.DeadlineExceeded, 503},
		{errProtectedUnsupported, 422},
		{errors.New("generic"), 503},
	} {
		rec := httptest.NewRecorder()
		writeProtectedFailure(rec, tc.err)
		if rec.Code != tc.code {
			t.Fatalf("writeProtectedFailure(%v) = %d, want %d", tc.err, rec.Code, tc.code)
		}
	}

	// 5. protectedOperation: various actions
	opInsert := api.Operation{ID: "op1", ExecutionClass: az.ExecutionDTQL, Action: "insert", Resource: az.Resource{Table: "orders", RowID: "1"}, Mutation: &api.Mutation{Data: map[string]any{"x": 1}}}
	if _, err := protectedOperation(opInsert); err != nil {
		t.Fatalf("expected insert success, got %v", err)
	}

	opSet := api.Operation{ID: "op1", ExecutionClass: az.ExecutionDTQL, Action: "set", Resource: az.Resource{Table: "orders", RowID: "1"}, Mutation: &api.Mutation{Data: map[string]any{"x": 1}}}
	if _, err := protectedOperation(opSet); err != nil {
		t.Fatalf("expected set success, got %v", err)
	}

	opDelete := api.Operation{ID: "op1", ExecutionClass: az.ExecutionDTQL, Action: "delete", Resource: az.Resource{Table: "orders", RowID: "1"}, Mutation: &api.Mutation{IfDataRevision: "rev1"}}
	if _, err := protectedOperation(opDelete); err != nil {
		t.Fatalf("expected delete success, got %v", err)
	}

	opUpdateBadVal := api.Operation{
		ID:             "op1",
		ExecutionClass: az.ExecutionDTQL,
		Action:         "update",
		Resource:       az.Resource{Table: "orders", RowID: "1"},
		Mutation:       &api.Mutation{Changes: []api.Change{{Path: []string{"name"}, Value: []byte("invalid-json")}}},
	}
	if _, err := protectedOperation(opUpdateBadVal); err == nil {
		t.Fatal("expected error on invalid update change value")
	}

	opUnknown := api.Operation{ID: "op1", ExecutionClass: az.ExecutionDTQL, Action: "unknown", Resource: az.Resource{Table: "orders", RowID: "1"}}
	if _, err := protectedOperation(opUnknown); !errors.Is(err, errProtectedUnsupported) {
		t.Fatalf("expected errProtectedUnsupported, got %v", err)
	}

	// 6. handleProtectedUpdate with nil coordinator
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvDB := New("test", map[string]*core.Database{"db1": db})
	recUpdate := httptest.NewRecorder()
	reqUpdate := httptest.NewRequest(http.MethodPatch, "/v1/databases/db1/records/orders/1", nil)
	srvDB.handleProtectedUpdate(recUpdate, reqUpdate, db, record.NewKeyWithID("orders", "1"))
	if recUpdate.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for uncoordinated protected update, got %d", recUpdate.Code)
	}

	// 7. writeQueryAuthorizationError
	recAuthErr := httptest.NewRecorder()
	reqAuthErr := httptest.NewRequest(http.MethodGet, "/v1/databases/db1/query", nil)
	opRead := api.Operation{ID: "op1", Action: "query", Resource: az.Resource{DatabaseID: "db1", Path: "/orders", Table: "orders"}, ExecutionClass: az.ExecutionDTQL}
	srvDB.writeQueryAuthorizationError(recAuthErr, reqAuthErr, db, opRead, &access.PolicyProviderError{Err: errors.New("provider down")})
	if recAuthErr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for PolicyProviderError, got %d", recAuthErr.Code)
	}

	// With enforcement unsupported
	recAuthUnsupported := httptest.NewRecorder()
	srvDB.writeQueryAuthorizationError(recAuthUnsupported, reqAuthErr, db, opRead, &access.DeniedError{Decision: access.Decision{Allowed: false, Code: access.CodeEnforcementUnsupported}, Decisions: []access.Decision{{Allowed: false, Code: access.CodeEnforcementUnsupported}}})
	if recAuthUnsupported.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for CodeEnforcementUnsupported, got %d", recAuthUnsupported.Code)
	}
}

// --- 6. server.go, connect.go, databases_create.go coverage ---

func TestServerAndConnectBranches(t *testing.T) {
	t.Parallel()

	srv := New("test", nil, WithAuth(&auth.Config{OwnerToken: "owner"}))

	// 1. handleDatabases without owner credentials -> 403
	recDbs := httptest.NewRecorder()
	reqDbs := httptest.NewRequest(http.MethodGet, "/v1/databases", nil)
	srv.handleDatabases(recDbs, reqDbs)
	if recDbs.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unauthenticated handleDatabases, got %d", recDbs.Code)
	}

	// 2. handleDatabase: missing db -> 404
	recDb := httptest.NewRecorder()
	reqDb := httptest.NewRequest(http.MethodGet, "/v1/databases/unknown", nil)
	reqDb.SetPathValue("db", "unknown")
	srv.handleDatabase(recDb, reqDb)
	if recDb.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown db, got %d", recDb.Code)
	}

	// 3. handleInferredSchema: missing db -> 404
	recSchema := httptest.NewRecorder()
	reqSchema := httptest.NewRequest(http.MethodGet, "/v1/databases/unknown/schema/inferred", nil)
	reqSchema.SetPathValue("db", "unknown")
	srv.handleInferredSchema(recSchema, reqSchema)
	if recSchema.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown db inferred schema, got %d", recSchema.Code)
	}

	// 4. handleWellKnown
	recWK := httptest.NewRecorder()
	reqWK := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	srv.handleWellKnown(recWK, reqWK)
	if recWK.Code != http.StatusOK {
		t.Fatalf("expected 200 for well-known, got %d", recWK.Code)
	}

	// 5. UnmountContext
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "unm", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvUnm := New("test", map[string]*core.Database{"unm": db})
	if err := srvUnm.UnmountContext(context.Background(), "unm"); err != nil {
		t.Fatalf("expected successful unmount, got %v", err)
	}
}
