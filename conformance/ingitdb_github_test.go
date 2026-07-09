package conformance_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/dal-go/dalgo/end2end"
	dalgo2openvaultdb "github.com/dal-go/dalgo2openvaultdb"
)

// GitHub-backed inGitDB conformance runs only when OVDB_GH_TEST_REPO
// ("owner/repo") and OVDB_GITHUB_TOKEN are set and point at a repo the token
// can write to (with an existing branch — the tree writer commits onto it).
// Credentials never come from manifests, so plain CI stays green by skipping.
//
// Each dalgo end2end write batch becomes one commit on the branch, so the
// repo accumulates history; use a throwaway repo.
func TestConformance_InGitDB_GitHub_Strict(t *testing.T) {
	repo := os.Getenv("OVDB_GH_TEST_REPO")
	token := os.Getenv("OVDB_GITHUB_TOKEN")
	ref := os.Getenv("OVDB_GH_TEST_REF")
	if ref == "" {
		ref = "main"
	}
	if repo == "" || token == "" {
		t.Skip("OVDB_GH_TEST_REPO / OVDB_GITHUB_TOKEN not set; skipping GitHub inGitDB conformance")
	}
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("OVDB_GH_TEST_REPO must be owner/repo, got %q", repo)
	}
	url := startServer(t, fmt.Sprintf(`
database:
  id: e2e-gh
  schema_mode: strict
storage:
  engine: ingitdb
  ingitdb:
    github:
      owner: %s
      repo: %s
      ref: %s
      token_env: OVDB_GITHUB_TOKEN
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
`, owner, name, ref))
	db, err := dalgo2openvaultdb.NewDB(url, "e2e-gh")
	if err != nil {
		t.Fatal(err)
	}
	// GitHub is eventually consistent for freshly written content via the
	// contents/tree API; give reads a moment after writes.
	end2end.TestDalgoDB(t, db, nil, true)
}

func splitRepo(s string) (owner, repo string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:], i > 0 && i < len(s)-1
		}
	}
	return "", "", false
}
