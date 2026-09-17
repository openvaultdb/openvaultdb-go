package mount

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// treeDigest lists every file under dir (including .git internals except the
// index, which `git status` itself may refresh) with its content hash.
func treeDigest(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == filepath.Join(".git", "index") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s %x", rel, sha256.Sum256(b)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestFileWithOptions_ConnectLeavesGitRepoUntouched verifies
// database-setup-and-providers#ac:connect-leaves-folder-untouched at the
// mount layer: connecting an existing inGitDB git repository that has
// records and only a global git identity adds or changes no file in it.
func TestFileWithOptions_ConnectLeavesGitRepoUntouched(t *testing.T) {
	root := t.TempDir()
	// Only a global identity: no env identity, a temp global config file.
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		unsetEnvForTest(t, k)
	}
	globalConfig := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[user]\n\tname = Global User\n\temail = global@example.com\n[init]\n\tdefaultBranch = main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "init", "-q")
	opts := Options{CatalogueDir: filepath.Join(root, "catalogue"), SkipGitIdentity: true}
	manifestDir := filepath.Join(root, "manifests")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(manifestDir, "notes.yaml")
	yaml := "database:\n  id: notes\n  schema_mode: schemaless\nstorage:\n  engine: ingitdb\n  path: " + repo + "\n"
	if err := os.WriteFile(manifestPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed records (committed by the driver with the global identity).
	seed, err := FileWithOptions(manifestPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	key, err := core.ParseKey("notes", "n1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = seed.Apply(ctx, []core.Op{{Op: "insert", Key: key, Data: map[string]any{"text": "hi"}}}, "seed"); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err = seed.Close(); err != nil {
		t.Fatal(err)
	}
	// Commit whatever the seed left (collection definitions etc.) so the
	// repository starts clean; the driver's own commit granularity is not
	// what this test is about.
	gitOut(t, repo, "add", "-A")
	if gitOut(t, repo, "status", "--porcelain") != "" {
		gitOut(t, repo, "commit", "-q", "-m", "seed layout")
	}
	if status := gitOut(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("seeded repo not clean before connect:\n%s", status)
	}
	configBefore, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	digestBefore := treeDigest(t, repo)

	// Connect.
	db, err := FileWithOptions(manifestPath, opts)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	data, err := db.Get(ctx, key)
	if err != nil || data["text"] != "hi" {
		t.Fatalf("read after connect = %v, %v", data, err)
	}
	if _, err = db.Collections(ctx); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	if status := gitOut(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("git status after connect not empty:\n%s", status)
	}
	configAfter, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configBefore, configAfter) {
		t.Errorf(".git/config changed by connect:\nbefore:\n%s\nafter:\n%s", configBefore, configAfter)
	}
	if digestAfter := treeDigest(t, repo); digestAfter != digestBefore {
		t.Errorf("files changed by connect:\nbefore:\n%s\nafter:\n%s", digestBefore, digestAfter)
	}
	if _, err = os.Stat(filepath.Join(repo, ".ovdb")); !os.IsNotExist(err) {
		t.Errorf(".ovdb inside the connected folder: %v", err)
	}
	if _, err = os.Stat(filepath.Join(opts.CatalogueDir, "notes.inferred.json")); err != nil {
		t.Errorf("inferred catalogue not in CatalogueDir: %v", err)
	}
}

func TestFileWithOptions_ZeroValueMatchesFile(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dataDir, "init", "-q")
	db, err := FileWithOptions(writeIngitdbManifest(t, dir, "data"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Default behaviour still stamps a missing local identity.
	if got := gitOut(t, dataDir, "config", "--local", "--get", "user.email"); got == "" {
		t.Error("zero Options did not stamp the local git identity")
	}
}

const validSQLiteManifest = "database:\n  id: todo\n  schema_mode: strict\nstorage:\n  engine: sqlite\n  path: ./todo.sqlite\nschemas:\n  collections:\n    lists:\n      fields:\n        title: {type: string}\n"

func TestDirReport_BrokenManifestDoesNotStopOthers(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.yaml")
	for name, body := range map[string]string{
		"todo.yaml":   validSQLiteManifest,
		"broken.yaml": "database:\n  id: broken\nstorage:\n  engine: nosuch\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dbs, failures, err := DirReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range dbs {
		t.Cleanup(func() { _ = db.Close() })
	}
	if len(dbs) != 1 || dbs["todo"] == nil {
		t.Errorf("mounted = %v, want only todo", dbs)
	}
	if len(failures) != 1 || failures[broken] == nil {
		t.Errorf("failures = %v, want one for %s", failures, broken)
	}

	// Dir keeps failing on the first broken manifest.
	if _, err = Dir(dir); err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Errorf("Dir error = %v, want the broken manifest's error", err)
	}
	if _, _, err = DirReport(filepath.Join(dir, "missing")); err == nil {
		t.Error("DirReport of a missing directory succeeded")
	}
}

func TestDirReport_DuplicateID(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.yaml", "b.yml"} {
		body := strings.Replace(validSQLiteManifest, "./todo.sqlite", "./"+name+".sqlite", 1)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dbs, failures, err := DirReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range dbs {
		t.Cleanup(func() { _ = db.Close() })
	}
	dup := failures[filepath.Join(dir, "b.yml")]
	if len(dbs) != 1 || dup == nil || !strings.Contains(dup.Error(), "duplicate database id") {
		t.Errorf("dbs = %v, failures = %v; want a.yaml mounted and b.yml a duplicate", dbs, failures)
	}
}
