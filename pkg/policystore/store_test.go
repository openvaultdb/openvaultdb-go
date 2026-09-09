package policystore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dal-go/dalgo/access"
)

func policy(t *testing.T, field string) access.DTQLDocument {
	t.Helper()
	doc, err := access.ParseDTQLPolicy([]byte(`apiVersion: dtql.org/access/v1
kind: AccessPolicy
metadata: {name: customers}
target: {database: crm}
composition: dalgo-hierarchical-v1
default: deny
scopes:
  - path: /customers
    rules: [{id: read, effect: allow, operations: [query], fields: [` + field + `]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
func openTestStore(t *testing.T, root string) *Store {
	t.Helper()
	store, err := Open(root, Owner{Enabled: true, Database: "crm", Realm: "local"})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
func TestGenerationActivationCASAndValidation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, t.TempDir())
	if _, err := store.Load(ctx); err == nil {
		t.Fatal("missing active generation allowed")
	}
	first, err := store.Activate(ctx, "", []access.DTQLDocument{policy(t, "name")})
	if err != nil {
		t.Fatal(err)
	}
	same, err := store.Activate(ctx, first.Revision, []access.DTQLDocument{policy(t, "name")})
	if err != nil || same.Revision != first.Revision {
		t.Fatalf("no-op: %v", err)
	}
	second, err := store.Activate(ctx, first.Revision, []access.DTQLDocument{policy(t, "email")})
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision == first.Revision {
		t.Fatal("changed policy retained generation")
	}
	if _, err := store.Activate(ctx, first.Revision, []access.DTQLDocument{policy(t, "secret")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("CAS: %v", err)
	}
	bad := policy(t, "name")
	bad.Target.Database = "other"
	if _, err := store.Activate(ctx, second.Revision, []access.DTQLDocument{bad}); err == nil {
		t.Fatal("invalid candidate published")
	}
	controller, err := NewController(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := controller.Snapshot()
	snapshot.Documents[0].ID = "mutated"
	intact, _ := controller.Snapshot()
	if intact.Documents[0].ID == "mutated" {
		t.Fatal("snapshot metadata mutable")
	}
	// Corruption cannot reactivate an older unreferenced generation.
	path := filepath.Join(store.root, "generations", second.Revision, second.Documents[0].File)
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Reload(ctx); err == nil {
		t.Fatal("corrupt generation loaded")
	}
	if _, err := controller.Policies(ctx); err == nil {
		t.Fatal("corrupt reload failed open")
	}
}
func TestGenerationCrashRecovery(t *testing.T) {
	if root := os.Getenv("OVDB_POLICY_CRASH_ROOT"); root != "" {
		store := openTestStore(t, root)
		store.phase = func(phase string) error {
			if phase == os.Getenv("OVDB_POLICY_CRASH_PHASE") {
				os.Exit(87)
			}
			return nil
		}
		if _, err := store.Activate(context.Background(), os.Getenv("OVDB_POLICY_EXPECTED"), []access.DTQLDocument{policy(t, "email")}); err != nil {
			t.Fatal(err)
		}
		t.Fatal("crash checkpoint not reached")
	}
	for _, phase := range []string{"generation-synced", "generation-published", "pointer-synced", "pointer-published"} {
		t.Run(phase, func(t *testing.T) {
			store := openTestStore(t, t.TempDir())
			old, err := store.Activate(context.Background(), "", []access.DTQLDocument{policy(t, "name")})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestGenerationCrashRecovery$")
			cmd.Env = append(os.Environ(), "OVDB_POLICY_CRASH_ROOT="+store.root, "OVDB_POLICY_CRASH_PHASE="+phase, "OVDB_POLICY_EXPECTED="+old.Revision)
			if err := cmd.Run(); err == nil {
				t.Fatal("worker did not crash")
			}
			loaded, err := openTestStore(t, store.root).Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if (loaded.Revision == old.Revision) != (phase != "pointer-published") {
				t.Fatalf("wrong recovery state at %s", phase)
			}
		})
	}
}
func TestGenerationMissingReferenceAndUncertainPublication(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	ctx := context.Background()
	old, err := store.Activate(ctx, "", []access.DTQLDocument{policy(t, "name")})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	store.phase = func(phase string) error {
		if phase == "pointer-published" {
			return errors.New("simulated response failure")
		}
		return nil
	}
	if _, err := controller.Activate(ctx, old.Revision, []access.DTQLDocument{policy(t, "email")}); err == nil {
		t.Fatal("publication uncertainty hidden")
	}
	current, err := controller.Snapshot()
	if err != nil || current.Revision == old.Revision {
		t.Fatal("admitted stale snapshot after publication")
	}
	if err := os.RemoveAll(filepath.Join(store.root, "generations", current.Revision)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate(ctx, "", []access.DTQLDocument{policy(t, "name")}); err == nil {
		t.Fatal("missing enabled generation treated as bootstrap")
	}
}

func TestGenerationPrivateEditPreservesPublicRevision(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	ctx := context.Background()
	public := policy(t, "name")
	public.Metadata.Name = "public"
	private := policy(t, "name")
	private.Metadata.Name = "private"
	private.Metadata.Visibility = "private"
	first, err := store.Activate(ctx, "", []access.DTQLDocument{public, private})
	if err != nil {
		t.Fatal(err)
	}
	private.Metadata.Description = "updated internal policy"
	next, err := store.Activate(ctx, first.Revision, []access.DTQLDocument{private, public})
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision == first.Revision {
		t.Fatal("private edit did not change internal generation")
	}
	if next.Documents[1].ID != "public" || next.Documents[1].Revision != first.Documents[1].Revision {
		t.Fatal("private edit changed public ETag")
	}
}
func TestGenerationRejectsSymlinkAndDuplicatePointer(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	ctx := context.Background()
	first, err := store.Activate(ctx, "", []access.DTQLDocument{policy(t, "name")})
	if err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(store.root, "active.json")
	bad := []byte("{\"generation\":\"" + first.Revision + "\",\"generation\":\"" + first.Revision + "\"}")
	if err := os.WriteFile(active, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx); err == nil {
		t.Fatal("duplicate active pointer accepted")
	}
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "active")
	if err := os.WriteFile(target, []byte("{\"generation\":\""+first.Revision+"\"}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, active); err != nil {
		t.Skip(err)
	}
	if _, err := store.Load(ctx); err == nil {
		t.Fatal("symlink pointer accepted")
	}
}
