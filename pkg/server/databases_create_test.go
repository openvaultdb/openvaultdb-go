package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// startAuthServerWithDataDir mounts one seed database behind an auth-enabled
// server with runtime database creation enabled. Returns the test server and
// the data-dir (for restart-persistence assertions).
func startAuthServerWithDataDir(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(manifestPath, []byte(`
database:
  id: dev
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data
`), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "created")
	if err = os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := server.New("test", map[string]*core.Database{"dev": db},
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}),
		server.WithDataDir(dataDir))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, dataDir
}

// mintCreateDBToken creates a server-level databases:create token via the
// owner token admin API and returns the secret.
func mintCreateDBToken(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	status, body := request(t, ts, http.MethodPost, "/v1/tokens", ownerToken,
		`{"label":"provisioner","capabilities":["databases:create"]}`)
	if status != http.StatusCreated {
		t.Fatalf("mint create-db token: %d %s", status, body)
	}
	var resp struct {
		Token      string `json:"token"`
		DatabaseID string `json:"databaseId"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.DatabaseID != "" {
		t.Fatalf("create-db token must be server-level (empty databaseId), got %q", resp.DatabaseID)
	}
	return resp.Token
}

func TestDatabaseCreate_OwnerNoAutoToken(t *testing.T) {
	ts, _ := startAuthServerWithDataDir(t)
	status, body := request(t, ts, http.MethodPost, "/v1/databases", ownerToken,
		`{"id":"owner-db"}`)
	if status != http.StatusCreated {
		t.Fatalf("owner create: %d %s", status, body)
	}
	var resp struct {
		Database struct {
			ID         string `json:"id"`
			Engine     string `json:"engine"`
			SchemaMode string `json:"schemaMode"`
		} `json:"database"`
		Token *json.RawMessage `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Database.ID != "owner-db" || resp.Database.Engine != "ingitdb" || resp.Database.SchemaMode != "schemaless" {
		t.Fatalf("database info: %+v", resp.Database)
	}
	if resp.Token != nil {
		t.Fatal("owner-created database must not mint an auto-token")
	}
	// The database is live immediately: owner can write to it.
	if status, body = request(t, ts, http.MethodPut, "/v1/databases/owner-db/records/notes/n1", ownerToken,
		`{"data":{"text":"hi"}}`); status != http.StatusNoContent {
		t.Fatalf("write to created db: %d %s", status, body)
	}
}

func TestDatabaseCreate_AppTokenMintsScopedToken(t *testing.T) {
	ts, _ := startAuthServerWithDataDir(t)
	createToken := mintCreateDBToken(t, ts)

	// The provisioning token cannot read/write records anywhere.
	if status, _ := request(t, ts, http.MethodGet, "/v1/databases/dev/records/notes/n1", createToken, ""); status != http.StatusForbidden {
		t.Fatalf("create-db token reading records: want 403, got %d", status)
	}

	// Create a database with the app provisioning token.
	status, body := request(t, ts, http.MethodPost, "/v1/databases", createToken,
		`{"id":"app-db","label":"my app"}`)
	if status != http.StatusCreated {
		t.Fatalf("app create: %d %s", status, body)
	}
	var resp struct {
		Database struct {
			ID string `json:"id"`
		} `json:"database"`
		Token *struct {
			ID           string   `json:"id"`
			Token        string   `json:"token"`
			Label        string   `json:"label"`
			DatabaseID   string   `json:"databaseId"`
			Capabilities []string `json:"capabilities"`
		} `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == nil {
		t.Fatal("app-created database must include a minted scoped token")
	}
	if resp.Token.DatabaseID != "app-db" {
		t.Fatalf("minted token databaseId = %q, want app-db", resp.Token.DatabaseID)
	}
	if resp.Token.Label != "my app" {
		t.Fatalf("minted token label = %q, want request label", resp.Token.Label)
	}
	appToken := resp.Token.Token

	// The minted token can immediately write records to the new database.
	if status, body = request(t, ts, http.MethodPut, "/v1/databases/app-db/records/notes/n1", appToken,
		`{"data":{"text":"app data"}}`); status != http.StatusNoContent {
		t.Fatalf("minted token write: %d %s", status, body)
	}
	if status, _ = request(t, ts, http.MethodGet, "/v1/databases/app-db/records/notes/n1", appToken, ""); status != http.StatusOK {
		t.Fatalf("minted token read: %d", status)
	}
	// ...but CANNOT touch another database.
	if status, _ = request(t, ts, http.MethodGet, "/v1/databases/dev/records/notes/n1", appToken, ""); status != http.StatusForbidden {
		t.Fatalf("minted token on other db: want 403, got %d", status)
	}
	// The provisioning token still cannot read the new database's records.
	if status, _ = request(t, ts, http.MethodGet, "/v1/databases/app-db/records/notes/n1", createToken, ""); status != http.StatusForbidden {
		t.Fatalf("create-db token reading created db: want 403, got %d", status)
	}
}

func TestDatabaseCreate_RecordsOnlyTokenForbidden(t *testing.T) {
	ts, _ := startAuthServerWithDataDir(t)
	appToken := connectFlow(t, ts, "records:read,records:write")
	if status, _ := request(t, ts, http.MethodPost, "/v1/databases", appToken,
		`{"id":"nope"}`); status != http.StatusForbidden {
		t.Fatalf("records-only token creating db: want 403, got %d", status)
	}
}

func TestDatabaseCreate_DuplicateID(t *testing.T) {
	ts, _ := startAuthServerWithDataDir(t)
	// "dev" is already mounted.
	if status, _ := request(t, ts, http.MethodPost, "/v1/databases", ownerToken,
		`{"id":"dev"}`); status != http.StatusConflict {
		t.Fatalf("duplicate id: want 409, got %d", status)
	}
	// A created id also conflicts on a second create.
	if status, _ := request(t, ts, http.MethodPost, "/v1/databases", ownerToken,
		`{"id":"dup"}`); status != http.StatusCreated {
		t.Fatal("first create must succeed")
	}
	if status, _ := request(t, ts, http.MethodPost, "/v1/databases", ownerToken,
		`{"id":"dup"}`); status != http.StatusConflict {
		t.Fatalf("second create: want 409, got %d", status)
	}
}

func TestDatabaseCreate_BadID(t *testing.T) {
	ts, _ := startAuthServerWithDataDir(t)
	for _, id := range []string{"", "-bad", "has space", "a/b", "../evil"} {
		body, _ := json.Marshal(map[string]string{"id": id})
		if status, resp := request(t, ts, http.MethodPost, "/v1/databases", ownerToken, string(body)); status != http.StatusBadRequest {
			t.Errorf("id %q: want 400, got %d %s", id, status, resp)
		}
	}
}

func TestDatabaseCreate_NoDataDir(t *testing.T) {
	// The plain startAuthServer has no data-dir configured.
	ts := startAuthServer(t)
	status, body := request(t, ts, http.MethodPost, "/v1/databases", ownerToken, `{"id":"x"}`)
	if status != http.StatusNotImplemented {
		t.Fatalf("no data-dir: want 501, got %d %s", status, body)
	}
}

func TestDatabaseCreate_RestartPersistence(t *testing.T) {
	ts, dataDir := startAuthServerWithDataDir(t)
	// Create and write.
	if status, body := request(t, ts, http.MethodPost, "/v1/databases", ownerToken,
		`{"id":"persisted"}`); status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	if status, _ := request(t, ts, http.MethodPut, "/v1/databases/persisted/records/notes/n1", ownerToken,
		`{"data":{"text":"survives"}}`); status != http.StatusNoContent {
		t.Fatal("write failed")
	}

	// "Restart": rescan the data-dir the way `ovdb serve --data-dir` does and
	// build a second server over it.
	created, err := mount.Dir(dataDir)
	if err != nil {
		t.Fatalf("rescan data-dir: %v", err)
	}
	db, ok := created["persisted"]
	if !ok {
		t.Fatalf("created database not remounted from data-dir; got %d dbs", len(created))
	}
	ts2 := httptest.NewServer(server.New("test", map[string]*core.Database{"persisted": db}).Handler())
	t.Cleanup(ts2.Close)
	status, body := request(t, ts2, http.MethodGet, "/v1/databases/persisted/records/notes/n1", "", "")
	if status != http.StatusOK {
		t.Fatalf("read after restart: %d %s", status, body)
	}
}

// TestTokenAdmin_EmptyDBRequiresCreateCap verifies the tokens API rule: an
// empty databaseId is only allowed when capabilities include databases:create.
func TestTokenAdmin_EmptyDBRequiresCreateCap(t *testing.T) {
	ts := startAuthServer(t)
	// Empty db without databases:create → 400.
	if status, _ := request(t, ts, http.MethodPost, "/v1/tokens", ownerToken,
		`{"capabilities":["records:read"]}`); status != http.StatusBadRequest {
		t.Fatalf("empty db without create cap: want 400, got %d", status)
	}
	// Empty db WITH databases:create → 201.
	if status, body := request(t, ts, http.MethodPost, "/v1/tokens", ownerToken,
		`{"capabilities":["databases:create"]}`); status != http.StatusCreated {
		t.Fatalf("empty db with create cap: want 201, got %d %s", status, body)
	}
}
