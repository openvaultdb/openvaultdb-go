package mount

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// gitIdentityName and gitIdentityEmail are the repo-local git identity
// ensureGitIdentity stamps on a database repository that ovdb owns when git
// cannot resolve any identity for it.
const (
	gitIdentityName  = "OpenVaultDB"
	gitIdentityEmail = "ovdb@localhost"
)

// ensureGitIdentity best-effort gives a database repository an identity so
// any commit dalgo2ingitdb runs against it (see gitCommitPaths in
// github.com/ingitdb/dalgo2ingitdb) succeeds on a host with no ambient git
// identity — every fresh GitHub Actions runner and fresh production host,
// where the commit would otherwise fail with "empty ident name ... not
// allowed" and turn every write into a 500.
//
// It writes LOCAL (per-repository, never --global/--system) config, and only
// when BOTH guards hold:
//
//  1. dir is itself the top level of a git repository (isRepoTopLevel) — a
//     repository ovdb owns. A folder that merely lives inside a larger
//     repository (a user's project that happens to hold an inGitDB folder)
//     is never touched: stamping would rewrite the enclosing repository's
//     config and re-author every later commit there. dalgo2ingitdb runs git
//     with the process environment and offers no per-database identity
//     option, so ovdb cannot supply an identity for such a folder's commits
//     alone; if git has no identity for it, writes there fail with git's own
//     clear error until the operator configures one — an operator-owned
//     repository is never altered silently.
//  2. git cannot resolve an identity for dir at all (gitIdentityResolves).
//     Any identity git can already use — repo-local, global, system, env, or
//     git's own passwd fallback — wins and is left alone.
//
// Only the missing key is written: a resolvable user.name is never replaced
// just because user.email is absent. It is idempotent, and a no-op — same
// tolerance as the `git init` it complements — when dir is not a git
// repository or the git binary is unavailable, so mounting a plain
// directory never turns it into a repository.
//
// Called from openInGitDB, the single choke point every local inGitDB mount
// passes through (runtime create, restart remount, direct --manifest), so a
// database repository from an older build or restored backup self-heals the
// next time it is mounted.
func ensureGitIdentity(dir string) {
	if !isRepoTopLevel(dir) || gitIdentityResolves(dir) {
		return
	}
	ensureLocalGitConfig(dir, "user.name", gitIdentityName)
	ensureLocalGitConfig(dir, "user.email", gitIdentityEmail)
}

// isRepoTopLevel reports whether dir is the root of a git work tree, as
// opposed to a subdirectory of an enclosing repository (where
// `rev-parse --is-inside-work-tree` is also true) or not a repository at all.
// Both paths are symlink-resolved before comparing, since git prints the
// physical path (macOS /var vs /private/var, /tmp vs /private/tmp).
func isRepoTopLevel(dir string) bool {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return false // not a git work tree (or no git binary)
	}
	top := strings.TrimRight(string(out), "\r\n")
	return realPath(filepath.FromSlash(top)) == realPath(dir)
}

// realPath returns p with symlinks resolved and cleaned, or p unchanged when
// it cannot be resolved.
func realPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// gitIdentityResolves reports whether git can determine both the author and
// committer identity for dir, exactly as a commit there would. `git var
// GIT_*_IDENT` runs git's strict identity resolution (config at every level,
// GIT_AUTHOR_*/GIT_COMMITTER_* env, user.useConfigOnly, passwd fallback), so
// it fails iff `git commit` would fail with "empty ident name" / "unable to
// auto-detect email address". Checking effective identity this way, rather
// than only the repo-local config, is what keeps a perfectly good global or
// system identity from being shadowed.
func gitIdentityResolves(dir string) bool {
	for _, v := range []string{"GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"} {
		if exec.Command("git", "-C", dir, "var", v).Run() != nil {
			return false
		}
	}
	return true
}

// ensureLocalGitConfig sets key=value as LOCAL git config in dir only when
// key has no effective (any-level) value, leaving an existing value
// untouched.
func ensureLocalGitConfig(dir, key, value string) {
	if out, err := exec.Command("git", "-C", dir, "config", "--get", key).Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return // already configured at some level
	}
	_ = exec.Command("git", "-C", dir, "config", "--local", key, value).Run()
}
