package commands

import (
	"strconv"
	"strings"

	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/occupancy"
	"github.com/hausfold/scruff/internal/registry"
)

type sweepMode int

const (
	// sweepParked touches only lanes with no checkout on disk. Nothing a pane
	// could be sitting in is at risk, which is why the listing runs it.
	sweepParked sweepMode = iota
	// sweepAll additionally considers live checkouts — clean, landed and
	// unoccupied ones only. Opt-in, via `scruff reap`.
	sweepAll
)

// SweepResult is what one sweep did, and what it deliberately did not do.
type SweepResult struct {
	Reaped      []string
	Strays      []string
	SkippedLive []string      // reapable but for a live process standing in the checkout
	Dirty       []string      // reapable but for uncommitted work in the checkout
	Relanded    []string      // landed PR, but the branch committed past it
	Diverged    []string      // landed PR, but the tip isn't built on what merged
	DeadEnds    []deadEndLane // nothing will ever land these: PR closed, repo archived
	Degraded    bool          // occupancy was unknowable, so live checkouts were spared
}

// deadEndLane is a lane nothing will ever land, and the forge record that says
// so. The sweep only ever NAMES these; `scruff reap --dead-ends` is what
// retires them, and it needs the lane itself rather than the sentence about it.
type deadEndLane struct {
	lane Entry
	why  string
}

// Note is the "kept" line a sweep prints for a dead end: the reason, and the
// two ways out. Both are real — the commits were rejected, not landed, so
// rescuing them is as likely to be what you want as dropping them.
func (d deadEndLane) Note() string {
	return d.lane.Label() + " — " + d.why +
		": rescue the commits, or scruff drop " + d.lane.Name()
}

// reapSweep removes every LANDED lane the mode allows, and nothing else.
//
// Every `continue` in here is a safety invariant, not an optimisation. The
// failure direction is always "a branch lingers": a branch that outlives its
// usefulness is a nuisance, a branch reaped with work still on it is the thing
// scruff exists to never do.
//
// What makes a lane reapable is not yet a policy seam (SPEC.md §6.5). It is
// the one decision here that reaches through THREE of scruff's inherited
// opinions at once — occupancy, dirtiness and landedness — and a seam over the
// lot of them has to wait for the shape those settle into.
func (e *Env) reapSweep(mode sweepMode) SweepResult {
	var res SweepResult
	occ := e.Occupancy()
	if !occ.Known() && mode == sweepAll {
		// "Landed and clean" does NOT mean "nobody is standing here". Without a
		// way to ask, the live half of the sweep is unsafe, so degrade to
		// parked-only rather than guess.
		mode = sweepParked
		res.Degraded = true
		// "No provider vouched for absence", which on a developer machine means
		// no lsof. Leases alone never reach this branch: they assert presence
		// only, so a lane nobody leased is still an open question. An embedder
		// that owns every session it serves says so with SCRUFF_OCCUPANCY=lease.
		e.Warn("no lsof — can't tell which checkouts have a pane open, so only PARKED lanes were swept (an embedder that owns every session can answer with SCRUFF_OCCUPANCY=lease)")
	}
	selfTop, _ := gitx.Toplevel(e.Cwd)

	for _, entry := range e.discover() {
		switch entry.State {
		case Stray:
			// A husk: the contents are preserved but git has disowned it.
			// Reported, never swept — `scruff <name>` moves it aside and rebuilds.
			res.Strays = append(res.Strays,
				entry.Label()+" → "+entry.Path)
			continue

		case Live:
			if mode != sweepAll {
				continue
			}
			if entry.Path == selfTop {
				continue // never the checkout we are being run from
			}
			if held := occ.Holders(entry.Path); len(held) > 0 {
				// Landed or not, something live is standing in it. Removing the
				// checkout yanks the cwd out from under it: the shell and the
				// agent keep running in a deleted directory and every
				// subsequent tool call fails.
				res.SkippedLive = append(res.SkippedLive, occupiedNote(entry, held))
				continue
			}
			dirt, err := gitx.Status(entry.Path)
			if err != nil {
				// git could not answer, so we do not know the tree is clean —
				// and this is the branch that DELETES. Uncertainty resolves to
				// keep, out loud.
				res.Dirty = append(res.Dirty, entry.Label()+
					" — git could not read the checkout, so scruff cannot tell whether"+
					" there is unsaved work in it; nothing is reaped on a guess: "+entry.Path)
				continue
			}
			if dirt != "" {
				// Say so. Every other refusal in this loop leaves a note, and
				// this one used to `continue` in silence — so a landed,
				// unoccupied lane held back by one stray untracked file read as
				// a lane scruff had simply forgotten, with the summary line's
				// "unmerged, dirty, or in use" left to guess between.
				res.Dirty = append(res.Dirty, dirtyNote(entry, dirt))
				continue // uncommitted work — leave it for a human
			}
			if !e.Landed(entry.Main, entry.Branch).Landed {
				e.noteRelanded(&res, entry)
				continue
			}
			if _, err := gitx.Run(entry.Main, "worktree", "remove", entry.Path); err != nil {
				continue // free the branch first, or don't touch the branch
			}
		}

		if e.reapBranch(entry.Main, entry.Branch) {
			_ = e.Reg.Delete(entry.Path)
			// The lane is gone, so a fin still asking you to go to it is asking
			// about somewhere that no longer exists — its `Go to lane` action
			// would run `scruff focus` against a name nothing can match. Being
			// reaped IS this lane's answer, so take the fin down and drop the
			// marker, which is one of the two shapes nothing else would ever
			// clear (notify.go's section header has both).
			//
			// The marker is the gate on the trill launch, not an afterthought
			// to it: it reports whether there WAS a fin, and the ordinary reap
			// is of a lane that ended its turn cleanly and has nothing on the
			// ledge. A sweep of forty lanes launches nothing.
			takeDownAsk(askKey(laneID(entry.Main, entry.Name()), nil))
			res.Reaped = append(res.Reaped, entry.Label())
		} else {
			e.noteRelanded(&res, entry)
		}
	}
	e.pruneRegistry()
	// Housekeeping for the same reason pruneRegistry is here: a sweep is the
	// only thing that runs regularly and is allowed to throw state away.
	pruneStaleAsks()
	return res
}

// occupiedNote names WHO is standing there, because "a pane is open in it" is a
// claim scruff cannot actually make and the user cannot check.
//
// lsof observes a cwd, not a pane. The two coincide for a terminal and diverge
// for everything else a checkout accumulates — a dev server, a language server,
// a watcher, a telemetry daemon reparented to pid 1 days ago. Told "a pane is
// open", a user goes looking for a window, finds none, and reasonably concludes
// the tool is wrong; told "pid 46864 node", they see a stray and kill it. The
// verdict is unchanged either way (invariant 2: occupied ⇒ keep) — what changes
// is that the evidence outlives the sweep that found it.
//
// Deliberately NOT a suggestion to kill anything. scruff does not know whose
// process that is, and the whole point of naming it is that the human can tell.
func occupiedNote(entry Entry, held []occupancy.Holder) string {
	return entry.Label() +
		" — something is standing in the checkout: " + occupancy.Describe(entry.Path, held) +
		". Nothing is reaped out from under a live process; close the pane, or if" +
		" that is a stray, end it — then reap again: " + entry.Path
}

// dirtyNote names what is actually in the way, because "dirty" alone sends you
// to `git status` in a checkout you have to find first — and the paths are
// usually the whole answer (the case that prompted this was one untracked
// directory a tool had dropped there, obvious junk the moment it was named).
//
// Deliberately NOT "scruff park it": park commits the tree as a `wip:` commit,
// which makes the branch unlanded and moves the lane from this refusal to the
// next one. The two real ways out are commit it or clean it.
func dirtyNote(entry Entry, porcelain string) string {
	return entry.Label() +
		" — uncommitted work in the checkout: " + dirtyPaths(porcelain) +
		". Nothing is reaped over that; commit it or clean it, then reap again: " + entry.Path
}

// dirtyPaths names what is actually uncommitted, capped at dirtyPathsShown.
//
// Shared with `drop`'s refusal for the same reason the occupancy refusal names
// its processes: "has uncommitted changes" about a checkout you are not
// standing in is unanswerable. Two stray screenshots and a half-written
// migration want opposite decisions, and from here you can see neither.
func dirtyPaths(porcelain string) string {
	lines := strings.Split(strings.TrimRight(porcelain, "\n"), "\n")
	shown := lines
	if len(shown) > dirtyPathsShown {
		shown = shown[:dirtyPathsShown]
	}
	paths := make([]string, 0, len(shown))
	for _, l := range shown {
		paths = append(paths, porcelainPath(l))
	}
	out := strings.Join(paths, ", ")
	if len(lines) > len(shown) {
		out += ", +" + itoa(len(lines)-len(shown)) + " more"
	}
	return out
}

// dirtyPathsShown caps the named paths, so a checkout with a build tree in it
// reports a line rather than a screenful.
const dirtyPathsShown = 3

// porcelainPath is the path out of one `git status --porcelain` line.
//
// Porcelain v1 is "XY path": two status columns then a space, and a rename's
// " -> " tail is worth keeping as written. But the first column is a SPACE for
// an unstaged-only change (" M path"), and this porcelain reaches us through
// gitx.Run, which TrimSpace's the whole output — so the FIRST line, and only
// the first, arrives one column short ("M path"). Cutting a fixed three ate
// the path's first character, and a lane held back by one modified file
// reported "odules/core/haus.sh": not a path anyone can paste, and one that
// reads like corruption rather than an off-by-one. Find the separator instead
// of counting to it, and both shapes parse.
func porcelainPath(l string) string {
	switch {
	case len(l) > 2 && l[2] == ' ':
		l = l[3:]
	case len(l) > 1 && l[1] == ' ':
		l = l[2:]
	}
	// Porcelain C-quotes any path with non-ASCII or special characters
	// (`?? "caf\303\251.txt"`). Trimming the quotes alone leaves the
	// escapes in, which is not a path anyone can paste back.
	if unq, err := strconv.Unquote(l); err == nil {
		l = unq
	}
	return l
}

// noteRelanded records why a lane declined to be reaped, so it says something
// instead of silently persisting — and points at the right fix. Three shapes,
// mutually exclusive, each with its own remedy:
//
//   - "moved on" — real work after the merge → `scruff reship`.
//   - "diverged" — a stale or sideways tip that never built on what merged (a
//     second checkout of the same branch that pushed first, a rebase, an amend).
//     Same nonzero commit count as "moved on" and the opposite remedy: its
//     content already landed, so reshipping would push and PR what the merge
//     already superseded. Remove the checkout instead.
//   - "dead end" — no merged PR at all, no open one, and there never will be
//     either, because the PR was closed unmerged or the repo is archived →
//     `scruff drop`, or `scruff reap --dead-ends` for the lot.
//
// The dead-end question is asked LAST and only when the count is zero, so its
// up-to-three forge calls stay off the path every healthy lane walks.
func (e *Env) noteRelanded(res *SweepResult, entry Entry) {
	name := entry.Label()
	n, pr, diverged := e.postMergeAhead(entry.Main, entry.Branch)
	if n == 0 {
		// The plain sweep still won't touch a dead end — the commits are
		// unlanded and it is automatic — but a lane that can never land has to
		// SAY so, or it reads exactly like one still in flight and outlives
		// everything around it. `scruff reap --dead-ends` is the typed word
		// that acts on this list.
		if why := e.deadEnd(entry.Main, entry.Branch); why != "" {
			res.DeadEnds = append(res.DeadEnds, deadEndLane{lane: entry, why: why})
		}
		return
	}
	if diverged {
		res.Diverged = append(res.Diverged,
			name+" — merged PR #"+itoa(pr)+", but the tip isn't built on what merged."+
				" Its content already landed; remove the checkout instead of reshipping it.")
		return
	}
	res.Relanded = append(res.Relanded,
		name+" — merged PR #"+itoa(pr)+
			", "+itoa(n)+" commit(s) since, covered by no PR: scruff reship "+entry.Name())
}

// reapBranch deletes a branch, and ONLY once it has provably landed.
//
// `git branch -d` is not the gate: it measures against the checkout's current
// HEAD, so it happily deletes a branch merged only into whatever side branch the
// main checkout is parked on. Landed measures against the repo's DEFAULT branch
// (and the branch's merged PR for squash merges), which is the question we
// actually mean. Having confirmed it, -d/-D is just the mechanism: -d first so
// git's own safety net still gets a say, -D for the squash case it cannot see.
func (e *Env) reapBranch(main, branch string) bool {
	v := e.Landed(main, branch)
	if !v.Landed {
		return false
	}
	// The ledger is written BEFORE the delete and only here, because this is the
	// single choke point every deletion goes through — the listing's parked
	// sweep, `scruff reap`, and the remove hook all arrive at this function. The
	// SHA is only resolvable while the branch still exists, and it is the whole
	// point: `git branch -D` takes the branch's reflog with it, so without this
	// line a lane that went away is unrecoverable AND unexplainable.
	e.noteReaped(main, branch, v)
	if gitx.OK(main, "branch", "-d", branch) {
		return true
	}
	return gitx.OK(main, "branch", "-D", branch)
}

// pruneRegistry drops rows whose branch no longer means anything.
func (e *Env) pruneRegistry() {
	_ = e.Reg.Prune(func(row registry.Row) bool {
		return e.branchAlive(Entry{
			Main:   row.Main,
			Branch: row.Branch,
			Path:   row.Path,
			State:  checkoutState(row.Path),
		})
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
