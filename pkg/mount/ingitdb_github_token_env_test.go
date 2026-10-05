package mount

import (
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
)

// markerToken is a token used as a token_env value.
const markerToken = "github_pat_MARKER_52d1c9"

// githubMountManifest is the manifest of a GitHub-backed inGitDB mount that
// declares no schemas, so that a mount which gets as far as them is refused for
// that reason and never opens a connection.
func githubMountManifest(tokenEnv string) *manifest.Manifest {
	return &manifest.Manifest{
		Database: manifest.Database{ID: "ghmount"},
		Storage: manifest.Storage{Engine: "ingitdb", InGitDB: &manifest.InGitDBOptions{
			GitHub: &manifest.InGitDBGitHubOptions{Owner: "me", Repo: "data", TokenEnv: tokenEnv},
		}},
	}
}

// TestGitHubMountChecksTheTokenVariableNameBeforeReadingIt: a GitHub-backed mount
// reads the token from the variable that token_env names only when the name
// passed the manifest's check. A manifest that was not validated and holds
// anything else there is refused before the environment is read, with a message
// that does not repeat the value.
func TestGitHubMountChecksTheTokenVariableNameBeforeReadingIt(t *testing.T) {
	const name = "tok-MARKER-52d1/with.dots"
	t.Setenv(name, "a-token")
	_, _, err := openInGitDBGitHub(githubMountManifest(name))
	const want = "storage.ingitdb.github.token_env is not the name of an environment variable"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

// TestGitHubMountNamesOnlyTheDefaultTokenVariable: a token that is not set is
// reported with the name of the default variable, and with the name of the field
// when the manifest gives the variable (a token written where the name belongs is
// a valid name, and is never repeated).
func TestGitHubMountNamesOnlyTheDefaultTokenVariable(t *testing.T) {
	t.Setenv("OVDB_GITHUB_TOKEN", "")
	t.Setenv(markerToken, "")
	for _, c := range []struct{ name, tokenEnv, want string }{
		{"the default variable", "", "GitHub token not set: expected a contents:write token in $OVDB_GITHUB_TOKEN"},
		{"a variable the manifest names", markerToken, "GitHub token not set: expected a contents:write token in the variable that storage.ingitdb.github.token_env names"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := openInGitDBGitHub(githubMountManifest(c.tokenEnv))
			if err == nil || err.Error() != c.want {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), "MARKER") {
				t.Errorf("the message repeats the name the manifest gives: %v", err)
			}
		})
	}
	t.Run("a token that is set", func(t *testing.T) {
		t.Setenv(markerToken, "a-token")
		_, _, err := openInGitDBGitHub(githubMountManifest(markerToken))
		if err == nil || !strings.Contains(err.Error(), "requires declared schemas.collections") {
			t.Fatalf("got %v", err)
		}
	})
}
