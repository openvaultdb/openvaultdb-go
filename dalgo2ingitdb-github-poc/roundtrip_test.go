package ingitdbgithubpoc_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/ingitdb/dalgo2ingitdb"
	"github.com/ingitdb/ingitdb-go/ingitdb/validator"

	poc "github.com/openvaultdb/openvaultdb-go/dalgo2ingitdb-github-poc"
)

func gitClone(t *testing.T, ctx context.Context, src, dst string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "git", "clone", src, dst).CombinedOutput()
	if err != nil {
		t.Fatalf("git clone %s %s: %v: %s", src, dst, err, out)
	}
}

func mustRunGit(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v: %s", args, dir, err, b)
	}
	return string(b)
}

// setupVault git-inits dir, sets a local commit identity, and scaffolds the
// contacts collection layout — everything dalgo2ingitdb.NewDatabase needs.
func setupVault(t *testing.T, ctx context.Context, dir string) {
	t.Helper()
	mustRunGit(t, ctx, dir, "init", "-b", "main")
	mustRunGit(t, ctx, dir, "config", "user.email", "poc@openvaultdb.example")
	mustRunGit(t, ctx, dir, "config", "user.name", "OpenVaultDB PoC")
	if err := poc.ScaffoldContactsCollection(dir); err != nil {
		t.Fatalf("ScaffoldContactsCollection: %v", err)
	}
	// dalgo2ingitdb's opt-in git commit (git_commit.go) only stages the
	// specific record-file paths it wrote during a transaction -- it never
	// stages the schema/mapping files (.ingitdb/root-collections.yaml,
	// <collection>/.collection/definition.yaml). Those are "provisioning"
	// (creating the vault's schema), not a DALgo write, so committing them is
	// this harness's job, exactly as a real "create a new OVDB-backed space"
	// flow would commit the initial schema once when the space is created.
	mustRunGit(t, ctx, dir, "add", "-A")
	mustRunGit(t, ctx, dir, "commit", "-m", "Scaffold contacts collection schema")
}

func openDB(t *testing.T, dir string) dal.DB {
	t.Helper()
	db, err := dalgo2ingitdb.NewDatabase(dir, validator.NewCollectionsReader())
	if err != nil {
		t.Fatalf("dalgo2ingitdb.NewDatabase(%s): %v", dir, err)
	}
	return db
}

func readRecordFile(t *testing.T, dir, id string) map[string]any {
	t.Helper()
	p := filepath.Join(dir, poc.ContactsCollectionID, "$records", id+".json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("record file not found at %s: %v", p, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("record file %s is not valid JSON: %v (raw: %s)", p, err, b)
	}
	return m
}

// TestRoundTrip_ContactusRecordThroughGitHubStandInRepo is the PoC's core
// evidence for the real OVDB MVP architecture: dalgo2ingitdb (the same local
// adapter ovdb-server already uses) writes+commits a contactus-shaped record
// in a git working tree, the commit is pushed to a remote (a local bare repo
// standing in for a real GitHub remote — see roundtrip_test.go's doc comment
// on why this PoC could not create one), and a completely independent fresh
// clone of that remote can read the same record back through a second,
// unrelated dalgo2ingitdb.Database instance. That is a real write -> git
// history -> remote -> clone -> read round trip, not just a same-process
// echo.
//
// GAP DISCLOSED: this test pushes to a local bare repository
// (`git init --bare`), NOT a real github.com remote. Creating a real GitHub
// repo for this session was blocked by policy (repo creation requires the
// user's own direct request, not a coordinator-relayed instruction — see the
// session's final report). Git's plumbing (commit objects, refs, push,
// clone) behaves identically against any remote, local or GitHub, so this is
// strong evidence, but it is NOT proof that @ingitdb/client-github's actual
// HTTPS calls against api.github.com/raw.githubusercontent.com succeed
// end-to-end — that needs a real repo, which the user can create in one
// click (see the report) and hand off to a follow-up run.
func TestRoundTrip_ContactusRecordThroughGitHubStandInRepo(t *testing.T) {
	ctx := context.Background()
	const id = "contact-jane-doe"

	bareRepo := t.TempDir()
	mustRunGit(t, ctx, bareRepo, "init", "--bare", "-b", "main")

	workDir := t.TempDir()
	setupVault(t, ctx, workDir)
	db := openDB(t, workDir)

	person := map[string]any{
		"firstName": "Jane",
		"lastName":  "Doe",
		"email":     "jane@example.com",
		"roles":     []any{"owner", "driver"},
	}
	key := dal.NewKeyWithID(poc.ContactsCollectionID, id)

	if err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Set(ctx, dal.NewRecordWithData(key, person))
	}, dal.TxWithMessage("Add contact Jane Doe")); err != nil {
		t.Fatalf("Set (with commit): %v", err)
	}

	// The transaction message must have triggered a real git commit (see
	// dalgo2ingitdb's RunReadwriteTransaction + git_commit.go).
	log := mustRunGit(t, ctx, workDir, "log", "--oneline")
	if log == "" {
		t.Fatalf("expected a git commit after Set with TxWithMessage, git log is empty")
	}
	t.Logf("workDir git log:\n%s", log)

	// Read back through the SAME dal.DB instance -- the plain round trip.
	got := dal.NewRecordWithData(dal.NewKeyWithID(poc.ContactsCollectionID, id), map[string]any{})
	if err := db.Get(ctx, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Exists() {
		t.Fatalf("Get: record does not Exist() after Set")
	}
	gotData := got.Data().(map[string]any)
	if gotData["firstName"] != "Jane" || gotData["lastName"] != "Doe" || gotData["email"] != "jane@example.com" {
		t.Errorf("unexpected fields after Get: %#v", gotData)
	}

	// Push the commit to the remote (standing in for a real GitHub repo).
	mustRunGit(t, ctx, workDir, "remote", "add", "origin", bareRepo)
	mustRunGit(t, ctx, workDir, "push", "-u", "origin", "main")

	// A completely independent fresh clone -- this is the proof the record
	// is really in the remote's git history, not just this process's working
	// tree.
	cloneDir := filepath.Join(t.TempDir(), "clone")
	gitClone(t, ctx, bareRepo, cloneDir)

	onDisk := readRecordFile(t, cloneDir, id)
	if onDisk["firstName"] != "Jane" || onDisk["lastName"] != "Doe" {
		t.Errorf("cloned record file mismatch: %#v", onDisk)
	}
	roles, _ := onDisk["roles"].([]any)
	if len(roles) != 2 || roles[0] != "owner" || roles[1] != "driver" {
		t.Errorf("cloned record roles = %v, want [owner driver]", onDisk["roles"])
	}

	// Read it back through a SECOND, independent dalgo2ingitdb.Database
	// rooted at the fresh clone -- the same adapter, a different process
	// concern entirely, reading data that arrived purely via git push+clone.
	cloneDB := openDB(t, cloneDir)
	gotFromClone := dal.NewRecordWithData(dal.NewKeyWithID(poc.ContactsCollectionID, id), map[string]any{})
	if err := cloneDB.Get(ctx, gotFromClone); err != nil {
		t.Fatalf("Get from clone: %v", err)
	}
	if !gotFromClone.Exists() {
		t.Fatalf("record does not Exist() when read from the fresh clone")
	}

	// Delete, commit, push, and confirm it's gone from a re-clone too.
	if err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Delete(ctx, key)
	}, dal.TxWithMessage("Remove contact Jane Doe")); err != nil {
		t.Fatalf("Delete (with commit): %v", err)
	}
	mustRunGit(t, ctx, workDir, "push", "origin", "main")

	cloneDir2 := filepath.Join(t.TempDir(), "clone2")
	gitClone(t, ctx, bareRepo, cloneDir2)
	if _, err := os.Stat(filepath.Join(cloneDir2, poc.ContactsCollectionID, "$records", id+".json")); err == nil {
		t.Fatalf("record file still present in re-clone after Delete")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat re-clone record file: %v", err)
	}
}

// genericCreateAndRead is the SAME thesis-check function used in the
// dalgo2ovdb PoC (../dalgo2ovdb/roundtrip_test.go): it takes any dal.DB and
// does a create+read using ONLY dal.DB/dal.Record/dal.Key -- no
// ingitdb-specific type or import. Handing it dalgo2ingitdb's *Database here
// (instead of dalgo2ovdb's *DB, or a dalgo2firestore *Database) is the
// concrete "same module code, storage swapped" proof for the real MVP path.
func genericCreateAndRead(ctx context.Context, db dal.DB, collection, id string, data map[string]any) (map[string]any, error) {
	key := dal.NewKeyWithID(collection, id)
	if err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Set(ctx, dal.NewRecordWithData(key, data))
	}); err != nil {
		return nil, err
	}
	got := dal.NewRecordWithData(dal.NewKeyWithID(collection, id), map[string]any{})
	if err := db.Get(ctx, got); err != nil {
		return nil, err
	}
	return got.Data().(map[string]any), nil
}

func TestThesis_GenericDALgoFunctionOverDalgo2ingitdb(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	setupVault(t, ctx, workDir)
	db := openDB(t, workDir)

	const id = "contact-generic"
	got, err := genericCreateAndRead(ctx, db, poc.ContactsCollectionID, id, map[string]any{
		"firstName": "Generic",
		"lastName":  "Person",
	})
	if err != nil {
		t.Fatalf("genericCreateAndRead(dalgo2ingitdb DB): %v", err)
	}
	if got["firstName"] != "Generic" || got["lastName"] != "Person" {
		t.Errorf("got = %#v", got)
	}
}
