package commands

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/hausfold/scruff/internal/exitcode"
	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/registry"
	"github.com/hausfold/scruff/internal/ui"
)

// Park sets the working tree aside as one wip: commit on the current branch.
//
// Why this exists at all: `git stash` looks per-worktree and isn't. The stash
// stack lives in the shared .git dir, so every worktree of a repo — and the main
// checkout — push and pop ONE stack. Two parallel agents stashing means either
// can pop the other's entry, and the loser's edits land in a tree that never
// asked for them. A wip commit has no such stack: it sits on the branch only
// this pane has checked out, survives a pane close, and `scruff unpark` puts it
// back.
func (e *Env) Park(label string) error {
	top, err := gitx.Toplevel(e.Cwd)
	if err != nil {
		return exitcode.Usagef("not in a git repo — nothing to park")
	}
	branch := gitx.CurrentBranch(top)
	// Detached HEAD is the one place this would recreate stash's failure mode:
	// the commit is reachable from nothing, so the next checkout orphans it.
	if branch == "" {
		return exitcode.Refusedf("HEAD is detached — a parked commit here would be unreachable. Check out a branch first.")
	}
	// A label that is really a LANE NAME is the one way this command silently
	// does the wrong thing, and `scruff drop` sends people straight into it:
	// its dirty refusal says "`scruff park` them first" about a lane in another
	// repo, so the obvious next keystroke is `scruff park <that lane>`. Park has
	// always worked on the checkout you are standing in, so from anywhere else
	// that typed the lane's name onto THIS tree — reporting "nothing to park"
	// about a repo nobody asked about, exit 0, the named lane untouched.
	elsewhere, isLane := e.laneNamed(label, top)

	dirty := gitx.Porcelain(top)
	if dirty == "" {
		if isLane {
			return exitcode.Refusedf("'%s' is a lane in %s, not a label — park works on the checkout you're standing in, and %s is clean here. %s",
				label, filepath.Base(elsewhere.Main), branch, parkThere(elsewhere))
		}
		ui.Say("nothing to park — %s is already clean.", branch)
		return nil
	}

	stamp := time.Now().Format("2006-01-02 15:04")
	msg := "wip: parked " + stamp
	if label != "" {
		msg = "wip: " + label + " (parked " + stamp + ")"
	}
	if err := wipCommit(top, msg); err != nil {
		return exitcode.Usagef("commit failed — nothing was parked; `git -C %s status` will say why.", top)
	}

	ui.Say("parked %d change(s) on %s → %s", len(gitx.Lines(dirty)), branch, gitx.ShortRev(top, "HEAD"))
	if !strings.HasPrefix(branch, "worktree-") {
		ui.Say("note: '%s' isn't an agent branch — don't push this wip commit.", branch)
	}
	// Dirty and the label names a lane: the park itself was real work, so it
	// stands — but say whose tree it took, because the odds are the label was
	// meant as a target.
	if isLane {
		ui.Warn("'%s' is also a lane in %s — this parked %s here, not that lane. %s",
			label, filepath.Base(elsewhere.Main), branch, parkThere(elsewhere))
	}
	ui.Say("bring them back with: scruff unpark")
	return nil
}

// laneNamed resolves a label that names some OTHER lane's checkout.
//
// The registry, not `discover` — invariant 3 says it is the source of truth,
// and it answers in one file read where discover shells out to git per lane.
// Park is typed constantly and has to stay free; the surprise is worth exactly
// one stat of a file scruff already owns.
func (e *Env) laneNamed(label, top string) (registry.Row, bool) {
	if label == "" || e.Reg == nil {
		return registry.Row{}, false
	}
	rows, err := e.Reg.Load()
	if err != nil {
		return registry.Row{}, false
	}
	// `<repo>/<name>` is the qualified spelling matchLane accepts and the
	// listing prints, so a user who copied a cell out of it lands here too.
	repo, name := "", label
	if i := strings.Index(label, "/"); i >= 0 {
		repo, name = label[:i], label[i+1:]
	}
	for _, r := range rows {
		if r.Name != name || r.Path == top {
			continue
		}
		if repo != "" && !strings.HasPrefix(filepath.Base(r.Main), repo) {
			continue
		}
		return r, true
	}
	return registry.Row{}, false
}

// parkThere is the next keystroke for parking a lane that is not this one —
// which depends on whether its checkout is still on disk. A parked lane has
// none, so `cd` into it is advice that fails.
func parkThere(row registry.Row) string {
	if checkoutState(row.Path) != Live {
		return "That lane has no checkout on disk — `scruff " + row.Name + "` rebuilds it, then park from inside it."
	}
	return "Park it from inside it: cd " + row.Path + " && scruff park"
}

// wipCommit stages everything, untracked included, and commits it. Sweeping in
// untracked files is the point of "set the tree aside" — a half-written new file
// is exactly the work a pane close would otherwise lose.
func wipCommit(top, msg string) error {
	if _, err := gitx.Run(top, "add", "-A"); err != nil {
		return err
	}
	_, err := gitx.Run(top, "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
	return err
}

// Unpark rewinds the last wip: commit, putting those changes back in the
// working tree, uncommitted — the `git stash pop` half.
func (e *Env) Unpark() error {
	top, err := gitx.Toplevel(e.Cwd)
	if err != nil {
		return exitcode.Usagef("not in a git repo — nothing to unpark")
	}
	if gitx.CurrentBranch(top) == "" {
		return exitcode.Refusedf("HEAD is detached — check out a branch first.")
	}
	subject := gitx.Subject(top, "HEAD")
	if !strings.HasPrefix(subject, "wip:") {
		return exitcode.Refusedf("HEAD isn't a parked commit (it's %q) — nothing to unpark.", subject)
	}
	if gitx.Rev(top, "HEAD^") == "" {
		return exitcode.Refusedf("that wip commit is the branch's first commit — there's nothing to rewind onto.")
	}
	// Refuse to rewrite anything already published. A parked commit that got
	// pushed is visible in an open PR, so rewinding it locally turns "give me my
	// files back" into a force-push — never do that behind the user's back.
	if gitx.PushedAnywhere(top, "HEAD") {
		return exitcode.Refusedf("that wip commit is already pushed — unparking would rewrite published history. If you mean it: git reset --mixed HEAD^")
	}
	// --mixed, not --hard: the files stay on disk exactly as parked and go back
	// to being uncommitted (staged adds become untracked again), which is what
	// pop does.
	if _, err := gitx.Run(top, "reset", "-q", "--mixed", "HEAD^"); err != nil {
		return exitcode.Usagef("reset failed — the parked commit is untouched.")
	}
	ui.Say("unparked %q on %s — those changes are back in the working tree, uncommitted.",
		subject, gitx.CurrentBranch(top))
	return nil
}
