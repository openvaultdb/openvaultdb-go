package mount

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// unsetEnvForTest removes key from the test process's environment (not just
// blanks it — an empty GIT_AUTHOR_NAME is still "set" as far as git's ident
// resolution is concerned) and restores whatever was there afterwards.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// simulateBareHost reproduces, as closely as a test process can, a host with
// no ambient git identity: every fresh GitHub Actions runner, and every
// fresh production host, since nothing in this server configures git.
//
// Clearing GIT_AUTHOR_*/GIT_COMMITTER_* and redirecting GIT_CONFIG_GLOBAL and
// GIT_CONFIG_SYSTEM to /dev/null is necessary but NOT sufficient on macOS:
// verified locally (see below) that with all of that cleared, `git commit`
// still succeeds there, because git falls back to deriving an identity from
// the OS passwd entry (GECOS full name + hostname) — a runtime fallback
// baked into git itself, not sourced from any config file, so redirecting
// config files cannot defeat it. Reproducing:
//
//	D=$(mktemp -d) && git init -q "$D" && cd "$D"
//	unset GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL
//	export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
//	echo hi > f && git add f && git commit -m test
//	# => succeeds: "Committer: <user> <user@host.lan>
//	#    Your name and email address were configured automatically ..."
//
// git's own knob for disabling exactly that fallback is the LOCAL config
// user.useConfigOnly: with it true, an unconfigured repo fails with "fatal:
// no email was given and auto-detection is disabled" — the same failure
// mode a bare CI/production host hits because it has no passwd entry (or a
// GECOS-less one) to fall back to. Setting it on the freshly-`git init`-ed
// test repo, on top of the env/global/system clearing above, reproduces the
// production failure deterministically on any OS, including this Mac.
func simulateBareHost(t *testing.T, dir string) {
	t.Helper()
	for _, k := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	} {
		unsetEnvForTest(t, k)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	if out, err := exec.Command("git", "-C", dir, "config", "--local", "user.useConfigOnly", "true").CombinedOutput(); err != nil {
		t.Fatalf("git config user.useConfigOnly: %v: %s", err, out)
	}
}

// writeIngitdbManifest writes a schemaless ingitdb manifest at dir/db.yaml
// whose storage.path is dataSubdir (relative, mirroring how provisionDatabase
// and ovdb serve manifests are shaped), and returns the manifest path.
func writeIngitdbManifest(t *testing.T, dir, dataSubdir string) string {
	t.Helper()
	manifestPath := filepath.Join(dir, "db.yaml")
	yaml := "database:\n  id: dev\n  schema_mode: schemaless\nstorage:\n  engine: ingitdb\n  path: ./" + dataSubdir + "\n"
	if err := os.WriteFile(manifestPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return manifestPath
}

// TestOpenInGitDB_RemountStampsIdentityOnBareHost is the residual-gap
// regression: a database directory that was `git init`-ed with NO identity
// of its own — the shape left behind by an older build, a restored backup,
// or a directory an operator prepared themselves, none of which ever went
// through provisionDatabase — must still be writable once mounted, on a
// host with no ambient git identity.
//
// Proven to fail against pre-fix code: with the call to ensureGitIdentity
// removed from openInGitDB (i.e. reverting this change), this test fails
// with "dalgo2ingitdb: git commit: exit status 128: ... empty ident name
// ... not allowed" — confirmed by running it against the pre-fix tree
// before wiring ensureGitIdentity in.
func TestOpenInGitDB_RemountStampsIdentityOnBareHost(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// git init WITHOUT going through provisionDatabase, exactly like an
	// older build, a restored backup, or an operator-prepared directory —
	// none of which ever ran the create-path identity stamping.
	if out, err := exec.Command("git", "-C", dataDir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	simulateBareHost(t, dataDir)

	manifestPath := writeIngitdbManifest(t, dir, "data")
	db, err := File(manifestPath)
	if err != nil {
		t.Fatalf("File (mount): %v", err)
	}

	key, err := core.ParseKey("notes", "n1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ops := []core.Op{{Op: "insert", Key: key, Data: map[string]any{"text": "hi"}}}
	if _, err = db.Apply(ctx, ops, "seed"); err != nil {
		t.Fatalf("Apply on a bare-host remount of a pre-existing, identity-less git repo: %v", err)
	}
}

// TestOpenInGitDB_PreservesOperatorConfiguredIdentity confirms
// ensureGitIdentity fills in only a MISSING identity: an operator who
// deliberately configured user.name/user.email on a database's repository
// keeps their own identity, not the ones this package would otherwise
// stamp in.
func TestOpenInGitDB_PreservesOperatorConfiguredIdentity(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dataDir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	const wantName, wantEmail = "Operator Name", "operator@example.com"
	if out, err := exec.Command("git", "-C", dataDir, "config", "--local", "user.name", wantName).CombinedOutput(); err != nil {
		t.Fatalf("git config user.name: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dataDir, "config", "--local", "user.email", wantEmail).CombinedOutput(); err != nil {
		t.Fatalf("git config user.email: %v: %s", err, out)
	}

	manifestPath := writeIngitdbManifest(t, dir, "data")
	if _, err := File(manifestPath); err != nil {
		t.Fatalf("File (mount): %v", err)
	}

	gotName, err := exec.Command("git", "-C", dataDir, "config", "--local", "--get", "user.name").Output()
	if err != nil {
		t.Fatalf("read back user.name: %v", err)
	}
	if got := string(gotName); got != wantName+"\n" {
		t.Fatalf("user.name after mount = %q, want operator's %q (must not be overwritten)", got, wantName)
	}
	gotEmail, err := exec.Command("git", "-C", dataDir, "config", "--local", "--get", "user.email").Output()
	if err != nil {
		t.Fatalf("read back user.email: %v", err)
	}
	if got := string(gotEmail); got != wantEmail+"\n" {
		t.Fatalf("user.email after mount = %q, want operator's %q (must not be overwritten)", got, wantEmail)
	}
}
