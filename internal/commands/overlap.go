package commands

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hausfold/scruff/internal/exitcode"
	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/ui"
)

// ── scruff overlap: what the other lanes on this repo are already inside ─────
//
// Parallel lanes are branches of ONE repo in ONE shared object store, all on
// this machine. That makes coordination unnecessary: every fact a claims
// ledger would ask an agent to *declare* — which files, which regions, whose
// work is where — can just be *measured*, offline, in milliseconds. Nobody has
// to register anything, nobody can forget to, and a lane whose agent never runs
// this costs its siblings nothing. There is no lock, no claim and no state file
// here on purpose: the only thing `overlap` writes is its own output.
//
// Two signals, deliberately different in what they can see:
//
//   - the hunk index — every lane's changed line ranges since the common
//     ancestor, INCLUDING uncommitted and untracked work. Broad and early.
//     This is the half `git merge-tree` structurally cannot see, and it is
//     where a lane spends most of its life.
//   - merge-tree — a real three-way merge of the two branches, no working tree
//     touched. Exact, but committed work only.
//
// Overlap is only ever defined WITHIN one repo — a `scruff child` lane on
// another repo has its own object store and cannot textually collide with this
// one — so the same-repo filter is the whole scoping rule and needs no flag.
//
// Advisory, never a gate (SPEC.md §7). It refuses nothing and blocks nothing;
// it reports, and the exit code says how loudly: 0 clear · 3 same file · 4
// same region, or a merge-tree conflict. Ported from the workshop's `bench
// overlap`, prod fixes and all; this is the verb §7 milestoned, in the binary
// that owns the registry it reads.

// overlapFuzz is git's own default context. Two edits within this many lines
// of each other are what `git merge` will present as one conflicted hunk, so it
// is the right fuzz for "are we in the same room", not a tuned magic number.
const overlapFuzz = 3

// overlapWhole is the range a whole-file add or delete claims.
const overlapWhole = math.MaxInt32

// span is one changed range of one file, in the BASE file's coordinates.
type span struct {
	path   string
	lo, hi int
}

// side is one party to a comparison: a live checkout, read with its working
// tree (ref == ""), or a branch read out of the main checkout's object store —
// a parked lane has no checkout left, and its branch IS the work.
type side struct {
	dir string
	ref string
}

func (s side) live() bool { return s.ref == "" }

// finding is what two sides have in common in one file.
type finding struct {
	path   string
	hunk   bool // the same region, within the fuzz; false: the same file only
	lo, hi int
}

func (f finding) detail() string {
	switch {
	case !f.hunk:
		return "-"
	case f.hi >= overlapWhole:
		return "the whole file"
	}
	return fmt.Sprintf("L%d-%d", f.lo, f.hi)
}

// hunkSpans turns a `git diff -U0` stream into one span per hunk, in the base
// file's coordinates.
//
// Base-side, not new-side, on purpose: two lanes that branched from the same
// commit have diverging line numbers in their own trees (every insertion shifts
// everything below it), so their new-side numbers aren't comparable at all. The
// ancestor's numbering is the one coordinate system both sides share, and both
// diffs report it for free in the `-a,b` half of each hunk header.
//
// A whole-file add or delete claims the entire range: add/add and delete/edit
// always conflict, whatever the line numbers say.
func hunkSpans(diff string) []span {
	var out []span
	path, whole, inHunk := "", false, false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			path, whole, inHunk = "", false, false
		// The ---/+++ headers are only headers BEFORE the first @@ of a file.
		// Inside a hunk, an added line whose own text starts with "++ " arrives
		// as "+++ ", and reading that as a file header files every later hunk
		// under a garbage path — the file silently drops out of the comparison.
		// (A removed "-- " line is the same trap on the other side.)
		case strings.HasPrefix(line, "--- "):
			if inHunk {
				continue
			}
			if s := line[4:]; s == "/dev/null" {
				whole = true
			} else if path == "" {
				path = diffPath(s)
			}
		case strings.HasPrefix(line, "+++ "):
			if inHunk {
				continue
			}
			if s := line[4:]; s == "/dev/null" {
				whole = true
			} else {
				path = diffPath(s)
			}
		case strings.HasPrefix(line, "@@ "):
			inHunk = true
			if path == "" {
				continue
			}
			if whole {
				out = append(out, span{path, 0, overlapWhole})
				continue
			}
			// The second field is the old-side spec: "-a,b", or "-a" when the
			// count is 1.
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			old := strings.TrimPrefix(f[1], "-")
			start, length := 0, 1
			if i := strings.Index(old, ","); i >= 0 {
				start, _ = strconv.Atoi(old[:i])
				length, _ = strconv.Atoi(old[i+1:])
			} else {
				start, _ = strconv.Atoi(old)
			}
			// A pure insertion is "-a,0": nothing of the base was touched, but
			// the seam between a and a+1 is still a place two lanes can collide.
			if length == 0 {
				out = append(out, span{path, start, start + 1})
			} else {
				out = append(out, span{path, start, start + length - 1})
			}
		}
	}
	return out
}

// diffPath is the repo-relative path out of a `--- a/…` or `+++ b/…` header:
// the prefix off the front, and off the back the tab git appends when the name
// holds a space. Not split on whitespace — a path with a space in it would
// shear on fields, and the file would silently never match anyone else's.
func diffPath(s string) string {
	s = strings.TrimSuffix(s, "\t")
	if len(s) > 2 && s[1] == '/' {
		s = s[2:]
	}
	return s
}

// sideSpans is everything one side changed since base.
func sideSpans(s side, base string) []span {
	// core.quotePath off, so a name with an accent in it arrives as itself
	// rather than as octal the other side will never spell the same way.
	args := []string{"-c", "core.quotePath=false", "diff", "-U0", "--no-color", "--no-ext-diff", "--no-renames", base}
	if !s.live() {
		// Committed work only — that's all a branch has.
		args = append(args, s.ref)
	}
	// With no second rev, `git diff <base>` compares the base to the WORKING
	// TREE, so one call covers committed and uncommitted together. A failed
	// diff is an empty side, never an error: a lane git cannot read holds
	// nothing this tool can measure.
	out, _ := gitx.Run(s.dir, args...)
	spans := hunkSpans(out)
	if !s.live() {
		return spans
	}
	// Untracked files are invisible to `git diff` and to `merge-tree` alike,
	// but two lanes creating the same new file is a real add/add conflict — and
	// the one this whole tool would otherwise be blindest to, because a
	// brand-new file is exactly what an agent writes without checking first.
	if untracked, err := gitx.Run(s.dir, "ls-files", "--others", "--exclude-standard"); err == nil {
		for _, f := range gitx.Lines(untracked) {
			spans = append(spans, span{f, 0, overlapWhole})
		}
	}
	return spans
}

// compare finds, per file both sides touched, whether their edits share a
// region. A side that changed nothing is an ordinary state here — a lane that
// has not started yet — and reads as no findings, never as a mismatch.
func compare(as, bs []span) []finding {
	byPath := func(spans []span) map[string][]span {
		m := map[string][]span{}
		for _, s := range spans {
			m[s.path] = append(m[s.path], s)
		}
		return m
	}
	am, bm := byPath(as), byPath(bs)
	var out []finding
	for p, bl := range bm {
		al, ok := am[p]
		if !ok {
			continue
		}
		hot, lo, hi := false, overlapWhole, -1
		for _, a := range al {
			for _, b := range bl {
				if a.lo-overlapFuzz <= b.hi && b.lo-overlapFuzz <= a.hi {
					hot = true
					lo = min(lo, min(a.lo, b.lo))
					hi = max(hi, max(a.hi, b.hi))
				}
			}
		}
		if hot {
			out = append(out, finding{p, true, lo, hi})
		} else {
			out = append(out, finding{path: p})
		}
	}
	// Loud above quiet, then by path, so two readers of the same report see
	// the same order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].hunk != out[j].hunk {
			return out[i].hunk
		}
		return out[i].path < out[j].path
	})
	return out
}

// overlap is one invocation's measuring context.
type overlap struct {
	e    *Env
	main string   // the main checkout, whose object store every side shares
	base string   // the default branch's name
	tips []string // where main's tip may be: origin/<base> first, then <base>
	// The memo is why this is a struct: the reader is one side of every pair
	// and the matrix form compares every lane with every other, so each side
	// is asked "spent?" many times and answered once.
	spentMemo     map[side]bool
	committedOnly bool
}

func (e *Env) newOverlap(main string, committedOnly bool) *overlap {
	base := gitx.DefaultBranch(main)
	// origin/<base> FIRST: a sibling landing its PR moves the remote ref, and
	// the local branch doesn't hear about it until somebody pulls — which is
	// exactly the conflict this check exists to see coming.
	return &overlap{
		e:             e,
		main:          main,
		base:          base,
		tips:          []string{"origin/" + base, base},
		spentMemo:     map[side]bool{},
		committedOnly: committedOnly,
	}
}

// sideOf is how a lane is read: its checkout, carrying its uncommitted work,
// when it has one git still recognises; its branch otherwise. A stray is read
// as parked — the directory is there, but git has disowned it.
func (o *overlap) sideOf(entry Entry) side {
	if o.committedOnly || entry.State != Live {
		return side{o.main, entry.Branch}
	}
	return side{entry.Path, ""}
}

// tip is the commit a side stands on.
func (o *overlap) tip(s side) string {
	if s.live() {
		return gitx.Rev(s.dir, "HEAD")
	}
	return gitx.Rev(o.main, s.ref)
}

// mainRef is the ref main's tip is read from, or "" when neither resolves.
func (o *overlap) mainRef() string {
	for _, r := range o.tips {
		if gitx.Rev(o.main, r) != "" {
			return r
		}
	}
	return ""
}

// landed is the set of ranges MAIN itself put between the base and its tip,
// as far as this side already carries them.
//
// Normally empty, and that is the point: two lanes cut from the current main
// share it as their merge base, nothing sits between, and every line below is
// a no-op. It earns its keep in the one case where the base falls BEHIND main
// — a SQUASH merge, which lands a lane's work as a brand-new commit and leaves
// the lane's own commits unreachable, so `merge-base` keeps answering with the
// pre-merge commit for as long as the branch exists. The shared coordinate
// system is then honest and the ATTRIBUTION is not: each side is measured from
// the base, so main's own commits since the base count as the lane's work — on
// both sides, since the reader's checkout contains the squash — and the two
// "collide" over a diff neither of them is still holding. It does not age out;
// it stands until somebody deletes the branch.
//
// Subtracted PER SIDE, and only from a side that actually CARRIES main's commit
// — the second IsAncestor below, which is the whole correctness of this. A lane
// that never rebased did not inherit main's work, so none of main's work is in
// its diff to subtract, and subtracting anyway would delete a lane's real edit
// whenever it happened to produce the same hunk boundaries. Two lanes editing
// the same line independently give both the very same base-side range.
//
// Never a WHOLE-FILE claim: hunkSpans gives an added or deleted file the whole
// range, which does not just match main's range for that path — it SUBSUMES
// every edit the side made to it, so a side that inherited main's add of a file
// would have its own work in that file erased. Keeping a whole-file add
// unsubtracted costs at most one loud row on a path two lanes both created.
func (o *overlap) landed(base, rev string) []span {
	seen := map[string]bool{}
	var out []span
	for _, tip := range o.tips {
		oid := gitx.Rev(o.main, tip)
		// The usual state is that both tips resolve to the same commit; reading
		// it twice would cost a full diff to throw away.
		if oid == "" || oid == base || seen[oid] {
			continue
		}
		seen[oid] = true
		if !gitx.IsAncestor(o.main, base, oid) || !gitx.IsAncestor(o.main, oid, rev) {
			continue
		}
		for _, s := range sideSpans(side{o.main, oid}, base) {
			if s.hi != overlapWhole {
				out = append(out, s)
			}
		}
	}
	return out
}

// spent reports whether a side has nothing left to give main at all.
//
// `--is-ancestor` answers that for a fast-forward or a merge commit and gets it
// wrong for the merge most forges actually do. A squash writes a BRAND-NEW
// commit, so a lane's tip is never an ancestor of main however completely its
// work landed, and an unreaped branch goes on measuring as live for as long as
// it exists — every lane spawned from a lane that just shipped would draw a ⚠
// against it, over a region whose content the two sides agree on exactly.
//
// Content, not ancestry, and not the forge: merge this side into main in the
// object store and ask whether main's tree moved. Nothing moved means nothing
// to land — as true of a squash as of a rebase somebody did elsewhere, and
// neither will ever be merged again. Offline, no PR query per lane, and no
// network to be wrong about. (Landed's merge-tree-empty rung is the same
// question, asked there to advise a sweep and here to drop a false alarm.)
//
// What it must never call spent is `scruff reship`'s live+N: a merged PR with
// commits made after it. A three-way merge sees those and answers for itself —
// but merge-tree cannot see a working tree, so a live checkout is asked
// `git status` first and anything uncommitted or untracked keeps the lane. That
// check runs second because it is the expensive one and most lanes never reach
// it: a lane with real work fails the merge-tree above and is gone by then.
func (o *overlap) spent(s side) bool {
	if v, ok := o.spentMemo[s]; ok {
		return v
	}
	rc := false
	if rev := o.tip(s); rev != "" {
		for _, tip := range o.tips {
			tree := gitx.Rev(o.main, tip+"^{tree}")
			if tree == "" {
				continue
			}
			// A branch with no commits main hasn't got has nothing to be spent
			// OF: it is a lane that has not started yet, or one that merged by
			// fast-forward. Either way it is already silent by construction —
			// the merge base IS its tip, so it claims no ranges and can conflict
			// with nobody — and calling it spent would only take a just-spawned
			// neighbour out of the roll call `--brief` exists to be, which is
			// the one lane worth knowing about before you plan. Spent is for
			// work that LANDED, not work that never happened.
			if n, err := gitx.Run(o.main, "rev-list", "--count", tip+".."+rev); err != nil || n == "0" {
				continue
			}
			// A conflict exits non-zero, and a side that conflicts with main is
			// holding something main hasn't got — the loudest possible "not
			// spent".
			merged, err := gitx.Run(o.main, "merge-tree", "--write-tree", tip, rev)
			if err != nil || firstLine(merged) != tree {
				continue
			}
			// Committed work is all in. A live checkout can still be holding
			// the half merge-tree structurally cannot see.
			if s.live() && gitx.Dirty(s.dir) {
				break
			}
			rc = true
			break
		}
	}
	o.spentMemo[s] = rc
	return rc
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// unlanded is a side's OWN work since base: what it changed, minus what main
// landed INTO it. A side with nothing left to land holds none of what it
// changed — main does — and is asked first because there is nothing there to
// subtract from: every range it has is main's own copy, under a lane's name.
func (o *overlap) unlanded(base, rev string, s side) []span {
	if o.spent(s) {
		return nil
	}
	landed := map[span]bool{}
	for _, l := range o.landed(base, rev) {
		landed[l] = true
	}
	// Subtracted by EXACT range, never by overlap: a lane that edited INSIDE a
	// region main also touched produces different hunk boundaries, and keeping
	// that finding is the safe direction for a tool that only ever advises.
	var out []span
	for _, sp := range sideSpans(s, base) {
		if !landed[sp] {
			out = append(out, sp)
		}
	}
	return out
}

// pair is the findings between two sides, or nothing at all.
func (o *overlap) pair(a, b side, only string) []finding {
	arev, brev := o.tip(a), o.tip(b)
	if arev == "" || brev == "" {
		return nil
	}
	// The merge base is both the coordinate system for the ranges and the point
	// a real merge would three-way from. No base (unrelated histories) → no
	// answer.
	base, err := gitx.Run(o.main, "merge-base", arev, brev)
	if err != nil || base == "" {
		return nil
	}
	as, bs := o.unlanded(base, arev, a), o.unlanded(base, brev, b)
	if only != "" {
		as, bs = onlyPath(as, only), onlyPath(bs, only)
	}
	return compare(as, bs)
}

func onlyPath(spans []span, path string) []span {
	var out []span
	for _, s := range spans {
		if s.path == path {
			out = append(out, s)
		}
	}
	return out
}

// size is how many files of its OWN a side changed since base — derived from
// the same unlanded ranges the findings are, not from a second `--name-only`
// pass, because this number decides who rebases onto whom: a side credited with
// main's commits can win a nomination it should have lost.
func (o *overlap) size(base, rev string, s side) int {
	paths := map[string]bool{}
	for _, sp := range o.unlanded(base, rev, s) {
		paths[sp.path] = true
	}
	return len(paths)
}

// conflicts is merge-tree's verdict on two committed tips: the paths a real
// three-way merge would leave conflicted, or none. Exit 1 means conflicts, and
// the conflicted paths are the lines between the tree OID and the blank line.
func (o *overlap) conflicts(arev, brev string) []string {
	out, code := gitx.Exit(o.main, "merge-tree", "--write-tree", "--name-only", arev, brev)
	if code != 1 {
		return nil
	}
	var paths []string
	for i, line := range strings.Split(out, "\n") {
		if i == 0 {
			continue
		}
		if line == "" {
			break
		}
		paths = append(paths, line)
	}
	return paths
}

// pushed reports whether a branch has a remote-tracking counterpart at origin.
func (o *overlap) pushed(branch string) bool {
	if branch == "" {
		return false
	}
	return gitx.OK(o.main, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch)
}

// order is who lands first. Not a rule anyone enforces; a default, so two
// agents that both read it reach the same answer without talking to each
// other. The tiebreak is deliberately observable from either side: pushed-ness
// and diffstat are facts in the shared repo, not opinions either lane has to
// publish. Returns the lane that goes first and why.
func (o *overlap) order(myBranch string, mySize int, theirBranch string, theirSize int, them, me string) (first, reason string) {
	mp, tp := o.pushed(myBranch), o.pushed(theirBranch)
	switch {
	case tp && !mp:
		return them, "already pushed"
	case mp && !tp:
		return me, "already pushed"
	case mySize < theirSize:
		return them, fmt.Sprintf("bigger: %d files vs %d", theirSize, mySize)
	case theirSize < mySize:
		return me, fmt.Sprintf("bigger: %d files vs %d", mySize, theirSize)
	case me < them:
		return me, "same size — alphabetical, so both sides pick the same one"
	}
	return them, "same size — alphabetical, so both sides pick the same one"
}

func (o *overlap) orderLine(first, reason, a, b string) string {
	second := b
	if first == b {
		second = a
	}
	switch reason {
	case "already pushed":
		return fmt.Sprintf("%s lands first (already pushed) — then %s rebases onto %s", first, second, o.base)
	case "same size — alphabetical, so both sides pick the same one":
		return fmt.Sprintf("%s lands first (%s)", first, reason)
	}
	return fmt.Sprintf("%s lands first (%s) — the smaller branch rebases", first, reason)
}

// ── the report ───────────────────────────────────────────────────────────────

// overlapLane is one lane as the report saw it.
type overlapLane struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Path   string `json:"path"`
	State  string `json:"state"`
	// Intent is the lane's last commit subject — what it is DOING, for free,
	// since our commit subjects already say so and no "what are you working
	// on" field has to be kept current. "" for a lane with no commits of its
	// own yet: its last commit is the ancestor's, and quoting that would read
	// the shared history back as the neighbour's plan.
	Intent string `json:"intent"`
}

// overlapFinding is one file two lanes both touched.
type overlapFinding struct {
	A     string `json:"a"`
	B     string `json:"b"`
	Path  string `json:"path"`
	Level string `json:"level"` // hunk: the same region · file: the same file only
	From  *int   `json:"from"`  // the shared region, in the merge base's line numbers; null for `file`
	To    *int   `json:"to"`
	Whole bool   `json:"whole_file"` // an add/add or delete/edit — the whole file is the region
}

// overlapConflict is merge-tree's word on two committed tips.
type overlapConflict struct {
	A     string   `json:"a"`
	B     string   `json:"b"` // a lane, or the ref main was read at
	Paths []string `json:"paths"`
}

// overlapOrder is the landing order the report proposes for a loud pair.
type overlapOrder struct {
	A      string `json:"a"`
	B      string `json:"b"`
	First  string `json:"first"`
	Reason string `json:"reason"`
}

// overlapReport is the whole answer, and the `--json` envelope.
type overlapReport struct {
	Scruff    string            `json:"scruff"`
	Schema    int               `json:"schema"`
	Repo      string            `json:"repo"`
	Main      string            `json:"main"`
	Mode      string            `json:"mode"`   // lane | matrix
	Reader    *overlapLane      `json:"reader"` // the lane asking; null in matrix mode
	Lanes     []overlapLane     `json:"lanes"`  // measured — a lane with nothing left to land is not here
	Findings  []overlapFinding  `json:"findings"`
	Conflicts []overlapConflict `json:"merge_tree"`
	Order     []overlapOrder    `json:"order"`
	Exit      int               `json:"exit"`
	Warnings  []string          `json:"warnings"`

	mainRef string
}

func (r *overlapReport) add(a, b string, f finding) {
	out := overlapFinding{A: a, B: b, Path: f.path, Level: "file"}
	if f.hunk {
		out.Level = "hunk"
		if f.hi >= overlapWhole {
			out.Whole = true
		} else {
			lo, hi := f.lo, f.hi
			out.From, out.To = &lo, &hi
		}
	}
	r.Findings = append(r.Findings, out)
}

func (r *overlapReport) loud() int {
	n := 0
	for _, f := range r.Findings {
		if f.Level == "hunk" {
			n++
		}
	}
	return n
}

func (r *overlapReport) quiet() int { return len(r.Findings) - r.loud() }

// code is the exit code the findings earn: 0 clear · 3 same file · 4 same
// region or a merge-tree conflict.
func (r *overlapReport) code() int {
	switch {
	case r.loud() > 0 || len(r.Conflicts) > 0:
		return exitcode.Conflict
	case r.quiet() > 0:
		return exitcode.Degraded
	}
	return exitcode.OK
}

// ── the verb ─────────────────────────────────────────────────────────────────

// Overlap is `scruff overlap [--brief] [--path <file>] [--json] [--committed-only]
// [--pair <a> <b>]`.
func (e *Env) Overlap(args []string) error {
	var brief, asJSON, committedOnly bool
	var only string
	var pair []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "":
		case "--brief":
			brief = true
		case "--json":
			asJSON = true
		case "--committed-only":
			committedOnly = true
		case "--path":
			if i+1 >= len(args) || args[i+1] == "" {
				return exitcode.Usagef("`scruff overlap --path` wants a file path — the one you are about to edit")
			}
			i++
			only = args[i]
		case "--pair":
			if i+2 >= len(args) || args[i+1] == "" || args[i+2] == "" {
				return exitcode.Usagef("`scruff overlap --pair` wants two lane names")
			}
			pair = []string{args[i+1], args[i+2]}
			i += 2
		default:
			if strings.HasPrefix(a, "-") {
				return unknownFlag("overlap", a)
			}
			return exitcode.Usagef("`scruff overlap` takes no argument — %q is not one, so nothing ran. `scruff overlap --help` explains the verb (a file goes in --path, lanes in --pair)", a)
		}
	}

	if pair != nil {
		return e.overlapPair(pair[0], pair[1], only, brief, asJSON, committedOnly)
	}

	top, err := gitx.Toplevel(e.Cwd)
	if err != nil || top == "" {
		return exitcode.Usagef("scruff overlap: run it inside a git checkout — the lane you are in is one side of every comparison")
	}
	main, err := gitx.MainCheckout(top)
	if err != nil || main == "" {
		return exitcode.Usagef("scruff overlap: can't find this checkout's main checkout")
	}
	// --path takes whatever you would type; the index is repo-relative.
	if only != "" {
		only = repoRelative(only, top, e.Cwd)
	}

	o := e.newOverlap(main, committedOnly)
	lanes := e.overlapLanes(main)

	// Standing in the MAIN checkout rather than a lane: there is no "you" to
	// compare against, so answer the other useful question — which lanes
	// collide with EACH OTHER. That is the verdict a batch merge of every open
	// PR reaches, minus the PRs, the merges and the build.
	if top == main {
		return o.matrix(lanes, only, brief, asJSON, "")
	}
	return o.lane(top, lanes, only, brief, asJSON)
}

// overlapLanes is every lane of one repo: registry rows, live checkouts and
// orphan branches alike (discover), parked ones included — the lanes you have
// forgotten about are precisely the ones rotting against main.
func (e *Env) overlapLanes(main string) []Entry {
	var out []Entry
	for _, entry := range e.discover() {
		// A row whose branch is gone is a corpse, not a lane.
		if entry.Main != main || !e.branchAlive(entry) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// repoRelative turns whatever the user typed into the path the index knows.
func repoRelative(path, top, cwd string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	if rel, err := filepath.Rel(top, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// intent is a lane's last commit subject, or "" when it has no commits of its
// own past main — see overlapLane.Intent.
func (o *overlap) intent(entry Entry) string {
	ref := o.mainRef()
	if ref == "" {
		return ""
	}
	if n, err := gitx.Run(o.main, "rev-list", "--count", ref+".."+entry.Branch); err != nil || n == "0" {
		return ""
	}
	return gitx.Subject(o.main, entry.Branch)
}

func (o *overlap) laneJSON(entry Entry) overlapLane {
	return overlapLane{
		Name:   entry.Name(),
		Branch: entry.Branch,
		Path:   entry.Path,
		State:  string(entry.State),
		Intent: o.intent(entry),
	}
}

func (o *overlap) newReport(mode string) *overlapReport {
	return &overlapReport{
		Scruff:    Version,
		Schema:    2,
		Repo:      repoSlug(o.main),
		Main:      o.main,
		Mode:      mode,
		Lanes:     []overlapLane{},
		Findings:  []overlapFinding{},
		Conflicts: []overlapConflict{},
		Order:     []overlapOrder{},
		Warnings:  []string{},
		mainRef:   o.mainRef(),
	}
}

// lane is the report from inside a lane: who is in YOUR files, and where.
func (o *overlap) lane(top string, lanes []Entry, only string, brief, asJSON bool) error {
	e := o.e
	myBranch := gitx.CurrentBranch(top)
	myHead := gitx.Rev(top, "HEAD")
	if myHead == "" {
		return exitcode.Usagef("scruff overlap: this checkout has no commits yet — there is no history to measure from")
	}
	me := side{top, ""}
	if o.committedOnly {
		me = side{o.main, myHead}
	}
	reader := Entry{Main: o.main, Branch: myBranch, Path: top, State: Live}
	for _, l := range lanes {
		if l.Path == top {
			reader = l
			break
		}
	}
	myLane := reader.Name()
	if myLane == "" {
		myLane = "HEAD"
	}

	rep := o.newReport("lane")
	rj := o.laneJSON(reader)
	rep.Reader = &rj

	type row struct{ mark, lane, file, note string }
	var body []row
	var intents []string
	var verdicts [][]string
	for _, entry := range lanes {
		if entry.Path == top || (myBranch != "" && entry.Branch == myBranch) {
			continue
		}
		s := o.sideOf(entry)
		// A branch whose work is already in main is a corpse too. Dropped whole
		// rather than merely emptied of ranges: a merge-tree verdict against a
		// branch nobody will ever merge again is the same false alarm wearing
		// the other glyph, and the true half of what it reports — your work
		// against main's copy of theirs — is what the verdict against main at
		// the bottom says, attributed to main, where it can be acted on.
		if o.spent(s) {
			continue
		}
		lane := entry.Name()
		rep.Lanes = append(rep.Lanes, o.laneJSON(entry))
		findings := o.pair(me, s, only)

		// The merge-tree verdict is computed for EVERY lane, before the
		// empty-index shortcut below can skip the rest of the loop. Subtracting
		// landed ranges is exactly what makes the index empty while merge-tree
		// still conflicts (an add/add on a file main has since landed), and the
		// failure was not a missed ⚠ but a printed "merge-tree: clean against
		// every lane", which is worse: the line this tool ends on would have
		// stated something false. Two signals disagreeing is the information;
		// one of them going quiet because the other did is the averaging this
		// block exists to refuse.
		if paths := o.conflicts(myHead, entry.Branch); len(paths) > 0 {
			rep.Conflicts = append(rep.Conflicts, overlapConflict{A: myLane, B: lane, Paths: paths})
			verdicts = append(verdicts, []string{ui.Cell(ui.Err, "✗"), lane, strings.Join(paths, " ")})
		}

		if len(findings) == 0 {
			if brief && only == "" {
				body = append(body, row{ui.Cell(ui.Muted, "·"), lane, ui.Cell(ui.Muted, "nothing shared"), ""})
			}
			continue
		}

		loud := false
		for _, f := range findings {
			rep.add(myLane, lane, f)
			// The mark and the filename are the two cells whose colour changes
			// per ROW — amber where the edits are in the same hunk, muted where
			// they only share a file — so both carry their own role rather
			// than the column's.
			if f.hunk {
				loud = true
				body = append(body, row{ui.Cell(ui.Caution, "⚠"), lane, ui.Cell(ui.Caution, f.path), f.detail()})
			} else {
				body = append(body, row{ui.Cell(ui.Muted, "·"), lane, ui.Cell(ui.Muted, f.path), "elsewhere in the file"})
			}
		}

		// Intent and the landing order are prose ABOUT the rows, not a fourth
		// finding — and prose does not go in a cell: the order is meant to be
		// copied into a PR body word for word, and a cell gives up its tail
		// where a line does not.
		if loud && !brief {
			if subject := o.intent(entry); subject != "" {
				intents = append(intents, fmt.Sprintf("%s — “%s”", lane, subject))
			} else {
				intents = append(intents, fmt.Sprintf("%s — (uncommitted work only — no commits yet)", lane))
			}
			if base, err := gitx.Run(o.main, "merge-base", myHead, entry.Branch); err == nil && base != "" {
				ms := o.size(base, myHead, me)
				ts := o.size(base, o.tip(s), s)
				first, reason := o.order(myBranch, ms, entry.Branch, ts, lane, myLane)
				rep.Order = append(rep.Order, overlapOrder{A: myLane, B: lane, First: first, Reason: reason})
				intents = append(intents, "   ↳ "+o.orderLine(first, reason, myLane, lane))
			}
		}
	}

	// main moves under you while you work, and a sibling landing its PR is the
	// commonest way a lane acquires a conflict it did nothing to earn. Cheapest
	// check here, and the one that pays for the whole command on its own.
	if rep.mainRef != "" {
		if paths := o.conflicts(myHead, rep.mainRef); len(paths) > 0 {
			rep.Conflicts = append(rep.Conflicts, overlapConflict{A: myLane, B: rep.mainRef, Paths: paths})
			verdicts = append(verdicts, []string{ui.Cell(ui.Err, "✗"), rep.mainRef, strings.Join(paths, " ")})
		}
	}

	rep.Exit = rep.code()
	rep.Warnings = append(rep.Warnings, e.Warnings...)
	if asJSON {
		return rep.emit()
	}

	bodyCells := make([][]string, 0, len(body))
	for _, r := range body {
		bodyCells = append(bodyCells, []string{r.mark, r.lane, r.file, r.note})
	}
	// --path is the hook-shaped form: when the file is clear, the answer is
	// silence. Anything that prints on a clear file gets muted within a day.
	if only != "" {
		if len(bodyCells) == 0 && len(verdicts) == 0 {
			return nil
		}
		overlapBody(bodyCells)
		overlapProse(intents)
		overlapVerdicts(verdicts)
		return rep.done()
	}

	label := repoName(o.main)
	n := len(rep.Lanes)
	if n == 0 {
		ui.Say("overlap — no other lanes on %s; nothing to bump into", label)
		return nil
	}
	loud, quiet := rep.loud(), rep.quiet()
	if loud == 0 && quiet == 0 {
		ui.Say("overlap — %d other lane(s) on %s, none in your files", n, label)
	} else {
		ui.Say("overlap — %d other lane(s) on %s; %d in your way, %d nearby", n, label, loud, quiet)
	}
	overlapBody(bodyCells)
	overlapProse(intents)
	if len(verdicts) > 0 {
		ui.Warn("merge-tree already conflicts (committed work only):")
		overlapVerdicts(verdicts)
		ui.Hint("rebase onto %s once the other branch lands — never merge %s into yours", o.base, o.base)
	} else if !brief {
		against := "every lane"
		if rep.mainRef != "" {
			against = "every lane and " + rep.mainRef
		}
		ui.Report(ui.Muted, "   merge-tree: clean against %s", against)
	}
	return rep.done()
}

// matrix is the report from the main checkout: lane against lane, every pair
// that shares a file. named is set by --pair, where the two lanes were asked
// for by name and a silent answer wants a sentence.
func (o *overlap) matrix(lanes []Entry, only string, brief, asJSON bool, named string) error {
	rep := o.newReport("matrix")
	var live []Entry
	var sides []side
	for _, entry := range lanes {
		s := o.sideOf(entry)
		// A branch whose work is already in main cannot be half of a live pair
		// — and counting it would make the summary promise lanes that aren't
		// there.
		if o.spent(s) {
			if named != "" && !asJSON {
				ui.Say("%s has nothing left to land — its work is in %s already", entry.Name(), o.base)
			}
			continue
		}
		live = append(live, entry)
		sides = append(sides, s)
		rep.Lanes = append(rep.Lanes, o.laneJSON(entry))
	}

	label := repoName(o.main)
	n := len(live)
	var body [][]string
	if n >= 2 {
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				a, b := live[i].Name(), live[j].Name()
				for _, f := range o.pair(sides[i], sides[j], only) {
					rep.add(a, b, f)
					if f.hunk {
						body = append(body, []string{ui.Cell(ui.Caution, "⚠"), a, b, ui.Cell(ui.Caution, f.path), f.detail()})
					} else {
						body = append(body, []string{ui.Cell(ui.Muted, "·"), a, b, ui.Cell(ui.Muted, f.path), "elsewhere in the file"})
					}
				}
				if paths := o.conflicts(o.tip(sides[i]), o.tip(sides[j])); len(paths) > 0 {
					rep.Conflicts = append(rep.Conflicts, overlapConflict{A: a, B: b, Paths: paths})
				}
			}
		}
	}
	rep.Exit = rep.code()
	rep.Warnings = append(rep.Warnings, o.e.Warnings...)
	if asJSON {
		return rep.emit()
	}

	// --path is the hook-shaped form wherever it is run from: silence is the
	// answer when nothing is wrong, here as much as inside a lane.
	if n < 2 {
		if only == "" {
			ui.Say("overlap — %s has fewer than two lanes; nothing can collide", label)
		}
		return nil
	}
	if len(body) == 0 && len(rep.Conflicts) == 0 {
		if only == "" {
			ui.Say("overlap — %d lanes on %s, no two of them in the same file", n, label)
		}
		return nil
	}
	if only == "" {
		ui.Say("overlap — %d lanes on %s, pairs that share a file", n, label)
	}
	if len(body) > 0 {
		// `↔` is the column head rather than a glyph repeated on every row: the
		// two lane names are two columns, and what sits between them is the
		// same on all of them, which is what a head is for.
		ui.Table([]ui.Col{
			{Head: "!", Min: 1, Weight: 1, Role: ui.Body, Cut: ui.CutNever},
			{Head: "lane", Min: 10, Weight: 2, Role: ui.Subject, Cut: ui.CutRight},
			{Head: "↔", Min: 10, Weight: 2, Role: ui.Subject, Cut: ui.CutRight},
			{Head: "file", Min: 8, Weight: 3, Role: ui.Path, Cut: ui.CutLeft},
			{Head: "note", Min: 0, Weight: 3, Role: ui.Muted, Cut: ui.CutRight},
		}, body)
	}
	if len(rep.Conflicts) > 0 {
		var verdicts [][]string
		for _, c := range rep.Conflicts {
			verdicts = append(verdicts, []string{ui.Cell(ui.Err, "✗"), c.A + " ↔ " + c.B, strings.Join(c.Paths, " ")})
		}
		ui.Warn("merge-tree already conflicts (committed work only):")
		overlapVerdicts(verdicts)
	}
	if only == "" && !brief {
		ui.Hint("the ⚠ pairs are the ones that will not merge together — land one, rebase the other")
	}
	return rep.done()
}

// overlapPair is `--pair <a> <b>`: the matrix, for exactly two named lanes,
// from wherever you are standing.
func (e *Env) overlapPair(a, b, only string, brief, asJSON, committedOnly bool) error {
	ea, err := e.matchLane(a, "scruff overlap --pair")
	if err != nil {
		return err
	}
	eb, err := e.matchLane(b, "scruff overlap --pair")
	if err != nil {
		return err
	}
	if ea.Main != eb.Main {
		return exitcode.Usagef("%s and %s are lanes of different repos — overlap is only defined within one, and two object stores cannot textually collide", ea.Label(), eb.Label())
	}
	if ea.Branch == eb.Branch {
		return exitcode.Usagef("%s and %s are the same lane", a, b)
	}
	if only != "" {
		only = repoRelative(only, ea.Main, e.Cwd)
	}
	o := e.newOverlap(ea.Main, committedOnly)
	return o.matrix([]Entry{ea, eb}, only, brief, asJSON, a+" "+b)
}

// ── rendering ────────────────────────────────────────────────────────────────

// overlapBody draws the per-lane findings: FOUR cells a row. Lane names carry a
// uniquifying suffix and are long by design; one of them must not shear the
// whole table, which is what the budget is for.
func overlapBody(rows [][]string) {
	if len(rows) == 0 {
		return
	}
	ui.Grid([]ui.Col{
		{Head: "!", Min: 1, Weight: 1, Role: ui.Body, Cut: ui.CutNever},
		{Head: "lane", Min: 10, Weight: 2, Role: ui.Subject, Cut: ui.CutRight},
		{Head: "file", Min: 8, Weight: 3, Role: ui.Path, Cut: ui.CutLeft},
		{Head: "note", Min: 0, Weight: 4, Role: ui.Muted, Cut: ui.CutRight},
	}, rows)
}

// overlapProse is the intent and landing-order lines under the table, each
// naming the lane it belongs to, printed whole so they can be pasted.
func overlapProse(lines []string) {
	for _, l := range lines {
		ui.Report(ui.Muted, "   %s", l)
	}
}

// overlapVerdicts draws merge-tree's word: THREE cells a row.
func overlapVerdicts(rows [][]string) {
	if len(rows) == 0 {
		return
	}
	ui.Grid([]ui.Col{
		{Head: "!", Min: 1, Weight: 1, Role: ui.Body, Cut: ui.CutNever},
		{Head: "lane", Min: 10, Weight: 2, Role: ui.Subject, Cut: ui.CutRight},
		{Head: "conflicts", Min: 12, Weight: 5, Role: ui.Body, Cut: ui.CutRight},
	}, rows)
}

// emit is the --json form: the report, and nothing else, on stdout — with the
// same exit code the human form would have ended on, because a finding is a
// finding whichever way it is spelled.
func (r *overlapReport) emit() error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return err
	}
	return r.done()
}

// done is the exit code, with nothing left to say: the report IS the message.
func (r *overlapReport) done() error {
	if r.Exit == exitcode.OK {
		return nil
	}
	return exitcode.Code(r.Exit)
}
