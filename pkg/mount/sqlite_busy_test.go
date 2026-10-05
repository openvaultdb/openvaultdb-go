package mount

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	"github.com/dal-go/dalgo2sqlite"
	"github.com/dal-go/record"
	_ "modernc.org/sqlite"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// A SQLite file takes one writer at a time and refuses a write that meets a reader at
// once with SQLITE_BUSY unless the handle waits for the lock. Every handle the mount
// opens (the one the driver reads and writes with, and the one that supplies the fields
// of a table) waits for up to sqliteBusyLimit, so a write that meets a reader succeeds once
// the reader ends, and a wait that outlives the request ends with the request's own error
// and not with a hang or a lock error.
//
// The tests use real SQLite files. A reader is a transaction of a handle of the test's
// own, held open while the mount is used. The helpers of this file start with
// sqliteBusy so they cannot clash with the others of the package.

// sqliteBusyLimit is the wait the mount promises a request: five seconds. The tests that
// wait for it out use a shorter one, which they set before they mount the file.
const sqliteBusyLimit = 5 * time.Second

// sqliteBusyShorten sets the busy timeout of the mounts the test opens after it.
func sqliteBusyShorten(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := busyTimeout
	busyTimeout = timeout
	t.Cleanup(func() { busyTimeout = previous })
}

const sqliteBusyPeople = "    people:\n      fields:\n        name: {type: string}\n"

// sqliteBusyMount creates a file with one table and mounts it. It returns the mount and
// the path of the file.
func sqliteBusyMount(t *testing.T) (*core.Database, string) {
	t.Helper()
	dir := sqliteFieldsStorage(t, `CREATE TABLE "people" ("id" TEXT PRIMARY KEY, "name" TEXT)`, `INSERT INTO "people" VALUES ('p1', 'Ada')`)
	return sqliteFieldsMount(t, dir, sqliteBusyPeople), filepath.Join(dir, "data.sqlite")
}

// sqliteBusyReader opens a handle on the file and starts a transaction that has read the
// table, which holds the shared lock of the file until the transaction ends. The
// transaction is rolled back when the test ends, if the test has not done it.
func sqliteBusyReader(t *testing.T, path string) *sql.Tx {
	t.Helper()
	handle, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	tx, err := handle.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	rows, err := tx.Query(`SELECT "id" FROM "people"`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return tx
}

// sqliteBusyInsert is a write of one record through the mount.
func sqliteBusyInsert(ctx context.Context, db *core.Database, id string) error {
	key, err := core.ParseKey("people", id)
	if err != nil {
		return err
	}
	_, err = db.Apply(ctx, []core.Op{{Op: "insert", Key: key, Data: map[string]any{"name": "Bea"}}}, "busy")
	return err
}

// sqliteBusyBlockedFor reports whether done delivers nothing within the time given: the
// call it stands for is still waiting. It never fails a test that is slow, only one whose
// call has already ended.
func sqliteBusyBlockedFor(done <-chan error, wait time.Duration) (error, bool) {
	select {
	case err := <-done:
		return err, false
	case <-time.After(wait):
		return nil, true
	}
}

func TestAWriteThatMeetsAReaderWaitsForItAndSucceedsOnceTheReaderEnds(t *testing.T) {
	db, path := sqliteBusyMount(t)
	reader := sqliteBusyReader(t, path)

	done := make(chan error, 1)
	go func() { done <- sqliteBusyInsert(context.Background(), db, "p2") }()
	if err, blocked := sqliteBusyBlockedFor(done, 200*time.Millisecond); !blocked {
		t.Fatalf("the write ended while a reader held the file: %v", err)
	}
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the write failed after the reader ended: %v", err)
		}
	case <-time.After(sqliteBusyLimit):
		t.Fatalf("the write did not end within the busy timeout of %v", sqliteBusyLimit)
	}
	if _, err := db.Get(context.Background(), mustKey(t, "people", "p2")); err != nil {
		t.Fatalf("the record written after the wait is not there: %v", err)
	}
}

func mustKey(t *testing.T, collection, id string) *record.Key {
	t.Helper()
	key, err := core.ParseKey(collection, id)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// The wait for a lock is the engine's, and it ends when the busy timeout does, whatever
// the deadline of the request: the write ends then, with the deadline's error and not with
// a lock error, and it does not hang.
func TestAWriteThatWaitsPastTheDeadlineOfTheRequestEndsWithTheDeadline(t *testing.T) {
	sqliteBusyShorten(t, 800*time.Millisecond)
	db, path := sqliteBusyMount(t)
	sqliteBusyReader(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- sqliteBusyInsert(ctx, db, "p2") }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the write ended with %v, want the deadline of the request", err)
		}
		if waited := time.Since(started); waited < 150*time.Millisecond {
			t.Fatalf("the write ended after %v, before the deadline: it did not wait for the lock", waited)
		}
	case <-time.After(sqliteBusyLimit):
		t.Fatal("the write was still waiting after the busy timeout")
	}
}

// A writer that holds the file keeps the handle that reads the columns of a table from
// reading them; it waits for the writer, and ends with the request's deadline when that
// comes first.
func TestTheHandleThatSuppliesFieldListsWaitsForAWriterToo(t *testing.T) {
	sqliteBusyShorten(t, 1500*time.Millisecond)
	db, path := sqliteBusyMount(t)
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	conn, err := writer.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	fields := func(ctx context.Context) ([]string, error) {
		return sqliteJoinFieldsOf2(ctx, t, db, dal.NewRootCollectionRef("people", ""))
	}

	type answer struct {
		fields []string
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		got, err := fields(context.Background())
		done <- answer{got, err}
	}()
	select {
	case a := <-done:
		t.Fatalf("the fields were read while a writer held the file: %v, %v", a.fields, a.err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-done:
		if want := []string{"id", "name"}; a.err != nil || !reflect.DeepEqual(a.fields, want) {
			t.Fatalf("fields = %v, %v, want %v after the writer ended", a.fields, a.err, want)
		}
	case <-time.After(sqliteBusyLimit):
		t.Fatalf("the fields were not read within the busy timeout of %v", busyTimeout)
	}

	// Locked again, a read whose request ends first ends with the deadline, when the wait
	// ends (the engine does not end it before).
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	late := make(chan error, 1)
	go func() {
		_, err := fields(ctx)
		late <- err
	}()
	select {
	case err := <-late:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the read ended with %v, want the deadline of the request", err)
		}
	case <-time.After(sqliteBusyLimit):
		t.Fatal("the read was still waiting after the busy timeout")
	}
}

func sqliteJoinFieldsOf2(ctx context.Context, t *testing.T, db *core.Database, source dal.RecordsetSource) ([]string, error) {
	t.Helper()
	provider, ok := db.Executor().(dal.JoinFieldsProvider)
	if !ok {
		t.Fatal("the executor of a database does not offer JoinFields")
	}
	return provider.JoinFields(ctx, source)
}

// Both handles are opened at the name that carries the busy timeout, by the driver's
// option, and the timeout is five seconds.
func TestEveryHandleTheMountOpensIsOpenedWithTheBusyTimeout(t *testing.T) {
	if busyTimeout != 5*time.Second {
		t.Fatalf("busyTimeout = %v, want 5s", busyTimeout)
	}
	if got, want := busyTimeoutDSN("/data/db.sqlite"), "/data/db.sqlite?_busy_timeout=5000"; got != want {
		t.Fatalf("busyTimeoutDSN = %q, want %q", got, want)
	}

	var mainName, columnsName string
	openMain, openColumns := newSQLiteDatabase, openColumnsDB
	newSQLiteDatabase = func(name string, schema dal.Schema, options dalgo2sql.DbOptions) (*dalgo2sqlite.Database, error) {
		mainName = name
		return openMain(name, schema, options)
	}
	openColumnsDB = func(name string) (*sql.DB, error) {
		columnsName = name
		return openColumns(name)
	}
	t.Cleanup(func() { newSQLiteDatabase, openColumnsDB = openMain, openColumns })

	_, path := sqliteBusyMount(t)
	want := path + "?_busy_timeout=5000"
	if mainName != want || columnsName != want {
		t.Fatalf("the handles were opened at %q and %q, want both at %q", mainName, columnsName, want)
	}
}

// overDeadline reports the end of a request for a lock error only: any other error, and
// the lock error of a request that is still running, are returned as they are. The errors
// are the driver's own, from a real file: a statement of a handle that does not wait meets
// a reader, and one that breaks a key.
func TestOverDeadlineReportsALockErrorOfAnEndedRequestAsTheEndOfTheRequest(t *testing.T) {
	_, path := sqliteBusyMount(t)
	handle, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	_, constraint := handle.Exec(`INSERT INTO "people" VALUES ('p1', 'twice')`)
	reader := sqliteBusyReader(t, path)
	_, busy := handle.Exec(`INSERT INTO "people" VALUES ('p9', 'late')`)
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	if busy == nil || constraint == nil {
		t.Fatalf("the driver gave %v and %v, want a lock error and a constraint error", busy, constraint)
	}

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	lock := fmt.Errorf("failed to commit transaction: %w", busy)

	if got := overDeadline(ended, nil); got != nil {
		t.Fatalf("nil became %v", got)
	}
	for name, tc := range map[string]struct {
		ctx  context.Context
		err  error
		want error // the error the result must be, by errors.Is; nil means the same error as it was
	}{
		"a lock, the request cancelled":                  {ended, lock, context.Canceled},
		"a lock, the deadline passed":                    {expired, lock, context.DeadlineExceeded},
		"a lock, the request still running":              {context.Background(), lock, nil},
		"another error of the engine, the request ended": {expired, constraint, nil},
		"an error that is not the engine's":              {expired, errors.New("boom"), nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := overDeadline(tc.ctx, tc.err)
			if tc.want == nil {
				if got != tc.err {
					t.Fatalf("got %v, want the error as it was", got)
				}
				return
			}
			if !errors.Is(got, tc.want) || !errors.Is(got, errors.Unwrap(tc.err)) {
				t.Fatalf("got %v, want %v with the lock error in its chain", got, tc.want)
			}
		})
	}
}

// A handle that fails to open is reported, with the file it was for and the cause, and the
// second handle is never opened.
func TestOpeningASQLiteMountReportsTheFailureToOpenItsFirstHandle(t *testing.T) {
	errOpen := errors.New("open failed")
	previous, previousColumns := newSQLiteDatabase, openColumnsDB
	newSQLiteDatabase = func(string, dal.Schema, dalgo2sql.DbOptions) (*dalgo2sqlite.Database, error) { return nil, errOpen }
	openColumnsDB = func(string) (*sql.DB, error) {
		t.Error("the second handle was opened after the first failed")
		return nil, errOpen
	}
	t.Cleanup(func() { newSQLiteDatabase, openColumnsDB = previous, previousColumns })

	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: first, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n" + sqliteBusyPeople
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := File(path)
	if db != nil || !errors.Is(err, errOpen) || !strings.Contains(err.Error(), "data.sqlite") || strings.Contains(err.Error(), "_busy_timeout") {
		t.Fatalf("got %v, %v; want the failure to open, naming the file and not the name the driver is given", db, err)
	}
}
