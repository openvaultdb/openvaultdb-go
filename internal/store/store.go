// Package store provides on-disk persistence for the OpenVaultDB reference
// server: vaults, namespaces (with role catalogs), and records persisted in an
// inGitDB-style layout under a data directory.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Op is a collection-level operation, mirroring the `Op` enum in main.tsp.
type Op string

const (
	OpRead   Op = "read"
	OpWrite  Op = "write"
	OpDelete Op = "delete"
)

// Vault mirrors the `Vault` model in main.tsp.
type Vault struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Backend string `json:"backend"`
}

// Namespace mirrors the `Namespace` model in main.tsp, plus the role catalog
// used by the connect flow (roles are not exposed on the wire by the
// namespaces listing, only their collection list is).
type Namespace struct {
	ID          string           `json:"id"`
	Owner       string           `json:"owner"`
	Collections []string         `json:"collections"`
	Roles       map[string]Scope `json:"-"`
}

// Scope maps a collection name to the operations granted on it.
type Scope map[string][]Op

// Record is an opaque JSON record governed by the namespace's collection schema.
type Record map[string]any

// Store is the in-memory index plus on-disk record persistence.
type Store struct {
	dir        string
	ownerToken string

	mu         sync.Mutex
	vaults     []Vault
	namespaces map[string]*Namespace // by namespace id
}

// Open initialises the data directory, loads or seeds the owner token, and
// seeds the demo vault/namespace on first run.
func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}

	s := &Store{
		dir:        abs,
		namespaces: map[string]*Namespace{},
	}
	if err := s.loadOrCreateOwnerToken(); err != nil {
		return nil, err
	}
	s.seed()
	if err := s.ensureCollectionSchemas(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Dir() string        { return s.dir }
func (s *Store) OwnerToken() string { return s.ownerToken }

func (s *Store) loadOrCreateOwnerToken() error {
	path := filepath.Join(s.dir, "owner-token")
	if b, err := os.ReadFile(path); err == nil {
		s.ownerToken = strings.TrimSpace(string(b))
		if s.ownerToken != "" {
			return nil
		}
	}
	tok, err := randomHex(24)
	if err != nil {
		return err
	}
	s.ownerToken = tok
	return os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

// seed populates the default vaults (personal, family, work) and the demo
// namespace. Records are isolated per vault; the namespace is shared.
func (s *Store) seed() {
	s.vaults = []Vault{
		{ID: "personal", Name: "Personal", Backend: "ingit"},
		{ID: "family", Name: "Family", Backend: "ingit"},
		{ID: "work", Name: "Work", Backend: "ingit"},
	}
	ns := &Namespace{
		ID:          "todo-demo.openvaultdb.app/openvaultdb/todos",
		Owner:       "todo-demo.openvaultdb.app",
		Collections: []string{"tasks"},
		Roles: map[string]Scope{
			"editor": {"tasks": {OpRead, OpWrite, OpDelete}},
			"viewer": {"tasks": {OpRead}},
		},
	}
	s.namespaces[ns.ID] = ns
}

// Vaults returns the hosted vaults.
func (s *Store) Vaults() []Vault {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Vault, len(s.vaults))
	copy(out, s.vaults)
	return out
}

// Vault looks up a vault by id.
func (s *Store) Vault(id string) (Vault, bool) {
	for _, v := range s.Vaults() {
		if v.ID == id {
			return v, true
		}
	}
	return Vault{}, false
}

// Namespaces returns namespaces (vault-scoped; the MVP shares one set).
func (s *Store) Namespaces() []Namespace {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Namespace, 0, len(s.namespaces))
	for _, ns := range s.namespaces {
		out = append(out, *ns)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Namespace looks up a namespace by id.
func (s *Store) Namespace(id string) (*Namespace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ns, ok := s.namespaces[id]
	return ns, ok
}

// ScopeForRole resolves a role name within a namespace to its scope.
func (ns *Namespace) ScopeForRole(role string) (Scope, bool) {
	sc, ok := ns.Roles[role]
	return sc, ok
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ---------------------------------------------------------------------------
// Record persistence (inGitDB-style: one JSON file per record under
// <dir>/vaults/<vault>/ns/<sanitized-ns>/collections/<col>/$records/<id>.json)
// ---------------------------------------------------------------------------

func (s *Store) collectionDir(vault, nsID, collection string) string {
	return filepath.Join(s.dir, "vaults", vault, "ns", nsPath(nsID), "collections", collection)
}

func (s *Store) recordsDir(vault, nsID, collection string) string {
	return filepath.Join(s.collectionDir(vault, nsID, collection), "$records")
}

// nsPath turns a domain-bounded namespace id (which contains '/') into nested
// path segments, so the on-disk layout mirrors the namespace hierarchy — e.g.
// "todo-demo.openvaultdb.app/openvaultdb/todos" becomes the directories
// todo-demo.openvaultdb.app/openvaultdb/todos. Each segment is sanitized and
// empty/relative segments ("", ".", "..") are dropped to keep records within
// the collection tree.
func nsPath(nsID string) string {
	parts := strings.Split(nsID, "/")
	safe := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "." || p == ".." {
			continue
		}
		p = strings.ReplaceAll(p, "\\", "_")
		safe = append(safe, p)
	}
	return filepath.Join(safe...)
}

// ensureCollectionSchemas writes a .ingitdb-collection.yaml for each seeded
// collection so the on-disk store is a valid inGitDB collection.
func (s *Store) ensureCollectionSchemas() error {
	for _, v := range s.vaults {
		for _, ns := range s.namespaces {
			for _, col := range ns.Collections {
				dir := s.collectionDir(v.ID, ns.ID, col)
				if err := os.MkdirAll(filepath.Join(dir, "$records"), 0o755); err != nil {
					return err
				}
				schemaPath := filepath.Join(dir, ".ingitdb-collection.yaml")
				if _, err := os.Stat(schemaPath); err == nil {
					continue
				}
				if err := os.WriteFile(schemaPath, []byte(tasksCollectionSchema), 0o644); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ListRecords returns all records in a collection.
func (s *Store) ListRecords(vault, nsID, collection string) ([]Record, error) {
	dir := s.recordsDir(vault, nsID, collection)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Record{}, nil
		}
		return nil, err
	}
	out := []Record{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var r Record
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["id"]) < fmt.Sprint(out[j]["id"])
	})
	return out, nil
}

// CreateRecord persists a new record, generating id/createdAt/done defaults.
func (s *Store) CreateRecord(vault, nsID, collection string, rec Record) (Record, error) {
	if rec == nil {
		rec = Record{}
	}
	id, _ := rec["id"].(string)
	if id == "" {
		id = uuid.NewString()
		rec["id"] = id
	}
	if _, ok := rec["createdAt"]; !ok {
		rec["createdAt"] = time.Now().UTC().Format(time.RFC3339)
	}
	if _, ok := rec["done"]; !ok {
		rec["done"] = false
	}
	if err := s.writeRecord(vault, nsID, collection, id, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// GetRecord loads a single record by id.
func (s *Store) GetRecord(vault, nsID, collection, id string) (Record, bool, error) {
	b, err := os.ReadFile(s.recordPath(vault, nsID, collection, id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, false, err
	}
	return r, true, nil
}

// UpdateRecord merges a patch into an existing record.
func (s *Store) UpdateRecord(vault, nsID, collection, id string, patch Record) (Record, bool, error) {
	cur, ok, err := s.GetRecord(vault, nsID, collection, id)
	if err != nil || !ok {
		return nil, ok, err
	}
	for k, v := range patch {
		if k == "id" {
			continue
		}
		cur[k] = v
	}
	if err := s.writeRecord(vault, nsID, collection, id, cur); err != nil {
		return nil, true, err
	}
	return cur, true, nil
}

// DeleteRecord removes a record by id.
func (s *Store) DeleteRecord(vault, nsID, collection, id string) (bool, error) {
	err := os.Remove(s.recordPath(vault, nsID, collection, id))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) recordPath(vault, nsID, collection, id string) string {
	return filepath.Join(s.recordsDir(vault, nsID, collection), safeID(id)+".json")
}

func (s *Store) writeRecord(vault, nsID, collection, id string, rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.recordsDir(vault, nsID, collection)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.recordPath(vault, nsID, collection, id), b, 0o644)
}

// safeID prevents path traversal via record ids.
func safeID(id string) string {
	id = strings.ReplaceAll(id, "/", "_")
	id = strings.ReplaceAll(id, "\\", "_")
	id = strings.ReplaceAll(id, "..", "_")
	return id
}

// tasksCollectionSchema is the inGitDB collection schema for `tasks`,
// matching the fields pinned in INTEGRATION.md.
const tasksCollectionSchema = `titles:
  en: Tasks
recordsDir: $records
columns:
  id:
    type: string
    titles:
      en: ID
    primaryKey: true
  title:
    type: string
    titles:
      en: Title
    required: true
  done:
    type: boolean
    titles:
      en: Done
    default: false
  createdAt:
    type: string
    titles:
      en: Created At
columnsOrder:
  - id
  - title
  - done
  - createdAt
`
