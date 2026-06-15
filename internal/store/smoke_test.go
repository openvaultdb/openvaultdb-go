package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSmokeCRUD exercises full record CRUD against a scratch data dir backed by
// the inGitDB (dalgo2ingitdb) store, and asserts the on-disk file layout.
func TestSmokeCRUD(t *testing.T) {
	dir := "/tmp/ovdb-dalgo-test"
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const (
		vault = "personal"
		nsID  = "todo-demo.openvaultdb.app/openvaultdb/todos"
		col   = "tasks"
	)

	// Create.
	created, err := s.CreateRecord(vault, nsID, col, Record{"title": "Buy milk"})
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("created record has no id")
	}
	if created["done"] != false {
		t.Errorf("done default: got %v", created["done"])
	}
	if _, ok := created["createdAt"]; !ok {
		t.Error("createdAt default missing")
	}

	// File exists under <vault>/<owner>/<name>/tasks/$records/<id>.json with NO id field.
	recPath := filepath.Join(dir, "vaults", vault, "todo-demo.openvaultdb.app", "todos", "tasks", "$records", id+".json")
	b, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatalf("read record file %s: %v", recPath, err)
	}
	if strings.Contains(string(b), `"id"`) {
		t.Errorf("record file must not contain id field:\n%s", b)
	}
	if !strings.Contains(string(b), `"title"`) {
		t.Errorf("record file missing title:\n%s", b)
	}
	t.Logf("record file %s:\n%s", recPath, b)

	// List reconstructs id.
	recs, err := s.ListRecords(vault, nsID, col)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(recs) != 1 || recs[0]["id"] != id {
		t.Fatalf("ListRecords: got %v", recs)
	}

	// Get reconstructs id.
	got, ok, err := s.GetRecord(vault, nsID, col, id)
	if err != nil || !ok {
		t.Fatalf("GetRecord: ok=%v err=%v", ok, err)
	}
	if got["id"] != id || got["title"] != "Buy milk" {
		t.Fatalf("GetRecord: got %v", got)
	}

	// Update.
	upd, ok, err := s.UpdateRecord(vault, nsID, col, id, Record{"done": true, "id": "ignored"})
	if err != nil || !ok {
		t.Fatalf("UpdateRecord: ok=%v err=%v", ok, err)
	}
	if upd["done"] != true || upd["id"] != id {
		t.Fatalf("UpdateRecord: got %v", upd)
	}

	// Missing get/update/delete.
	if _, ok, _ := s.GetRecord(vault, nsID, col, "nope"); ok {
		t.Error("GetRecord(missing) ok=true")
	}
	if _, ok, _ := s.UpdateRecord(vault, nsID, col, "nope", Record{"done": true}); ok {
		t.Error("UpdateRecord(missing) ok=true")
	}
	if ok, _ := s.DeleteRecord(vault, nsID, col, "nope"); ok {
		t.Error("DeleteRecord(missing) ok=true")
	}

	// Delete.
	ok, err = s.DeleteRecord(vault, nsID, col, id)
	if err != nil || !ok {
		t.Fatalf("DeleteRecord: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(recPath); !os.IsNotExist(err) {
		t.Errorf("record file still exists after delete: %v", err)
	}
	recs, _ = s.ListRecords(vault, nsID, col)
	if len(recs) != 0 {
		t.Fatalf("ListRecords after delete: got %v", recs)
	}
}
