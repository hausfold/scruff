// Package config is scruff's machine-wide configuration and, through it, scruff's
// policy seams.
//
// A *seam* is a place where scruff makes a decision that looks universal but
// isn't: what "landed" means, what makes a lane reapable, whether a dirty
// tree is worth a wip commit, what "reopen this session" means on a machine
// that runs its agents inside a multiplexer. scruff ships an opinion for each —
// the one haus grew up with — and every one of them is wrong for
// somebody.
//
// So each is a named hook with a built-in default. A consumer that supplies
// one replaces scruff's opinion at that point and nothing else; a consumer that
// supplies none gets exactly the behaviour scruff had before this package
// existed. That is the whole design: scruff is the substrate, the hooks are where
// somebody else's house style goes, and neither has to know about the other.
//
// The protocol is a program, not an expression language. scruff execs the hook's
// argv, hands it the situation twice over (JSON on stdin for programs, SCRUFF_*
// environment variables for shell one-liners), and reads the answer off the
// exit code. Nothing is interpreted, nothing is templated into a shell, and a
// branch named `--force; rm -rf ~` is just a string in argv.
package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the resolved contents of ~/.config/scruff/config.toml.
type Config struct {
	// Agent is the top-level `agent = "codex"` key: the client a new lane
	// opens in. It is the static, zero-process spelling of the `agent` hook —
	// a constant answer needs no program to return it.
	Agent string

	// Namer is the top-level `namer = "claude"` key: the id of the adapter
	// that turns a lane's first-turn task into the lane's name. Empty — the
	// default, and what every install had before this key existed — means an
	// unnamed lane keeps taking a random word pair. See SPEC.md §5.6.
	Namer string

	// NameMax is the top-level `name_max = "46"` key: the longest lane KEY —
	// `scruff/<repo>/<lane>`, the spelling askKey writes and the one a lane
	// backend joins on — that this machine can carry, in bytes. 0, the
	// default and what every install had before this key existed, is no cap.
	//
	// It is a number about the BACKEND, not about taste. haus renders that key
	// as a zmx session name (`scruff.<repo>.<lane>`, the same length), and zmx
	// puts its sockets in $TMPDIR, so the name has a hard ceiling set by how
	// long that directory's path is: on macOS with a three-digit uid, 46 bytes.
	// Past it the session cannot be created at all — the lane's window dies
	// before the client starts, with an error only Ghostty ever shows. So the
	// machine that knows the ceiling states it here, and scruff refuses a name
	// it cannot carry at the moment the name is chosen, which is the only
	// moment the name can still change.
	//
	// Written as a quoted string because that is what this parser reads, and
	// taken bare (`name_max = 46`) too, because that is what TOML says an
	// integer looks like and a hand-written config will say it that way. A
	// value that is not a number at all is warned about and dropped.
	NameMax int

	// Hooks maps a seam name to the argv scruff runs for it. Absent means "use
	// the built-in", which is what an empty config gets and therefore what
	// every scruff install had before hooks existed.
	Hooks map[string][]string

	// Path is where this came from, or "" when no config file was found. Only
	// diagnostics use it.
	Path string
}

// Hook names. Every one of these has a built-in that runs when the hook is
// absent or defers, so this list is a list of overridable OPINIONS, not of
// required configuration.
const (
	// HookAgent answers "which client should this new lane open in?".
	// stdout: the client id. Built-in: the `agent` config key, then SCRUFF_AGENT,
	// then claude.
	HookAgent = "agent"

	// HookLanded answers "has this branch's work reached the default branch?".
	// It is the seam with teeth: a yes here is what lets a branch be DELETED.
	// stdout (optional): {"via": "...", "confidence": "certain"|"heuristic"}.
	// Built-in: SPEC.md §3's ladder — ancestry, merged-PR head OID,
	// patch-equivalence.
	HookLanded = "landed"

	// HookPreserve answers "does this dirty tree need a wip commit before the
	// checkout goes away?". Built-in: yes, unless the only changes are
	// untracked files on an already-landed branch (build scratch).
	HookPreserve = "preserve"

	// HookResume is the action seam behind `scruff <name>`: the checkout has been
	// rebuilt and the session needs reopening. A machine that runs its agents
	// in a multiplexer wants a new pane cd'd into the lane here, not an
	// agent exec'd into scruff's own process. Built-in: chdir + exec the client's
	// resume command.
	HookResume = "resume"

	// HookOpen is HookResume's counterpart for a lane that was just
	// created and has no session yet. Built-in: chdir + exec the client.
	HookOpen = "open"

	// HookFocus is the action seam behind `scruff focus <name>`: the lane is
	// already running somewhere and the user wants to be looking at it. A
	// machine that opens a window per lane raises the one it already has here
	// — the join from a lane to a window is its own, and scruff has no business
	// knowing it. Defer means "no window of mine holds that lane", and scruff
	// falls back to resume, which opens one. Built-in: resume.
	HookFocus = "focus"
)

// Answer is what a hook said.
type Answer int

const (
	// Defer means the hook had no opinion, or there was no hook. scruff runs its
	// built-in. Every failure mode resolves here, so a broken hook costs you
	// the override and never the operation.
	Defer Answer = iota
	// Yes is true, for a predicate, and "handled — do nothing further", for an
	// action.
	Yes
	// No is false, for a predicate, and "I refused or failed", for an action.
	No
)

// Exit codes a hook may return. 0/1/2 mean what they mean in scruff's own
// exit-code table (SPEC.md §2.4), so a hook and a wrapper script speak the same
// language; 3 is the one addition, and it is deliberately NOT 0, 1 or 2 so that
// the ways a script dies by accident — 1 from `set -e`, 126 from a lost +x bit,
// 127 from a typo'd command — can never be mistaken for an opinion.
const (
	exitYes     = 0 // yes / handled
	exitNo      = 1 // no / refused or failed
	exitRefused = 2 // no, declined for safety — same meaning as scruff's own 2
	exitDefer   = 3 // no opinion: run the built-in
)

// Result is one hook invocation.
type Result struct {
	Answer Answer
	// Refused distinguishes exit 2 from exit 1 so a caller can propagate the
	// safety refusal rather than flattening it into a generic failure.
	Refused bool
	// Data is the JSON object the hook printed on stdout, when it printed one.
	// It is how a predicate enriches a bare yes/no — a `landed` hook saying
	// which rule fired, so the answer stays attributable in `--json`.
	Data map[string]any
	// Warning is set when the hook misbehaved: it could not be exec'd, or it
	// exited with a code that means nothing. scruff defers in both cases and
	// surfaces this as a warnings[] entry, because a policy override that
	// silently stopped applying is how you end up trusting the wrong answer.
	Warning string
}

// Defined reports whether a hook is configured for this seam.
func (c *Config) Defined(hook string) bool {
	if c == nil {
		return false
	}
	return len(c.Hooks[hook]) > 0
}

// Ask runs a PREDICATE hook: stdin gets the payload as JSON, stdout is
// captured, stderr goes to the user.
//
// Predicates must be silent and fast. Everything a predicate hook prints on
// stdout is parsed as its answer, so it is the wrong place for a progress line
// — that is what stderr is for, and it is passed through untouched.
func (c *Config) Ask(hook string, payload map[string]string) Result {
	return c.run(hook, payload, false)
}

// Do runs an ACTION hook: it inherits the terminal, because it is replacing
// something scruff would have exec'd and may well be interactive.
//
// Its stdout is redirected to STDERR rather than inherited. Both are the same
// terminal for an interactive hook, so a TUI still draws; but scruff's stdout
// carries data under a contract other programs parse (`cd "$(scruff child …)"`,
// Claude Code's create hook), and a hook must not be able to break that.
//
// The hook runs as a child rather than replacing scruff via exec, because scruff
// has to see the exit code to know whether the hook handled the work or
// deferred. scruff exits as soon as the child does, so a pane whose lifetime is
// the session's still ends when the session does — with one cheap extra process
// in the middle for its duration.
func (c *Config) Do(hook string, payload map[string]string) Result {
	return c.run(hook, payload, true)
}

func (c *Config) run(hook string, payload map[string]string, action bool) Result {
	if !c.Defined(hook) {
		return Result{Answer: Defer}
	}
	argv := c.Hooks[hook]

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), hookEnv(hook, payload)...)
	cmd.Stderr = os.Stderr

	var stdout strings.Builder
	if action {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stderr
	} else {
		body, _ := json.Marshal(payload)
		cmd.Stdin = strings.NewReader(string(body) + "\n")
		cmd.Stdout = &stdout
	}

	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if !asExitError(err, &ee) {
			// Not launched at all: a path that moved out from under a
			// generated config, a lost +x bit, a hook pointing at a store path
			// from a rebuild ago. The operation continues on the built-in.
			return Result{Answer: Defer, Warning: fmt.Sprintf(
				"the %s hook (%s) wouldn't run (%v) — scruff used its own %s rule instead",
				hook, argv[0], err, hook)}
		}
		switch ee.ExitCode() {
		case exitNo:
			return Result{Answer: No, Data: decode(stdout.String())}
		case exitRefused:
			return Result{Answer: No, Refused: true, Data: decode(stdout.String())}
		case exitDefer:
			return Result{Answer: Defer}
		default:
			return Result{Answer: Defer, Warning: fmt.Sprintf(
				"the %s hook (%s) exited %d, which means nothing to scruff — scruff used its own %s rule instead (0 yes, 1 no, 2 refused, 3 no opinion)",
				hook, argv[0], ee.ExitCode(), hook)}
		}
	}
	return Result{Answer: Yes, Data: decode(stdout.String())}
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// decode reads the optional JSON object a predicate printed. A hook that prints
// nothing, or prints prose, has still answered with its exit code — the parse
// failing is not an error, it just means there was nothing to enrich with.
func decode(s string) map[string]any {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// hookEnv is the payload again, as SCRUFF_* variables, so a hook can be three
// lines of shell instead of a program with a JSON parser. Same data as stdin,
// same names as the adapter template variables (SPEC.md §5.2).
//
// Three collisions to know about, all the same shape: scruff's own environment
// got to the name first, so a field named after it would hand scruff back its own
// input. SCRUFF_BASE is the lane base DIRECTORY, so the repo's default branch is
// SCRUFF_BASE_BRANCH. SCRUFF_STATE is the state DIRECTORY and SCRUFF_AGENT is the
// one-invocation default-client override (both env.go), so the lane's fields
// are SCRUFF_LANE_STATE and SCRUFF_LANE_AGENT.
//
// This is not hypothetical tidiness. A hook that spawns a pane leaks this whole
// environment into the shell it starts, and into every window opened from it —
// so a bare run inside an agent pane once resolved its
// machine-global state to the relative path "live" under the cwd (a git
// checkout, routinely), and read the lane's own client as an override sitting
// ABOVE the operator's config key. A hook that wants the client scruff was about
// to run has SCRUFF_COMMAND, which is the resolved invocation rather than an id.
//
// Renaming any of the three would break something that already exists.
//
// The three collisions above apply identically to the prefix, and the test
// asserts SCRUFF_STATE and SCRUFF_AGENT stay absent for exactly that reason.
func hookEnv(hook string, payload map[string]string) []string {
	env := []string{"SCRUFF_HOOK=" + hook}
	for k, v := range payload {
		suffix := strings.ToUpper(k)
		switch k {
		case "base":
			suffix = "BASE_BRANCH"
		case "state":
			suffix = "LANE_STATE"
		case "agent":
			suffix = "LANE_AGENT"
		}
		env = append(env, "SCRUFF_"+suffix+"="+v)
	}
	return env
}

// ── loading ──────────────────────────────────────────────────────────────────

// Dir is the config directory: $XDG_CONFIG_HOME/scruff, or ~/.config/scruff.
//
// ~/.config on every platform, macOS included, rather than os.UserConfigDir's
// Application Support — this is a terminal tool and its config lives where the
// rest of a terminal user's config lives.
func Dir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "scruff")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "scruff")
}

// Load reads the machine config. A missing file is not an error — it is the
// overwhelmingly common case, and it means "every default".
//
// The parser is a deliberate subset of TOML rather than a dependency: top-level
// `key = "string"`, a `[hooks]` table, and values that are either a string or
// an array of strings. scruff stays dependency-free through 0.1 (go.mod says so),
// and this file has no syntax worth a parser beyond that. A line it cannot
// understand is SKIPPED with a warning rather than fatal: a config typo must
// not be able to stop a pane from opening.
func Load() (*Config, []string) {
	cfg := &Config{Hooks: map[string][]string{}}
	dir := Dir()
	if dir == "" {
		return cfg, nil
	}
	path := filepath.Join(dir, "config.toml")
	f, err := os.Open(path)
	if err != nil {
		return cfg, nil
	}
	defer f.Close()
	cfg.Path = path

	var warnings []string
	section := ""
	scan := bufio.NewScanner(f)
	for line := 1; scan.Scan(); line++ {
		text := strings.TrimSpace(stripComment(scan.Text()))
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
			section = strings.Trim(text, "[] \t")
			continue
		}
		key, raw, ok := strings.Cut(text, "=")
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s:%d isn't `key = value` — ignored", path, line))
			continue
		}
		key = strings.TrimSpace(key)
		// name_max is read before parseValue because it is the one key whose
		// natural TOML spelling is a bare integer, which parseValue — strings
		// and lists of strings, deliberately — would reject as unreadable.
		if section == "" && key == "name_max" {
			text := strings.Trim(strings.TrimSpace(raw), `"'`)
			n, err := strconv.Atoi(text)
			if err != nil || n < 0 {
				warnings = append(warnings, fmt.Sprintf("%s:%d — `name_max` wants a byte count like 46, not %q, so lane names are uncapped", path, line, text))
				continue
			}
			cfg.NameMax = n
			continue
		}
		argv, ok := parseValue(strings.TrimSpace(raw))
		if !ok || len(argv) == 0 {
			warnings = append(warnings, fmt.Sprintf("%s:%d — couldn't read a string or a list of strings from %q, so `%s` is unset", path, line, strings.TrimSpace(raw), key))
			continue
		}
		switch section {
		case "":
			switch key {
			case "agent":
				cfg.Agent = argv[0]
			case "namer":
				cfg.Namer = argv[0]
			}
		case "hooks":
			cfg.Hooks[key] = argv
		}
	}
	return cfg, warnings
}

// stripComment drops a trailing `# …`, respecting quotes so a path or flag
// containing a hash survives.
func stripComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#':
			return line[:i]
		}
	}
	return line
}

// parseValue reads either `"a string"` or `["an", "argv", "list"]` into argv
// form, so a hook can be written as a bare program path when it takes no
// arguments and as a list when it does.
func parseValue(raw string) ([]string, bool) {
	if !strings.HasPrefix(raw, "[") {
		s, ok := unquote(raw)
		if !ok {
			return nil, false
		}
		return []string{s}, true
	}
	if !strings.HasSuffix(raw, "]") {
		return nil, false // a multi-line array; not supported, and said so
	}
	inner := strings.TrimSpace(raw[1 : len(raw)-1])
	if inner == "" {
		return nil, true // an explicitly empty hook: defined as "no override"
	}
	var out []string
	for _, part := range splitTop(inner) {
		s, ok := unquote(strings.TrimSpace(part))
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// splitTop splits on commas that are not inside quotes.
func splitTop(s string) []string {
	var parts []string
	var quote rune
	start := 0
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ',':
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func unquote(s string) (string, bool) {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1], true
	}
	return "", false
}
