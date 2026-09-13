package commands

import (
	"strings"

	"github.com/hausfold/scruff/internal/config"
	"github.com/hausfold/scruff/internal/exitcode"
	"github.com/hausfold/scruff/internal/gitx"
)

// Focus is "take me to that lane" — `scruff focus <name>`, or
// `scruff focus <repo>/<name>` when the name lives in two repos.
//
// It is `scruff <name>`'s narrower sibling, and the difference is the whole
// reason it exists as its own seam. Resume assumes the lane needs rebuilding
// and its conversation reopening, so on a machine that opens a window per lane
// it opens ANOTHER one. Focus assumes the lane is already running somewhere and
// asks the desktop to look at it — which a machine with a window manager can
// answer by raising the window it already has, and a machine without one can't
// answer at all.
//
// So the built-in is resume. A scruff with no `focus` hook configured does what
// scruff has always done for "go to this lane", and a consumer that knows how to
// find windows overrides exactly that step. Nothing about terminals, sessions
// or window ids enters scruff either way.
//
// The caller is usually not a human: trill's `focus_lane` banner action runs
// this when a lane's banner is clicked (its ActionRouter). That is why the
// no-hook path still has to do something useful rather than fail — and why this
// is the one command whose LATENCY is a feature. A click that takes a second to
// land reads as a click that missed.
func (e *Env) Focus(want string) error {
	if want == "" {
		return exitcode.Usagef("name the lane to go to: scruff focus <name>  (scruff, to see them)")
	}
	entry, err := e.lane(want)
	if err != nil {
		return err
	}

	if e.Cfg.Defined(config.HookFocus) {
		payload := e.hookPayload(entry.Main, entry.Branch, entry.Path, e.agentForPath(entry.Path))
		payload["state"] = string(entry.State)
		res := e.Cfg.Do(config.HookFocus, payload)
		e.noteHook(res)
		if res.Answer != config.Defer {
			return hookOutcome(config.HookFocus, res)
		}
		// Deferred: the window layer looked and found nothing of this lane to
		// raise. Falling through to resume is what makes that honest — a lane
		// with no window is detached, not gone, and opening one onto it is the
		// answer to the click.
	}
	// By ENTRY, not by name: matchLane has already run above, and resume's own
	// lookup would be the same whole-machine discover over again.
	return e.resumeEntry(entry, false)
}

// lane names the lane `focus` was asked for, taking the registry's word for it
// when the registry is provably right, and falling back to matchLane when it
// isn't.
//
// matchLane is thorough by design: discover reads the registry, globs every
// checkout under the base, and asks every main checkout it reached for its
// worktree-* branches, so it finds a lane whose registry row was lost. That
// thoroughness is ~130 git subprocesses on a machine holding a few dozen lanes
// — a second of forking, MEASURED — and every other caller of it (drop, reap,
// resume from a prompt) is a considered act where a second is nothing.
//
// A banner click is not. This is the path trill runs on `focus_lane`, and the
// whole of it — scruff, the hook, the window raise — has to feel like the click
// did it. So take invariant 3 at its word: the registry is the source of
// truth. One row, matched unambiguously by name, whose checkout resolves and
// whose branch is the branch it claims, is the same Entry discover would build
// — and answering from it costs two git calls instead of a hundred and thirty.
//
// Anything less certain hands over: no row, several rows, a checkout that is
// parked or strayed, a branch that has moved on. Those are exactly the cases
// discover exists for, including the ambiguity errors it words for the user.
func (e *Env) lane(want string) (Entry, error) {
	if entry, ok := e.registryLane(want); ok {
		return entry, nil
	}
	return e.matchLane(want, "scruff focus")
}

// registryLane is that fast path: one live, unambiguous registry row, verified
// on disk. It accepts a strict SUBSET of what matchLane accepts, which is what
// makes it safe to try first — a lane it takes is a lane matchLane would have
// returned, and everything else falls through unanswered rather than wrong.
func (e *Env) registryLane(want string) (Entry, bool) {
	repo, name := "", want
	if i := strings.Index(want, "/"); i >= 0 {
		repo, name = want[:i], want[i+1:]
	}
	if name == "" {
		return Entry{}, false
	}
	rows, err := e.Reg.Load()
	if err != nil {
		return Entry{}, false
	}
	var found Entry
	hits := 0
	for _, row := range rows {
		if row.Main == "" || row.Branch == "" || row.Path == "" {
			continue
		}
		// The branch is the name's source, exactly as Entry.Name is — never the
		// row's own Name column, which a hand-edited registry can disagree with.
		if strings.TrimPrefix(row.Branch, "worktree-") != name {
			continue
		}
		if !repoMatches(row.Main, repo) {
			continue
		}
		hits++
		found = Entry{Main: row.Main, Branch: row.Branch, Path: row.Path}
	}
	// Not exactly one: `scruff focus <name>` in two repos is an ambiguity with a
	// worded refusal, and matchLane owns the wording.
	if hits != 1 {
		return Entry{}, false
	}
	// The registry records where a checkout WAS. Two questions make that a fact
	// again — does git resolve it, and is it still on this branch — and they are
	// the same two discover would ask on the way to the same answer.
	if checkoutState(found.Path) != Live {
		return Entry{}, false
	}
	if gitx.CurrentBranch(found.Path) != found.Branch {
		return Entry{}, false
	}
	found.State = Live
	return found, true
}
