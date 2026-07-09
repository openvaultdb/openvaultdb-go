package conformance_test

import (
	"os"
	"testing"

	"github.com/dal-go/dalgo/end2end"
	dalgo2openvaultdb "github.com/dal-go/dalgo2openvaultdb"
)

// Firestore conformance runs only against the local emulator
// (`gcloud emulators firestore start`) — set FIRESTORE_EMULATOR_HOST.
// Credentials never come from manifests, so plain CI stays green by skipping.
func requireFirestoreEmulator(t *testing.T) {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST not set; skipping Firestore conformance")
	}
}

func TestConformance_Firestore_Schemaless(t *testing.T) {
	requireFirestoreEmulator(t)
	url := startServer(t, `
database:
  id: e2e-firestore
  schema_mode: schemaless
storage:
  engine: firestore
  firestore:
    project: ovdb-e2e
`)
	db, err := dalgo2openvaultdb.NewDB(url, "e2e-firestore")
	if err != nil {
		t.Fatal(err)
	}
	end2end.TestDalgoDB(t, db, nil, false)
}
