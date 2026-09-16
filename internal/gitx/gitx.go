// Package gitx is scruff's git layer: subprocess calls to the real `git`, never
// a reimplementation of it.
//
// Shelling out is a deliberate choice, not laziness. scruff's whole job is to
// agree with what the user's git does — their config, their hooks, their
// credential helper, their version's merge semantics. A pure-Go git library
// agrees with a *model* of git, and every place the model drifts is a place
// scruff reaps a branch git would have called unmerged.
package gitx

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

// Run executes git in dir and returns trimmed stdout. stderr is folded into the
// error so callers can report why something failed.
//
// An EMPTY dir is an error, never "use the current directory". That fallback is
// how a path-building bug turns into git operating on whatever repo the user
// happens to be standing in — and scruff's whole job is manipulating repos other
// than its own. `git -C ""` has exactly this behaviour, which is what let the
// test suite commit into the real scruff checkout (see test/scruff.bats's fixture
// guards). Refusing it here means the same class of bug surfaces as a loud
// error instead of a commit in someone's tree.
func Run(dir string, args ...string) (string, error) {
	if dir == "" {
		return "", errors.New("git: refusing to run with no directory (this would fall through to the caller's cwd)")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(out.String()), errors.New(msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// OK reports whether the command succeeded, discarding its output. For the
// many git calls that are questions rather than requests.
func OK(dir string, args ...string) bool {
	_, err := Run(dir, args...)
	return err == nil
}

// Exit runs git and returns its trimmed stdout WITH its exit status, for the
// questions git answers in a code rather than a yes/no: `merge-tree
// --write-tree` exits 1 for a conflict and lists the conflicted paths on
// stdout, which Run would fold into an error. -1 means git could not be run at
// all. The empty-dir refusal is Run's, for Run's reason.
func Exit(dir string, args ...string) (string, int) {
	if dir == "" {
		return "", -1
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return "", -1
		}
		return strings.TrimSpace(out.String()), ee.ExitCode()
	}
	return strings.TrimSpace(out.String()), 0
}

// Lines splits trimmed output into non-empty lines.
func Lines(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// MainCheckout returns the main checkout backing any worktree of a repo.
//
// It must FAIL — not fall back — when dir is gone or isn't a repo. Without
// that, the caller resolves an empty path against its own cwd, and a stale
// registry row pointing at a deleted checkout makes scruff list the current
// repo's branches a second time under a repo literally named ".". That was a
// real bug in the bash version (haus#131) and the fix belongs here, once.
func MainCheckout(dir string) (string, error) {
	common, err := Run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if common == "" {
		return "", errors.New("git gave no common dir for " + dir)
	}
	return filepath.Dir(common), nil
}

// Toplevel is the working-tree root of the repo containing dir.
func Toplevel(dir string) (string, error) {
	return Run(dir, "rev-parse", "--show-toplevel")
}

// CurrentBranch is the checked-out branch, or "" on a detached HEAD.
func CurrentBranch(dir string) string {
	b, err := Run(dir, "branch", "--show-current")
	if err != nil {
		return ""
	}
	return b
}

// DefaultBranch is the branch a PR in this repo would land on.
//
// NOT `symbolic-ref HEAD`: that is whatever the main checkout happens to have
// checked out right now. Land a side branch there that happens to contain an
// agent branch, and the agent branch reads as merged though it never reached
// main — and gets reaped, taking the only copy of that work with it.
func DefaultBranch(main string) string {
	branch, _ := DefaultBranchDetail(main)
	return branch
}

// DefaultBranchDetail is DefaultBranch plus WHICH rung answered, for `scruff
// doctor` (SPEC.md §6.4). One implementation, deliberately: the doctor's whole
// value here is reporting the branch the sweep will actually measure against,
// and a second copy of this ladder is a second copy that can disagree with it.
//
// via is `origin-head` | `conventional` | `head` | `none`, weakest last. Only
// the first is a fact the repo asserted about itself; `conventional` is scruff
// guessing from a name, and `head` is scruff guessing from whatever the main
// checkout happens to have out — the resolution most worth seeing before you
// trust a reap, because it is the one that moves when somebody checks out a
// side branch in the main checkout.
func DefaultBranchDetail(main string) (branch, via string) {
	if d, err := Run(main, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && d != "" {
		return strings.TrimPrefix(d, "origin/"), "origin-head"
	}
	for _, d := range []string{"main", "master", "trunk"} {
		if OK(main, "show-ref", "-q", "--verify", "refs/heads/"+d) {
			return d, "conventional"
		}
	}
	// No conventional default and no origin/HEAD (a fresh repo, an odd remote):
	// HEAD is the best guess left.
	if d, err := Run(main, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		return d, "head"
	}
	return "HEAD", "none"
}

// IsAncestor reports whether ref is reachable from base — the offline,
// always-safe half of the landed question (SPEC.md §3): fast-forward, merge
// commit, and any rebase that kept the commits.
func IsAncestor(dir, ref, base string) bool {
	return OK(dir, "merge-base", "--is-ancestor", ref, base)
}

// HasBranch reports whether a local branch exists.
func HasBranch(dir, branch string) bool {
	return OK(dir, "show-ref", "-q", "--verify", "refs/heads/"+branch)
}

// Rev resolves a revision to a full OID, or "" if it doesn't resolve.
func Rev(dir, rev string) string {
	oid, err := Run(dir, "rev-parse", rev)
	if err != nil {
		return ""
	}
	return oid
}

// ShortRev resolves a revision to an abbreviated OID.
func ShortRev(dir, rev string) string {
	oid, err := Run(dir, "rev-parse", "--short", rev)
	if err != nil {
		return ""
	}
	return oid
}

// Status is `git status --porcelain` with the failure KEPT, for the callers
// whose "" must not be allowed to mean "clean".
//
// Porcelain's "" is doing double duty: a clean tree and a checkout git could
// not read at all (a corrupt index, a permission wall, an interrupted rebase)
// look identical to it. That is fine where the answer only decides whether to
// park, and wrong where it decides whether to DELETE — the sweep must resolve
// an unknown to "keep", which it cannot do without seeing the error.
func Status(dir string) (string, error) { return Run(dir, "status", "--porcelain") }

// Porcelain returns `git status --porcelain` output; "" means a clean tree.
func Porcelain(dir string) string {
	s, err := Status(dir)
	if err != nil {
		return ""
	}
	return s
}

// Dirty reports whether the working tree has any change at all, tracked or not.
func Dirty(dir string) bool { return Porcelain(dir) != "" }

// HasCommits reports whether HEAD resolves — false in a repo with no commits
// yet, where a worktree has to be created with --orphan.
func HasCommits(dir string) bool {
	return OK(dir, "rev-parse", "--verify", "HEAD")
}

// Subject is the first line of a revision's commit message.
func Subject(dir, rev string) string {
	s, err := Run(dir, "log", "-1", "--format=%s", rev)
	if err != nil {
		return ""
	}
	return s
}

// PushedAnywhere reports whether a commit is contained in any remote-tracking
// branch. Rewriting such a commit turns "give me my files back" into a
// force-push, so unpark refuses it.
func PushedAnywhere(dir, rev string) bool {
	out, err := Run(dir, "branch", "-r", "--contains", rev)
	return err == nil && out != ""
}

// Remotes lists the repo's remotes in git's own order, which is alphabetical.
//
// Identity takes the FIRST one that answers (RemoteSlug); `scruff doctor` wants
// all of them, because two remotes resolving to different slugs is the fork
// workflow and the only place it shows is a report (SPEC.md §4).
func Remotes(dir string) []string {
	out, err := Run(dir, "remote")
	if err != nil || out == "" {
		return nil
	}
	return Lines(out)
}

// RemoteSlug is owner/name parsed from a remote URL, for the forge adapter.
//
// This — not the directory basename — is a repo's identity (SPEC.md §4). Two
// checkouts named `api` under different orgs is the common case, not the
// exotic one.
//
// `origin` wins outright, and the rungs below it are a fallback for a repo that
// has no `origin` at all — NOT a vote between remotes. In a fork workflow the
// slug is therefore the fork, which is the deliberate answer and also the one
// that makes every PR query miss; `scruff doctor` names that (`fork-remotes`),
// and §4 says why the landing check does not go looking for a better remote.
func RemoteSlug(dir string) (string, error) {
	var url string
	var err error
	for _, remote := range []string{"origin", "upstream"} {
		if url, err = Run(dir, "remote", "get-url", remote); err == nil && url != "" {
			break
		}
	}
	if url == "" {
		// Any remote at all, alphabetically, before giving up.
		names := Remotes(dir)
		if len(names) == 0 {
			return "", errors.New("no remote to take an identity from")
		}
		if url, err = Run(dir, "remote", "get-url", names[0]); err != nil {
			return "", err
		}
	}
	return ParseSlug(url), nil
}

// ParseSlug strips scheme, user and host off a remote URL, leaving owner/name.
func ParseSlug(url string) string {
	url = strings.TrimSuffix(url, ".git")
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
	}
	if i := strings.LastIndex(url, "@"); i >= 0 {
		url = url[i+1:]
	}
	// Drop host + its separator (':' for scp-style, '/' for URLs).
	if i := strings.IndexAny(url, ":/"); i >= 0 {
		url = url[i+1:]
	}
	return strings.Trim(url, "/")
}
