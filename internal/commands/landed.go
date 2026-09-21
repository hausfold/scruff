package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hausfold/scruff/internal/config"
	"github.com/hausfold/scruff/internal/gitx"
)

// This file answers the one question the whole safety story rests on: has this
// branch's work already landed on the default branch? It decides whether a
// branch DIES, so a wrong answer in the permissive direction destroys work.
//
// The full merge-strategy matrix is SPEC.md §3. In short:
//
//	nothing ever committed on the branch                   → never-diverged, offline
//	fast-forward / merge commit / rebase-that-kept-commits → ancestry, offline
//	forge rebase / squash                                  → the merged PR's headRefOid
//	cherry-pick / rebase done elsewhere                    → patch-equivalence
//	local squash with no PR                                → merge-tree-empty, advisory only
//	forge unreachable                                      → NOT landed. Keep.
//
// Uncertainty always resolves to "not landed", in every branch of this file.

// Verdict is how confident scruff is that a branch has landed.
type Verdict struct {
	Landed     bool
	Via        string // never-diverged | ancestry | pr-head-oid | patch-equivalence | merge-tree-empty
	Confidence string // certain | heuristic
	PR         int
}

// forgeTimeout bounds every forge call. A stalled network must never hang a
// pane's teardown — the remove hook runs while zellij waits.
const forgeTimeout = 6 * time.Second

// cacheTTL is how long a forge answer is reused. `reap` sets it to 0: a sweep
// that deletes branches must ask fresh.
var cacheTTL = 120 * time.Second

// Landed reports whether branch has already landed in main's default branch.
//
// The `landed` hook comes first and, when it answers, is the whole answer. This
// is the seam with teeth — a yes here is what permits a branch to be DELETED —
// and it is overridable anyway, because "landed" is a house rule wearing a
// universal name. A repo that merges into a release train, a shop whose CI
// stamps a ref on release, a monorepo where the PR is in a different repo than
// the code: none of those are visible to the ladder below, and all of them are
// two lines of shell to the person who runs them.
//
// A hook that defers, or that isn't there, or that is broken, leaves the ladder
// exactly as it was.
func (e *Env) Landed(main, branch string) Verdict {
	if e.Cfg.Defined(config.HookLanded) {
		res := e.Cfg.Ask(config.HookLanded, e.hookPayload(main, branch, "", ""))
		e.noteHook(res)
		if res.Answer != config.Defer {
			return hookVerdict(res)
		}
	}
	return e.builtinLanded(main, branch)
}

// hookVerdict turns a hook's answer into a Verdict, letting the hook name its
// own rule so a reap stays attributable in `--json`: `via: "release-train"`
// beats `via: "hook"` when you are working out why a branch went away.
func hookVerdict(res config.Result) Verdict {
	v := Verdict{Landed: res.Answer == config.Yes, Via: "hook:landed", Confidence: "certain"}
	if s, ok := res.Data["via"].(string); ok && s != "" {
		v.Via = s
	}
	if s, ok := res.Data["confidence"].(string); ok && s != "" {
		v.Confidence = s
	}
	if n, ok := res.Data["pr"].(float64); ok {
		v.PR = int(n)
	}
	return v
}

// builtinLanded is scruff's own ladder — the default the `landed` hook replaces.
func (e *Env) builtinLanded(main, branch string) Verdict {
	base := gitx.DefaultBranch(main)

	// 1. Ancestry — offline, exact, always safe.
	if gitx.IsAncestor(main, branch, base) {
		// …but ancestry alone cannot tell WORK THAT LANDED from work that never
		// existed. A lane cut from main seconds ago is trivially an ancestor of
		// it, so the freshest possible branch got the same "landed, certain"
		// verdict as one whose PR merged an hour ago — and every consumer that
		// renders a verdict rendered a brand-new lane as `merged`.
		//
		// The two are still both REAPABLE (there is nothing on either to lose),
		// so Landed stays true and the sweep is untouched; what separates them
		// is only what a reader should be told. See neverDiverged.
		if neverDiverged(main, base, branch) {
			return Verdict{Landed: true, Via: "never-diverged", Confidence: "certain"}
		}
		return Verdict{Landed: true, Via: "ancestry", Confidence: "certain"}
	}

	// 2. The branch's merged PR. Authoritative for squash and forge-rebase
	//    merges, and it survives the remote branch being deleted on merge.
	//    Landed ONLY when the local tip is exactly what that PR merged: a tip
	//    that moved on (post-merge commits, an auto-wip commit) means there is
	//    un-landed work here.
	if state, head, pr := e.mergedPR(main, branch); state == "MERGED" && head != "" {
		if tip := gitx.Rev(main, branch); tip != "" && tip == head {
			return Verdict{Landed: true, Via: "pr-head-oid", Confidence: "certain", PR: pr}
		}
	}

	// 3. Patch-equivalence. `git cherry` marks every commit whose patch-id
	//    already exists upstream with '-'; all of them '-' means the work is
	//    upstream under different SHAs — a cherry-pick, or a rebase somebody
	//    did elsewhere. Offline, and no forge needed.
	if patchEquivalent(main, base, branch) {
		return Verdict{Landed: true, Via: "patch-equivalence", Confidence: "certain"}
	}

	// 4. merge-tree-empty. Strategy-agnostic and offline, but it cannot tell a
	//    squash merge from a branch that never did anything, so it is reported
	//    and never acted on — `reap` keeps the lane, `scruff drop` takes it.
	if mergeTreeEmpty(main, base, branch) {
		return Verdict{Landed: false, Via: "merge-tree-empty", Confidence: "heuristic"}
	}

	return Verdict{Landed: false}
}

// neverDiverged reports whether a branch has never carried a commit of its own —
// the "nothing has happened here yet" state, as distinct from "it happened and
// it landed". Both are ancestors of the default branch, which is why ancestry
// cannot answer this and something else has to.
//
// Two facts, both offline:
//
//  1. no commits unique to the branch (so there is nothing here NOW), and
//  2. a reflog holding NOTHING but the branch's own creation (so there never was).
//
// (2) is the one that does the work. A lane that committed and then merged by
// fast-forward looks, by commit count alone, exactly like a lane that has done
// nothing — but its reflog kept the entry, and a fresh branch's holds only
// `branch: Created from …`.
//
// The inverted reflog test is reflogOnlyCreation's. Uncertainty resolves to
// false here — "not fresh", i.e. the old `ancestry` answer — because the cost of
// guessing wrong is a display that says `merged` where it could have said
// `fresh`, which is exactly where this ladder was before. neverWorkedOn is the
// same question asked by the code that DELETES, and resolves it the other way.
func neverDiverged(main, base, branch string) bool {
	if carriesCommits(main, base, branch) {
		return false
	}
	only, known := reflogOnlyCreation(main, branch)
	return known && only
}

// neverWorkedOn is the same question with its uncertainty resolved the OTHER
// way, for the caller that deletes rather than the one that renders: has
// nothing PROVABLY ever happened on this branch?
//
// The two differ only where the reflog cannot answer, and they differ there on
// purpose. neverDiverged is a display label, so an unreadable reflog costs it
// nothing to fall back to `ancestry`. The sweep's grace window (see laneGrace)
// is a live agent's checkout, so the same silence has to resolve to keep — a
// repo with `core.logAllRefUpdates=false` is exactly where a one-second-old
// lane would otherwise still be swept out from under somebody.
//
// It is also deliberately asked of GIT rather than read off the Verdict. A
// `landed` hook names its own `via` (hookVerdict), so a shop that merges into a
// release train would get no grace on any lane if this keyed on the verdict's
// spelling — and the hook is a claim about landedness, which is not the
// question here. What the grace is for is that occupancy cannot see an agent,
// and no house rule about merging changes that.
//
// The cost, stated: in a repo with no reflogs, a lane that fast-forward-merged
// carries no commits of its own either, so it is held for an hour after the
// merge instead of being swept at once. An hour of an empty checkout, in a
// configuration that is already degraded, against invariant 2.
func neverWorkedOn(main, branch string) bool {
	if carriesCommits(main, gitx.DefaultBranch(main), branch) {
		return false
	}
	only, known := reflogOnlyCreation(main, branch)
	return only || !known
}

// carriesCommits reports whether the branch has commits of its own right now.
func carriesCommits(main, base, branch string) bool {
	out, err := gitx.Run(main, "rev-list", "--count", base+".."+branch)
	return err != nil || out != "0"
}

// reflogOnlyCreation reports whether a branch's reflog holds NOTHING but its own
// creation, and whether it could be read at all.
//
// The test is deliberately inverted: it requires that nothing but creation ever
// happened, rather than enumerating what "something happened" looks like. That
// enumeration is a trap — `git commit` writes `commit:`, but cherry-pick writes
// `cherry-pick:`, revert `revert:`, rebase `rebase (finish):`, `reset --hard`
// `reset: moving to`, and `branch -f` `branch: Reset to`. A list of prefixes
// silently calls every one of those "fresh", which is this bug pointing the
// other way — a lane whose work really did land losing its landed verdict.
//
// The second return is the whole reason this is not a plain bool: a repo with
// reflogs disabled (`core.logAllRefUpdates=false`) or entries aged out by gc
// prints nothing, an empty reflog proves nothing either way, and the two
// callers need that silence resolved in opposite directions.
func reflogOnlyCreation(main, branch string) (only, known bool) {
	out, err := gitx.Run(main, "reflog", "show", "--format=%gs", branch)
	if err != nil {
		return false, false
	}
	lines := gitx.Lines(out)
	if len(lines) == 0 {
		return false, false
	}
	for _, l := range lines {
		// The ONE subject a branch that has only ever been created can carry.
		// Note the trailing space: it excludes `branch: Reset to …`, which is a
		// hand-moved tip and very much something happening.
		if !strings.HasPrefix(l, "branch: Created from ") {
			return false, true
		}
	}
	return true, true
}

// patchEquivalent reports whether every commit on branch has a patch-id
// equivalent already in base. An empty branch is not "landed" — it is empty —
// so a branch with no commits of its own returns false.
func patchEquivalent(main, base, branch string) bool {
	out, err := gitx.Run(main, "cherry", base, branch)
	if err != nil {
		return false
	}
	lines := gitx.Lines(out)
	if len(lines) == 0 {
		return false
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "-") {
			return false
		}
	}
	return true
}

// mergeTreeEmpty reports whether merging branch into base would add nothing.
//
// True for a squash merge, a manual re-implementation, and an empty branch
// alike — which is exactly why the caller treats it as advisory.
func mergeTreeEmpty(main, base, branch string) bool {
	merged, err := gitx.Run(main, "merge-tree", "--write-tree", base, branch)
	if err != nil {
		return false // a conflict exits non-zero, and a conflict is not "landed"
	}
	// --write-tree prints the resulting tree OID on the first line.
	lines := gitx.Lines(merged)
	if len(lines) == 0 {
		return false
	}
	baseTree, err := gitx.Run(main, "rev-parse", base+"^{tree}")
	if err != nil {
		return false
	}
	return lines[0] == baseTree
}

var mergeInfoRe = regexp.MustCompile(`^(\S+)\s+(\S+)\s+(\d+)`)

// mergedPR asks the forge for this branch's merged PR: its state, the SHA it
// merged, and its number.
//
// The argv is GitHub-shaped and hardcoded for 0.1; it becomes one forge adapter
// TOML in 0.2 (SPEC.md §5.4), which is why the shape is kept in one place.
func (e *Env) mergedPR(main, branch string) (state, headOID string, pr int) {
	slug, err := gitx.RemoteSlug(main)
	if err != nil || slug == "" {
		return "", "", 0
	}
	out := e.cachedForge("head-"+slug+"-"+branch,
		"pr", "list", "-R", slug, "--head", branch, "--state", "merged", "--limit", "1",
		"--json", "number,state,headRefOid",
		"--jq", `.[0] // empty | "\(.state) \(.headRefOid) \(.number)"`)
	m := mergeInfoRe.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return "", "", 0
	}
	n, _ := strconv.Atoi(m[3])
	return m[1], m[2], n
}

// forgeCachePath is where one forge answer is memoised. Dot-prefixed so the
// $BASE/*/* worktree globs never see it.
func (e *Env) forgeCachePath(key string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, key)
	return filepath.Join(e.Base, ".cache", safe)
}

// forgetForge drops a memoised answer, for the caller that just made it wrong.
// Only writes are entitled to this: a read that finds a stale answer should
// wait out the TTL rather than stampede the forge.
func (e *Env) forgetForge(key string) { _ = os.Remove(e.forgeCachePath(key)) }

// cachedForge runs a forge query, memoised on disk.
//
// On disk rather than in memory because the cache must span invocations: the
// statusline refresher and a `scruff` listing seconds apart should cost one query
// between them, not two. A failed query writes an empty file only when nothing
// is cached, so an offline run asks once rather than once per row — and never
// clobbers a good answer with an empty one.
func (e *Env) cachedForge(key string, args ...string) string {
	file := e.forgeCachePath(key)

	if cacheTTL > 0 {
		if fi, err := os.Stat(file); err == nil && time.Since(fi.ModTime()) < cacheTTL {
			if b, err := os.ReadFile(file); err == nil {
				return string(b)
			}
		}
	}
	if _, err := exec.LookPath("gh"); err != nil {
		e.Warn("no forge CLI on PATH — PR state is unknown, so nothing will be reaped on that basis")
		return ""
	}
	_ = os.MkdirAll(filepath.Dir(file), 0o755)

	cmd := exec.Command("gh", args...)
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() { out, runErr = cmd.Output(); close(done) }()
	select {
	case <-done:
	case <-time.After(forgeTimeout):
		_ = cmd.Process.Kill()
		<-done
		e.Warn("the forge timed out — PR state is stale")
		runErr = os.ErrDeadlineExceeded
	}
	if runErr != nil {
		if _, err := os.Stat(file); err != nil {
			_ = os.WriteFile(file, nil, 0o644)
		}
		b, _ := os.ReadFile(file)
		return string(b)
	}
	_ = os.WriteFile(file, out, 0o644)
	return string(out)
}
