package server_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestNestedKeyOnInGitDBIsASubcollectionOfItsParent: the inGitDB adapter
// addresses notes/n1/items/i1 as record i1 of the subcollection items under
// record n1 of notes, in every call a key reaches it by. The record is written
// under the parent's directory, no top-level collection items appears, and the
// capability that governs it is the one on the root collection (notes), not on
// the leaf name.
func TestNestedKeyOnInGitDBIsASubcollectionOfItsParent(t *testing.T) {
	ts, dir := startKeyValidationServer(t)
	const records = "/v1/databases/dev/records/"
	for _, c := range []struct {
		name, method, key, body string
		status                  int
	}{
		{"seed the parent", "PUT", "notes/n1", `{"data":{"t":"n"}}`, http.StatusNoContent},
		{"PUT", "PUT", "notes/n1/items/i1", `{"data":{"t":"i1"}}`, http.StatusNoContent},
		{"POST", "POST", "notes/n1/items/i2", `{"data":{"t":"i2"}}`, http.StatusCreated},
		{"PATCH", "PATCH", "notes/n1/items/i1", `{"updates":[{"fieldName":"t","value":"patched"}]}`, http.StatusNoContent},
		{"GET", "GET", "notes/n1/items/i1", "", http.StatusOK},
		{"HEAD", "HEAD", "notes/n1/items/i1", "", http.StatusOK},
		{"DELETE", "DELETE", "notes/n1/items/i2", "", http.StatusNoContent},
		{"HEAD of the deleted record", "HEAD", "notes/n1/items/i2", "", http.StatusNotFound},
	} {
		if status, body := request(t, ts, c.method, records+c.key, ownerToken, c.body); status != c.status {
			t.Fatalf("%s: status %d, want %d: %s", c.name, status, c.status, body)
		}
	}

	files := snapshotFiles(t, dir)
	nested := filepath.Join("data", "notes", "n1", "items", "$records", "i1.yaml")
	if !strings.Contains(files[nested], "patched") {
		t.Errorf("%s holds %q", nested, files[nested])
	}
	if _, ok := files[filepath.Join("data", "notes", "n1", "items", "$records", "i2.yaml")]; ok {
		t.Error("the deleted record is still on disk")
	}
	for path := range files {
		if strings.HasPrefix(path, filepath.Join("data", "items")+string(filepath.Separator)) {
			t.Errorf("a top-level collection items was written: %s", path)
		}
	}
	if roots := files[filepath.Join("data", ".ingitdb", "root-collections.yaml")]; strings.Contains(roots, "items") {
		t.Errorf("items is a root collection: %s", roots)
	}

	// The capability is checked on the root collection of the key.
	notesToken := connectFlow(t, ts, "records:read:notes,records:write:notes")
	if status, body := request(t, ts, http.MethodGet, records+"notes/n1/items/i1", notesToken, ""); status != http.StatusOK {
		t.Errorf("a grant on notes reads a record of its subcollection: %d %s", status, body)
	}
	itemsToken := connectFlow(t, ts, "records:read:items,records:write:items")
	for _, method := range []string{"GET", "PUT"} {
		if status, _ := request(t, ts, method, records+"notes/n1/items/i1", itemsToken, `{"data":{"t":"x"}}`); status != http.StatusForbidden {
			t.Errorf("a grant on the leaf name items, %s: status %d, want 403", method, status)
		}
	}
}
