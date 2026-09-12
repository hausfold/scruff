package commands

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hausfold/scruff/internal/gitx"
)

// State is what a lane's checkout is doing on disk.
type State string

const (
	// Live: git resolves the checkout. A pane may be sitting in it.
	Live State = "live"
	// Parked: nothing on disk. The BRANCH is the work, and `scruff <name>`
	// rebuilds the checkout from it.
	Parked State = "parked"
	// Stray: a directory is there but git has disowned it — a husk left by a
	// `git worktree remove` that died between unregistering and deleting.
	Stray State = "stray"
)

// checkoutState answers live | stray | parked for a checkout path.
//
// The test is NOT `stat(path/.git)`, and that distinction cost real debugging:
// `git worktree remove` deletes the repo's admin dir (.git/worktrees/<id>)
// BEFORE the working tree, so a removal that fails part-way — an ignored
// node_modules it cannot unlink, a file another process holds — leaves a
// directory whose .git points at a gitdir that is gone. Existence calls that
// live; every git command inside it exits 128. Believing existence meant the
// husk listed as live forever, resume refused to rebuild it so the branch was
// unreachable, the sweep treated it as occupied and never swept it, and the
// statusline refresher died on it and froze the bar at hours-old data.
func checkoutState(path string) State {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return Parked
	}
	if gitx.OK(path, "--no-optional-locks", "rev-parse", "--git-dir") {
		return Live
	}
	return Stray
}

// Entry is one discovered lane, before any forge question is asked.
type Entry struct {
	Main   string
	Branch string
	Path   string
	State  State
}

// Name is the lane name — the branch minus the agent-branch prefix.
func (e Entry) Name() string { return strings.TrimPrefix(e.Branch, "worktree-") }

// Label is how a lane is named to a human: `<name> (<repo>)`. Lanes of the same
// name in two repos are ordinary here, so the repo is never decoration.
func (e Entry) Label() string { return e.Name() + " (" + filepath.Base(e.Main) + ")" }

// discover returns every resumable or live lane, deduped.
//
// Fully generic: the set of repos is discovered, never configured. Three
// sources, because no one of them is complete:
//
//  1. the registry — authoritative paths, and the only source that survives the
//     checkout being deleted (a parked lane exists nowhere else);
//  2. every live checkout under the base directory — this is what makes "all
//     repos with an open lane" work without being told about any repo;
//  3. orphan worktree-* branches in any main checkout the first two reached —
//     a branch whose registry row was lost still has work on it.
//
// First hit per (main, branch) wins, so a real path beats a synthesized one.
func (e *Env) discover() []Entry {
	type key struct{ main, branch string }
	var rows []Entry
	seen := map[key]bool{}
	add := func(main, branch, path string) {
		if main == "" || branch == "" {
			return
		}
		k := key{main, branch}
		if seen[k] {
			return
		}
		seen[k] = true
		rows = append(rows, Entry{Main: main, Branch: branch, Path: path})
	}

	var mainCandidates []string
	if regRows, err := e.Reg.Load(); err == nil {
		for _, r := range regRows {
			add(r.Main, r.Branch, r.Path)
			mainCandidates = append(mainCandidates, r.Main)
		}
	}

	// Live checkouts on disk: $BASE/<bucket>/<name>.
	buckets, _ := filepath.Glob(filepath.Join(e.Base, "*", "*"))
	for _, d := range buckets {
		if _, err := os.Stat(filepath.Join(d, ".git")); err != nil {
			continue
		}
		main, err := gitx.MainCheckout(d)
		if err != nil {
			continue
		}
		if b := gitx.CurrentBranch(d); b != "" {
			add(main, b, d)
		}
		mainCandidates = append(mainCandidates, main)
	}

	// Normalise every candidate to its real main checkout and keep only true
	// mains — one whose .git is a DIRECTORY. A linked worktree's .git is a file,
	// and treating one as a main checkout is how a worktree's branches got
	// listed twice under a repo that didn't exist.
	var mains []string
	seenMain := map[string]bool{}
	for _, c := range mainCandidates {
		m, err := gitx.MainCheckout(c)
		if err != nil || m == "" || seenMain[m] {
			continue
		}
		if fi, err := os.Stat(filepath.Join(m, ".git")); err != nil || !fi.IsDir() {
			continue
		}
		seenMain[m] = true
		mains = append(mains, m)
	}

	// Orphan agent branches in those repos.
	for _, m := range mains {
		out, err := gitx.Run(m, "branch", "--list", "worktree-*", "--format=%(refname:short)")
		if err != nil {
			continue
		}
		for _, b := range gitx.Lines(out) {
			add(m, b, filepath.Join(e.Base, filepath.Base(m), strings.TrimPrefix(b, "worktree-")))
		}
	}

	// Correct every path against git before anyone reads it. The path a row
	// arrives with is a GUESS — the registry's record of where the checkout was
	// put, or a name synthesised from the bucket convention. git knows where a
	// branch is actually checked out; rows git has no checkout for keep their
	// guess, which is exactly the parked case and where resume rebuilds.
	//
	// Correcting here rather than at one call site is the point: list, resume
	// and reap all inherit it.
	actual := checkoutMap(mains)
	for i := range rows {
		if p, ok := actual[key{rows[i].Main, rows[i].Branch}]; ok {
			rows[i].Path = p
		}
		rows[i].State = checkoutState(rows[i].Path)
	}
	return rows
}

// checkoutMap reports where each branch is actually checked out, per repo.
func checkoutMap(mains []string) map[struct{ main, branch string }]string {
	out := map[struct{ main, branch string }]string{}
	for _, m := range mains {
		porcelain, err := gitx.Run(m, "worktree", "list", "--porcelain")
		if err != nil {
			continue
		}
		var path string
		for _, line := range gitx.Lines(porcelain) {
			switch {
			case strings.HasPrefix(line, "worktree "):
				path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "branch "):
				ref := strings.TrimPrefix(line, "branch ")
				branch := strings.TrimPrefix(ref, "refs/heads/")
				if path != "" && branch != "" {
					out[struct{ main, branch string }{m, branch}] = path
				}
			}
		}
	}
	return out
}
