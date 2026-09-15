package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hausfold/scruff/internal/config"
	"github.com/hausfold/scruff/internal/exitcode"
	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/registry"
	"github.com/hausfold/scruff/internal/ui"
)

// Resume rebuilds a lane's checkout and reopens the agent chat that made it.
//
// `want` is a name, or `<repo>/<name>` when the same name exists in two repos.
// `pick` forces the client's session picker instead of continuing the lane's
// newest conversation — see resumeArgv.
func (e *Env) Resume(want string, pick bool) error {
	if want == "" {
		return e.List(false)
	}
	entry, err := e.matchLane(want, "scruff")
	if err != nil {
		return err
	}
	return e.resumeEntry(entry, pick)
}

// resumeEntry is Resume once the lane is known.
//
// The split exists for `focus`, not for tidiness: a deferred focus hook — the
// window layer looked and found nothing to raise — falls through to resume, and
// calling Resume by NAME there would send the same invocation through
// matchLane's whole-machine discover a second time. That is ~130 git
// subprocesses on a machine with a few dozen lanes, and it is paid on the
// slowest path there is: the one where a banner click has to open a window.
func (e *Env) resumeEntry(entry Entry, pick bool) error {
	// Resolve the client BEFORE the checkout is re-registered: a five-column
	// registry row predates the client field and is Claude forever, even if the
	// machine's default has since changed.
	agent := e.agentForPath(entry.Path)

	switch entry.State {
	case Live:
		ui.Say("'%s' is still live at %s", entry.Branch, entry.Path)

	case Stray:
		// A husk: the directory is there, git disowns it. It is in the way —
		// `git worktree add` refuses a non-empty directory — and it may hold
		// real uncommitted work, so it is MOVED, never deleted. The rebuilt
		// checkout beside it has the branch's committed state; whatever was only
		// in the husk is one `diff -ru` away, and the path is printed so it
		// cannot be lost silently.
		husk := entry.Path + ".stray-" + time.Now().Format("20060102-150405")
		if err := os.Rename(entry.Path, husk); err != nil {
			return exitcode.Usagef("couldn't move the dangling checkout aside: %s", entry.Path)
		}
		ui.Say("dangling checkout moved to %s (nothing deleted — it may hold uncommitted work)", husk)
		if err := e.rebuild(entry, agent); err != nil {
			return err
		}
		ui.Say("compare what the husk had: diff -ru %s %s", husk, entry.Path)

	default: // Parked
		if err := e.rebuild(entry, agent); err != nil {
			return err
		}
	}

	spec, known := specFor(agent)
	if !known {
		return exitcode.Usagef("unknown agent %q recorded for this lane", agent)
	}

	// A spawned lane (`scruff child`, or a nested spawn) has no chat of its own —
	// reopen the client session that spawned it. The checkout above is still
	// rebuilt either way, so the branch's files are on disk; this only decides
	// which directory the client's picker opens in.
	chat := e.chatHome(agent, entry.Path)
	// Nothing here and no parent to fall back on: the conversation may be at a
	// path this lane has since moved off, and if it is, it comes with it. Done
	// before the argv below is resolved, because bringing it over is what makes
	// "continue the newest conversation here" an answerable question again.
	if chat == entry.Path && !agentHasChat(agent, entry.Path) {
		if was := e.recoverChat(agent, entry); was != "" {
			ui.Say("the checkout moved since this lane last ran — brought its conversation over from %s", was)
		}
	}
	// Whether the chat is the lane's own is exactly the question "can scruff name
	// the conversation?" — one checkout only this lane's agent ever ran in has
	// one newest conversation and that is it; a shared parent has many, and
	// only the user can say which.
	argv := resumeArgv(spec, chat == entry.Path, pick)
	// Still nothing to continue. `claude --continue` in a directory with no
	// transcript is not a resume, it is an exit 1 into a pane that closes on
	// "No conversation found to continue" — so a lane that has never been
	// talked in opens a fresh session instead. Only ever said about a client
	// whose store scruff can actually read (agentProbeable); `--pick` is the
	// user asking for the picker by hand and is left alone.
	if chat == entry.Path && !pick && agentProbeable(agent) && !agentHasChat(agent, entry.Path) && len(spec.open) > 0 {
		ui.Say("no conversation in this lane yet — %s opens a fresh one", agent)
		argv = spec.open
	}

	if chat != entry.Path {
		ui.Say("no chat in this lane — it was spawned from a pane in %s", chat)
		// A shared parent checkout (a workshop pane that spawned several
		// children) lists many sessions in its picker — point at the right one.
		ui.Say("in the picker, pick the session for '%s' — last commit:", entry.Branch)
		ui.Say("  %s", gitx.Subject(entry.Main, entry.Branch))
		// Claude keys the transcript off the cwd, so that directory has to
		// exist. If the parent checkout was reaped, anchor a bare dir purely to
		// reopen the chat — the work is safe on the branch, and the child
		// checkout with the files was rebuilt above.
		_ = os.MkdirAll(chat, 0o755)
	}

	// The checkout is on disk and the chat's home is known; what "reopen this
	// session" MEANS is the machine's business from here. scruff's own answer —
	// chdir, then become the client — is right for a tool invoked from the pane
	// that will host it, and wrong for every machine that would rather open a
	// pane of its own. That is the `resume` hook.
	if res := e.openSession(config.HookResume, entry, agent, chat, argv); res.Answer != config.Defer {
		return hookOutcome(config.HookResume, res)
	}

	if ui.IsTTY(os.Stdout) && clientInstalled(agent) {
		ui.Say("reopening the %s chat …", agent)
		if err := os.Chdir(chat); err != nil {
			return exitcode.Usagef("could not enter %s", chat)
		}
		return execClient(argv)
	}
	// No tty (piped, or driven by a script) or no client installed: print the
	// command rather than exec into something nobody can see.
	ui.Say("checkout ready. Reopen the %s chat with:", agent)
	ui.Out("    cd %s && %s\n", shellQuote(chat), shellCommand(argv))
	return nil
}

// rebuild re-adds a checkout for a branch that still exists.
func (e *Env) rebuild(entry Entry, agent string) error {
	ui.Say("rebuilding checkout for %s → %s", entry.Branch, entry.Path)
	if err := os.MkdirAll(filepath.Dir(entry.Path), 0o755); err != nil {
		return err
	}
	if out, err := gitx.Run(entry.Main, "worktree", "add", entry.Path, entry.Branch); err != nil {
		return exitcode.Usagef("git worktree add failed: %v", err)
	} else if out != "" {
		ui.Say("%s", out)
	}
	// Empty Parent preserves whatever the existing row had — resume knows the
	// path but not the original spawner, and blanking it would orphan the row.
	_ = e.Reg.Put(registry.Row{
		Name: entry.Name(), Main: entry.Main, Branch: entry.Branch,
		Path: entry.Path, Agent: agent,
	})
	return nil
}

// openSession puts an action seam — `resume` or `open` — the question scruff was
// about to answer by exec'ing a client.
//
// `chat` is the seam's whole reason for existing beyond `path`: a spawned
// lane's conversation lives in the pane that made it, so a hook opening a
// pane must be told to cd somewhere that is NOT the checkout it just rebuilt.
// Getting that wrong is how a resumed child lane opens an empty session.
//
// `argv` is the command scruff was about to exec, already resolved to continue-
// the-newest or open-the-picker. A hook that spawns a pane should run it rather
// than re-derive it, or the pane lands on the picker scruff just avoided.
func (e *Env) openSession(hook string, entry Entry, agent, chat string, argv []string) config.Result {
	if !e.Cfg.Defined(hook) {
		return config.Result{Answer: config.Defer}
	}
	payload := e.hookPayload(entry.Main, entry.Branch, entry.Path, agent)
	payload["chat"] = chat
	payload["state"] = string(entry.State)
	payload["command"] = shellCommand(argv)
	res := e.Cfg.Do(hook, payload)
	e.noteHook(res)
	return res
}

// hookOutcome turns an action hook's answer into scruff's exit code. A hook that
// handled the work is a success; one that refused keeps its refusal's meaning,
// because a wrapper script has to tell "you asked wrong" from "I declined".
func hookOutcome(hook string, res config.Result) error {
	switch {
	case res.Answer == config.Yes:
		return nil
	case res.Refused:
		return exitcode.Refusedf("the %s hook declined", hook)
	default:
		return exitcode.Usagef("the %s hook failed", hook)
	}
}

func clientInstalled(id string) bool {
	_, err := exec.LookPath(id)
	return err == nil
}

// shellCommand renders an argv as a command STRING a hook can run through a
// shell — which is what the `command` field and `SCRUFF_COMMAND` have always been
// (hausfold.co/docs/scruff/seams: "run it; don't rebuild it"), and what every opener does with it.
//
// Each element is quoted independently. Until `--prompt`, every argv scruff handed
// a hook was one or two bare words (`claude`, `codex resume --last`) where a
// space-join and a quoted join are byte-identical — so this changes nothing that
// already exists, and stops the one thing that would otherwise break the moment
// a prompt is in there. A brief is multi-line and routinely holds quotes and
// `$`; joined bare it shatters into words at best, and unbalances the opener's
// `bash -lc` at worst.
func shellCommand(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`*?[]{}()&;|<>#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
