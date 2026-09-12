package commands

import (
	"os"
	"strings"

	"github.com/hausfold/scruff/internal/exitcode"
)

// Version is stamped at build time (-ldflags "-X …commands.Version=…").
var Version = "0.1.0-dev"

const usage = `scruff — the worktree-lifecycle substrate

A LANE is one agent's branch, checkout and pane, from create to reaped.

  scruff                    list every live/parked lane, across all repos
  scruff <name>             resume one: rebuild its checkout, reopen its agent
                          --pick to choose the session instead of the newest
  scruff focus <name>       go to the window a lane is already running in
                          ([hooks] focus; falls back to resume without one)
  scruff new [name]         a lane on THIS repo; prints its path: cd "$(scruff new)"
                          --open [agent] open a session in it (--agent <id>)
                          --cmd '<command>' run something else in it instead
                          --prompt '<task>' | --prompt-file <file|-> open it on
                          a first turn (implies --open) · --image <file>
  scruff child <repo>       a lane on ANOTHER repo, as a child of this pane
  scruff spawn <repo> <name>
                          a named lane for a spawner with no pane of its own
                          --prompt/--prompt-file/--image as above, and the lane
                          is opened through [hooks] open — exit 3 if none
                          <name> is optional with a --prompt: set namer = "<id>"
                          in the config and the task names the lane
                          --derived-name <name> for a name YOU worked out from
                          the task: fit to what this machine carries, not refused
  scruff park [label]       set the working tree aside as a wip: commit on this branch
  scruff unpark             put the last parked commit's changes back, uncommitted
  scruff reap               sweep every LANDED lane that nobody is standing in
                          occupied, dirty and unlanded lanes are kept and named;
                          scruff reaped has the SHA to undo any of it
                          --dead-ends also retires the lanes nothing will ever
                          land (PR closed unmerged, repo archived) — same
                          refusals and the same undo as scruff drop
  scruff reaped             what scruff has reaped, why, and the SHA to get it back
  scruff drop <name>        retire a lane whose work will never land (closed PR,
                          archived repo) — recorded in scruff reaped, undoable
  scruff heartbeat [path]   hold the occupancy lease on a lane, so reap spares it
                          --pid N (0 = TTL-only) · --release to drop it
  scruff doctor             diagnose this machine and the repo you're standing in:
                          the base, forge auth, occupancy, reflink, submodules /
                          LFS / sparse-checkout, the default-branch resolution,
                          stray checkouts, orphan branches, disk per repo
                          --json for the same as data. It fixes nothing, and a
                          finding still exits 0 — the findings ARE the report
  scruff doctor --migrate-base
                          move the base to ~/.cache/scruff (§8.2): refuses with
                          exit 2 while any lane is occupied, repairs every
                          checkout, leaves the old path a symlink for one release
  scruff watch --json       lifecycle events on stdout, one NDJSON object per line
  scruff reship [name]      push a branch that outran its merged PR, open the follow-up
  scruff runtime up <name>  stand up a lane's runtime-isolation backend
                          --backend <id> (required — never automatic)
                          tart is built in: a headless macOS per lane
  scruff runtime enter <name> --backend <id>
                          drop into it interactively
  scruff runtime down <name> --backend <id>
                          tear it down
  scruff runtime eject tart print the built-in backend as an adapter file to edit
  scruff skill [<name>]     print an agent skill: scruff's own, or handoff
  scruff skill install      write them all into every agent client found
                          --client claude|codex|opencode|pi · --dir <path>
                          never overwrites — exit 2 if anything was left alone
  scruff hook create        [hook] open a lane — JSON on stdin, path on stdout
  scruff hook remove        [hook] retire one without losing work — JSON on stdin
  scruff hook notify        [hook] client events → a trill banner for the lane:
                          Notification hangs an ask, Stop replaces it with a
                          done, UserPromptSubmit/PostToolUse resolve it —
                          JSON on stdin, exit 0 always, no-op without trill

  --json                  machine-readable listing: scruff --json, scruff list --json
  --version               print the version
  <verb> --help           just that verb's lines — no verb does its work on a
                          help flag, and no verb ignores an argument it can't
                          explain

Exit codes: 0 ok · 1 usage · 2 refused for safety · 3 degraded · 4 conflict found
            5 registry locked
`

// Run dispatches one invocation and returns the error to exit on.
func Run(args []string) error {
	// Before anything else: scruff is invoked by hooks that supply no PATH, and it
	// resolves git for every single operation. See rescuePATH.
	rescuePATH()

	env, err := NewEnv()
	if err != nil {
		return err
	}

	if len(args) == 0 {
		return env.List(false)
	}

	// A verb's own -h/--help prints THAT verb's lines and runs nothing. `scruff
	// reap --help` is why this exists: help was spelled only at the top level,
	// `Reap` never looked at its arguments, and the flag that asks a question
	// swept instead of answering it. Agents hit it repeatedly, because trying
	// `--help` on an unfamiliar verb is exactly what you do.
	if len(args) > 1 && helpAsked(args[1:]) {
		os.Stderr.WriteString(verbUsage(args[0]))
		return nil
	}

	switch args[0] {
	case "-h", "--help", "help":
		os.Stderr.WriteString(usage)
		return nil

	case "--version", "version":
		os.Stdout.WriteString(Version + "\n")
		return nil

	case "park":
		label, err := oneArg("park", args[1:])
		if err != nil {
			return err
		}
		return env.Park(label)

	case "unpark":
		if err := noArgs("unpark", args[1:]); err != nil {
			return err
		}
		return env.Unpark()

	case "list":
		if err := onlyFlags("list", args[1:], "--json"); err != nil {
			return err
		}
		return env.List(hasFlag(args, "--json"))

	// `scruff --json` is `scruff list --json`. Bare `scruff` IS the listing, so its
	// machine-readable form has to be spellable without naming the implied verb
	// — the statusline runs it several times a minute and every consumer that
	// reached for the obvious spelling got "unknown flag" instead.
	case "--json":
		return env.List(true)

	// The strictest verb in the CLI, because it is the one that DELETES on its
	// own initiative. An argument scruff cannot explain stops the run: the class
	// of typo is unbounded (`--help`, `--dry-run`, `-n`, a lane name), and a
	// sweep is not the thing to do while unsure what was asked for.
	case "reap":
		if err := noWords("reap", args[1:], "--dead-ends"); err != nil {
			return err
		}
		return env.Reap(hasFlag(args, "--dead-ends"))

	case "reaped":
		if err := noArgs("reaped", args[1:]); err != nil {
			return err
		}
		return env.Ledger()

	case "drop":
		name, err := oneArg("drop", args[1:])
		if err != nil {
			return err
		}
		return env.Drop(name)

	case "heartbeat":
		return env.Heartbeat(args[1:])

	case "doctor":
		return env.Doctor(args[1:])

	case "watch":
		return env.Watch(args[1:])

	case "resume":
		name, err := oneArg("resume", args[1:], "--pick")
		if err != nil {
			return err
		}
		return env.Resume(name, hasFlag(args, "--pick"))

	// `scruff focus` is `scruff <name>` minus the reopening: go to the window the
	// lane is already running in. It is typed rarely and clicked often — it is
	// what trill runs when a lane's banner is clicked.
	case "focus":
		name, err := oneArg("focus", args[1:])
		if err != nil {
			return err
		}
		return env.Focus(name)

	case "new":
		return env.NewCmd(args[1:])

	case "child":
		if err := onlyFlags("child", args[1:]); err != nil {
			return err
		}
		return env.Child(argAt(args, 1), argAt(args, 2))

	case "spawn":
		return env.SpawnCmd(args[1:])

	case "agent":
		return env.AgentCmd(args[1:])

	case "reship":
		name, err := oneArg("reship", args[1:])
		if err != nil {
			return err
		}
		return env.Reship(name)

	case "runtime":
		return env.RuntimeCmd(args[1:])

	// A3 of the family agent surface. Not `docs agent`, which SPEC.md §14.5
	// reserved before this landed — see skill.go for which name won and why.
	case "skill":
		return env.Skill(args[1:])

	// `scruff hook create` is the documented spelling. The bare `create` /
	// `remove` verbs are kept because that is what the shipped Claude Code hook
	// configuration calls today, and cutover must not require editing both the
	// hook config and the binary in the same breath (SPEC.md §10).
	case "hook":
		switch argAt(args, 1) {
		case "create":
			return env.HookCreate(os.Stdin)
		case "remove":
			return env.HookRemove(os.Stdin)
		case "notify":
			return env.HookNotify(os.Stdin)
		default:
			return exitcode.Usagef("usage: scruff hook <create|remove|notify>  (JSON on stdin)")
		}
	case "create":
		return env.HookCreate(os.Stdin)
	case "remove":
		return env.HookRemove(os.Stdin)

	default:
		// A bare token is a lane name: `scruff sparkle` resumes it. This is the
		// spelling that gets typed, so an unknown one must fail the way resume
		// does — naming the listing — not with a generic "unknown command".
		if strings.HasPrefix(args[0], "-") {
			return exitcode.Usagef("unknown flag %q — try `scruff --help`", args[0])
		}
		if err := onlyFlags(args[0], args[1:], "--pick"); err != nil {
			return err
		}
		if extra := firstBare(args[1:]); extra != "" {
			return exitcode.Usagef("`scruff %s` resumes one lane — %q would be a second; run them one at a time", args[0], extra)
		}
		return env.Resume(args[0], hasFlag(args, "--pick"))
	}
}

// firstBare is the first argument that isn't a flag — `scruff resume --pick back`
// and `scruff resume back --pick` have to mean the same thing, because both get
// typed.
func firstBare(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// helpAsked reports whether a verb's own arguments are asking for its usage
// rather than for its work.
//
// It skips the value of a flag that takes one and stops at `--`, so the two
// places a literal `--help` is DATA still mean what they say: `scruff new
// --prompt '--help'` opens a lane on that (odd) task, and `scruff agent start
// claude -- --help` passes it to the client.
func helpAsked(args []string) bool {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return false
		case a == "-h", a == "-help", a == "--help":
			return true
		case flagWantsValue[a]:
			i++
		}
	}
	return false
}

// flagWantsValue is every flag in the CLI whose NEXT argument is data. Only
// helpAsked reads it — each verb still parses its own flags — and it exists so
// scanning for `--help` never mistakes a user's text for one of ours.
var flagWantsValue = map[string]bool{
	"--agent":        true,
	"--backend":      true,
	"--client":       true,
	"--cmd":          true,
	"--derived-name": true,
	"--dir":          true,
	"--image":        true,
	"--pid":          true,
	"--prompt":       true,
	"--prompt-file":  true,
}

// verbUsage is the usage block that documents one verb — the lines the person
// who typed `scruff reap --help` actually wanted, without twenty other verbs to
// read past. Every block starts with "  scruff <verb>" and runs to the next
// blank line; a verb `usage` never names (the internal spellings: `resume`,
// `list`, the bare hook aliases) falls back to the whole thing, which is never
// wrong, only long.
func verbUsage(verb string) string {
	var block []string
	keep := false
	for _, line := range strings.Split(usage, "\n") {
		switch {
		case strings.HasPrefix(line, "  scruff"):
			f := strings.Fields(line)
			keep = len(f) > 1 && f[1] == verb
		case strings.TrimSpace(line) == "", !strings.HasPrefix(line, "   "):
			keep = false
		}
		if keep {
			block = append(block, line)
		}
	}
	if len(block) == 0 {
		return usage
	}
	return strings.Join(block, "\n") + "\n"
}

// noArgs refuses a verb that takes none, instead of doing its work anyway.
//
// This is the other half of the `scruff reap --help` fix, and the load-bearing
// half: help now prints, but the next unrecognised argument someone types is
// one nobody has thought of yet. A verb that swallows what it cannot explain
// turns every such typo into a run of itself — which for `reap` means a sweep.
// Invariant 1's failure direction is "nothing happened".
func noArgs(verb string, args []string) error {
	for _, a := range args {
		if a == "" {
			continue
		}
		return exitcode.Usagef("`scruff %s` takes no arguments — %q is not one, so nothing ran. `scruff %s --help` explains the verb", verb, a, verb)
	}
	return nil
}

// noWords is noArgs for a verb that takes named flags but no bare words. Same
// refusal, same reason: the flag it does accept is spelled out, and everything
// else stops the run rather than being swallowed by the verb that DELETES.
func noWords(verb string, args []string, allowed ...string) error {
	for _, a := range args {
		switch {
		case a == "":
		case strings.HasPrefix(a, "-"):
			if !flagAllowed(a, allowed) {
				return unknownFlag(verb, a)
			}
		default:
			return exitcode.Usagef("`scruff %s` takes no arguments — %q is not one, so nothing ran. `scruff %s --help` explains the verb", verb, a, verb)
		}
	}
	return nil
}

// oneArg parses `<verb> [flags] [<word>]` strictly: a flag the verb does not
// accept, or a second bare word, is a typo — and a typo must not silently
// resolve to a lane or a label nobody named.
func oneArg(verb string, args []string, allowed ...string) (string, error) {
	word := ""
	for _, a := range args {
		switch {
		case a == "":
		case strings.HasPrefix(a, "-"):
			if !flagAllowed(a, allowed) {
				return "", unknownFlag(verb, a)
			}
		case word == "":
			word = a
		default:
			return "", exitcode.Usagef("`scruff %s` takes one argument — got %q and %q; quote it if the two are one thing", verb, word, a)
		}
	}
	return word, nil
}

// onlyFlags is oneArg's flag half, for the verbs that take more than one word.
func onlyFlags(verb string, args []string, allowed ...string) error {
	for _, a := range args {
		if a == "" || !strings.HasPrefix(a, "-") {
			continue
		}
		if !flagAllowed(a, allowed) {
			return unknownFlag(verb, a)
		}
	}
	return nil
}

func flagAllowed(flag string, allowed []string) bool {
	for _, a := range allowed {
		if flag == a {
			return true
		}
	}
	return false
}

func unknownFlag(verb, flag string) error {
	return exitcode.Usagef("unknown flag %q for `scruff %s` — nothing ran; `scruff %s --help` lists what it takes", flag, verb, verb)
}

func argAt(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
