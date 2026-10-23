package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"gopkg.in/yaml.v3"
)

// Helper mock types

type mockSnapshotTempFile struct {
	writeErr error
	closeErr error
	name     string
	written  []byte
}

func (m *mockSnapshotTempFile) Write(p []byte) (int, error) {
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	m.written = append(m.written, p...)
	return len(p), nil
}

func (m *mockSnapshotTempFile) Close() error {
	return m.closeErr
}

type oneRecordReader struct {
	done bool
}

func (r *oneRecordReader) Next() (record.Record, error) {
	if r.done {
		return nil, io.EOF
	}
	r.done = true
	return record.NewRecordWithData(record.NewKeyWithID("customers", "1"), map[string]any{"name": "Alice"}), nil
}

func (r *oneRecordReader) Cursor() (string, error) { return "", nil }
func (r *oneRecordReader) Close() error            { return nil }

type oneRecordDriver struct {
	dal.DB
}

func (oneRecordDriver) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return &oneRecordReader{}, nil
}

func (m *mockSnapshotTempFile) Name() string {
	if m.name != "" {
		return m.name
	}
	return "mock-snapshot-temp"
}

type failDeadlineWriter struct {
	*httptest.ResponseRecorder
}

func (failDeadlineWriter) SetWriteDeadline(time.Time) error {
	return errors.New("deadline failed")
}

func TestPrepareSnapshotDirCoverage(t *testing.T) {
	// Not parallel: modifies package-level snapshotMkdirAll, snapshotLstat, readSnapshotDir
	oldMkdirAll := snapshotMkdirAll
	oldLstat := snapshotLstat
	oldReadDir := readSnapshotDir
	defer func() {
		snapshotMkdirAll = oldMkdirAll
		snapshotLstat = oldLstat
		readSnapshotDir = oldReadDir
	}()

	// 1. snapshotMkdirAll error
	snapshotMkdirAll = func(path string, perm os.FileMode) error {
		return errors.New("mkdir failed")
	}
	if _, err := prepareSnapshotDir(); err == nil {
		t.Fatal("expected error on mkdirAll failure")
	}
	snapshotMkdirAll = oldMkdirAll

	// 2. snapshotLstat error
	snapshotLstat = func(name string) (os.FileInfo, error) {
		return nil, errors.New("lstat failed")
	}
	if _, err := prepareSnapshotDir(); err == nil {
		t.Fatal("expected error on lstat failure")
	}
	snapshotLstat = oldLstat

	// 3. readSnapshotDir error
	readSnapshotDir = func(name string) ([]os.DirEntry, error) {
		return nil, errors.New("readdir failed")
	}
	if _, err := prepareSnapshotDir(); err == nil {
		t.Fatal("expected error on readDir failure")
	}
	readSnapshotDir = oldReadDir

	// 4. Successful call and cleanup of expired file
	dir, err := prepareSnapshotDir()
	if err != nil {
		t.Fatalf("prepareSnapshotDir failed: %v", err)
	}

	// Create an expired snapshot file
	oldFile := filepath.Join(dir, "snapshot-expired-test")
	if err := os.WriteFile(oldFile, []byte("data"), 0o600); err == nil {
		oldTime := time.Now().Add(-10 * time.Minute)
		_ = os.Chtimes(oldFile, oldTime, oldTime)
		// Call prepareSnapshotDir again: should remove oldFile
		_, _ = prepareSnapshotDir()
		if _, statErr := os.Stat(oldFile); !os.IsNotExist(statErr) {
			_ = os.Remove(oldFile)
		}
	}
}

func TestSpoolTugQLCoverage(t *testing.T) {
	// Not parallel: modifies package-level createSnapshotTemp, marshalSnapshotRow
	oldCreate := createSnapshotTemp
	oldMarshal := marshalSnapshotRow
	defer func() {
		createSnapshotTemp = oldCreate
		marshalSnapshotRow = oldMarshal
	}()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "spool_db", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}

	query, _, err := classifyTugQLDocument([]byte("from: {name: customers}\n"))
	if err != nil {
		t.Fatal(err)
	}

	// 1. db.NoRetention() returns errRetentionNotAuthorized
	if _, _, err := spoolTugQL(context.Background(), db, query, t.TempDir(), DefaultSnapshotLimits(), 0); !errors.Is(err, errRetentionNotAuthorized) {
		t.Fatalf("expected errRetentionNotAuthorized, got %v", err)
	}

	// Create a database with retention and oneRecordDriver
	mRet := &manifest.Manifest{
		Database: manifest.Database{ID: "live", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	dbRet, err := core.Open(mRet, oneRecordDriver{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}

	// 2. createSnapshotTemp fails
	createSnapshotTemp = func(dir string) (snapshotTempFile, error) {
		return nil, errors.New("temp file failed")
	}
	if _, _, err := spoolTugQL(context.Background(), dbRet, query, t.TempDir(), DefaultSnapshotLimits(), 0); err == nil {
		t.Fatal("expected error on temp file failure")
	}

	// 3. createSnapshotTemp returns mock file whose Close fails
	createSnapshotTemp = func(dir string) (snapshotTempFile, error) {
		return &mockSnapshotTempFile{closeErr: errors.New("close failed")}, nil
	}
	if _, _, err := spoolTugQL(context.Background(), dbRet, query, t.TempDir(), DefaultSnapshotLimits(), 0); err == nil {
		t.Fatal("expected error on close failure")
	}
	createSnapshotTemp = oldCreate

	// 4. marshalSnapshotRow error
	marshalSnapshotRow = func(v any) ([]byte, error) {
		return nil, errors.New("marshal error")
	}
	if _, _, err := spoolTugQL(context.Background(), dbRet, query, t.TempDir(), DefaultSnapshotLimits(), 0); err == nil {
		t.Fatal("expected error on marshal error")
	}
	marshalSnapshotRow = oldMarshal

	// 5. Row exceeds page size limit
	limits := DefaultSnapshotLimits()
	if _, _, err := spoolTugQL(context.Background(), dbRet, query, t.TempDir(), limits, maxPageBytes); err == nil {
		t.Fatal("expected error on row budget exceeded")
	}

	// 6. Limit rows reached
	limitsRow0 := SnapshotLimits{Rows: 0, Bytes: 1000000, Slots: 10}
	if _, _, err := spoolTugQL(context.Background(), dbRet, query, t.TempDir(), limitsRow0, 0); err == nil {
		t.Fatal("expected error on row limit exceeded")
	}

	// 7. Limit bytes reached
	limitsByte0 := SnapshotLimits{Rows: 100, Bytes: 1, Slots: 10}
	if _, _, err := spoolTugQL(context.Background(), dbRet, query, t.TempDir(), limitsByte0, 0); err == nil {
		t.Fatal("expected error on byte limit exceeded")
	}

	// 8. Temp file Write fails
	createSnapshotTemp = func(dir string) (snapshotTempFile, error) {
		return &mockSnapshotTempFile{writeErr: errors.New("write failed")}, nil
	}
	if _, _, err := spoolTugQL(context.Background(), dbRet, query, t.TempDir(), DefaultSnapshotLimits(), 0); err == nil {
		t.Fatal("expected error on write failure")
	}
	createSnapshotTemp = oldCreate

	// 9. Successful spool
	path, size, err := spoolTugQL(context.Background(), dbRet, query, t.TempDir(), DefaultSnapshotLimits(), 0)
	if err != nil {
		t.Fatalf("spoolTugQL failed: %v", err)
	}
	if size == 0 {
		t.Fatal("expected non-zero size")
	}
	_ = os.Remove(path)
}

func TestServeSnapshotPageAndCloseCoverage(t *testing.T) {
	// Not parallel: modifies package-level seekSnapshotFile, removeSnapshotFile
	oldSeek := seekSnapshotFile
	oldRemove := removeSnapshotFile
	defer func() {
		seekSnapshotFile = oldSeek
		removeSnapshotFile = oldRemove
	}()

	db, _ := retentionFixture(t, "sqlite", "")
	srv := New("test", map[string]*core.Database{"live": db})

	// 1. RefuseRetainedOperation: when db has RetentionNone
	mNoRet := &manifest.Manifest{
		Database: manifest.Database{ID: "no_ret", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	dbNoRet, _ := core.Open(mNoRet, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	recNoRet := httptest.NewRecorder()
	srv.serveSnapshotPage(recNoRet, "tok", dbNoRet, [32]byte{}, [32]byte{}, 10)
	if recNoRet.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for refuseRetainedOperation, got %d", recNoRet.Code)
	}

	recCloseNoRet := httptest.NewRecorder()
	srv.closeSnapshot(recCloseNoRet, "tok", dbNoRet, [32]byte{}, [32]byte{}, 10)
	if recCloseNoRet.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for closeSnapshot refuseRetainedOperation, got %d", recCloseNoRet.Code)
	}

	// 2. Token invalid -> closeSnapshot returns 400
	recCloseBad := httptest.NewRecorder()
	srv.closeSnapshot(recCloseBad, "invalid.token", db, [32]byte{}, [32]byte{}, 10)
	if recCloseBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid token in closeSnapshot, got %d", recCloseBad.Code)
	}

	// Create a real temp file with one row for snapshot tests
	tmpFile, err := os.CreateTemp(t.TempDir(), "snap-*")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tmpFile.WriteString("{\"key\":\"customers/1\",\"data\":{\"name\":\"Alice\"}}\n")
	_ = tmpFile.Close()

	queryHash := [32]byte{1, 2, 3}
	actorHash := [32]byte{4, 5, 6}
	snapID := "snap1"
	snap := &querySnapshot{
		id:        snapID,
		db:        db,
		path:      tmpFile.Name(),
		bytes:     49,
		expiresAt: time.Now().Add(5 * time.Minute),
		queryHash: queryHash,
		actorHash: actorHash,
		pageSize:  10,
	}

	srv.snapshotMu.Lock()
	if srv.snapshots == nil {
		srv.snapshots = make(map[string]*querySnapshot)
	}
	srv.snapshots[snapID] = snap
	srv.snapshotSlots++
	srv.snapshotMu.Unlock()

	validToken := srv.pageToken(snapID, 0, db.ID(), queryHash, actorHash, 10)

	// 3. Mismatched database or credentials in closeSnapshot
	recCloseMismatch := httptest.NewRecorder()
	srv.closeSnapshot(recCloseMismatch, validToken, db, [32]byte{9, 9, 9}, actorHash, 10)
	if recCloseMismatch.Code != http.StatusBadRequest && recCloseMismatch.Code != http.StatusForbidden {
		t.Fatalf("expected 400 or 403, got %d", recCloseMismatch.Code)
	}

	// 4. serveSnapshotPage with missing snapshot -> 410 Gone
	recGone := httptest.NewRecorder()
	srv.serveSnapshotPage(recGone, "missing.0.sig", db, queryHash, actorHash, 10)
	if recGone.Code != http.StatusGone {
		t.Fatalf("expected 410 Gone for missing snapshot, got %d", recGone.Code)
	}

	// 5. serveSnapshotPage with expired snapshot -> 410 Gone
	snapExpired := &querySnapshot{
		id:        "expired",
		db:        db,
		path:      tmpFile.Name(),
		bytes:     49,
		expiresAt: time.Now().Add(-1 * time.Minute),
		queryHash: queryHash,
		actorHash: actorHash,
		pageSize:  10,
	}
	srv.snapshotMu.Lock()
	srv.snapshots["expired"] = snapExpired
	srv.snapshotMu.Unlock()

	recExpired := httptest.NewRecorder()
	srv.serveSnapshotPage(recExpired, "expired.0.sig", db, queryHash, actorHash, 10)
	if recExpired.Code != http.StatusGone {
		t.Fatalf("expected 410 Gone for expired snapshot, got %d", recExpired.Code)
	}

	// 6. serveSnapshotPage with seekSnapshotFile failure -> 410 Gone
	seekSnapshotFile = func(f *os.File, offset int64, whence int) (int64, error) {
		return 0, errors.New("seek failed")
	}
	recSeekErr := httptest.NewRecorder()
	srv.serveSnapshotPage(recSeekErr, validToken, db, queryHash, actorHash, 10)
	if recSeekErr.Code != http.StatusGone {
		t.Fatalf("expected 410 Gone for seek failure, got %d", recSeekErr.Code)
	}
	seekSnapshotFile = oldSeek

	// 7. removeSnapshotLocked with os.Remove failure
	removeSnapshotFile = func(path string) error {
		return errors.New("permission denied")
	}
	srv.snapshotMu.Lock()
	srv.removeSnapshotLocked(&querySnapshot{id: "err_snap", path: "/tmp/nonexistent-err-snap"})
	srv.snapshotMu.Unlock()
	removeSnapshotFile = oldRemove

	// 8. CloseSnapshots, expireSnapshotsForDB, releaseSnapshotSlot
	srv.CloseSnapshots()
	srv.expireSnapshotsForDB(db)
	srv.snapshotSlots = 5
	srv.releaseSnapshotSlot()
	if srv.snapshotSlots != 4 {
		t.Fatalf("expected 4 slots, got %d", srv.snapshotSlots)
	}
}

type emptyRecordsReader struct{}

func (emptyRecordsReader) Next() (record.Record, error) { return nil, io.EOF }
func (emptyRecordsReader) Close() error                 { return nil }
func (emptyRecordsReader) Cursor() (string, error)      { return "", nil }

type emptyQueryDB struct{ dal.DB }

func (emptyQueryDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return emptyRecordsReader{}, nil
}

func TestRecordsAndBatchCoverage(t *testing.T) {
	// Not parallel: modifies finishRecordStream
	oldFinish := finishRecordStream
	defer func() { finishRecordStream = oldFinish }()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "rec_db", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"rec_db": db})

	// 1. handleRecord MethodNotAllowed (e.g. OPTIONS) -> 405
	recOpt := httptest.NewRecorder()
	reqOpt := httptest.NewRequest(http.MethodOptions, "/v1/databases/rec_db/records/users/1", nil)
	reqOpt.SetPathValue("db", "rec_db")
	srv.handleRecord(recOpt, reqOpt)
	if recOpt.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recOpt.Code)
	}

	// 2. handleRecord PUT with invalid JSON -> 400
	recPutBad := httptest.NewRecorder()
	reqPutBad := httptest.NewRequest(http.MethodPut, "/v1/databases/rec_db/records/users/1", strings.NewReader("{invalid"))
	reqPutBad.SetPathValue("db", "rec_db")
	srv.handleRecord(recPutBad, reqPutBad)
	if recPutBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad PUT body, got %d", recPutBad.Code)
	}

	// 3. handleRecord PUT with nil data -> 400
	recPutNil := httptest.NewRecorder()
	reqPutNil := httptest.NewRequest(http.MethodPut, "/v1/databases/rec_db/records/users/1", strings.NewReader(`{"other":1}`))
	reqPutNil.SetPathValue("db", "rec_db")
	srv.handleRecord(recPutNil, reqPutNil)
	if recPutNil.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for PUT without data, got %d", recPutNil.Code)
	}

	// 4. handleRecord PATCH with invalid JSON -> 400
	recPatchBad := httptest.NewRecorder()
	reqPatchBad := httptest.NewRequest(http.MethodPatch, "/v1/databases/rec_db/records/users/1", strings.NewReader("{invalid"))
	reqPatchBad.SetPathValue("db", "rec_db")
	srv.handleRecord(recPatchBad, reqPatchBad)
	if recPatchBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad PATCH body, got %d", recPatchBad.Code)
	}

	// 5. handleRecord PATCH with empty updates -> 400
	recPatchEmpty := httptest.NewRecorder()
	reqPatchEmpty := httptest.NewRequest(http.MethodPatch, "/v1/databases/rec_db/records/users/1", strings.NewReader(`{"updates":[]}`))
	reqPatchEmpty.SetPathValue("db", "rec_db")
	srv.handleRecord(recPatchEmpty, reqPatchEmpty)
	if recPatchEmpty.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty updates, got %d", recPatchEmpty.Code)
	}

	// 6. handleBatch with bad JSON -> 400
	recBatchBad := httptest.NewRecorder()
	reqBatchBad := httptest.NewRequest(http.MethodPost, "/v1/databases/rec_db/batch", strings.NewReader("{invalid"))
	reqBatchBad.SetPathValue("db", "rec_db")
	srv.handleBatch(recBatchBad, reqBatchBad)
	if recBatchBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad batch JSON, got %d", recBatchBad.Code)
	}

	// 7. decodeQuery with invalid JSON (line 316)
	q := &core.Query{}
	reqGetBad := httptest.NewRequest(http.MethodGet, "/v1/databases/rec_db/query?q={invalid", nil)
	if err := decodeQuery(httptest.NewRecorder(), reqGetBad, q); err == nil {
		t.Fatal("expected error on invalid JSON query")
	}

	// 8. executeQuery with streamErr (via failDeadlineWriter)
	recDeadline := failDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	reqDeadline := httptest.NewRequest(http.MethodPost, "/v1/databases/rec_db/query", strings.NewReader(`{"collection":"users"}`))
	reqDeadline.SetPathValue("db", "rec_db")
	srv.executeQuery(recDeadline, reqDeadline, db, core.Query{Collection: "users"})
	if recDeadline.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for streamErr, got %d", recDeadline.Code)
	}

	// 9. finishRecordStream error
	finishRecordStream = func(s *jsonRowStream, footer map[string]any) error {
		return errors.New("finish stream error")
	}
	recFinishErr := httptest.NewRecorder()
	reqFinishErr := httptest.NewRequest(http.MethodPost, "/v1/databases/rec_db/query", strings.NewReader(`{"collection":"users"}`))
	reqFinishErr.SetPathValue("db", "rec_db")
	srv.executeQuery(recFinishErr, reqFinishErr, db, core.Query{Collection: "users"})
	if recFinishErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for finishRecordStream error, got %d", recFinishErr.Code)
	}
	finishRecordStream = oldFinish
}

func TestTugQLAndStreamCoverage(t *testing.T) {
	// Not parallel: modifies finishTugQLStream, finishRelationalStream, flushJSONStream
	oldTugQLFinish := finishTugQLStream
	oldRelFinish := finishRelationalStream
	oldFlush := flushJSONStream
	defer func() {
		finishTugQLStream = oldTugQLFinish
		finishRelationalStream = oldRelFinish
		flushJSONStream = oldFlush
	}()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "tugql_cov_db", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"tugql_cov_db": db})

	// 1. handleTugQL with streamErr via failDeadlineWriter
	recDeadline := failDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	reqDeadline := httptest.NewRequest(http.MethodPost, "/v1/databases/tugql_cov_db/tugql", strings.NewReader("from: {name: users}\n"))
	reqDeadline.SetPathValue("db", "tugql_cov_db")
	srv.handleTugQL(recDeadline, reqDeadline)
	if recDeadline.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on streamErr in handleTugQL, got %d", recDeadline.Code)
	}

	// 2. handleTugQL with finishTugQLStream error
	finishTugQLStream = func(s *jsonRowStream, footer map[string]any) error {
		return errors.New("finish tugql error")
	}
	recFinishErr := httptest.NewRecorder()
	reqFinishErr := httptest.NewRequest(http.MethodPost, "/v1/databases/tugql_cov_db/tugql", strings.NewReader("from: {name: users}\n"))
	reqFinishErr.SetPathValue("db", "tugql_cov_db")
	srv.handleTugQL(recFinishErr, reqFinishErr)
	if recFinishErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on finishTugQLStream error, got %d", recFinishErr.Code)
	}
	finishTugQLStream = oldTugQLFinish

	// 3. tugql URL query errors in parseTugQLURL
	urlTests := []struct {
		url     string
		wantErr string
	}{
		{"/v1/databases/tugql_cov_db/tugql?unknown=1", "unsupported TugQL URL parameter"},
		{"/v1/databases/tugql_cov_db/tugql?q=", "TugQL URL requires exactly one nonempty q parameter"},
		{"/v1/databases/tugql_cov_db/tugql?q=test&parameters=", "TugQL URL requires one JSON parameters object"},
		{"/v1/databases/tugql_cov_db/tugql?q=test&parameters=[1]", "TugQL URL parameters must be a JSON object"},
	}
	for _, tc := range urlTests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.url, nil)
		req.SetPathValue("db", "tugql_cov_db")
		srv.handleTugQL(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d: %s", tc.url, rec.Code, rec.Body.String())
		}
	}

	// 4. bindTugQLParameters errors
	paramTests := []struct {
		body    string
		wantErr string
	}{
		{`{"query":"from: {name: users}\nwhere: {param: p}\n","parameters":{"p":"invalid JSON}`, "invalid TugQL JSON"},
		{`{"query":"from: {name: users}\nwhere: {param: p}\n","parameters":{"p":null}}`, "cannot be null"},
		{`{"query":"from: {name: users}\nwhere: {param: p}\n","parameters":{"p":[null]}}`, "must contain only non-null scalars"},
		{`{"query":"from: {name: users}\nwhere: {param: p}\n","parameters":{"p":{"nested":1}}}`, "must be a scalar or array"},
	}
	for _, tc := range paramTests {
		_, err := bindTugQLParameters([]byte(tc.body))
		if err == nil {
			t.Fatalf("expected error for %s, got nil", tc.body)
		}
	}

	// 5. Array with > 1000 items in bindTugQLParameters
	bigArr := make([]any, 1001)
	for i := range bigArr {
		bigArr[i] = i
	}
	bigArrJSON, _ := json.Marshal(bigArr)
	body1001 := fmt.Sprintf(`{"query":"from: {name: users}\nwhere: {param: p}\n","parameters":{"p":%s}}`, string(bigArrJSON))
	if _, err := bindTugQLParameters([]byte(body1001)); err == nil {
		t.Fatal("expected error for >1000 parameters")
	}

	// 6. encodeTugQLParamNode error
	oldEncode := encodeTugQLParamNode
	encodeTugQLParamNode = func(node *yaml.Node, value any) error {
		return errors.New("encode error")
	}
	if _, err := bindTugQLParameters([]byte(`{"query":"from: {name: users}\nwhere: {param: p}\n","parameters":{"p":"valid"}}`)); err == nil {
		t.Fatal("expected error when encodeTugQLParamNode fails")
	}
	encodeTugQLParamNode = oldEncode

	// 7. Stream JSON coverage
	// FinishFailure errors
	recStream := httptest.NewRecorder()
	stream, _, _ := newJSONRowStream(recStream, time.Second, false)
	if err := stream.FinishFailure(map[string]any{"msg": "err"}); err == nil {
		t.Fatal("expected error when errorCompletion is false")
	}

	// flushJSONStream error
	flushJSONStream = func(w http.ResponseWriter) error {
		return errors.New("flush failed")
	}
	if err := stream.flush(); err == nil {
		t.Fatal("expected error on flush failure")
	}
	flushJSONStream = oldFlush
}

func TestRelationalTugQLStreamFinishCoverage(t *testing.T) {
	// Not parallel: modifies finishRelationalStream
	oldRelFinish := finishRelationalStream
	defer func() { finishRelationalStream = oldRelFinish }()

	service, _ := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())

	// 1. streamErr via failDeadlineWriter on relational TugQL
	relDoc := "from: {database: alpha, name: orders}\n"
	recDeadline := failDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	reqDeadline := httptest.NewRequest(http.MethodPost, "/v1/tugql", strings.NewReader(relDoc))
	service.handleCrossDatabaseTugQL(recDeadline, reqDeadline)
	if recDeadline.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on relational streamErr, got %d: %s", recDeadline.Code, recDeadline.Body.String())
	}

	// 2. finishRelationalStream error when stream.started is false
	finishRelationalStream = func(s *jsonRowStream, footer map[string]any) error {
		return errors.New("relational finish error")
	}
	recRelFinishErr := httptest.NewRecorder()
	reqRelFinishErr := httptest.NewRequest(http.MethodPost, "/v1/tugql", strings.NewReader(relDoc))
	service.handleCrossDatabaseTugQL(recRelFinishErr, reqRelFinishErr)
	if recRelFinishErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on finishRelationalStream error, got %d: %s", recRelFinishErr.Code, recRelFinishErr.Body.String())
	}
	finishRelationalStream = oldRelFinish
}
