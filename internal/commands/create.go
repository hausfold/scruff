package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hausfold/scruff/internal/exitcode"
	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/registry"
	"github.com/hausfold/scruff/internal/ui"
)

// hookField reads the first present key from a hook payload.
//
// Key names drift across Claude Code versions — the docs say
// worktree_name/base_path, 2.1.x sends name/cwd — so scruff accepts a set of
// aliases per logical field, first hit wins (SPEC.md §2.3). Which alias fired
// is worth knowing when a future bump changes them again, so it is reported to
// the caller rather than swallowed.
func hookField(payload map[string]any, keys ...string) (value, matched string) {
	for _, k := range keys {
		if v, ok := payload[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s, k
			}
		}
	}
	return "", ""
}

func readHookPayload(r io.Reader) (map[string]any, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, exitcode.Usagef("could not read the hook payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, exitcode.Usagef("the hook payload isn't JSON: %v", err)
	}
	return payload, nil
}

// HookCreate implements the WorktreeCreate hook: JSON on stdin, and ONLY the
// new checkout path on stdout.
//
// The stdout-purity rule is load-bearing, not stylistic: Claude Code reads the
// path off stdout, and `cd "$(scruff child …)"` does too. Every diagnostic goes
// to stderr.
func (e *Env) HookCreate(stdin io.Reader) error {
	payload, err := readHookPayload(stdin)
	if err != nil {
		return err
	}
	name, _ := hookField(payload, "name", "worktree_name")
	base, _ := hookField(payload, "base_path", "cwd")
	if name == "" || base == "" {
		return exitcode.Usagef(
			"the hook payload has none of the keys I wanted (name/worktree_name, base_path/cwd) — got: %s",
			strings.Join(keysOf(payload), ", "))
	}

	main, err := gitx.MainCheckout(base)
	if err != nil {
		return exitcode.Usagef("%q isn't inside a git repo", base)
	}
	// The name is the CLIENT's — Claude Code has already made the branch and
	// told us about it — so `name_max` (SPEC.md §5.7) can only warn here, not
	// refuse. Worth warning: this is the one lane-creation path where a name
	// over what the machine's lane backend can carry still gets through, and
	// the failure it leads to says nothing at all (a window that closes on its
	// own launch error). One line now beats that.
	if budget := e.laneNameBudget(main); budget > 0 && len(name) > budget {
		e.Warn(fmt.Sprintf("lane name %q is %d bytes and a lane in %s can carry %d — its window may not open (name_max); `scruff drop %s` and make it again with a shorter name",
			name, len(name), filepath.Base(main), budget, name))
	}

	// Keyed on `main`, not on the payload's cwd — those agree for a pane in the
	// main checkout and diverge for one standing in a subdirectory of it, where
	// the old basename bucket was whatever source folder the hook fired from.
	dir := filepath.Join(e.Base, repoKey(main), name)
	if err := e.addWorktree(main, name, dir); err != nil {
		return err
	}

	// Record it so scruff can rebuild and reopen this lane later — even after the
	// checkout is gone, and even for a repo it has never otherwise heard of.
	// The spawning pane's cwd is the parent, so a pane can be shown only the
	// lanes IT spawned. This is Claude Code's own hook, so the client is known
	// even when the machine-wide default is Codex or OpenCode.
	// NOT ignored. A checkout with no registry row is a lane scruff can no
	// longer find, resume, or reap — the branch survives, but every affordance
	// around it is gone. Say so loudly rather than let a lock timeout eat it
	// silently; the caller still gets the path, because the checkout is real.
	if err := e.Reg.Put(registry.Row{
		Name: name, Main: main, Branch: "worktree-" + name,
		Path: dir, Parent: base, Agent: "claude",
	}); err != nil {
		ui.Warn("the checkout exists but the registry row could not be written (%v) — `scruff` won't list it until you re-run this", err)
	}
	ui.Out("%s\n", dir)
	return nil
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// addWorktree creates the checkout on branch worktree-<name>.
func (e *Env) addWorktree(repo, name, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	branch := "worktree-" + name
	args := []string{"worktree", "add", "-b", branch, dir, "HEAD"}
	if !gitx.HasCommits(repo) {
		// A repo with no commits yet has no HEAD to branch from. --orphan gives
		// the lane a real checkout on an unborn branch; the ref appears with
		// its first commit. Without this, `scruff new` in a freshly-inited repo
		// fails at exactly the moment a scaffolding agent is most useful.
		args = []string{"worktree", "add", "--orphan", "-b", branch, dir}
	}
	out, err := gitx.Run(repo, args...)
	if err != nil {
		return exitcode.Usagef("git worktree add failed: %v", err)
	}
	if out != "" {
		fmt.Fprintln(os.Stderr, out)
	}
	return nil
}

// sayCreated announces a fresh lane — and, in a repo with submodules, the one
// thing about that lane which is not yet true.
//
// `git worktree add` does not recurse, so the lane's vendored directories are
// there and EMPTY. A build that dies on a missing directory says nothing about
// why, and the explanation otherwise lives one verb away in `scruff doctor` —
// which is no help at all to somebody who has no reason to suspect submodules
// and so never runs it. Same sentence as doctor's (submoduleWhy), plus the
// command the reader now has to run.
//
// Both lines are stderr, and that is a contract, not a preference (SPEC.md
// §2.3): stdout carries the path alone, because the documented use is
// `cd "$(scruff new)"`.
func sayCreated(main, name, dir string) {
	ui.Say("created %s lane '%s' → %s", repoKey(main), name, dir)
	if n := countSubmodules(main); n > 0 {
		ui.Warn("submodules %s: run `git submodule update --init --recursive` in the lane", submoduleNote(n))
	}
}
