package commands

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hausfold/scruff/internal/exitcode"
	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/occupancy"
	"github.com/hausfold/scruff/internal/ui"
)

// This file is scruff's answer to the lanes `reap` will never take: the ones
// whose PR was CLOSED unmerged, and the ones in a repo the forge has archived.
//
// Both were reported as "not landed", which is correct and also useless — the
// work cannot land, so the lane sits in every listing forever and the only exit
// is `git branch -D` by hand, outside scruff, unrecorded.
//
// The fix is deliberately NOT to widen `reap`. A closed PR means the work was
// REJECTED, not landed, and an archived repo means it can no longer be
// submitted — in both cases the commits are the only copy, and a sweep that
// deleted them would be scruff doing the exact thing it exists to never do. The
// asymmetry is the point: `reap` is automatic and therefore may only ever take
// landed work; `drop` is typed by a human at a named lane and may take
// anything, because a person just said so and the ledger can hand it back.

// matchLane resolves a lane by `name` or `<repo>/<name>`, exact first, then by
// a unique PREFIX of either part — `verb` is the command prefix the error lines
// quote (`scruff`, `scruff reship`), so each refusal suggests its own form.
// The prefix pass exists because the consumer of this resolution is the
// LISTING: its cells arrive cut (`test-producer-deskt…`, repo `joshua…`), and
// what the user can see is all the user can type. Only an UNAMBIGUOUS prefix
// resolves; several hits still die, now naming every lane they match.
func (e *Env) matchLane(want, verb string) (Entry, error) {
	repo, name := "", want
	if i := strings.Index(want, "/"); i >= 0 {
		repo, name = want[:i], want[i+1:]
	}
	repo, name = cutCell(repo), cutCell(name)

	var exact, prefix []Entry
	for _, entry := range e.discover() {
		if !e.branchAlive(entry) {
			continue
		}
		if repo != "" {
			r := filepath.Base(entry.Main)
			if r != repo && !strings.HasPrefix(r, repo) {
				continue
			}
		}
		switch {
		case entry.Name() == name:
			exact = append(exact, entry)
		case name != "" && strings.HasPrefix(entry.Name(), name):
			prefix = append(prefix, entry)
		}
	}

	qualified := func(m Entry) string { return filepath.Base(m.Main) + "/" + m.Name() }
	switch {
	case len(exact) == 1:
		return exact[0], nil
	case len(exact) > 1:
		return Entry{}, exitcode.Usagef("'%s' exists in more than one repo — qualify it: %s <repo>/%s", name, verb, name)
	case len(prefix) == 1:
		ui.Say("'%s' is '%s' in %s — matched by prefix", want, prefix[0].Name(), filepath.Base(prefix[0].Main))
		return prefix[0], nil
	case len(prefix) > 1:
		labels := make([]string, 0, len(prefix))
		for _, m := range prefix {
			labels = append(labels, qualified(m))
		}
		return Entry{}, exitcode.Usagef("'%s' matches several lanes — be more specific: %s", want, strings.Join(labels, ", "))
	default:
		return Entry{}, exitcode.Usagef("no lane named '%s' — run: scruff", want)
	}
}

// cutCell unwraps a cell the listing had to cut: snug marks the elision with
// `…` (U+2026), which a shell may have handed back as `...`, and pastes often
// carry the surrounding space. What remains is the prefix matchLane resolves by.
func cutCell(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "…")
	return strings.TrimSuffix(s, "...")
}

// Drop retires a lane whose work will never land — `scruff drop <name>`.
func (e *Env) Drop(want string) error {
	if want == "" {
		return exitcode.Usagef("name the lane to drop: scruff drop <name>  (scruff, to see them)")
	}
	entry, err := e.matchLane(want, "scruff drop")
	if err != nil {
		return err
	}
	return e.retire(entry)
}

// retire is Drop's destructive half, with the lane already resolved — the seam
// `scruff reap --dead-ends` arrives through, so the bulk form and the single
// typed name destroy a lane by exactly the same code, refusals included.
//
// Everything `reap` refuses on is refused here too EXCEPT landedness: a pane
// standing in the checkout still wins (removing it yanks the cwd out from under
// a running client), and so does an uncommitted tree, because an unlanded lane's
// dirt has no PR anywhere to fall back on. Only the "has it landed?" gate is
// waived, and only because a human said so — a lane name, or the flag. Nothing
// automatic reaches here, which is the whole of SPEC.md §6.4b's asymmetry.
func (e *Env) retire(entry Entry) error {
	label := entry.Label()

	if entry.State == Live {
		selfTop, _ := gitx.Toplevel(e.Cwd)
		if entry.Path == selfTop {
			return exitcode.Refusedf("that is the checkout you are standing in — drop %s from another pane", label)
		}
		// Named, not merely asserted. A drop is a refusal a human has to
		// clear before they can proceed, so "close it first" is useless advice
		// when the thing standing there is a dev server they forgot about
		// rather than a window they can close. Same evidence the reap prints.
		if held := e.Occupancy().Holders(entry.Path); len(held) > 0 {
			return exitcode.Refusedf("something is standing in %s: %s — close the pane, or end that process, first",
				label, occupancy.Describe(entry.Path, held))
		}
		// Named, for the same reason, and pointed AT the checkout: park works
		// on the tree you are standing in, so "`scruff park` them first" typed
		// from the pane that just read this refusal parks the wrong repo — or,
		// if that one is clean, nothing at all while reporting success.
		dirt, err := gitx.Status(entry.Path)
		if err != nil {
			// git could not answer, and this is the branch that DELETES. Same
			// rule the sweep holds to: uncertainty resolves to keep, out loud.
			return exitcode.Refusedf("git couldn't read %s's checkout, so scruff can't tell whether there's unsaved work in it — nothing is dropped on a guess: %s",
				label, entry.Path)
		}
		if dirt != "" {
			return exitcode.Refusedf("%s has uncommitted changes and no PR to fall back on: %s — park them from inside it first: cd %s && scruff park",
				label, dirtyPaths(dirt), entry.Path)
		}
	}

	// Ledger BEFORE anything is destroyed, and echo the recovery line even on
	// the happy path. A drop is the one deletion scruff performs that no forge
	// record justifies, so the undo has to be in front of you at the moment it
	// happens rather than filed away for a bad day.
	sha := gitx.Rev(entry.Main, entry.Branch)
	e.noteReaped(entry.Main, entry.Branch, Verdict{Via: "dropped", Confidence: "certain"})

	if entry.State == Live {
		if _, err := gitx.Run(entry.Main, "worktree", "remove", entry.Path); err != nil {
			if _, err := gitx.Run(entry.Main, "worktree", "remove", "--force", entry.Path); err != nil {
				return exitcode.Refusedf("git wouldn't remove the checkout at %s — the branch is untouched (%v)", entry.Path, err)
			}
		}
		if _, err := os.Stat(entry.Path); err == nil {
			_ = os.RemoveAll(entry.Path)
		}
	}
	if !gitx.OK(entry.Main, "branch", "-D", entry.Branch) {
		return exitcode.Refusedf("couldn't delete %s — the checkout is gone but the branch remains", entry.Branch)
	}
	_ = e.Reg.Delete(entry.Path)

	ui.Say("dropped %s — %s was at %s", label, entry.Branch, shortSHA(sha))
	ui.Say("undo: git -C %s branch %s %s", entry.Main, entry.Branch, shortSHA(sha))
	return nil
}

// deadEnd is why a lane can never land, or "" when it still can.
//
// Asked only of lanes a sweep has already declined to reap, so its forge calls
// never touch the common path — a listing of healthy lanes pays nothing for it.
func (e *Env) deadEnd(main, branch string) string {
	if e.repoArchived(main) {
		slug, _ := gitx.RemoteSlug(main)
		return slug + " is archived on the forge — no PR can ever land this"
	}
	if pr := e.closedPR(main, branch); pr > 0 {
		return "PR #" + itoa(pr) + " was closed unmerged — nothing is going to land these commits"
	}
	return ""
}

// repoArchived reports whether the forge has archived this repo.
func (e *Env) repoArchived(main string) bool {
	slug, err := gitx.RemoteSlug(main)
	if err != nil || slug == "" {
		return false
	}
	out := e.cachedForge("archived-"+slug,
		"repo", "view", slug, "--json", "isArchived", "--jq", ".isArchived")
	return strings.TrimSpace(out) == "true"
}

// closedPR is the number of this branch's most recent PR that was closed
// WITHOUT merging, or 0.
//
// `--state closed` is the forge's word for "not open", so it returns merged PRs
// too; the jq filter is what makes this mean what it says. A branch with both a
// merged and a closed PR is not a dead end — the merged one landed something —
// so the merged case is checked first by the caller.
//
// And the forge answers about the branch NAME, which a reaped lane's successor
// wears too. `ownsPR` is what keeps a lane cut this morning from being called a
// dead end because last week's lane of the same name had its PR closed — that
// verdict reads as "drop this, nothing will ever land it", which is the worst
// possible thing to say about work that has not been reviewed once.
func (e *Env) closedPR(main, branch string) int {
	slug, err := gitx.RemoteSlug(main)
	if err != nil || slug == "" {
		return 0
	}
	if state, _, _ := e.mergedPR(main, branch); state == "MERGED" {
		return 0
	}
	// An OPEN PR is a louder "this can still land" than a closed one is a "it
	// can't". Closing a PR and opening a fresh one on the same branch is
	// ordinary — a wrong base, a thread gone stale, a rename — and the closed
	// record outlives the replacement forever. Without this rung the lane reads
	// as rejected while it sits in review: a wrong word on a `kept` line before
	// `reap --dead-ends` existed, and a deletion of work under review after it.
	if _, pr := e.openMapLookup(main, branch); pr > 0 {
		return 0
	}
	out := e.cachedForge("closed-"+slug+"-"+branch,
		"pr", "list", "-R", slug, "--head", branch, "--state", "closed", "--limit", "5",
		"--json", "number,state,headRefOid,closedAt",
		"--jq", `[.[] | select(.state == "CLOSED")] | first // empty | "\(.number)\t\(.headRefOid // "")\t\(.closedAt // "")"`)
	f := strings.Split(strings.TrimSpace(out), "\t")
	n := 0
	for _, r := range f[0] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return 0
	}
	// The two trailing fields are newer than this cache file may be; absent,
	// `ownsPR` has nothing to fire on and answers yes, which is the behaviour
	// this function had before the gate.
	headOID, closedAt := "", ""
	if len(f) > 1 {
		headOID = f[1]
	}
	if len(f) > 2 {
		closedAt = f[2]
	}
	if !ownsPR(main, branch, headOID, closedAt) {
		return 0
	}
	return n
}
