package inferred

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakeTempFile struct {
	name     string
	writeErr error
	closeErr error
}

func (f *fakeTempFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *fakeTempFile) Close() error {
	return f.closeErr
}

func (f *fakeTempFile) Name() string {
	return f.name
}

func TestLoadAndPersistErrorPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// 1. os.Open fails with non-IsNotExist (ENOTDIR: parent is a regular file).
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(blocker, "inferred.json")); err == nil {
		t.Fatal("expected error when parent path is a file")
	}

	// 2. Decode error (corrupt JSON).
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(corrupt); err == nil {
		t.Fatal("expected error for corrupt JSON")
	}

	// 3. Loaded JSON with null collections and zero FirstSeen on collection.
	nullCol := filepath.Join(dir, "nullcol.json")
	if err := os.WriteFile(nullCol, []byte(`{"collections":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cNull, err := Load(nullCol)
	if err != nil {
		t.Fatalf("Load null collections: %v", err)
	}
	if len(cNull.Snapshot().Collections) != 0 {
		t.Fatal("expected empty collections map")
	}

	zeroFirst := filepath.Join(dir, "zerofirst.json")
	if err := os.WriteFile(zeroFirst, []byte(`{"collections":{"users":{"fields":{},"record_count":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cZero, err := Load(zeroFirst)
	if err != nil {
		t.Fatalf("Load zero FirstSeen: %v", err)
	}
	if err := cZero.Observe("users", map[string]any{"a": 1}); err != nil {
		t.Fatalf("Observe on zero FirstSeen: %v", err)
	}

	// 4. Snapshot on zero-initialized Catalogue (Collections == nil).
	emptyCat := &Catalogue{}
	if snap := emptyCat.Snapshot(); snap.Collections == nil {
		t.Fatal("expected non-nil Collections in Snapshot()")
	}

	// 5. persist MkdirAll failure (parent is a regular file).
	cBadDir := &Catalogue{
		filePath: filepath.Join(blocker, "inferred.json"),
		data:     catalogueData{Collections: map[string]*CollectionStats{}},
	}
	if err := cBadDir.Observe("users", map[string]any{"a": 1}); err == nil {
		t.Fatal("expected Observe error when MkdirAll fails")
	}

	// 6. persist Rename failure (target path is an existing directory).
	targetDir := filepath.Join(dir, "targetdir")
	if err := os.MkdirAll(filepath.Join(targetDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	cBadRename := &Catalogue{
		filePath: targetDir,
		data:     catalogueData{Collections: map[string]*CollectionStats{}},
	}
	if err := cBadRename.Observe("users", map[string]any{"a": 1}); err == nil {
		t.Fatal("expected Observe error when Rename fails")
	}
}

// Not parallel: mutates package-level seam createTemp.
func TestPersistTempFileSeams(t *testing.T) {
	origCreateTemp := createTemp
	t.Cleanup(func() {
		createTemp = origCreateTemp
	})

	dir := t.TempDir()
	c, err := Load(filepath.Join(dir, "inferred.json"))
	if err != nil {
		t.Fatal(err)
	}

	// 1. createTemp error.
	createTemp = func(dir, pattern string) (tempFile, error) {
		return nil, errors.New("createTemp failed")
	}
	if err := c.Observe("users", map[string]any{"a": 1}); err == nil {
		t.Fatal("expected error when createTemp fails")
	}

	// 2. Encode (Write) error.
	createTemp = func(dir, pattern string) (tempFile, error) {
		return &fakeTempFile{name: filepath.Join(dir, "tmp1"), writeErr: errors.New("write failed")}, nil
	}
	if err := c.Observe("users", map[string]any{"a": 1}); err == nil {
		t.Fatal("expected error when Encode fails")
	}

	// 3. Close error.
	createTemp = func(dir, pattern string) (tempFile, error) {
		return &fakeTempFile{name: filepath.Join(dir, "tmp2"), closeErr: errors.New("close failed")}, nil
	}
	if err := c.Observe("users", map[string]any{"a": 1}); err == nil {
		t.Fatal("expected error when Close fails")
	}
}
