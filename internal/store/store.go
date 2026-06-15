// Package store provides on-disk persistence for the OpenVaultDB reference
// server: vaults, namespaces (with role catalogs), and records persisted in an
// inGitDB-style layout under a data directory.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/google/uuid"
	"github.com/ingitdb/ingitdb-cli/pkg/dalgo2ingitdb"
	"github.com/ingitdb/ingitdb-cli/pkg/ingitdb/validator"
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
	dbs        map[string]dal.DB     // by vault id; inGitDB-backed record store
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
		dbs:        map[string]dal.DB{},
	}
	if err := s.loadOrCreateOwnerToken(); err != nil {
		return nil, err
	}
	s.seed()
	if err := s.ensureCollectionSchemas(); err != nil {
		return nil, err
	}
	if err := s.openDatabases(); err != nil {
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
// <dir>/vaults/<vault>/<owner-domain>/<name>/<col>/$records/<id>.json).
// The data dir defaults to ~/openvaultdb.
// ---------------------------------------------------------------------------

// vaultDir is the inGitDB project root for a vault (the projectPath passed to
// dalgo2ingitdb.NewDatabase). It must exist before the DB is opened.
func (s *Store) vaultDir(vault string) string {
	return filepath.Join(s.dir, "vaults", vault)
}

func (s *Store) collectionDir(vault, nsID, collection string) string {
	return filepath.Join(s.vaultDir(vault), nsDiskPath(nsID), collection)
}

// collectionRelPath is the collection directory relative to the vault project
// root, using "/" separators (the path recorded in root-collections.yaml).
func collectionRelPath(nsID, collection string) string {
	return filepath.ToSlash(filepath.Join(nsDiskPath(nsID), collection))
}

// collectionID is the dalgo collection identifier for a (namespace, collection)
// pair. dalgo encodes a record key as "<collectionID>/<recordKey>" and validates
// the collection segment with ingitdb.ValidateCollectionID, which allows only
// alphanumerics, '.' and '_' (no '/' and no '-'). So the nested relative path is
// flattened to a dotted, sanitized handle. It is decoupled from the on-disk path
// (mapped via root-collections.yaml), so collisions only matter within one vault.
func collectionID(nsID, collection string) string {
	raw := collectionRelPath(nsID, collection)
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
		case r == '/':
			b.WriteRune('.')
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "._")
}

// nsDiskPath maps a domain-bounded namespace id to nested on-disk directories.
// The "openvaultdb" infix from the namespace convention
// (<owner-domain>/openvaultdb/<name>) is dropped on disk — it stays in the
// namespace id and its manifest URL, but is redundant under a data dir that is
// already OpenVaultDB's. So "todo-demo.openvaultdb.app/openvaultdb/todos"
// becomes the directories todo-demo.openvaultdb.app/todos. Each segment is
// sanitized; empty/relative segments ("", ".", "..") are dropped to keep
// records within the tree.
func nsDiskPath(nsID string) string {
	parts := strings.Split(nsID, "/")
	// Drop the convention's "openvaultdb" infix at position 1.
	if len(parts) >= 2 && parts[1] == "openvaultdb" {
		parts = append(parts[:1:1], parts[2:]...)
	}
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

// ensureCollectionSchemas creates the inGitDB on-disk layout for every seeded
// vault×namespace×collection: the collection directory with its
// .collection/definition.yaml and an empty $records/ directory, plus the
// per-vault .ingitdb/root-collections.yaml mapping each collection's dalgo ID to
// its (nested) on-disk path. The vault project root must exist before
// dalgo2ingitdb.NewDatabase stats it, so this runs at startup.
func (s *Store) ensureCollectionSchemas() error {
	for _, v := range s.vaults {
		rootCollections := map[string]string{}
		for _, ns := range s.namespaces {
			for _, col := range ns.Collections {
				dir := s.collectionDir(v.ID, ns.ID, col)
				if err := os.MkdirAll(filepath.Join(dir, "$records"), 0o755); err != nil {
					return err
				}
				schemaDir := filepath.Join(dir, ".collection")
				if err := os.MkdirAll(schemaDir, 0o755); err != nil {
					return err
				}
				schemaPath := filepath.Join(schemaDir, "definition.yaml")
				if _, err := os.Stat(schemaPath); err != nil {
					if !os.IsNotExist(err) {
						return err
					}
					if err := os.WriteFile(schemaPath, []byte(tasksCollectionDefinition), 0o644); err != nil {
						return err
					}
				}
				rootCollections[collectionID(ns.ID, col)] = collectionRelPath(ns.ID, col)
			}
		}
		if err := s.writeRootCollections(v.ID, rootCollections); err != nil {
			return err
		}
	}
	return nil
}

// writeRootCollections writes <vault>/.ingitdb/root-collections.yaml, a flat
// YAML map of collection ID -> relative path that the inGitDB CollectionsReader
// uses to discover collections.
func (s *Store) writeRootCollections(vault string, rootCollections map[string]string) error {
	dir := filepath.Join(s.vaultDir(vault), ".ingitdb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ids := make([]string, 0, len(rootCollections))
	for id := range rootCollections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "%s: %s\n", id, rootCollections[id])
	}
	return os.WriteFile(filepath.Join(dir, "root-collections.yaml"), []byte(b.String()), 0o644)
}

// openDatabases opens one inGitDB-backed dal.DB per vault, rooted at the vault
// project directory (which ensureCollectionSchemas has already created).
func (s *Store) openDatabases() error {
	for _, v := range s.vaults {
		db, err := dalgo2ingitdb.NewDatabase(s.vaultDir(v.ID), validator.NewCollectionsReader())
		if err != nil {
			return fmt.Errorf("open inGitDB for vault %q: %w", v.ID, err)
		}
		s.dbs[v.ID] = db
	}
	return nil
}

func (s *Store) dbFor(vault string) (dal.DB, error) {
	db, ok := s.dbs[vault]
	if !ok {
		return nil, fmt.Errorf("no database for vault %q", vault)
	}
	return db, nil
}

// ListRecords returns all records in a collection, each reconstructed with its
// id (the record's $key / filename) re-attached.
func (s *Store) ListRecords(vault, nsID, collection string) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbFor(vault)
	if err != nil {
		return nil, err
	}
	colID := collectionID(nsID, collection)
	ctx := context.Background()

	q := dal.From(dal.NewRootCollectionRef(colID, "")).NewQuery().
		SelectIntoRecord(func() dal.Record {
			return dal.NewRecordWithData(dal.NewKeyWithID(colID, ""), map[string]any{})
		})
	reader, err := db.ExecuteQueryToRecordsReader(ctx, q)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	out := []Record{}
	for {
		r, err := reader.Next()
		if err == dal.ErrNoMoreRecords {
			break
		}
		if err != nil {
			return nil, err
		}
		rec := recordFromData(r.Data(), fmt.Sprintf("%v", r.Key().ID))
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["id"]) < fmt.Sprint(out[j]["id"])
	})
	return out, nil
}

// CreateRecord persists a new record, generating id/createdAt/done defaults. The
// id is the record's $key (filename); it is not stored inside the record file.
func (s *Store) CreateRecord(vault, nsID, collection string, rec Record) (Record, error) {
	if rec == nil {
		rec = Record{}
	}
	id, _ := rec["id"].(string)
	if id == "" {
		id = uuid.NewString()
	}
	rec["id"] = id
	if _, ok := rec["createdAt"]; !ok {
		rec["createdAt"] = time.Now().UTC().Format(time.RFC3339)
	}
	if _, ok := rec["done"]; !ok {
		rec["done"] = false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbFor(vault)
	if err != nil {
		return nil, err
	}
	colID := collectionID(nsID, collection)
	dataRec := dal.NewRecordWithData(dal.NewKeyWithID(colID, id), dataWithoutID(rec))
	err = db.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Insert(ctx, dataRec)
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// GetRecord loads a single record by id, re-attaching id.
func (s *Store) GetRecord(vault, nsID, collection, id string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getRecordLocked(vault, nsID, collection, id)
}

func (s *Store) getRecordLocked(vault, nsID, collection, id string) (Record, bool, error) {
	db, err := s.dbFor(vault)
	if err != nil {
		return nil, false, err
	}
	colID := collectionID(nsID, collection)
	rec := dal.NewRecordWithData(dal.NewKeyWithID(colID, id), map[string]any{})
	if err := db.Get(context.Background(), rec); err != nil {
		if dal.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return recordFromData(rec.Data(), id), true, nil
}

// UpdateRecord merges a patch into an existing record and rewrites it.
func (s *Store) UpdateRecord(vault, nsID, collection, id string, patch Record) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok, err := s.getRecordLocked(vault, nsID, collection, id)
	if err != nil || !ok {
		return nil, ok, err
	}
	for k, v := range patch {
		if k == "id" {
			continue
		}
		cur[k] = v
	}
	cur["id"] = id

	db, err := s.dbFor(vault)
	if err != nil {
		return nil, true, err
	}
	colID := collectionID(nsID, collection)
	dataRec := dal.NewRecordWithData(dal.NewKeyWithID(colID, id), dataWithoutID(cur))
	err = db.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Set(ctx, dataRec)
	})
	if err != nil {
		return nil, true, err
	}
	return cur, true, nil
}

// DeleteRecord removes a record by id, returning ok=false (no error) if missing.
func (s *Store) DeleteRecord(vault, nsID, collection, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok, err := s.getRecordLocked(vault, nsID, collection, id); err != nil || !ok {
		return ok, err
	}
	db, err := s.dbFor(vault)
	if err != nil {
		return false, err
	}
	colID := collectionID(nsID, collection)
	key := dal.NewKeyWithID(colID, id)
	err = db.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Delete(ctx, key)
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// dataWithoutID returns a copy of rec with the "id" field removed; the id is the
// record's $key (filename) in inGitDB and must not be stored in the file.
func dataWithoutID(rec Record) map[string]any {
	out := make(map[string]any, len(rec))
	for k, v := range rec {
		if k == "id" {
			continue
		}
		out[k] = v
	}
	return out
}

// recordFromData builds a Record from an inGitDB record payload, re-attaching id.
func recordFromData(data any, id string) Record {
	rec := Record{}
	if m, ok := data.(map[string]any); ok {
		for k, v := range m {
			rec[k] = v
		}
	}
	rec["id"] = id
	return rec
}

// tasksCollectionDefinition is the inGitDB collection definition for `tasks`,
// written to <collection>/.collection/definition.yaml. One JSON file per record
// under $records/, keyed by the record id ({key}). The id is the $key and is not
// a stored column.
const tasksCollectionDefinition = `titles:
  en: Tasks
record_file:
  name: "{key}.json"
  type: "map[string]any"
  format: json
columns:
  title:
    type: string
    required: true
  done:
    type: bool
  createdAt:
    type: string
columns_order:
  - title
  - done
  - createdAt
`
