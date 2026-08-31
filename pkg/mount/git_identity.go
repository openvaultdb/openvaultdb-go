package mount

import "os/exec"

// gitIdentityName and gitIdentityEmail are the repo-local git identity
// ensureGitIdentity stamps on a local inGitDB directory when it has none of
// its own.
const (
	gitIdentityName  = "OpenVaultDB"
	gitIdentityEmail = "ovdb@localhost"
)

// ensureGitIdentity best-effort fills in a missing LOCAL (per-repository,
// never --global/--system) git identity for dir, so any `git commit`
// dalgo2ingitdb runs against it (see gitCommitPaths in
// github.com/ingitdb/dalgo2ingitdb) succeeds regardless of the host's
// ambient config and regardless of how dir came to be a git repository —
// freshly `git init`-ed by provisionDatabase, `git init`-ed by an older
// build, restored from a backup, or prepared by an operator directly.
// Without this, `git commit` fails with "empty ident name ... not allowed"
// on any host with no ambient user.name/user.email — every fresh GitHub
// Actions runner, and any fresh production host, since nothing here ever
// configures git globally — turning every write into a 500.
//
// It is idempotent and never overwrites an identity that is already set:
// an operator's own user.name/user.email is left untouched. It is a no-op
// — same tolerance as the `git init` it complements — when dir is not (yet)
// a git work tree, or when the git binary is unavailable: dalgo2ingitdb
// tolerates a plain, non-git directory, so mounting one here must not
// silently turn it into a git repository.
//
// Called from openInGitDB, which is the single choke point every local
// inGitDB mount passes through — a runtime create (provisionDatabase calls
// mount.File right after `git init`), a restart remount (mount.Dir, used by
// both `ovdb serve --data-dir` rescans and `ovdb serve --dir`), and a
// directly-mounted `--manifest` file. Ensuring identity here, rather than
// only at creation time, means a database that already existed as a
// git repo without an identity — from an older build, a restored backup, or
// a directory an operator prepared themselves — self-heals the next time it
// is mounted, instead of failing every write forever.
func ensureGitIdentity(dir string) {
	if exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Run() != nil {
		return // not a git repo (or no git binary) — nothing to do
	}
	ensureLocalGitConfig(dir, "user.name", gitIdentityName)
	ensureLocalGitConfig(dir, "user.email", gitIdentityEmail)
}

// ensureLocalGitConfig sets key=value as LOCAL git config in dir only when
// key has no value there yet, leaving any existing value (an operator's own
// configured identity) untouched.
func ensureLocalGitConfig(dir, key, value string) {
	if exec.Command("git", "-C", dir, "config", "--local", "--get", key).Run() == nil {
		return // already configured
	}
	_ = exec.Command("git", "-C", dir, "config", "--local", key, value).Run()
}
