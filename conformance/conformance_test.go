// Package conformance_test proves the full OpenVaultDB stack against the
// official DALgo end2end suite:
//
//	dalgo end2end → dalgo2openvaultdb → HTTP → pkg/server → pkg/core → engine
package conformance_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dal-go/dalgo/end2end"
	dalgo2openvaultdb "github.com/dal-go/dalgo2openvaultdb"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func startServer(t *testing.T, manifestYAML string) string {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ts := httptest.NewServer(server.New("test", map[string]*core.Database{db.ID(): db}).Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestConformance_SQLite_Strict(t *testing.T) {
	url := startServer(t, `
database:
  id: e2e-sqlite
  schema_mode: strict
storage:
  engine: sqlite
  path: ./data/e2e.sqlite
schemas:
  collections:
    DalgoE2E_E2ETest1:
      fields:
        StringProp: {type: string}
        IntegerProp: {type: integer}
    DalgoE2E_E2ETest2:
      fields:
        StringProp: {type: string}
        IntegerProp: {type: integer}
    DalgoTest_Cities:
      fields:
        Name: {type: string}
        State: {type: string}
        Country: {type: string}
        Population: {type: integer}
        AreaSqKm: {type: integer}
        IsCapital: {type: boolean}
        HasAirport: {type: boolean}
        Founded: {type: string}
        LastUpdatedAt: {type: string}
`)
	db, err := dalgo2openvaultdb.NewDB(url, "e2e-sqlite")
	if err != nil {
		t.Fatal(err)
	}
	end2end.TestDalgoDB(t, db, nil, false)
}

func TestConformance_InGitDB_Schemaless(t *testing.T) {
	url := startServer(t, `
database:
  id: e2e-ingitdb
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data/e2e-ingitdb
`)
	db, err := dalgo2openvaultdb.NewDB(url, "e2e-ingitdb")
	if err != nil {
		t.Fatal(err)
	}
	end2end.TestDalgoDB(t, db, nil, false)
}

func TestConformance_InGitDB_Partial(t *testing.T) {
	url := startServer(t, `
database:
  id: e2e-ingitdb-partial
  schema_mode: partial
storage:
  engine: ingitdb
  path: ./data/e2e-ingitdb-partial
schemas:
  collections:
    DalgoE2E_E2ETest1:
      fields:
        StringProp: {type: string}
`)
	db, err := dalgo2openvaultdb.NewDB(url, "e2e-ingitdb-partial")
	if err != nil {
		t.Fatal(err)
	}
	end2end.TestDalgoDB(t, db, nil, false)
}
