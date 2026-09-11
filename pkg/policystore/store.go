// Package policystore publishes immutable filesystem-owned policy generations.
// Its APIs are for trusted owner administration; they are not HTTP permissions.
package policystore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/gofrs/flock"
)

var ErrConflict = errors.New("policy generation revision conflict")

const maxGenerationBytes = 16 << 20
const maxPolicies = 100

type Owner struct {
	Database string `json:"database"`
	Realm    string `json:"realm"`
	Enabled  bool   `json:"enabled"`
}
type Document struct {
	ID       string `json:"id"`
	File     string `json:"file"`
	Revision string `json:"revision"`
}
type manifest struct {
	Version   string     `json:"version"`
	Owner     Owner      `json:"owner"`
	Documents []Document `json:"documents"`
}
type pointer struct {
	Generation string `json:"generation"`
}
type Snapshot struct {
	Revision  string
	Owner     Owner
	Documents []Document
	policies  []access.Policy
}

func (s Snapshot) Policies() []access.Policy { return append([]access.Policy(nil), s.policies...) }

type Store struct {
	root  string
	owner Owner
	phase func(string) error
}

// Open opens an owner store without activating a generation. The caller must
// Load before serving an enabled owner. An absent pointer is never disabled ACL.
func Open(root string, owner Owner) (*Store, error) {
	if !owner.Enabled || owner.Database == "" {
		return nil, fmt.Errorf("enabled owner database is required")
	}
	if owner.Realm != "" {
		if err := (access.PrincipalRef{Realm: owner.Realm, Kind: access.PrincipalKindService, ID: "validation"}).Validate(); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("policy root must be a real directory")
	}
	return &Store{root: root, owner: owner}, nil
}

// Load verifies the selected manifest and every canonical document before
// compiling the complete policy set. Unreferenced generations are ignored.
func (s *Store) Load(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	data, err := s.read("active.json", 4096)
	if err != nil {
		return Snapshot{}, err
	}
	var active pointer
	if err = strictJSON(data, &active); err != nil {
		return Snapshot{}, err
	}
	return s.loadGeneration(ctx, active.Generation)
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func validDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func strictJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing generation content")
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(canonical, data) {
		return fmt.Errorf("noncanonical or duplicate generation content")
	}
	return nil
}
func (s *Store) read(name string, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	// Reject symlinks in every component, including generation directories.
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := range parts {
		if parts[i] == "" || parts[i] == "." || parts[i] == ".." {
			return nil, fmt.Errorf("invalid policy-store path")
		}
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink in policy-store path")
		}
	}
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("generation member must be regular")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("generation member changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("generation member exceeds limit")
	}
	return data, nil
}
func (s *Store) loadGeneration(ctx context.Context, revision string) (Snapshot, error) {
	if !validDigest(revision) {
		return Snapshot{}, fmt.Errorf("invalid generation reference")
	}
	prefix := filepath.Join("generations", revision)
	data, err := s.read(filepath.Join(prefix, "manifest.json"), maxGenerationBytes)
	if err != nil {
		return Snapshot{}, err
	}
	if digest(data) != revision {
		return Snapshot{}, fmt.Errorf("generation manifest digest mismatch")
	}
	var m manifest
	if err = strictJSON(data, &m); err != nil {
		return Snapshot{}, err
	}
	if m.Version != "ovdb.policy-generation/v1" || m.Owner != s.owner || len(m.Documents) == 0 || len(m.Documents) > maxPolicies {
		return Snapshot{}, fmt.Errorf("invalid generation owner or document set")
	}
	paths := make([]string, 0, len(m.Documents))
	seen := map[string]bool{}
	total := len(data)
	for _, entry := range m.Documents {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		if entry.ID == "" || seen[entry.ID] || !validDigest(entry.Revision) || entry.File != "policies/"+entry.Revision+".json" {
			return Snapshot{}, fmt.Errorf("invalid generation document")
		}
		seen[entry.ID] = true
		body, err := s.read(filepath.Join(prefix, entry.File), 1<<20)
		if err != nil {
			return Snapshot{}, err
		}
		total += len(body)
		if total > maxGenerationBytes {
			return Snapshot{}, fmt.Errorf("generation exceeds byte limit")
		}
		if digest(body) != entry.Revision {
			return Snapshot{}, fmt.Errorf("policy document digest mismatch")
		}
		doc, err := access.ParseDTQLPolicy(body)
		if err != nil {
			return Snapshot{}, err
		}
		if doc.Metadata.Name != entry.ID {
			return Snapshot{}, fmt.Errorf("policy identity mismatch")
		}
		paths = append(paths, filepath.Join(prefix, entry.File))
	}
	policies, err := access.LoadPolicyFiles(s.root, access.FilePolicyConfig{Enabled: true, Database: s.owner.Database, Realm: s.owner.Realm, Policies: paths})
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Revision: revision, Owner: m.Owner, Documents: append([]Document(nil), m.Documents...), policies: policies}, nil
}
func syncDir(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return file.Sync()
}
func writeSync(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
func (s *Store) checkpoint(phase string) error {
	if s.phase != nil {
		return s.phase(phase)
	}
	return nil
}

// Activate publishes one complete set using whole-owner CAS. expected is empty
// only for first activation. Per-document administration can merge under this
// same owner lock in a future management API; public ETags are document hashes.
func (s *Store) Activate(ctx context.Context, expected string, documents []access.DTQLDocument) (Snapshot, error) {
	if len(documents) == 0 || len(documents) > maxPolicies {
		return Snapshot{}, fmt.Errorf("generation requires 1..100 policies")
	}
	if expected != "" && !validDigest(expected) {
		return Snapshot{}, fmt.Errorf("invalid expected generation")
	}
	lockPath := filepath.Join(s.root, ".publication.lock")
	if info, err := os.Lstat(lockPath); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return Snapshot{}, fmt.Errorf("invalid publication lock")
	}
	lock := flock.New(lockPath)
	acquired, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return Snapshot{}, err
	}
	if !acquired {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		return Snapshot{}, fmt.Errorf("publication lock unavailable")
	}
	defer func() { _ = lock.Unlock() }()
	current, err := s.Load(ctx)
	if err != nil && (expected != "" || !errors.Is(err, os.ErrNotExist)) {
		return Snapshot{}, err
	}
	// A missing generation referenced by an existing active pointer must not
	// masquerade as an uninitialized store.
	if err != nil {
		if _, statErr := os.Lstat(filepath.Join(s.root, "active.json")); !errors.Is(statErr, os.ErrNotExist) {
			return Snapshot{}, err
		}
	}
	if current.Revision != expected {
		return Snapshot{}, ErrConflict
	}
	m := manifest{Version: "ovdb.policy-generation/v1", Owner: s.owner}
	bodies := map[string][]byte{}
	seen := map[string]bool{}
	total := 0
	for _, doc := range documents {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		if doc.Target.Database != s.owner.Database || seen[doc.Metadata.Name] {
			return Snapshot{}, fmt.Errorf("duplicate identity or wrong policy target")
		}
		seen[doc.Metadata.Name] = true
		body, err := access.MarshalDTQLPolicyJSON(doc)
		if err != nil {
			return Snapshot{}, err
		}
		total += len(body)
		if len(body) > 1<<20 || total > maxGenerationBytes {
			return Snapshot{}, fmt.Errorf("generation exceeds byte limit")
		}
		revision := digest(body)
		name := "policies/" + revision + ".json"
		m.Documents = append(m.Documents, Document{ID: doc.Metadata.Name, File: name, Revision: revision})
		bodies[name] = body
	}
	sort.Slice(m.Documents, func(i, j int) bool { return m.Documents[i].ID < m.Documents[j].ID })
	data, err := json.Marshal(m)
	if err != nil {
		return Snapshot{}, err
	}
	revision := digest(data)
	generations := filepath.Join(s.root, "generations")
	if err = os.MkdirAll(generations, 0700); err != nil {
		return Snapshot{}, err
	}
	info, err := os.Lstat(generations)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Snapshot{}, fmt.Errorf("invalid generations directory")
	}
	destination := filepath.Join(generations, revision)
	if _, err = os.Lstat(destination); errors.Is(err, os.ErrNotExist) {
		stage, err := os.MkdirTemp(generations, ".pending-")
		if err != nil {
			return Snapshot{}, err
		}
		defer func() { _ = os.RemoveAll(stage) }()
		if err = os.Mkdir(filepath.Join(stage, "policies"), 0700); err != nil {
			return Snapshot{}, err
		}
		for name, body := range bodies {
			if err = writeSync(filepath.Join(stage, name), body); err != nil {
				return Snapshot{}, err
			}
		}
		if err = writeSync(filepath.Join(stage, "manifest.json"), data); err != nil {
			return Snapshot{}, err
		}
		if err = syncDir(filepath.Join(stage, "policies")); err != nil {
			return Snapshot{}, err
		}
		if err = syncDir(stage); err != nil {
			return Snapshot{}, err
		}
		if err = s.checkpoint("generation-synced"); err != nil {
			return Snapshot{}, err
		}
		if err = os.Rename(stage, destination); err != nil {
			return Snapshot{}, err
		}
		if err = syncDir(generations); err != nil {
			return Snapshot{}, err
		}
	} else if err != nil {
		return Snapshot{}, err
	}
	candidate, err := s.loadGeneration(ctx, revision)
	if err != nil {
		return Snapshot{}, err
	}
	if err = s.checkpoint("generation-published"); err != nil {
		return Snapshot{}, err
	}
	if err = ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	activeData, _ := json.Marshal(pointer{Generation: revision})
	temp, err := os.CreateTemp(s.root, ".active-")
	if err != nil {
		return Snapshot{}, err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err = temp.Write(activeData); err != nil {
		_ = temp.Close()
		return Snapshot{}, err
	}
	if err = temp.Sync(); err != nil {
		_ = temp.Close()
		return Snapshot{}, err
	}
	if err = temp.Close(); err != nil {
		return Snapshot{}, err
	}
	if err = s.checkpoint("pointer-synced"); err != nil {
		return Snapshot{}, err
	}
	if err = ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if err = os.Rename(tempPath, filepath.Join(s.root, "active.json")); err != nil {
		return Snapshot{}, err
	}
	// Errors after the rename are uncertain publication, never rollback.
	if err = syncDir(s.root); err != nil {
		return Snapshot{}, fmt.Errorf("generation may be active: %w", err)
	}
	if err = s.checkpoint("pointer-published"); err != nil {
		return Snapshot{}, fmt.Errorf("generation may be active: %w", err)
	}
	return candidate, nil
}
