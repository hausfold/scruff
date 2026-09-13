package commands

import (
	"strconv"
	"strings"
	"time"

	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/ui"
)

// List renders every live/parked lane across every repo scruff can reach, and
// self-heals on the way in.
func (e *Env) List(asJSON bool) error {
	// Self-heal: reap parked branches whose PR has since merged. PARKED ONLY —
	// it must never disturb a live checkout that may still have an open pane.
	// The riskier live sweep is opt-in via `scruff reap`. Best-effort: a network
	// hiccup must not break the listing.
	sweep := e.reapSweep(sweepParked)
	if !asJSON {
		if n := len(sweep.Reaped); n > 0 {
			ui.Say("swept %d merged lane(s)", n)
		}
		// Husks are deliberately left by the sweep, so the listing is where they
		// surface — otherwise a half-removed checkout is a `stray` row with no
		// hint of what to do about it.
		if n := len(sweep.Strays); n > 0 {
			ui.Say("%d dangling checkout(s) — git lost the link; `scruff <name>` moves each aside and rebuilds:", n)
			for _, s := range sweep.Strays {
				ui.Say("  %s", s)
			}
		}
	}

	rows := e.rows()
	if asJSON {
		return e.listJSON(rows)
	}

	ui.Say("lanes you can resume (scruff <name>, or <repo>/<name>)")
	if len(rows) == 0 {
		ui.Say("none parked — every lane's branch is merged & cleaned up. The fog is even.")
		return nil
	}
	renderTable(rows)
	return nil
}

// listRow is one rendered line's worth of facts.
type listRow struct {
	Repo     string
	Name     string
	State    string
	Agent    string
	Last     string
	Entry    Entry
	Parent   string
	Depth    int
	Ahead    int
	AheadPR  int
	Relanded bool
	Diverged bool
}

func (e *Env) rows() []listRow {
	var out []listRow
	lanes := e.discover()
	for _, entry := range lanes {
		if !e.branchAlive(entry) {
			continue
		}
		// The state cell is the CHECKOUT's state — plus, when it applies, the
		// one fact invisible everywhere else: this branch's PR already merged
		// and it has committed since. Without the marker such a row is
		// indistinguishable from an ordinary in-flight branch, which is exactly
		// how un-shipped commits go unnoticed until someone tidies up by hand.
		state := string(entry.State)
		n, pr, diverged := e.postMergeAhead(entry.Main, entry.Branch)
		row := listRow{
			Repo:  entry.Main, // → its cell below, once the whole table is known
			Name:  entry.Name(),
			State: state,
			Entry: entry,
		}
		switch {
		case n > 0 && diverged:
			// Distinct glyph on purpose: a "+N" reader reasonably assumes
			// `scruff reship` is the fix, and here it is the wrong one.
			row.State = state + "~" + strconv.Itoa(n)
			row.Ahead, row.AheadPR, row.Diverged = n, pr, true
		case n > 0:
			row.State = state + "+" + strconv.Itoa(n)
			row.Ahead, row.AheadPR, row.Relanded = n, pr, true
		}
		row.Agent = e.agentFor(entry.Path)
		// Empty for an unborn branch (a lane opened in a repo with no
		// commits yet) — say so rather than printing a blank cell that reads
		// like a broken row.
		//
		// The subject alone, NOT git's relative date (`%cr`, "3 minutes
		// ago"): this value is also `--json`'s `last_commit` (json.go), a
		// frozen contract two spellings of the same listing must answer
		// byte-identically (SPEC.md §2.2) and `watch` diffs verbatim to
		// decide whether a lane actually changed (watch.go's `changed`
		// kind). A relative date drifts every second on its own, which
		// broke both: two `scruff --json` calls a heartbeat apart could
		// legitimately disagree, and a `watch` consumer would see a
		// `changed` event on every rescan tick even when nothing about the
		// lane had moved.
		last, err := gitx.Run(entry.Main, "log", "-1", "--format=%s", entry.Branch)
		if err != nil || last == "" {
			last = "no commits yet"
		}
		row.Last = last
		if reg, ok := e.Reg.Find(entry.Path); ok {
			row.Parent = reg.Parent
		}
		out = append(out, row)
	}
	// The repo cell is decided once the table is: its spelling depends on what
	// ELSE is on screen (repoCells), so it cannot be filled in row by row.
	cells := repoCells(lanes)
	for i := range out {
		out[i].Repo = cells[out[i].Repo]
	}
	return nest(out)
}

// nest orders the listing so a lane in ANOTHER repo sits directly under the
// lane that spawned it, one indent deeper.
//
// Another repo is the whole test, and `parent` on its own cannot carry it.
// That field records the cwd of the pane that spawned the lane, so a lane
// opened from inside another lane's pane is parented to that lane exactly as a
// `scruff child` is — SPEC.md §2.2 says so out loud, and it is why `chat`
// exists. The two are not the same relation: a `scruff child` lane has no pane
// of its own and its branch lives somewhere this repo's listing would never
// show, while a same-repo lane is a SIBLING with its own window and its own
// branch off the same main, subordinate to nothing. Filing a sibling under
// whichever pane happened to press the key buries it under an unrelated task,
// and in a consumer that caps its child rows it can push a genuine child out
// of view — which is the one row that had nowhere else to be.
//
// Repo identity rather than `chat` because it is stored, client-agnostic and
// stable: `chat` is "" for any client scruff cannot probe, and "" must be read
// as "show it", so nesting on it would quietly stop nesting real children in
// codex/opencode/pi panes. The compare is a plain string one for the same
// reason the parent-is-my-own-main test below is: both Mains come from
// discover(), the only thing that writes them.
//
// The listing must never DROP a child row, however subordinate it looks: a
// `scruff child` lane carries its own branch and its own PR, in another repo,
// and remove-on-close does not reap it. This listing is the only place that
// branch surfaces once the parent's pane is gone — so the answer to "it is
// noise" is where it sits, not whether it is there.
//
// A child whose parent is no longer listed stays at the top level, unmarked:
// nothing is nesting it any more, and an orphan pretending to be a child of
// something invisible is the one shape that would hide it.
func nest(rows []listRow) []listRow {
	byPath := make(map[string]int, len(rows))
	for i, r := range rows {
		byPath[r.Entry.Path] = i
	}
	kids := make(map[int][]int, len(rows))
	isChild := make([]bool, len(rows))
	for i, r := range rows {
		// A plain lane's parent is its OWN main checkout — nobody's pane spawned
		// it, so there is no lineage to draw.
		if r.Parent == "" || r.Parent == r.Entry.Main {
			continue
		}
		p, ok := byPath[r.Parent]
		if !ok || p == i {
			continue
		}
		// Same repo ⇒ siblings from one pane, not lineage. See above.
		if rows[p].Entry.Main == r.Entry.Main {
			continue
		}
		kids[p] = append(kids[p], i)
		isChild[i] = true
	}

	out := make([]listRow, 0, len(rows))
	seen := make([]bool, len(rows))
	var walk func(i, depth int)
	walk = func(i, depth int) {
		if seen[i] {
			return // a parent cycle can only come from a corrupt registry; do not spin on it
		}
		seen[i] = true
		row := rows[i]
		row.Depth = depth
		out = append(out, row)
		for _, k := range kids[i] {
			walk(k, depth+1)
		}
	}
	for i := range rows {
		if !isChild[i] {
			walk(i, 0)
		}
	}
	// Anything left is a cycle's tail — list it rather than lose it.
	for i := range rows {
		if !seen[i] {
			walk(i, 0)
		}
	}
	return out
}

// nameCell is the name column: a spawned lane is drawn under its parent, so
// the indent is what says "this one has no pane of its own", not a missing row.
func nameCell(r listRow) string {
	if r.Depth < 1 {
		return r.Name
	}
	return strings.Repeat("  ", r.Depth-1) + "└ " + r.Name
}

// agentFor is the client recorded for a lane. A registry row that predates
// the client column means Claude, never today's default — otherwise a parked
// Codex branch would reopen in the wrong client.
func (e *Env) agentFor(path string) string {
	if row, ok := e.Reg.Find(path); ok {
		return row.Agent
	}
	return e.Agent
}

// branchAlive reports whether a row still means something.
//
// Normally "does refs/heads/<branch> exist" — a branch that was merged and
// deleted, or hand-nuked, can't resume anything. EXCEPT a lane opened in a repo
// with no commits yet: its branch is UNBORN, checked out with no ref behind it
// until the first commit lands. By ref alone such a lane reads as dead the
// moment it starts.
func (e *Env) branchAlive(entry Entry) bool {
	if gitx.HasBranch(entry.Main, entry.Branch) {
		return true
	}
	if entry.State != Live {
		return false
	}
	head, err := gitx.Run(entry.Path, "symbolic-ref", "--short", "HEAD")
	return err == nil && head == entry.Branch
}

// postMergeAhead names the case reaping refuses to touch: the PR merged, then
// the lane kept committing. Those commits sit on a branch whose remote
// counterpart the forge deleted at merge — no PR covers them, nothing is pushed,
// and the only symptom used to be a lane that quietly declined to be reaped.
// Returns (0, 0, false) when this isn't that case.
//
// `diverged` is the other way a tip can differ from the merged SHA: not built
// on top of it at all — a second local checkout of the same branch that never
// pulled, a rebase, an amend. `ahead` still counts commits reachable from
// branch but not from head for these, because that count is not FALSE, but it
// answers a different question than "what should I reship" — those commits
// are not new work sitting on the merge, they are a stale or sideways tip.
// Reshipping one would push and PR content the merge already superseded.
func (e *Env) postMergeAhead(main, branch string) (ahead, pr int, diverged bool) {
	// Landed by ancestry beats everything: if the tip is already IN the default
	// branch, those later commits landed too (a second PR, a direct merge) and
	// there is nothing to ship. Local, cheap, and asked FIRST so the marker can
	// never contradict the sweep that would reap this branch.
	base := gitx.DefaultBranch(main)
	if gitx.IsAncestor(main, branch, base) {
		return 0, 0, false
	}
	head, num := e.mergedMapLookup(main, branch)
	if head == "" {
		return 0, 0, false
	}
	tip := gitx.Rev(main, branch)
	if tip == "" || tip == head {
		return 0, 0, false
	}
	// An OPEN PR standing at this exact tip already covers everything since the
	// merge, so the lane is simply in flight — neither behind a reship nor
	// sideways. Without this the marker never comes down: the map it reads
	// lists MERGED PRs only, so the follow-up PR `scruff reship` just opened is
	// invisible to it and the lane goes on being told to reship, forever.
	// Compared by OID rather than by mere existence, because a commit made
	// after that push is genuinely uncovered until it is pushed too — and that
	// is exactly the case the marker is for.
	if oid, _ := e.openMapLookup(main, branch); oid != "" && oid == tip {
		return 0, 0, false
	}
	// `--not base` is what keeps the count honest. A long-lived lane pulls the
	// default branch back in — a merge from main, a rebase onto it — and every
	// commit that ride brings along is reachable from the tip but not from the
	// merged head, so a bare `head..branch` counts other people's landed work as
	// this lane's un-shipped commits. That is how a lane with two real commits
	// came to read `live+131`: a number that size reads as "unreviewable, deal
	// with it later" rather than "one PR, two commits". The marker promises
	// "commits no PR covers", so anything already on the default branch — which
	// some PR demonstrably did cover — must not be in it.
	n := 1
	if out, err := gitx.Run(main, "rev-list", "--count", head+".."+branch, "--not", base); err == nil {
		if parsed, err := strconv.Atoi(out); err == nil {
			if parsed == 0 {
				// Every commit since the merge is on the default branch already:
				// this lane has nothing un-shipped, whatever the raw count says.
				return 0, 0, false
			}
			n = parsed
		}
	}
	// The merged SHA is normally an ancestor of the tip (we committed on top of
	// it) — that is the "outran its PR" case reship exists for. When it is not
	// reachable, ancestry alone can't tell WHY: `git rebase main` to catch up
	// (legitimate — the branch's own commit is still there, just replayed onto
	// a new base under a new SHA) looks identical, by ancestry, to a second
	// checkout that pushed a different tip under the same branch name first.
	// `builtOnMerge` tells them apart the way `Landed`'s own ladder does for
	// the same shape of problem (step 3): a rebase preserves the DIFF even
	// though it changes the SHA, so if `head`'s patch already exists among
	// commits unique to this branch, the branch built on the merge after all.
	if !builtOnMerge(main, base, head, branch) {
		return n, num, true
	}
	return n, num, false
}

// builtOnMerge reports whether branch's history — literally, or as an
// equivalent patch after a rebase — includes what actually merged.
func builtOnMerge(main, base, head, branch string) bool {
	// The default branch being an ancestor of the tip settles it before any
	// SHA is compared: everything on the default branch is in this history,
	// and what merged is on the default branch by definition. Neither check
	// below can see that, because a SQUASH merge puts the branch's content on
	// the default branch under a NEW sha whose patch is the whole PR at once —
	// so `head`, the PR's pre-squash tip, is not an ancestor of a branch that
	// has since rebased onto the default branch, and `git cherry` cannot match
	// it against that branch's own commits either. Without this, a lane that
	// did exactly the right thing — squash-merge its PR, rebase onto the
	// default branch, keep working — read as a stale, sideways checkout, and
	// both `reap` and `reship` told it to delete itself rather than ship the
	// real commits sitting on top.
	if gitx.IsAncestor(main, mergeRef(main, base), branch) {
		return true
	}
	if gitx.IsAncestor(main, head, branch) {
		return true
	}
	// "upstream" in git's terms, confusingly: this asks whether head's patch
	// is already applied ON branch, marking it '-' if so. Uncertainty (cherry
	// errors, or head's patch genuinely isn't there) resolves to "not built on
	// the merge" — the same direction everything else in this file resolves
	// uncertainty, because the cost of guessing wrong here is `reship` pushing
	// content the merge already superseded.
	out, err := gitx.Run(main, "cherry", branch, head)
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(out), "-")
}

// mergeRef is the ref the ancestry question above should actually be asked
// against: the remote-tracking default branch when there is one.
//
// A worktree-driven repo's LOCAL default branch is routinely stale — nobody
// checks it out, every lane branches from origin — and answering "did this
// build on the merge?" against a stale ref is the one way the check above can
// say yes when the honest answer is no.
func mergeRef(main, base string) string {
	if remote := "origin/" + base; gitx.Rev(main, remote) != "" {
		return remote
	}
	return base
}

// openMapLookup is mergedMapLookup's other half: the tip and number of this
// branch's OPEN PR, if it has one. Same one-query-per-repo shape, for the same
// reason — the listing asks this of every row, and a per-branch query costs
// ~0.5 s each.
//
// Deliberately NOT gated by `ownsPR`. An open PR's head ref is this branch's
// name, so a push from this lane lands on that very PR — it covers this lane's
// commits no matter which lane opened it. `reship`'s own `openPRFor` is the
// same case and is left alone for the same reason.
func (e *Env) openMapLookup(main, branch string) (headOID string, pr int) {
	slug, err := gitx.RemoteSlug(main)
	if err != nil || slug == "" {
		return "", 0
	}
	out := e.cachedForge(openMapKey(slug),
		"pr", "list", "-R", slug, "--state", "open", "--limit", "100",
		"--json", "number,headRefName,headRefOid",
		"--jq", `.[] | "\(.headRefName)\t\(.headRefOid)\t\(.number)"`)
	for _, line := range gitx.Lines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		// A cross-repo (fork) PR arrives as owner:branch — same suffix compare
		// the merged map does, and for the same reason.
		name := f[0]
		if i := strings.LastIndex(name, ":"); i >= 0 {
			name = name[i+1:]
		}
		if name == branch {
			n, _ := strconv.Atoi(f[2])
			return f[1], n
		}
	}
	return "", 0
}

// openMapKey is shared with reship, which forgets this entry the moment it
// opens a PR: the cache holds for 120 s, and a marker that goes on demanding
// the reship that just succeeded is exactly the bug this map exists to fix.
func openMapKey(slug string) string { return "open-" + slug }

// mergedMapLookup asks ONE repo-wide question rather than one per branch.
//
// A per-branch query costs ~0.5 s, and the listing asks this of every row — a
// repo with eight lanes would go from 0.3 s to seconds, in exactly the fog
// where you most want a fast listing. `Landed` deliberately keeps its own
// precise per-branch query: it decides whether a branch DIES, so it must not
// inherit this one's 100-PR horizon. Missing a merge here costs an annotation;
// missing it there would cost the work.
//
// The forge answers about a NAME, and a name is not a lane — `ownsPR` below is
// why the newest PR wearing it is not automatically this lane's.
func (e *Env) mergedMapLookup(main, branch string) (headOID string, pr int) {
	slug, err := gitx.RemoteSlug(main)
	if err != nil || slug == "" {
		return "", 0
	}
	out := e.cachedForge("merged-"+slug,
		"pr", "list", "-R", slug, "--state", "merged", "--limit", "100",
		"--json", "number,headRefName,headRefOid,closedAt",
		"--jq", `.[] | "\(.headRefName)\t\(.headRefOid)\t\(.number)\t\(.closedAt // "")"`)
	for _, line := range gitx.Lines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		// A cross-repo (fork) PR arrives as owner:branch, so compare on the
		// suffix. Without this a fork-merged branch reads as unlanded forever —
		// safe, but the +N marker and the sweep both go blind.
		name := f[0]
		if i := strings.LastIndex(name, ":"); i >= 0 {
			name = name[i+1:]
		}
		if name != branch {
			continue
		}
		// `closedAt` is the newest field on this line, so it is the one a cache
		// written by the PREVIOUS scruff does not carry. Reading it only when
		// it is there leaves a 120 s-old cache merely uninformative rather than
		// wrong: with no date the gate below cannot fire, which is exactly how
		// this lookup behaved before it existed.
		closedAt := ""
		if len(f) > 3 {
			closedAt = f[3]
		}
		if !ownsPR(main, branch, f[1], closedAt) {
			// gh lists newest-first, so a PR on this name that predates the
			// branch means every older one does too — there is nothing left to
			// fall through to, and "no merged PR" is the whole answer.
			return "", 0
		}
		n, _ := strconv.Atoi(f[2])
		return f[1], n
	}
	return "", 0
}

// prClockGrace is how far a PR may appear to have closed BEFORE the branch it
// belongs to was cut and still count as that branch's.
//
// The two stamps come off different clocks — the branch's from this machine,
// the PR's from the forge — and a Mac resumed from a snapshot (a Tart guest,
// routinely) runs ahead. Five minutes costs the stale case nothing: what the
// gate separates there is days, not seconds.
const prClockGrace = 5 * time.Minute

// ownsPR reports whether a PR the forge returned for this branch NAME belongs
// to the branch standing in front of us.
//
// The forge answers about names, and a name is not a lane. scruff coins lane
// names from a small word list and a task name gets reused outright —
// `worktree-continue-factory-docs` has carried seven PRs in one repo — so a
// lane cut minutes ago inherited the last lane's merged PR. `postMergeAhead`
// then saw a tip that differs from that PR's head, counted this lane's own
// commits against it, and the listing said `live+3` about a PR nobody here
// opened, with an orange `N^` in the bar to match.
//
// Two facts, in order, and every arm errs toward KEEPING the PR — a marker that
// stays up is noise, a marker that never comes up is un-shipped work nobody is
// told about:
//
//  1. ANCESTRY. The PR's head SHA reachable from this branch means this branch
//     is what that PR was opened from, whatever the name has meant since.
//  2. DATE. Otherwise, a PR that closed before this branch existed cannot be
//     about it.
//
// Anything else is a real doubt and keeps the PR: no head SHA (a cache from an
// older scruff), no closedAt (an OPEN PR has none — and a push to this name
// lands on it, so it is this branch's anyway), or a branch git cannot date.
func ownsPR(main, branch, headOID, closedAt string) bool {
	if headOID != "" && gitx.IsAncestor(main, headOID, branch) {
		return true
	}
	// Ancestry alone can't decide it because of the rebase. A lane that merged
	// and then rebased onto the default branch — the house advice for a branch
	// that has to catch up — no longer reaches its own merged SHA either, so it
	// arrives here holding a PR that is genuinely its own.
	ended, err := time.Parse(time.RFC3339, strings.TrimSpace(closedAt))
	if err != nil {
		return true
	}
	birth := branchBirth(main, branch)
	if birth.IsZero() {
		return true
	}
	return !ended.Add(prClockGrace).Before(birth)
}

// branchBirth dates the branch in front of us — this incarnation of the name,
// not the name.
//
// The reflog's OLDEST entry is `branch: Created from …`, written when the lane
// was cut. Git deletes a branch's reflog along with the branch, so a name worn
// three times before still dates the branch you actually have. (`neverDiverged`
// reads the same log, for the same reason.)
//
// Returns the zero time when it can't tell, which every caller reads as "keep".
func branchBirth(main, branch string) time.Time {
	if out, err := gitx.Run(main, "reflog", "show", "--date=unix", "--format=%gd", branch); err == nil {
		if lines := gitx.Lines(out); len(lines) > 0 {
			if t := selectorTime(lines[len(lines)-1]); !t.IsZero() {
				return t
			}
		}
	}
	// Reflogs can be off (core.logAllRefUpdates=false) or aged out by gc. Then
	// the oldest commit the branch carries of its OWN is the next-best "not
	// before this". AUTHOR date, not committer: a rebase rewrites every
	// committer date to now, which would date the rebased lane above AFTER its
	// own merge and drop the PR that really is its own.
	base := gitx.DefaultBranch(main)
	out, err := gitx.Run(main, "log", "--format=%at", base+".."+branch)
	if err != nil {
		return time.Time{}
	}
	lines := gitx.Lines(out)
	if len(lines) == 0 {
		return time.Time{}
	}
	return unixTime(lines[len(lines)-1])
}

// selectorTime pulls the seconds out of a `--date=unix` reflog selector —
// `worktree-foo@{1756900000}`. Branch names cannot contain `@{` (git forbids
// it in a refname), so the last one is the selector's.
func selectorTime(s string) time.Time {
	i := strings.LastIndex(s, "@{")
	if i < 0 || !strings.HasSuffix(s, "}") {
		return time.Time{}
	}
	return unixTime(s[i+2 : len(s)-1])
}

func unixTime(s string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// ── rendering ────────────────────────────────────────────────────────────────

// renderTable hands the listing to snug, which budgets the columns against the
// real window rather than a format string. What it replaced was a hand-rolled
// layout measuring in BYTES (`len`) and cutting in runes — both wrong for a
// branch name with an accent in it — over a width that came from `tput cols`,
// which answers a static 80 in a 40-column pane.
func renderTable(rows []listRow) {
	relanded, diverged, spawned := false, false, false
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		cells = append(cells, []string{r.Repo, nameCell(r), r.State, r.Agent, r.Last})
		relanded = relanded || r.Relanded
		diverged = diverged || r.Diverged
		spawned = spawned || r.Depth > 0
	}

	// `name` carries the weight because it is the one cell you retype at
	// `scruff <name>`, and `state` is the one that may never be abbreviated:
	// it carries the +N / ~N markers, and `live+2` cut to `live+…` is a wrong
	// fact rather than a short one. CutNever spends the column at its full
	// width or gives up the table entirely for the stacked fallback.
	ui.Table([]ui.Col{
		{Head: "repo", Min: 4, Weight: 1, Role: ui.Muted, Cut: ui.CutRight},
		{Head: "name", Min: 12, Weight: 3, Role: ui.Subject, Cut: ui.CutRight},
		{Head: "state", Min: 5, Weight: 1, Role: ui.Body, Cut: ui.CutNever},
		{Head: "agent", Min: 6, Weight: 1, Role: ui.Muted, Cut: ui.CutRight}, // 6 = `claude`, `codex`: an elided client name helps nobody
		{Head: "last commit", Min: 12, Weight: 2, Role: ui.Muted, Cut: ui.CutRight},
	}, cells)

	// Only ever printed when a row earned it, so the listing stays a table on a
	// normal day — and the day it isn't normal, the fix is one command away.
	if spawned {
		ui.Say("└ = another repo's lane, spawned from the lane above it (`scruff child`) — its branch and PR are its own, and closing that pane never reaps it")
	}
	if relanded {
		ui.Say("+N = commits landed AFTER that branch's PR merged — no PR covers them: scruff reship <name>")
	}
	if diverged {
		ui.Say("~N = the tip does not build on that branch's merged PR — a stale or sideways checkout, not new work. Its content already landed; remove the checkout instead of reshipping it.")
	}
}
