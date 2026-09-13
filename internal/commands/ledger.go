package commands

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/ui"
)

// This file answers the one question scruff could not answer about its own most
// consequential act: *why did that lane go away?*
//
// A branch deletion destroys its own evidence. `git branch -D` takes the
// branch's reflog with it, `git worktree remove` takes `.git/worktrees/<name>`
// with it, and scruff kept nothing — so a lane that vanished between two
// listings left a repo where the only honest answer was "something deleted it,
// and the record of what died with the thing it recorded". `watch` emits a
// `reaped` event, but only to whoever happened to be streaming at that instant.
//
// So every reap now writes one line first: what it deleted, the SHA it was at,
// and which rung of the ladder (§3) justified it. That makes the act
// attributable AND reversible — the SHA outlives the reflog entry, so
// `git branch <name> <sha>` still works long after git has forgotten.
//
// Deliberately NOT in the lane base: the base is globbed for checkouts
// (`discover`), and this is machine state, not a lane. It sits beside the
// occupancy leases in `stateDir()`.

// ledgerFile is the reap ledger's path. One file per machine, not per repo: the
// question it answers ("where did my lane go?") is asked by someone who no
// longer knows which repo to look in — that is the whole predicament.
func (e *Env) ledgerFile() string { return filepath.Join(stateDir(), "reaped.log") }

// ledgerEntry is one reaped lane, as one tab-separated line.
//
// TSV, and the same shape as the registry, for the same reason: a format you
// can read with your eyes and `cut` with your hands survives scruff's own
// versions. Field order is append-only — readers index, and a reordered column
// would silently re-label every historical line.
type ledgerEntry struct {
	When   string // RFC3339, so it sorts lexically
	Repo   string
	Name   string
	Branch string
	SHA    string // the recovery handle: git branch <branch> <sha>
	Via    string // which rung of the landed ladder said yes
	PR     int
}

func (l ledgerEntry) line() string {
	return strings.Join([]string{
		l.When, l.Repo, l.Name, l.Branch, l.SHA, l.Via, itoa(l.PR),
	}, "\t")
}

// noteReaped appends one line for a branch that is about to be deleted.
//
// Called BEFORE the delete, because the SHA is the point and it is only
// resolvable while the branch still exists. Best-effort in both directions: a
// ledger that cannot be written must never stop a reap that has already been
// judged safe, and a reap must never be reported as recorded when it wasn't.
func (e *Env) noteReaped(main, branch string, v Verdict) {
	sha := gitx.Rev(main, branch)
	if sha == "" {
		return // an unborn branch has nothing to recover, and nothing to record
	}
	entry := ledgerEntry{
		When:   time.Now().UTC().Format(time.RFC3339),
		Repo:   repoKey(main),
		Name:   strings.TrimPrefix(branch, "worktree-"),
		Branch: branch,
		SHA:    sha,
		Via:    v.Via,
		PR:     v.PR,
	}
	if entry.Via == "" {
		entry.Via = "unknown"
	}
	file := e.ledgerFile()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return
	}
	// O_APPEND on a single short line is atomic enough for the parallel-agent
	// case this exists to serve: several panes closing at once each write well
	// under PIPE_BUF, so their lines interleave whole rather than shredding one
	// another. The registry needs a lock because it read-modify-writes; this
	// never reads.
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(entry.line() + "\n")
}

// Ledger prints the reap ledger, newest last — `scruff reaped`.
//
// Newest LAST, against the usual instinct, because this is read in a terminal
// right after noticing something missing: the answer you want is the one your
// prompt is already sitting next to.
//
// Every row carries the command that undoes it. A ledger that told you what
// happened and left you to work out the recovery yourself would be a post-mortem
// — this is meant to be an undo button with a timestamp on it.
func (e *Env) Ledger() error {
	b, err := os.ReadFile(e.ledgerFile())
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		ui.Say("no lanes reaped on this machine yet — the ledger lives at %s", e.ledgerFile())
		return nil
	}
	shown := 0
	for _, l := range gitx.Lines(string(b)) {
		f := strings.Split(l, "\t")
		if len(f) < 7 {
			continue // a truncated write, or a future column order — skip, never guess
		}
		when, repo, name, branch, sha, via, pr := f[0], f[1], f[2], f[3], f[4], f[5], f[6]
		why := via
		if pr != "0" && pr != "" {
			why += ", PR #" + pr
		}
		ui.Say("%s  %s/%s — reaped: %s", when, repo, name, why)
		ui.Say("    recover: git -C <%s checkout> branch %s %s", repo, branch, shortSHA(sha))
		shown++
	}
	if shown == 0 {
		ui.Say("the ledger at %s has no rows this version can read", e.ledgerFile())
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
