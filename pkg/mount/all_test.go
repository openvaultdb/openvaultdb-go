package mount

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

func writeSQLiteManifest(t *testing.T, dir, name, id string) string {
	t.Helper()
	body := strings.Replace(strings.Replace(validSQLiteManifest, "./todo.sqlite", "./"+name+".sqlite", 1), "id: todo", "id: "+id, 1)
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAllReportsAnOptionalFailureAndServesTheRest(t *testing.T) {
	dir := t.TempDir()
	first := writeSQLiteManifest(t, dir, "first", "first")
	duplicate := writeSQLiteManifest(t, dir, "again", "first")
	missing := filepath.Join(dir, "missing.yaml")
	dbs, health, err := All([]Spec{{Path: first}, {Path: missing, Optional: true}, {Path: duplicate, Optional: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range dbs {
		t.Cleanup(func() { _ = db.Close() })
	}
	if len(dbs) != 1 || dbs["first"] == nil {
		t.Fatalf("mounted = %v, want only first", dbs)
	}
	if !health[0].Mounted || health[0].ID != "first" || health[0].Err != nil {
		t.Errorf("first: %+v", health[0])
	}
	if health[1].Mounted || health[1].Err == nil || !health[1].Optional {
		t.Errorf("missing: %+v", health[1])
	}
	if health[2].Mounted || health[2].Err == nil || !strings.Contains(health[2].Err.Error(), "duplicate database id") {
		t.Errorf("duplicate: %+v", health[2])
	}
}

func TestAllFailsOnARequiredMountAndClosesTheOthers(t *testing.T) {
	dir := t.TempDir()
	first := writeSQLiteManifest(t, dir, "first", "first")
	var opened []*core.Database
	mountFile := func(path string, opts Options) (*core.Database, error) {
		if path == "broken" {
			return nil, errors.New("catalogue read failed")
		}
		db, err := FileWithOptions(path, opts)
		opened = append(opened, db)
		return db, err
	}
	dbs, health, err := all([]Spec{{Path: first}, {Path: "broken"}}, mountFile)
	if err == nil || !strings.Contains(err.Error(), "catalogue read failed") || dbs != nil {
		t.Fatalf("All = %v, %v; want the required failure", dbs, err)
	}
	if len(health) != 2 || health[0].Mounted || health[1].Err == nil {
		t.Fatalf("health = %+v", health)
	}
	if len(opened) != 1 {
		t.Fatalf("opened %d databases, want the one that mounted", len(opened))
	}
}
