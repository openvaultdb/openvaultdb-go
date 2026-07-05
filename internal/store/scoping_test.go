package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFlatCollectionModel_NoParentRecordCollision locks in the reason
// openvaultdb-go is NOT susceptible to the per-space / parent-record key-scoping
// flaw that was fixed in dalgo2ingitdb (see SCOPING.md).
//
// OVDB addresses a record by the flat 4-tuple (vault, namespace, collection, id)
// — there is no "subcollection under a parent record" concept, so no parent
// chain ever reaches store.go. The closest analogue of the ingitdb collision
// scenario (dal keys spaces/family/contacts/c1 vs spaces/work/contacts/c1) is
// "same namespace+collection+id in two different vaults." OVDB isolates vaults
// by physical directory (store.go vaultDir / openDatabases), so the two records
// live in distinct files and cannot clobber each other.
func TestFlatCollectionModel_NoParentRecordCollision(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const (
		nsID = "todo-demo.openvaultdb.app/openvaultdb/todos"
		col  = "tasks"
		id   = "c1" // same record id in both vaults
	)

	// Two seeded vaults act as the two "parents" (family vs work).
	if _, err := s.CreateRecord("family", nsID, col, Record{"id": id, "title": "family milk"}); err != nil {
		t.Fatalf("CreateRecord(family): %v", err)
	}
	if _, err := s.CreateRecord("work", nsID, col, Record{"id": id, "title": "work report"}); err != nil {
		t.Fatalf("CreateRecord(work): %v", err)
	}

	// The two same-id records must land in DISTINCT files on disk.
	familyPath := filepath.Join(dir, "vaults", "family", "todo-demo.openvaultdb.app", "todos", col, "$records", id+".json")
	workPath := filepath.Join(dir, "vaults", "work", "todo-demo.openvaultdb.app", "todos", col, "$records", id+".json")
	if familyPath == workPath {
		t.Fatal("family and work resolve to the same path — scope collision")
	}
	fb, err := os.ReadFile(familyPath)
	if err != nil {
		t.Fatalf("read family record %s: %v", familyPath, err)
	}
	wb, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatalf("read work record %s: %v", workPath, err)
	}
	if string(fb) == string(wb) {
		t.Fatalf("family and work record files are identical — one clobbered the other:\n%s", fb)
	}

	// And reads from each vault must return that vault's own record, not the
	// other's — proving no on-disk clobber happened.
	fam, ok, err := s.GetRecord("family", nsID, col, id)
	if err != nil || !ok {
		t.Fatalf("GetRecord(family): ok=%v err=%v", ok, err)
	}
	if fam["title"] != "family milk" {
		t.Fatalf("family record clobbered: got title=%v", fam["title"])
	}
	work, ok, err := s.GetRecord("work", nsID, col, id)
	if err != nil || !ok {
		t.Fatalf("GetRecord(work): ok=%v err=%v", ok, err)
	}
	if work["title"] != "work report" {
		t.Fatalf("work record clobbered: got title=%v", work["title"])
	}
}

// TestCollectionRelPath_FlatTopLevel asserts the top-level (non-nested)
// collection path mapping is byte-stable: (namespace, collection) maps to a
// single nested directory path with the collection as the final segment, and no
// parent-record segment is ever introduced. If OVDB ever grows real
// subcollections, this test is the canary that the flat-collection contract
// documented in SCOPING.md has changed.
func TestCollectionRelPath_FlatTopLevel(t *testing.T) {
	const nsID = "todo-demo.openvaultdb.app/openvaultdb/todos"

	if got, want := collectionRelPath(nsID, "tasks"), "todo-demo.openvaultdb.app/todos/tasks"; got != want {
		t.Errorf("collectionRelPath = %q, want %q", got, want)
	}
	// The dalgo collection handle is a flat, sanitized token with no path
	// separators — it cannot carry a parent chain. '/' becomes '.', and the
	// disallowed '-' is sanitized to '_' (see collectionID's doc comment).
	if got, want := collectionID(nsID, "tasks"), "todo_demo.openvaultdb.app.todos.tasks"; got != want {
		t.Errorf("collectionID = %q, want %q", got, want)
	}
}
