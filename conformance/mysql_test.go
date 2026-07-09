package conformance_test

import (
	"os"
	"testing"

	"github.com/dal-go/dalgo/end2end"
	dalgo2openvaultdb "github.com/dal-go/dalgo2openvaultdb"
)

// MySQL conformance runs only when DALGO2MYSQL_TEST_DSN points at a reachable
// MySQL (e.g. the local docker container). Credentials never come from
// manifests, so plain CI stays green by skipping.
//
// The conformance collections (DalgoE2E_E2ETest1/2, DalgoTest_Cities) are
// declared in the manifest and provisioned as tables by ovdb at mount time.
func TestConformance_MySQL_Strict(t *testing.T) {
	dsn := os.Getenv("DALGO2MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("DALGO2MYSQL_TEST_DSN not set; skipping MySQL conformance")
	}
	t.Setenv("OVDB_MYSQL_DSN", dsn)
	url := startServer(t, `
database:
  id: e2e-mysql
  schema_mode: strict
storage:
  engine: mysql
  mysql:
    dsn_env: OVDB_MYSQL_DSN
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
	db, err := dalgo2openvaultdb.NewDB(url, "e2e-mysql")
	if err != nil {
		t.Fatal(err)
	}
	end2end.TestDalgoDB(t, db, nil, false)
}
