package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

// databaseCreateRequest is the body for POST /v1/databases.
type databaseCreateRequest struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// databaseCreateResponse is the 201 response for POST /v1/databases. Token is
// only present when the creator is NOT the owner: a freshly minted grant
// scoped to the new database (per-app isolation — the provisioning token
// carries databases:create only, each app gets its own db-scoped token).
type databaseCreateResponse struct {
	Database databaseInfo   `json:"database"`
	Token    *tokenResponse `json:"token,omitempty"`
}

type databaseInfo struct {
	ID         string `json:"id"`
	Engine     string `json:"engine"`
	SchemaMode string `json:"schemaMode"`
}

// handleDatabaseCreate implements POST /v1/databases: runtime provisioning of
// a new inGitDB schemaless database under --data-dir. Allowed for the owner
// or any principal whose grant allows the server-level databases:create
// capability.
func (s *Server) handleDatabaseCreate(w http.ResponseWriter, r *http.Request) {
	if !s.isOwner(r) && !auth.FromRequest(r).Allows("", auth.CapDatabasesCreate, "") {
		writeError(w, http.StatusForbidden, "forbidden",
			"token does not grant "+auth.CapDatabasesCreate)
		return
	}
	if s.dataDir == "" {
		writeError(w, http.StatusNotImplemented, "not_supported",
			"runtime database creation is disabled: start the server with --data-dir")
		return
	}
	var req databaseCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if err := manifest.ValidateID(req.ID); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Serialize creations: the duplicate check, filesystem provisioning and
	// live mount must be atomic with respect to concurrent creates.
	s.createMu.Lock()
	defer s.createMu.Unlock()

	if s.getDB(req.ID) != nil {
		writeError(w, http.StatusConflict, "already_exists", "database already exists: "+req.ID)
		return
	}

	db, err := provisionDatabase(s.dataDir, req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	s.mu.Lock()
	s.dbs[req.ID] = db
	s.mu.Unlock()

	resp := databaseCreateResponse{
		Database: databaseInfo{
			ID:         db.ID(),
			Engine:     db.Manifest.Storage.Engine,
			SchemaMode: string(db.Manifest.Database.SchemaMode),
		},
	}

	// Mint-on-create isolation: a non-owner creator (an app holding a
	// databases:create token) gets a fresh grant scoped to the new database.
	// The owner gets no auto-token — the owner token already has full access.
	if !s.isOwner(r) {
		token, tokenResp, err := s.mintDatabaseToken(r, req.ID, req.Label)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		tokenResp.Token = token
		resp.Token = tokenResp
	}
	writeJSON(w, http.StatusCreated, resp)
}

// mintDatabaseToken mints a never-expiring grant scoped to the new database
// with the full data capability set, labelled from the request label or the
// creator grant's label.
func (s *Server) mintDatabaseToken(r *http.Request, databaseID, label string) (string, *tokenResponse, error) {
	creator := auth.FromRequest(r)
	if label == "" && creator != nil && creator.Grant != nil {
		label = creator.Grant.Label
	}
	var principalID string
	if creator != nil && creator.Grant != nil {
		principalID = creator.Grant.PrincipalID
	}
	token, err := auth.NewToken()
	if err != nil {
		return "", nil, fmt.Errorf("failed to generate token: %w", err)
	}
	g := &auth.Grant{
		Label:       label,
		PrincipalID: principalID,
		DatabaseID:  databaseID,
		Capabilities: []auth.Capability{
			{Action: auth.CapRecordsRead},
			{Action: auth.CapRecordsWrite},
			{Action: auth.CapRecordsDelete},
			{Action: auth.CapCollectionsRead},
			{Action: auth.CapSchemaRead},
		},
	}
	if err = s.authCfg.Store.CreateGrant(g, token); err != nil {
		return "", nil, fmt.Errorf("failed to persist grant: %w", err)
	}
	tr := grantToResponse(g, "")
	return token, &tr, nil
}

// provisionDatabase creates <dataDir>/<id>/ (git init, best-effort), writes
// <dataDir>/<id>.yaml (an inGitDB schemaless manifest — the persisted record
// that lets a restart rescan remount the database), and mounts it live.
func provisionDatabase(dataDir, id string) (*core.Database, error) {
	dbDir := filepath.Join(dataDir, id)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}
	// git init so the inGitDB directory is a repository from the start
	// (commit history per write batch). The dalgo2ingitdb driver itself works
	// on a plain directory, so a missing git binary is not fatal. The local
	// git identity dalgo2ingitdb's commits need is NOT stamped here: the
	// mount.File call below mounts this same directory through
	// openInGitDB, which ensures it (see mount.ensureGitIdentity) — the one
	// place that covers this create path and every later remount alike.
	if _, err := os.Stat(filepath.Join(dbDir, ".git")); os.IsNotExist(err) {
		_ = exec.Command("git", "-C", dbDir, "init", "-q").Run()
	}
	manifestPath := filepath.Join(dataDir, id+".yaml")
	manifestYAML := fmt.Sprintf(
		"database:\n  id: %s\n  schema_mode: schemaless\nstorage:\n  engine: ingitdb\n  path: ./%s\n",
		id, id)
	if err := os.WriteFile(manifestPath, []byte(manifestYAML), 0o644); err != nil {
		return nil, fmt.Errorf("failed to write manifest: %w", err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		// Roll back the manifest so a broken database is not remounted on restart.
		_ = os.Remove(manifestPath)
		return nil, fmt.Errorf("failed to mount created database: %w", err)
	}
	return db, nil
}
