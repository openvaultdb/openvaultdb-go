package policystore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/gofrs/flock"
)

func TestLeaseAcquireAndRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t, t.TempDir())
	first, err := store.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewController(ctx, store)
	if err != nil {
		t.Fatal(err)
	}

	lease, err := c.AcquirePolicyLease(ctx)
	if err != nil {
		t.Fatalf("AcquirePolicyLease failed: %v", err)
	}
	if lease.Revision() != first.Revision {
		t.Fatalf("Revision = %q, want %q", lease.Revision(), first.Revision)
	}
	if len(lease.Policies()) != 1 {
		t.Fatalf("Policies = %d, want 1", len(lease.Policies()))
	}
	lease.Release()
	lease.Release() // multiple releases safe

	// Cancelled context
	cancCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.AcquirePolicyLease(cancCtx); err == nil {
		t.Fatal("expected error for cancelled ctx")
	}

	// Active == nil
	cEmpty := &Controller{store: store}
	if _, err := cEmpty.AcquirePolicyLease(ctx); err == nil {
		t.Fatal("expected error when c.active == nil")
	}

	// TryRLock contention loop with success
	cContended, err := NewController(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	cContended.mu.Lock()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(15 * time.Millisecond)
		cContended.mu.Unlock()
	}()
	lease2, err := cContended.AcquirePolicyLease(ctx)
	wg.Wait()
	if err != nil {
		t.Fatalf("expected lease after contention, got: %v", err)
	}
	lease2.Release()

	// TryRLock contention loop with context cancellation
	cContended2, err := NewController(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	cContended2.mu.Lock()
	cancCtx2, cancel2 := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel2()
	_, err = cContended2.AcquirePolicyLease(cancCtx2)
	cContended2.mu.Unlock()
	if err == nil {
		t.Fatal("expected timeout/cancellation error while holding lock")
	}
}

func TestControllerErrorsAndMethods(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Nil store
	if _, err := NewController(ctx, nil); err == nil {
		t.Fatal("expected error for nil store")
	}

	// Uninitialized store
	emptyStore := openTestStore(t, t.TempDir())
	if _, err := NewController(ctx, emptyStore); err == nil {
		t.Fatal("expected error for uninitialized store")
	}

	// Initialized store
	store := openTestStore(t, t.TempDir())
	first, err := store.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")})
	if err != nil {
		t.Fatal(err)
	}
	_ = first
	c, err := NewController(ctx, store)
	if err != nil {
		t.Fatal(err)
	}

	// Policies with cancelled ctx
	cancCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Policies(cancCtx); err == nil {
		t.Fatal("expected error for cancelled ctx in Policies")
	}

	// Policies success
	pols, err := c.Policies(ctx)
	if err != nil || len(pols) != 1 {
		t.Fatalf("unexpected policies: %v", err)
	}

	// Snapshot.Policies directly
	snap, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Policies()) != 1 {
		t.Fatal("Snapshot.Policies returned wrong count")
	}

	// Snapshot when active == nil
	cNoActive := &Controller{store: store}
	if _, err := cNoActive.Snapshot(); err == nil {
		t.Fatal("expected error from Snapshot when active is nil")
	}
	if _, err := cNoActive.Policies(ctx); err == nil {
		t.Fatal("expected error from Policies when active is nil")
	}

	// publishErr != nil AND loadErr != nil
	storeErrBoth := openTestStore(t, t.TempDir())
	firstBoth, err := storeErrBoth.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")})
	if err != nil {
		t.Fatal(err)
	}
	cBoth, err := NewController(ctx, storeErrBoth)
	if err != nil {
		t.Fatal(err)
	}
	storeErrBoth.phase = func(phase string) error {
		_ = os.Remove(filepath.Join(storeErrBoth.root, "active.json"))
		return errors.New("simulated publish failure")
	}
	if _, err := cBoth.Activate(ctx, firstBoth.Revision, []access.TugQLDocument{policy(t, "email")}); err == nil {
		t.Fatal("expected error")
	}

	// publishErr == nil AND loadErr != nil
	storeErrLoad := openTestStore(t, t.TempDir())
	firstLoad, err := storeErrLoad.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")})
	if err != nil {
		t.Fatal(err)
	}
	cLoad, err := NewController(ctx, storeErrLoad)
	if err != nil {
		t.Fatal(err)
	}
	storeErrLoad.phase = func(phase string) error {
		if phase == "pointer-published" {
			_ = os.Remove(filepath.Join(storeErrLoad.root, "active.json"))
		}
		return nil
	}
	if _, err := cLoad.Activate(ctx, firstLoad.Revision, []access.TugQLDocument{policy(t, "email")}); err == nil {
		t.Fatal("expected load error after pointer-published")
	}
}

func TestStoreOpenValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Not enabled
	if _, err := Open(dir, Owner{Enabled: false, Database: "crm"}); err == nil {
		t.Fatal("expected error for disabled owner")
	}

	// Empty database
	if _, err := Open(dir, Owner{Enabled: true, Database: ""}); err == nil {
		t.Fatal("expected error for empty database")
	}

	// Invalid realm
	if _, err := Open(dir, Owner{Enabled: true, Database: "crm", Realm: " bad "}); err == nil {
		t.Fatal("expected error for invalid realm")
	}

	// Root is a regular file
	filePath := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(filePath, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filePath, Owner{Enabled: true, Database: "crm"}); err == nil {
		t.Fatal("expected error when root is a file")
	}

	// Root is a symlink
	realDir := filepath.Join(dir, "real")
	_ = os.Mkdir(realDir, 0700)
	symDir := filepath.Join(dir, "symlink")
	if err := os.Symlink(realDir, symDir); err == nil {
		if _, err := Open(symDir, Owner{Enabled: true, Database: "crm"}); err == nil {
			t.Fatal("expected error when root is a symlink")
		}
	}
}

func TestStoreValidationAndErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// validDigest
	if validDigest("short") {
		t.Error("expected false for short digest")
	}
	if validDigest("ABCDEF" + strings.Repeat("0", 58)) {
		t.Error("expected false for uppercase digest")
	}
	if validDigest(strings.Repeat("z", 64)) {
		t.Error("expected false for non-hex digest")
	}
	valid := sha256.Sum256([]byte("test"))
	if !validDigest(hex.EncodeToString(valid[:])) {
		t.Error("expected true for valid digest")
	}

	// strictJSON
	var target pointer
	if err := strictJSON([]byte("not json"), &target); err == nil {
		t.Error("expected error for invalid json")
	}
	if err := strictJSON([]byte(`{"generation":"abc"} 123`), &target); err == nil {
		t.Error("expected error for trailing content")
	}

	// syncDir
	if err := syncDir("/nonexistent/directory/path"); err == nil {
		t.Error("expected error from syncDir on nonexistent dir")
	}

	// read error paths
	store := openTestStore(t, t.TempDir())
	if _, err := store.read("//invalid", 100); err == nil {
		t.Error("expected error for // path")
	}
	if _, err := store.read("..", 100); err == nil {
		t.Error("expected error for .. path")
	}
	if _, err := store.read(".", 100); err == nil {
		t.Error("expected error for . path")
	}
	if _, err := store.read("missing_subdir/file.txt", 100); err == nil {
		t.Error("expected error for missing subdir in read")
	}

	// Non-regular file (directory)
	dirName := "testdir"
	_ = os.Mkdir(filepath.Join(store.root, dirName), 0700)
	if _, err := store.read(dirName, 100); err == nil {
		t.Error("expected error when reading a directory")
	}

	// Exceeds limit
	smallFile := "small.txt"
	_ = os.WriteFile(filepath.Join(store.root, smallFile), []byte("1234567890"), 0600)
	if _, err := store.read(smallFile, 5); err == nil {
		t.Error("expected error when file exceeds limit")
	}

	// Load with cancelled context
	cancCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Load(cancCtx); err == nil {
		t.Error("expected error when loading with cancelled ctx")
	}
}

func TestStoreLoadGenerationValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t, t.TempDir())

	// Invalid digest reference
	if _, err := store.loadGeneration(ctx, "invalid-digest"); err == nil {
		t.Error("expected error for invalid generation digest")
	}

	// Digest mismatch
	dummyHash := hex.EncodeToString(make([]byte, 32))
	genDir := filepath.Join(store.root, "generations", dummyHash)
	_ = os.MkdirAll(genDir, 0700)
	_ = os.WriteFile(filepath.Join(genDir, "manifest.json"), []byte(`{"version":"v1"}`), 0600)
	if _, err := store.loadGeneration(ctx, dummyHash); err == nil {
		t.Error("expected error for manifest digest mismatch")
	}

	// Manifest with invalid version or owner
	writeManifest := func(m manifest) string {
		data, _ := json.Marshal(m)
		rev := digest(data)
		d := filepath.Join(store.root, "generations", rev)
		_ = os.MkdirAll(filepath.Join(d, "policies"), 0700)
		_ = os.WriteFile(filepath.Join(d, "manifest.json"), data, 0600)
		return rev
	}

	// Wrong version
	r1 := writeManifest(manifest{Version: "bad/v1", Owner: store.owner, Documents: []Document{{ID: "d1"}}})
	if _, err := store.loadGeneration(ctx, r1); err == nil {
		t.Error("expected error for wrong manifest version")
	}

	// Empty documents
	r2 := writeManifest(manifest{Version: "ovdb.policy-generation/v1", Owner: store.owner, Documents: []Document{}})
	if _, err := store.loadGeneration(ctx, r2); err == nil {
		t.Error("expected error for empty documents")
	}

	// More than 100 documents
	manyDocs := make([]Document, 101)
	r3 := writeManifest(manifest{Version: "ovdb.policy-generation/v1", Owner: store.owner, Documents: manyDocs})
	if _, err := store.loadGeneration(ctx, r3); err == nil {
		t.Error("expected error for > 100 documents")
	}

	// Document with empty ID
	r4 := writeManifest(manifest{Version: "ovdb.policy-generation/v1", Owner: store.owner, Documents: []Document{{ID: ""}}})
	if _, err := store.loadGeneration(ctx, r4); err == nil {
		t.Error("expected error for empty document ID")
	}

	// Document with duplicate ID
	r5 := writeManifest(manifest{Version: "ovdb.policy-generation/v1", Owner: store.owner, Documents: []Document{
		{ID: "d1", Revision: dummyHash, File: "policies/" + dummyHash + ".json"},
		{ID: "d1", Revision: dummyHash, File: "policies/" + dummyHash + ".json"},
	}})
	if _, err := store.loadGeneration(ctx, r5); err == nil {
		t.Error("expected error for duplicate document ID")
	}

	// Document with wrong file name
	r6 := writeManifest(manifest{Version: "ovdb.policy-generation/v1", Owner: store.owner, Documents: []Document{
		{ID: "d1", Revision: dummyHash, File: "policies/other.json"},
	}})
	if _, err := store.loadGeneration(ctx, r6); err == nil {
		t.Error("expected error for wrong document file name")
	}

	// Document file digest mismatch
	polFile := filepath.Join(store.root, "generations", dummyHash, "policies", dummyHash+".json")
	_ = os.WriteFile(polFile, []byte("mismatched content"), 0600)
	r7 := writeManifest(manifest{Version: "ovdb.policy-generation/v1", Owner: store.owner, Documents: []Document{
		{ID: "d1", Revision: dummyHash, File: "policies/" + dummyHash + ".json"},
	}})
	if _, err := store.loadGeneration(ctx, r7); err == nil {
		t.Error("expected error for policy document digest mismatch")
	}

	// Document with corrupt TugQL policy
	corruptBytes := []byte("invalid policy")
	corruptRev := digest(corruptBytes)
	cDir := filepath.Join(store.root, "generations")
	r8 := writeManifest(manifest{Version: "ovdb.policy-generation/v1", Owner: store.owner, Documents: []Document{
		{ID: "d1", Revision: corruptRev, File: "policies/" + corruptRev + ".json"},
	}})
	_ = os.WriteFile(filepath.Join(cDir, r8, "policies", corruptRev+".json"), corruptBytes, 0600)
	if _, err := store.loadGeneration(ctx, r8); err == nil {
		t.Error("expected error for corrupt TugQL policy")
	}

	// Document identity mismatch (Metadata.Name != entry.ID)
	doc := policy(t, "name")
	docBytes, _ := access.MarshalTugQLPolicyJSON(doc)
	docRev := digest(docBytes)
	r9 := writeManifest(manifest{Version: "ovdb.policy-generation/v1", Owner: store.owner, Documents: []Document{
		{ID: "different_name", Revision: docRev, File: "policies/" + docRev + ".json"},
	}})
	_ = os.WriteFile(filepath.Join(cDir, r9, "policies", docRev+".json"), docBytes, 0600)
	if _, err := store.loadGeneration(ctx, r9); err == nil {
		t.Error("expected error for document identity mismatch")
	}

	// Context cancelled during loadGeneration loop
	cancCtxLoop, cancelLoop := context.WithCancel(ctx)
	cancelLoop()
	if _, err := store.loadGeneration(cancCtxLoop, r9); err == nil {
		t.Error("expected context cancelled error in loadGeneration")
	}
}

func TestStoreActivateValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t, t.TempDir())

	// 0 policies
	if _, err := store.Activate(ctx, "", nil); err == nil {
		t.Error("expected error for 0 policies")
	}

	// > 100 policies
	manyPols := make([]access.TugQLDocument, 101)
	for i := range manyPols {
		manyPols[i] = policy(t, "name")
	}
	if _, err := store.Activate(ctx, "", manyPols); err == nil {
		t.Error("expected error for > 100 policies")
	}

	// Invalid expected generation digest
	if _, err := store.Activate(ctx, "invalid_digest", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Error("expected error for invalid expected digest")
	}

	// Publication lock is a directory
	lockPath := filepath.Join(store.root, ".publication.lock")
	_ = os.Mkdir(lockPath, 0700)
	if _, err := store.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Error("expected error when publication lock is a directory")
	}
	_ = os.RemoveAll(lockPath)

	// Lock unavailable: lock held by another
	fl := flock.New(lockPath)
	locked, _ := fl.TryLock()
	if locked {
		cancCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		if _, err := store.Activate(cancCtx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
			t.Error("expected error when publication lock is held")
		}
		_ = fl.Unlock()
	}

	// Duplicate policy name in Activate
	p1 := policy(t, "name")
	p2 := policy(t, "email")
	p2.Metadata.Name = p1.Metadata.Name
	if _, err := store.Activate(ctx, "", []access.TugQLDocument{p1, p2}); err == nil {
		t.Error("expected error for duplicate policy name")
	}

	// Context cancelled during Activate documents loop
	cancCtxDoc, cancelDoc := context.WithCancel(ctx)
	cancelDoc()
	if _, err := store.Activate(cancCtxDoc, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Error("expected context cancelled error in Activate documents loop")
	}

	// Context cancelled after pointer-synced
	storeCancel := openTestStore(t, t.TempDir())
	ctxPtr, cancelPtr := context.WithCancel(ctx)
	storeCancel.phase = func(phase string) error {
		if phase == "pointer-synced" {
			cancelPtr()
		}
		return nil
	}
	if _, err := storeCancel.Activate(ctxPtr, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Error("expected context cancelled error after pointer-synced")
	}

	// os.Lstat(destination) error other than ErrNotExist
	storeLstat := openTestStore(t, t.TempDir())
	genDir := filepath.Join(storeLstat.root, "generations")
	_ = os.MkdirAll(genDir, 0700)
	_ = os.Chmod(genDir, 0000)
	if _, err := storeLstat.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Error("expected error when generations directory is not accessible")
	}
	_ = os.Chmod(genDir, 0700)

	// Generations dir is a regular file
	store2 := openTestStore(t, t.TempDir())
	_ = os.WriteFile(filepath.Join(store2.root, "generations"), []byte("file"), 0600)
	if _, err := store2.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Error("expected error when generations path is a file")
	}

	// Checkpoint failure during activate
	store3 := openTestStore(t, t.TempDir())
	for _, failPhase := range []string{"generation-synced", "generation-published", "pointer-synced"} {
		store3.phase = func(phase string) error {
			if phase == failPhase {
				return errors.New("simulated phase failure: " + failPhase)
			}
			return nil
		}
		if _, err := store3.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
			t.Errorf("expected error when checkpoint failed at %s", failPhase)
		}
	}
}

type errSyncWriter struct {
	failWrite bool
	failSync  bool
}

func (e *errSyncWriter) Write(p []byte) (int, error) {
	if e.failWrite {
		return 0, errors.New("write error")
	}
	return len(p), nil
}

func (e *errSyncWriter) Sync() error {
	if e.failSync {
		return errors.New("sync error")
	}
	return nil
}

func (e *errSyncWriter) Close() error {
	return nil
}

type errTempFile struct {
	failWrite bool
	failSync  bool
	failClose bool
	name      string
}

func (e *errTempFile) Write(p []byte) (int, error) {
	if e.failWrite {
		return 0, errors.New("temp write error")
	}
	return len(p), nil
}

func (e *errTempFile) Sync() error {
	if e.failSync {
		return errors.New("temp sync error")
	}
	return nil
}

func (e *errTempFile) Close() error {
	if e.failClose {
		return errors.New("temp close error")
	}
	return nil
}

func (e *errTempFile) Name() string {
	return e.name
}

// Not parallel: modifies package-level seam variables
func TestStoreSeams(t *testing.T) {
	origOpenSyncFile := openSyncFile
	origCreateActiveTemp := createActiveTemp
	origSameFile := sameFile
	defer func() {
		openSyncFile = origOpenSyncFile
		createActiveTemp = origCreateActiveTemp
		sameFile = origSameFile
	}()

	ctx := context.Background()

	// sameFile returns false in read
	store := openTestStore(t, t.TempDir())
	first, err := store.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")})
	if err != nil {
		t.Fatal(err)
	}
	sameFile = func(fi1, fi2 os.FileInfo) bool { return false }
	if _, err := store.Load(ctx); err == nil {
		t.Fatal("expected error when sameFile returns false")
	}
	sameFile = origSameFile

	// openSyncFile write error
	openSyncFile = func(path string, flag int, perm os.FileMode) (syncWriter, error) {
		return &errSyncWriter{failWrite: true}, nil
	}
	store2 := openTestStore(t, t.TempDir())
	if _, err := store2.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on openSyncFile write error")
	}

	// openSyncFile sync error
	openSyncFile = func(path string, flag int, perm os.FileMode) (syncWriter, error) {
		return &errSyncWriter{failSync: true}, nil
	}
	store3 := openTestStore(t, t.TempDir())
	if _, err := store3.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on openSyncFile sync error")
	}
	openSyncFile = origOpenSyncFile

	// createActiveTemp error
	createActiveTemp = func(dir, pattern string) (activeTempFile, error) {
		return nil, errors.New("create temp error")
	}
	store4 := openTestStore(t, t.TempDir())
	if _, err := store4.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on createActiveTemp error")
	}

	// createActiveTemp write error
	tmpDir := t.TempDir()
	createActiveTemp = func(dir, pattern string) (activeTempFile, error) {
		return &errTempFile{failWrite: true, name: filepath.Join(tmpDir, "tmp")}, nil
	}
	store5 := openTestStore(t, t.TempDir())
	if _, err := store5.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on temp write error")
	}

	// createActiveTemp sync error
	createActiveTemp = func(dir, pattern string) (activeTempFile, error) {
		return &errTempFile{failSync: true, name: filepath.Join(tmpDir, "tmp")}, nil
	}
	store6 := openTestStore(t, t.TempDir())
	if _, err := store6.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on temp sync error")
	}

	// createActiveTemp close error
	createActiveTemp = func(dir, pattern string) (activeTempFile, error) {
		return &errTempFile{failClose: true, name: filepath.Join(tmpDir, "tmp")}, nil
	}
	store7 := openTestStore(t, t.TempDir())
	if _, err := store7.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on temp close error")
	}
	_ = first
}

func TestControllerActivateSuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t, t.TempDir())
	first, err := store.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewController(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Activate(ctx, first.Revision, []access.TugQLDocument{policy(t, "name")})
	if err != nil {
		t.Fatalf("c.Activate failed: %v", err)
	}
	if snap.Revision == "" {
		t.Fatal("empty revision")
	}
}

func TestStore_LoadGeneration_BadJSONManifest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t, t.TempDir())
	badJSON := []byte("this is not json at all!")
	rev := digest(badJSON)
	genDir := filepath.Join(store.root, "generations", rev)
	if err := os.MkdirAll(genDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "manifest.json"), badJSON, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.loadGeneration(ctx, rev); err == nil {
		t.Fatal("expected error on non-json manifest")
	}
}

func TestStore_LoadGeneration_ExceedsByteLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t, t.TempDir())

	genDir := filepath.Join(store.root, "generations", "temp")
	polDir := filepath.Join(genDir, "policies")
	if err := os.MkdirAll(polDir, 0700); err != nil {
		t.Fatal(err)
	}

	docs := make([]Document, 17)
	for i := 0; i < 17; i++ {
		id := fmt.Sprintf("doc-%d", i)
		pi := policy(t, id)
		pi.Metadata.Name = id
		piBytes, err := access.MarshalTugQLPolicyJSON(pi)
		if err != nil {
			t.Fatal(err)
		}
		padLen := 1000000 - len(piBytes)
		pi.Metadata.Description = strings.Repeat("a", padLen-100)
		piBytes, _ = access.MarshalTugQLPolicyJSON(pi)
		for len(piBytes) < 1000000 {
			pi.Metadata.Description += "x"
			piBytes, _ = access.MarshalTugQLPolicyJSON(pi)
		}
		pRev := digest(piBytes)
		docs[i] = Document{
			ID:       id,
			File:     "policies/" + pRev + ".json",
			Revision: pRev,
		}
		if err := os.WriteFile(filepath.Join(polDir, pRev+".json"), piBytes, 0600); err != nil {
			t.Fatal(err)
		}
	}

	m := manifest{
		Version:   "ovdb.policy-generation/v1",
		Owner:     store.owner,
		Documents: docs,
	}
	mData, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	mRev := digest(mData)

	realGenDir := filepath.Join(store.root, "generations", mRev)
	if err := os.Rename(genDir, realGenDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realGenDir, "manifest.json"), mData, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.loadGeneration(ctx, mRev); err == nil || err.Error() != "generation exceeds byte limit" {
		t.Fatalf("expected generation exceeds byte limit, got %v", err)
	}
}

// Not parallel: mutates package seams.
func TestStore_ErrorBranchesCoverage(t *testing.T) {
	ctx := context.Background()

	// 1. Open osLstat error (line 105)
	origOsLstat := osLstat
	defer func() { osLstat = origOsLstat }()
	osLstat = func(name string) (os.FileInfo, error) {
		return nil, errors.New("lstat error")
	}
	if _, err := Open(t.TempDir(), Owner{Enabled: true, Database: "db"}); err == nil {
		t.Fatal("expected error on osLstat failure in Open")
	}
	osLstat = origOsLstat

	// 2. read osOpenRoot error (line 155)
	origOpenRoot := osOpenRoot
	defer func() { osOpenRoot = origOpenRoot }()
	osOpenRoot = func(path string) (*os.Root, error) {
		return nil, errors.New("openroot error")
	}
	store := openTestStore(t, t.TempDir())
	if _, err := store.read("active.json", 4096); err == nil {
		t.Fatal("expected error on osOpenRoot failure in read")
	}
	osOpenRoot = origOpenRoot

	// 3. read rootLstat error (line 181)
	_ = os.WriteFile(filepath.Join(store.root, "active.json"), []byte("{}"), 0600)
	origRootLstat := rootLstat
	defer func() { rootLstat = origRootLstat }()
	rootLstat = func(root *os.Root, name string) (os.FileInfo, error) {
		return nil, errors.New("root lstat error")
	}
	if _, err := store.read("active.json", 4096); err == nil {
		t.Fatal("expected error on rootLstat failure in read")
	}
	rootLstat = origRootLstat

	// 4. read rootOpen error (line 181)
	origRootOpen := rootOpen
	defer func() { rootOpen = origRootOpen }()
	rootOpen = func(root *os.Root, name string) (*os.File, error) {
		return nil, errors.New("root open error")
	}
	_ = os.WriteFile(filepath.Join(store.root, "active.json"), []byte("{}"), 0600)
	if _, err := store.read("active.json", 4096); err == nil {
		t.Fatal("expected error on rootOpen failure in read")
	}
	rootOpen = origRootOpen

	// 5. read ioReadAll error (line 190)
	origReadAll := ioReadAll
	defer func() { ioReadAll = origReadAll }()
	ioReadAll = func(r io.Reader) ([]byte, error) {
		return nil, errors.New("io readall error")
	}
	if _, err := store.read("active.json", 4096); err == nil {
		t.Fatal("expected error on ioReadAll failure in read")
	}
	ioReadAll = origReadAll

	// 6. loadPolicyFiles error (line 249)
	origLoadPolicyFiles := loadPolicyFiles
	defer func() { loadPolicyFiles = origLoadPolicyFiles }()
	loadPolicyFiles = func(root string, cfg access.FilePolicyConfig) ([]access.Policy, error) {
		return nil, errors.New("load policy files error")
	}
	validStore := openTestStore(t, t.TempDir())
	if _, err := validStore.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error when loadPolicyFiles fails")
	}
	loadPolicyFiles = origLoadPolicyFiles

	// 7. openSyncFile error in writeSync (line 264)
	origOpenSyncFile := openSyncFile
	defer func() { openSyncFile = origOpenSyncFile }()
	openSyncFile = func(path string, flag int, perm os.FileMode) (syncWriter, error) {
		return nil, errors.New("open sync file error")
	}
	syncStore := openTestStore(t, t.TempDir())
	if _, err := syncStore.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error when openSyncFile fails")
	}
	openSyncFile = origOpenSyncFile

	// 8. TryLockContext unavailable (line 303-306)
	origPubLock := newPublicationLock
	defer func() { newPublicationLock = origPubLock }()

	newPublicationLock = func(path string) publicationLock {
		return &mockPubLock{acquired: false}
	}
	cancCtx, cancel := context.WithCancel(ctx)
	cancel()
	lockStore := openTestStore(t, t.TempDir())
	if _, err := lockStore.Activate(cancCtx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on canceled ctx when lock unavailable")
	}

	// Lock unavailable with normal ctx (line 306)
	if _, err := lockStore.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected publication lock unavailable error")
	}

	// TryLockContext error (line 300)
	newPublicationLock = func(path string) publicationLock {
		return &mockPubLock{err: errors.New("lock error")}
	}
	if _, err := lockStore.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error when TryLockContext returns error")
	}
	newPublicationLock = origPubLock

	// 9. Load error when expected != "" (line 311)
	uninitStore := openTestStore(t, t.TempDir())
	nonEmptyRev := strings.Repeat("a", 64)
	if _, err := uninitStore.Activate(ctx, nonEmptyRev, []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on uninitialized store with non-empty expected")
	}

	// 10. marshalPolicy error (line 337)
	origMarshalPolicy := marshalPolicy
	defer func() { marshalPolicy = origMarshalPolicy }()
	marshalPolicy = func(doc access.TugQLDocument) ([]byte, error) {
		return nil, errors.New("marshal error")
	}
	mStore := openTestStore(t, t.TempDir())
	if _, err := mStore.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error when marshalPolicy fails")
	}
	marshalPolicy = origMarshalPolicy

	// 10a. ctx.Err() in documents loop (line 338)
	doc1 := policy(t, "doc1")
	doc2 := policy(t, "doc2")
	loopCtx, loopCancel := context.WithCancel(ctx)
	marshalPolicy = func(doc access.TugQLDocument) ([]byte, error) {
		loopCancel()
		return origMarshalPolicy(doc)
	}
	loopStore := openTestStore(t, t.TempDir())
	if _, err := loopStore.Activate(loopCtx, "", []access.TugQLDocument{doc1, doc2}); err == nil {
		t.Fatal("expected error on canceled ctx during documents loop")
	}
	marshalPolicy = origMarshalPolicy

	// 11. Policy body > 1<<20 (line 341)
	marshalPolicy = func(doc access.TugQLDocument) ([]byte, error) {
		return make([]byte, (1<<20)+1), nil
	}
	bigStore := openTestStore(t, t.TempDir())
	if _, err := bigStore.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error when policy body exceeds 1MB")
	}
	marshalPolicy = origMarshalPolicy

	// 12. generations directory lstat error (line 366)
	origOsLstat = osLstat
	defer func() { osLstat = origOsLstat }()
	osLstat = func(name string) (os.FileInfo, error) {
		if filepath.Base(name) == "generations" {
			return nil, errors.New("generations lstat error")
		}
		return origOsLstat(name)
	}
	genFileStore := openTestStore(t, t.TempDir())
	if _, err := genFileStore.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error when generations lstat fails")
	}
	osLstat = origOsLstat

	// 13. osMkdirTemp error (line 363)
	origMkdirTemp := osMkdirTemp
	defer func() { osMkdirTemp = origMkdirTemp }()
	osMkdirTemp = func(dir, pattern string) (string, error) {
		return "", errors.New("mkdir temp error")
	}
	tmpStore := openTestStore(t, t.TempDir())
	if _, err := tmpStore.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error when osMkdirTemp fails")
	}
	osMkdirTemp = origMkdirTemp

	// 14. osMkdir policies error (line 367)
	origMkdir := osMkdir
	defer func() { osMkdir = origMkdir }()
	osMkdir = func(name string, perm os.FileMode) error {
		return errors.New("mkdir policies error")
	}
	polStore := openTestStore(t, t.TempDir())
	if _, err := polStore.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error when osMkdir fails")
	}
	osMkdir = origMkdir

	// 14b. writeSync manifest.json error (line 384)
	origOpenSyncFile = openSyncFile
	defer func() { openSyncFile = origOpenSyncFile }()
	openSyncFile = func(path string, flag int, perm os.FileMode) (syncWriter, error) {
		if strings.HasSuffix(path, "manifest.json") {
			return nil, errors.New("open manifest error")
		}
		return origOpenSyncFile(path, flag, perm)
	}
	manStore := openTestStore(t, t.TempDir())
	if _, err := manStore.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on manifest writeSync error")
	}
	openSyncFile = origOpenSyncFile

	// 15. syncDirFn errors (lines 378, 381, 390, 434)
	origSyncDirFn := syncDirFn
	defer func() { syncDirFn = origSyncDirFn }()
	syncDirFn = func(path string) error {
		if strings.HasSuffix(path, "policies") {
			return errors.New("sync policies error")
		}
		return nil
	}
	sdStore1 := openTestStore(t, t.TempDir())
	if _, err := sdStore1.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on sync policies error")
	}

	syncDirFn = func(path string) error {
		if strings.Contains(path, ".pending-") && !strings.HasSuffix(path, "policies") {
			return errors.New("sync stage error")
		}
		return nil
	}
	sdStore2 := openTestStore(t, t.TempDir())
	if _, err := sdStore2.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on sync stage error")
	}

	syncDirFn = func(path string) error {
		if filepath.Base(path) == "generations" {
			return errors.New("sync generations error")
		}
		return nil
	}
	sdStore3 := openTestStore(t, t.TempDir())
	if _, err := sdStore3.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on sync generations error")
	}

	sdStore4 := openTestStore(t, filepath.Join(t.TempDir(), "sd-root"))
	syncDirFn = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(sdStore4.root) {
			return errors.New("sync root error")
		}
		return nil
	}
	if _, err := sdStore4.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on sync root error")
	}
	syncDirFn = origSyncDirFn

	// 16. osRename errors (line 387, 430)
	origRename := osRename
	defer func() { osRename = origRename }()
	osRename = func(oldpath, newpath string) error {
		if strings.Contains(oldpath, ".pending-") {
			return errors.New("rename stage error")
		}
		return origRename(oldpath, newpath)
	}
	renStore1 := openTestStore(t, t.TempDir())
	if _, err := renStore1.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on rename stage error")
	}

	osRename = func(oldpath, newpath string) error {
		if strings.HasSuffix(newpath, "active.json") {
			return errors.New("rename active error")
		}
		return origRename(oldpath, newpath)
	}
	renStore2 := openTestStore(t, t.TempDir())
	if _, err := renStore2.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error on rename active error")
	}
	osRename = origRename

	// 17. ctx.Err() after published (line 403)
	cancellingStore := openTestStore(t, t.TempDir())
	dynCtx, dynCancel := context.WithCancel(ctx)
	cancellingStore.phase = func(p string) error {
		if p == "generation-published" {
			dynCancel()
		}
		return nil
	}
	if _, err := cancellingStore.Activate(dynCtx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error when context canceled at generation-published")
	}

	// 18. loadGeneration candidate error (line 397)
	loadPolicyFiles = func(root string, cfg access.FilePolicyConfig) ([]access.Policy, error) {
		return nil, errors.New("candidate load error")
	}
	candStore := openTestStore(t, t.TempDir())
	if _, err := candStore.Activate(ctx, "", []access.TugQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("expected error when candidate load fails")
	}
	loadPolicyFiles = origLoadPolicyFiles
}

type mockPubLock struct {
	acquired bool
	err      error
}

func (m *mockPubLock) TryLockContext(ctx context.Context, retryDelay time.Duration) (bool, error) {
	return m.acquired, m.err
}

func (m *mockPubLock) Unlock() error { return nil }
