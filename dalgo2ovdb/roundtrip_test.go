package dalgo2ovdb_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"

	"github.com/openvaultdb/openvaultdb-go/dalgo2ovdb"
	"github.com/openvaultdb/openvaultdb-go/internal/server"
	"github.com/openvaultdb/openvaultdb-go/internal/store"
)

// startOVDBServer boots a REAL ovdb-server (the same internal/server +
// internal/store code path `ovdb serve` runs) in-process over httptest,
// persisting to a fresh temp dir via the real inGitDB-backed store. This is
// what makes the round trip below honest: no mocks, no fakes, real HTTP, real
// JSON files on disk — the only thing swapped out is the TCP transport
// (httptest instead of a separately-run `go run ./cmd/ovdb-server`).
//
// This test file is the ONLY place in this module that imports
// internal/server or internal/store. It can, because Go's internal-package
// visibility rule is import-path-based, not module-based: this package's
// import path (github.com/openvaultdb/openvaultdb-go/dalgo2ovdb/...) is still
// nested under github.com/openvaultdb/openvaultdb-go, even though it has its
// own go.mod (see the replace directive there). The dalgo2ovdb package itself
// (database.go, client.go, connect.go, ...) never imports internal/* — it is
// a plain HTTP client, same as any real external caller would be.
func startOVDBServer(t *testing.T) (baseURL, dataDir string) {
	t.Helper()
	ts := httptest.NewUnstartedServer(http.NotFoundHandler())
	baseURL = "http://" + ts.Listener.Addr().String()

	dataDir = t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	srv := server.New(st, baseURL)
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return baseURL, dataDir
}

const (
	testClientID    = "contactus-poc.sneat.app"
	testRedirectURI = "http://localhost:0/callback"
	testVault       = "personal"
	testNamespace   = "todo-demo.openvaultdb.app/openvaultdb/todos"
	testRole        = "editor"

	// The reference server only ever seeds one namespace with one collection,
	// "tasks" (internal/store/store.go's seed()) — there is no route to
	// provision a "contacts" collection dynamically yet (a Track A gap: "fix
	// store.go:154 shared-namespace limitation" plus per-vault/namespace
	// provisioning generally). This PoC reuses "tasks" as the target
	// collection for a contactus-shaped record rather than inventing a
	// collection the server has no seed/schema/role-scope for.
	testCollection = "tasks"
)

// connectForTest runs the real connect flow (POST /authorize -> code -> POST
// /token) against the httptest-hosted ovdb-server and returns a ready
// dal.DB — exactly what a Sneat backend would do against a real local
// ovdb-server, modulo the automated (non-browser) consent approval Connect
// documents.
func connectForTest(t *testing.T, ctx context.Context, baseURL, state string) dal.DB {
	t.Helper()
	db, err := dalgo2ovdb.Connect(ctx, nil, baseURL, dalgo2ovdb.ConnectParams{
		ClientID:    testClientID,
		RedirectURI: testRedirectURI,
		Vault:       testVault,
		NamespaceID: testNamespace,
		Role:        testRole,
		State:       state,
	})
	if err != nil {
		t.Fatalf("dalgo2ovdb.Connect: %v", err)
	}
	return db
}

// recordPath mirrors internal/store/store.go's nsDiskPath()+collectionDir()
// on-disk layout, so the test can independently verify a record actually
// landed in the inGitDB vault (not just that the HTTP round trip echoed it
// back).
func recordPath(dataDir, vault, nsID, collection, id string) string {
	parts := strings.Split(nsID, "/")
	if len(parts) >= 2 && parts[1] == "openvaultdb" {
		parts = append(parts[:1:1], parts[2:]...)
	}
	return filepath.Join(append([]string{dataDir, "vaults", vault}, append(parts, collection, "$records", id+".json")...)...)
}

func assertOnDiskTitle(t *testing.T, dataDir, vault, nsID, collection, id, wantTitle string) {
	t.Helper()
	p := recordPath(dataDir, vault, nsID, collection, id)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("record not found on disk at %s: %v", p, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("on-disk record at %s is not valid JSON: %v", p, err)
	}
	if m["title"] != wantTitle {
		t.Errorf("on-disk record title = %v, want %v (raw: %s)", m["title"], wantTitle, b)
	}
}

func assertAbsentOnDisk(t *testing.T, dataDir, vault, nsID, collection, id string) {
	t.Helper()
	p := recordPath(dataDir, vault, nsID, collection, id)
	if _, err := os.Stat(p); err == nil {
		t.Fatalf("record file %s still exists after Delete", p)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", p, err)
	}
}

// TestRoundTrip_ContactusRecord is the PoC's core evidence: Set a
// contactus-shaped person record through dalgo2ovdb, Get it back, assert
// equality, Delete it, assert it's gone through both the adapter AND direct
// disk inspection of the inGitDB vault.
func TestRoundTrip_ContactusRecord(t *testing.T) {
	baseURL, dataDir := startOVDBServer(t)
	ctx := context.Background()
	db := connectForTest(t, ctx, baseURL, "roundtrip-state")

	// A contactus-shaped record: id (the dal.Key), names, roles. "title" is
	// present only because ovdb-server's handleCreateRecord hardcodes it as a
	// required field for every collection regardless of schema (a demo-server
	// rigidity, not a DALgo or OVDB-protocol requirement) — mapped here to the
	// person's display name, itself a defensible thing to require.
	const id = "contact-jane-doe"
	key := dal.NewKeyWithID(testCollection, id)
	person := map[string]any{
		"title":     "Jane Doe",
		"firstName": "Jane",
		"lastName":  "Doe",
		"roles":     []any{"owner", "driver"},
		"email":     "jane@example.com",
	}

	if err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Set(ctx, dal.NewRecordWithData(key, person))
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Read back through the SAME dal.DB interface, not an OVDB-specific
	// client — this is what a Sneat module's storage-agnostic code would call.
	got := dal.NewRecordWithData(dal.NewKeyWithID(testCollection, id), map[string]any{})
	if err := db.Get(ctx, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Exists() {
		t.Fatalf("Get: record does not Exist() after Set")
	}
	gotData := got.Data().(map[string]any)
	for k, want := range person {
		if k == "roles" {
			continue
		}
		if gotData[k] != want {
			t.Errorf("field %q = %v, want %v", k, gotData[k], want)
		}
	}
	gotRoles, _ := gotData["roles"].([]any)
	if len(gotRoles) != 2 || gotRoles[0] != "owner" || gotRoles[1] != "driver" {
		t.Errorf("roles = %v, want [owner driver]", gotData["roles"])
	}

	// Prove it's not just an HTTP-layer echo: the plan's PoC criterion is
	// "confirm the JSON lands in the inGitDB vault on disk".
	assertOnDiskTitle(t, dataDir, testVault, testNamespace, testCollection, id, "Jane Doe")

	if err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Delete(ctx, key)
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if exists, err := db.Exists(ctx, key); err != nil {
		t.Fatalf("Exists after Delete: %v", err)
	} else if exists {
		t.Fatalf("record still Exists() after Delete")
	}
	assertAbsentOnDisk(t, dataDir, testVault, testNamespace, testCollection, id)
}

// genericCreateAndRead is the thesis check: it takes any dal.DB and does a
// create+read using ONLY dal.DB/dal.Record/dal.Key — no OVDB import, no OVDB
// type, nothing storage-specific. This is deliberately what a real Sneat
// module's business logic looks like (e.g. a contactus repository function).
// The exact same function, unchanged, would run against a
// dalgo2firestore-backed dal.DB.
func genericCreateAndRead(ctx context.Context, db dal.DB, collection, id string, data map[string]any) (map[string]any, error) {
	key := dal.NewKeyWithID(collection, id)
	if err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Set(ctx, dal.NewRecordWithData(key, data))
	}); err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}
	got := dal.NewRecordWithData(dal.NewKeyWithID(collection, id), map[string]any{})
	if err := db.Get(ctx, got); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return got.Data().(map[string]any), nil
}

// TestThesis_GenericDALgoFunctionOverOVDB is the go/no-go test the investment
// plan asked for: hand genericCreateAndRead a dalgo2ovdb-backed dal.DB and
// confirm it works with ZERO OVDB-aware code in the caller.
func TestThesis_GenericDALgoFunctionOverOVDB(t *testing.T) {
	baseURL, _ := startOVDBServer(t)
	ctx := context.Background()
	db := connectForTest(t, ctx, baseURL, "thesis-state")

	const id = "contact-generic"
	got, err := genericCreateAndRead(ctx, db, testCollection, id, map[string]any{
		"title":     "Generic Person",
		"firstName": "Generic",
	})
	if err != nil {
		t.Fatalf("genericCreateAndRead(dalgo2ovdb DB): %v", err)
	}
	if got["title"] != "Generic Person" {
		t.Errorf("title = %v, want %q", got["title"], "Generic Person")
	}

	t.Cleanup(func() {
		_ = db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			return tx.Delete(ctx, dal.NewKeyWithID(testCollection, id))
		})
	})
}
