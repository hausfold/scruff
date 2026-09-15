package commands

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hausfold/scruff/internal/exitcode"
	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/registry"
	"github.com/hausfold/scruff/internal/ui"
)

// This is the ONE client-specific seam in scruff, and it is deliberately narrow.
// Every lane records its client in the registry, so changing the machine's
// default later never makes a parked Codex branch reopen in Claude.
//
// In 0.2 this whole file collapses into adapter TOML (SPEC.md §5.3) — which is
// why the per-client knowledge is concentrated in three small switches rather
// than spread through the commands that call them.

// ── the prompt is DATA, and every client's parser has to be told so ──────────
//
// A task typed into Pounce's Spawn Agent box is very often a markdown list, so
// its first character is `-`. Handed to a client as a bare argv element, that is
// a FLAG: `claude "- update the README"` dies with `error: unknown option '-
// update the README'` before the pane has drawn anything, and the same is true
// of any prompt starting with a dash. So every client's argv here ends its
// option parsing before the prompt — `--` for the three positional-prompt
// clients (commander, clap and pi's own parser all honour it), and
// `--prompt=<text>` for opencode, whose yargs would read a dashed VALUE after a
// separate `--prompt` as another flag.
// Never go back to appending the prompt bare.

// agentSpec is what scruff needs to know about a client. The 0.2 adapter loader
// produces exactly this struct from a TOML file.
type agentSpec struct {
	id    string
	start func(image, prompt string) []string
	open  []string
	// resume opens the client's session PICKER, filtered to the cwd. It is the
	// right answer only when scruff genuinely cannot tell which conversation is
	// meant — a lane whose chat lives in a shared parent checkout.
	resume []string
	// last continues the newest conversation in the cwd, with no picker. A
	// lane's own checkout is a directory only that lane's agent ever ran in, so
	// "the newest conversation here" IS the lane's chat — asking which one is a
	// question with one answer, and answering it for the user is the point of
	// `scruff <name>`. Empty means the client has no such mode and the picker
	// stands.
	last []string
	// imageFlag reports whether the client can attach a local image itself.
	// The ones that can't are TOLD about the file in their first turn, rather
	// than pretending an unsupported flag attached it.
	imageFlag bool
}

func specFor(id string) (agentSpec, bool) {
	switch id {
	case "claude":
		return agentSpec{
			id:     "claude",
			start:  func(_, prompt string) []string { return []string{"claude", "--", prompt} },
			open:   []string{"claude"},
			resume: []string{"claude", "--resume"},
			last:   []string{"claude", "--continue"},
		}, true
	case "codex":
		return agentSpec{
			id: "codex",
			start: func(image, prompt string) []string {
				if image != "" {
					return []string{"codex", "-i", image, "--", prompt}
				}
				return []string{"codex", "--", prompt}
			},
			open:      []string{"codex"},
			resume:    []string{"codex", "resume"},
			last:      []string{"codex", "resume", "--last"},
			imageFlag: true,
		}, true
	case "opencode":
		return agentSpec{
			id:    "opencode",
			start: func(_, prompt string) []string { return []string{"opencode", "--prompt=" + prompt} },
			open:  []string{"opencode"},
			// opencode's `--continue` is already continue-the-last-session; it
			// has no separate picker flag (its TUI lists sessions in-app), so
			// both rungs are the same command.
			resume: []string{"opencode", "--continue"},
			last:   []string{"opencode", "--continue"},
		}, true
	case "pi":
		return agentSpec{
			id: "pi",
			// pi attaches a local file by naming it `@path` in the message
			// itself rather than through a flag, and its usage line is
			// `pi [options] [--] [@files...] [messages...]` — so the attachment
			// goes AFTER the `--` and before the prompt, in that order.
			start: func(image, prompt string) []string {
				if image != "" {
					return []string{"pi", "--", "@" + image, prompt}
				}
				return []string{"pi", "--", prompt}
			},
			open: []string{"pi"},
			// `pi -r` opens the session picker for the current project, and
			// `pi -c` continues the newest session there — the same two rungs
			// codex has, spelled shorter.
			resume:    []string{"pi", "--resume"},
			last:      []string{"pi", "--continue"},
			imageFlag: true,
		}, true
	}
	return agentSpec{}, false
}

// resumeArgv picks between continuing the newest conversation and opening the
// picker.
//
// `own` says the lane's chat lives in the lane's OWN checkout. `pick` is the
// user overriding from the command line, for the case scruff's rule gets wrong:
// a lane whose newest conversation is not the one wanted (a throwaway session
// started in the same checkout, or a deliberate second thread).
func resumeArgv(spec agentSpec, own, pick bool) []string {
	if own && !pick && len(spec.last) > 0 {
		return spec.last
	}
	return spec.resume
}

func resolveAgent(id string) (agentSpec, error) {
	spec, ok := specFor(id)
	if !ok {
		return spec, exitcode.Usagef("unknown agent %q (expected claude, codex, opencode, or pi)", id)
	}
	if _, err := exec.LookPath(id); err != nil {
		return spec, exitcode.Usagef("%s is unavailable — install it, then try again", id)
	}
	return spec, nil
}

// execClient replaces this process with the client.
//
// A real exec, not a child: scruff IS the pane's process, so closing the client
// closes the pane — and under haus's binds that fires the same remove hook
// Claude's own exit does. A child process would leave scruff sitting in the middle,
// and the pane would outlive the client.
func execClient(argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return exitcode.Usagef("%s is unavailable — install it, then try again", argv[0])
	}
	return syscall.Exec(path, argv, os.Environ())
}

// shellOf is the shell `scruff new --cmd` runs a command string through: the
// user's own $SHELL when they have one, /bin/sh otherwise. Their shell, because
// the command was typed by them and may lean on their aliases and functions.
func shellOf() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// AgentCmd is the public client seam: `scruff agent <default|start|open|resume> …`.
func (e *Env) AgentCmd(args []string) error {
	switch argAt(args, 0) {
	case "default":
		ui.Out("%s\n", e.Agent)
		return nil
	case "start":
		return e.agentStart(args[1:])
	case "open":
		spec, err := resolveAgent(orDefault(argAt(args, 1), e.Agent))
		if err != nil {
			return err
		}
		return execClient(spec.open)
	case "resume":
		spec, err := resolveAgent(orDefault(argAt(args, 1), e.Agent))
		if err != nil {
			return err
		}
		return execClient(spec.resume)
	default:
		return exitcode.Usagef("usage: scruff agent <default|start|open|resume> …")
	}
}

// agentStart parses `[<agent>] [--image FILE] -- <prompt>` and execs the client.
func (e *Env) agentStart(args []string) error {
	id := e.Agent
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && args[0] != "--" {
		id, args = args[0], args[1:]
	}
	var image string
	if len(args) >= 2 && args[0] == "--image" {
		image, args = args[1], args[2:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	prompt := strings.Join(args, " ")

	spec, err := resolveAgent(id)
	if err != nil {
		return err
	}
	return execClient(startArgv(spec, image, prompt))
}

// startArgv is the one place a first-turn prompt becomes a client invocation.
//
// Shared by `scruff agent start` and by the `--prompt` endings of `new` and
// `spawn`, because all three are the same act — a lane whose session opens
// already knowing the task — and a second copy of the image rule would be a
// second copy to get wrong.
func startArgv(spec agentSpec, image, prompt string) []string {
	if image != "" {
		if _, err := os.Stat(image); err != nil {
			image = ""
		}
	}
	// A client with no image flag is told where the file is, in words. Silently
	// dropping it would leave the agent reasoning about a screenshot it was
	// never given.
	if image != "" && !spec.imageFlag {
		prompt += "\n\nA screenshot for this task is at " + image +
			". Inspect it before drawing conclusions."
		image = ""
	}
	return spec.start(image, prompt)
}

// ── inheriting the parent repo's workspace trust ─────────────────────────────

// trustWorktree stops a freshly-made worktree greeting its first Claude session
// with "Do you trust the files in this folder?".
//
// Claude Code keys workspace trust on the EXACT cwd, in `~/.claude.json` under
// `projects["<abs path>"].hasTrustDialogAccepted`. There is no inheritance from a
// parent directory and none from the git common dir — so a checkout scruff just
// made is, correctly, a directory Claude has never seen. Claude's own
// `--worktree` doesn't prompt because it seeds that key for the worktree it
// creates; every checkout scruff makes instead (the palette's `scruff spawn`,
// `scruff new` on a claude machine, `scruff child`) got the dialog. Same worktree,
// same repo, different answer depending on who ran `git worktree add` — which
// reads as a bug in the spawn, because it is one.
//
// Deliberately narrow, in three ways:
//
//   - It only ever COPIES a decision the user already made. If the parent repo
//     isn't trusted, this is a no-op — scruff never grants trust on the user's
//     behalf, it propagates it to a checkout of the same code.
//   - Every failure is silent and harmless. A missing/unreadable/unparseable
//     `~/.claude.json` costs one trust prompt, which is exactly the status quo;
//     nothing here is worth failing a spawn over.
//   - It is a no-op for every client with no such prompt. Codex and OpenCode
//     have none, and scruff must not invent one. pi does, and gets its own
//     propagation below — a different file, a different shape, the same rule.
//
// The write is read-modify-write on a file Claude Code also owns and rewrites
// wholesale, with no lock either side — so a Claude instance writing in the same
// instant can drop this key. The blast radius is one trust prompt, because
// everything else in that file is Claude's own telemetry which it is in the
// middle of rewriting anyway. Marshalling through a map also reorders the file's
// keys once (Go maps have no order); numbers are decoded as json.Number so the
// re-encode can't turn `1778838900185` into `1.778838900185e+12`.
func trustWorktree(agentID, main, dir string) {
	switch agentID {
	case "claude":
		trustWorktreeClaude(main, dir)
	case "pi":
		trustWorktreePi(main, dir)
	}
}

func trustWorktreeClaude(main, dir string) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	path := filepath.Join(home, ".claude.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return
	}
	projects, _ := doc["projects"].(map[string]any)
	if projects == nil {
		return
	}
	parent, _ := projects[main].(map[string]any)
	if trusted, _ := parent["hasTrustDialogAccepted"].(bool); !trusted {
		return
	}
	entry, _ := projects[dir].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	if already, _ := entry["hasTrustDialogAccepted"].(bool); already {
		return // nothing to write; don't churn a 180KB file for nothing
	}
	entry["hasTrustDialogAccepted"] = true
	projects[dir] = entry

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // a path or prompt with < > & stays readable
	enc.SetIndent("", "  ")  // what Claude Code itself writes
	if err := enc.Encode(doc); err != nil {
		return
	}
	// Temp file in the same directory + rename, so a crash mid-write can never
	// leave Claude with a truncated config. 0600 because this file holds
	// credentials, and CreateTemp's own 0600 is what we keep.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.scruff-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), path)
}

// trustWorktreePi is the same favour for pi, whose trust model is shaped
// differently in the one way that matters here: pi's `~/.pi/agent/trust.json`
// is a flat path → bool map and it DOES inherit from a parent folder, so one
// `{"/Users/you/code": true}` covers every repo underneath it. (Absolute — pi
// writes the resolved path and nothing here expands `~`.) That inheritance is
// also exactly why a lane still prompts: scruff's checkouts live at
// `~/.cache/scruff/<repo>/<name>`, outside whatever tree the user trusted, so
// no ancestor of the new directory has a decision saved.
//
// So the lookup walks the MAIN checkout's ancestors for the nearest saved
// decision — the same question pi itself would ask of the main checkout — and
// copies it onto the worktree only when the answer is yes. A saved `false`
// nearer the repo than a `true` further up means the user said no, and no is
// propagated by writing nothing at all: the lane prompts, which is what an
// untrusted repo should do.
//
// Same three narrowings as the Claude path. The blast radius is NOT quite the
// same, and the difference is worth naming: this is read-modify-write with no
// lock on a file pi also owns, so a `/trust` that flips the repo to `false`
// between the read and the rename gets the `true` written back over it. On the
// Claude side losing that race costs one extra prompt; here it costs a trust
// the user had just revoked — for one spawn, on one worktree path, and only
// while those two writes interleave. Re-encoded whole (the file is small and
// flat), preserving the mode pi left on it rather than tightening to 0600:
// `~/.claude.json` earns that mode by holding credentials and this does not.
func trustWorktreePi(main, dir string) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	path := filepath.Join(home, ".pi", "agent", "trust.json")
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc map[string]bool
	if err := json.Unmarshal(raw, &doc); err != nil {
		return
	}
	if !piTrusted(doc, main) {
		return
	}
	if doc[dir] {
		return // already there; don't rewrite the file for nothing
	}
	doc[dir] = true

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".trust.json.scruff-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	// CreateTemp makes 0600 and the rename would carry it, silently tightening
	// a file scruff does not own. Put pi's own mode back first.
	_ = os.Chmod(tmp.Name(), info.Mode().Perm())
	_ = os.Rename(tmp.Name(), path)
}

// piTrusted answers pi's own question for a path: the NEAREST saved decision on
// that folder or an ancestor wins, so a `false` on the repo beats a `true` on
// the directory above it. Walking stops at the filesystem root, which
// filepath.Dir reports by returning its argument unchanged.
func piTrusted(doc map[string]bool, path string) bool {
	for p := filepath.Clean(path); ; {
		if v, ok := doc[p]; ok {
			return v
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// ── where a lane's conversation lives ────────────────────────────────────────

// projStore is Claude Code's transcript store: one directory per cwd.
func projStore() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude", "projects")
}

// projEnc is how Claude Code names a cwd's transcript directory: every '/' and
// '.' becomes '-'.
//
// Lossy, and that matters below — `a.b` and `a-b` land on the same name, so
// nothing here ever decodes one back into a path. A directory found by its name
// is only ever a CANDIDATE; the cwd it recorded inside is what confirms it.
func projEnc(cwd string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '.' {
			return '-'
		}
		return r
	}, cwd)
}

// projDir is Claude Code's transcript directory for a cwd.
func projDir(cwd string) string { return filepath.Join(projStore(), projEnc(cwd)) }

// agentHasChat answers only when it is knowable.
//
// Clients own their transcript stores, and only Claude exposes a cheap
// cwd → transcript-directory test. Codex and OpenCode keep private session
// indexes, so their cwd-filtered pickers are the authority and scruff must not
// guess on their behalf — "unknown" is the honest answer, and the caller
// degrades to opening the picker.
func agentHasChat(agent, cwd string) bool {
	if !agentProbeable(agent) {
		return false
	}
	fi, err := os.Stat(projDir(cwd))
	return err == nil && fi.IsDir()
}

// chatHome is the cwd whose client picker should be opened for a lane.
//
// A SPAWNED lane never hosts an independent conversation: its chat lives in the
// pane that made it. Two signatures for that, both requiring the parent to be a
// genuinely different context than this lane's own repo:
//
//  1. the parent is itself a lane — a nested spawn;
//  2. the parent is a checkout of a DIFFERENT repo — a `scruff child`, e.g. a
//     workshop pane that spawned this sub-repo lane.
//
// A plain same-repo lane's parent is its OWN main checkout, whose transcripts
// are the user's unrelated on-main work. Never hijack resume to that — it falls
// through and the lane keeps its own chat.
func (e *Env) chatHome(agent, wt string) string {
	if agentHasChat(agent, wt) {
		return wt
	}
	row, ok := e.Reg.Find(wt)
	if !ok || row.Parent == "" {
		return wt
	}
	// A plain lane's parent IS its own main checkout — neither signature can
	// hold, and the cross-repo test below would spend two git invocations
	// proving it. Answered here because the listing asks this of every lane,
	// and for a client with no cheap transcript probe that is every lane.
	if row.Parent == row.Main {
		return wt
	}
	usable := func(parent string) bool {
		return agent != "claude" || agentHasChat(agent, parent)
	}
	if strings.HasPrefix(row.Parent, e.Base+string(filepath.Separator)) && usable(row.Parent) {
		return row.Parent
	}
	// Cross-repo? Compare the two checkouts' git common dirs — both resolved by
	// git, so symlink-consistent. A raw string compare against the stored path
	// breaks on macOS's /var → /private/var.
	pcommon, perr := gitx.Run(row.Parent, "rev-parse", "--path-format=absolute", "--git-common-dir")
	mcommon, _ := gitx.Run(row.Main, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if perr == nil && pcommon != "" && pcommon != mcommon && usable(row.Parent) {
		return row.Parent
	}
	return wt
}

// jsonChat is the `chat` field of `--json`: the checkout whose conversation
// `scruff <name>` will open, and "" when scruff cannot actually tell.
//
// It is NOT chatHome, and the difference is the whole point. chatHome must
// always name a directory — resume has to open something — so for a client
// whose transcripts scruff cannot probe it FALLS BACK to the parent, which is
// the better guess when you are about to exec a picker. A consumer reading
// `chat` is asking the opposite question ("does this lane have a pane of its
// own, or is it just a checkout somebody's pane edits?"), and there that
// fallback is a lie: every codex/opencode lane spawned from another lane's
// pane would answer "no chat of my own" and vanish from a picker that filtered
// on it, window and all.
//
// So the guess is not published. "" means undetermined, consumers must read it
// as "show it", and the field is only ever load-bearing for a client scruff can
// genuinely probe.
func (e *Env) jsonChat(agent, wt string) string {
	if !agentProbeable(agent) {
		return ""
	}
	return e.chatHome(agent, wt)
}

// agentProbeable reports whether "does this cwd have a conversation in it?" is
// a question scruff can answer for this client at all. Only Claude exposes a
// cheap cwd → transcript-directory mapping; the others keep private session
// indexes and their own cwd-filtered pickers are the authority (§5.3).
func agentProbeable(agent string) bool { return agent == "claude" }

// ── a conversation the checkout moved out from under ─────────────────────────

// Claude Code keys its transcripts on the EXACT cwd, and scruff moves a lane's
// checkout on its own — so a lane can end up standing somewhere the client has
// never seen, with a thousand messages intact one directory away and no way to
// reach them: `claude --continue` says "No conversation found to continue" and
// exits 1 into an empty pane (issue #129).
//
// Two things move a checkout. `doctor --migrate-base` moves the whole base and
// carries the transcripts with it, because it knows exactly which path became
// which. The other is this one: a lane whose registry row was lost is
// rediscovered as an orphan branch, and the path discover SYNTHESISES for it is
// today's bucket convention — which is not where the checkout was when its
// agent last ran. The bucket has been three things (the spawning pane's
// directory, the main checkout's basename, and SPEC.md §4's `<owner>-<repo>`
// slug), so a lane old enough has outlived its own path and nobody recorded the
// move. The old path has to be found.

// strandedChat is the transcript of a lane that used to live somewhere else
// under the same base: the directory holding it, and the path it was recorded
// at. Both are "" unless scruff is sure, and five things have to hold:
//
//  1. Same base, same lane NAME, different bucket. The name is what a lane
//     keeps across a bucket change, and the bucket is the only thing that
//     changed.
//  2. The candidate's own recorded cwd says so — read out of the transcript,
//     never decoded from the directory name, which cannot be decoded.
//  3. The branch it recorded is this lane's branch, where it recorded one.
//  4. Nothing else answers to that old path: no directory on disk, no registry
//     row. `scruff child` gives a child lane its parent's NAME on purpose, so
//     two lanes of one name in different repos is ordinary — and stealing a
//     live pane's conversation would be far worse than the empty lane this
//     fixes.
//  5. Exactly one candidate survives. Two is a question only the user can
//     answer, and `scruff doctor` is where it gets asked.
func strandedChat(store, base, path, branch string, inUse func(string) bool) (dir, cwd string) {
	base, path = filepath.Clean(base), filepath.Clean(path)
	if store == "" || base == "" || filepath.Dir(filepath.Dir(path)) != base {
		return "", "" // not a `$BASE/<bucket>/<name>` lane; nothing to reason about
	}
	name := filepath.Base(path)
	prefix, suffix, own := projEnc(base)+"-", "-"+projEnc(name), projEnc(path)
	ents, err := os.ReadDir(store)
	if err != nil {
		return "", ""
	}
	for _, ent := range ents {
		n := ent.Name()
		if !ent.IsDir() || n == own || !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, suffix) {
			continue
		}
		was, wasBranch := transcriptOrigin(filepath.Join(store, n))
		if was = filepath.Clean(was); was == "." || was == path {
			continue
		}
		if filepath.Dir(filepath.Dir(was)) != base || filepath.Base(was) != name {
			continue // the name matched by accident: '.' and '/' encode alike
		}
		if wasBranch != "" && wasBranch != branch {
			continue
		}
		if _, err := os.Stat(was); err == nil {
			continue // a checkout is standing there — that conversation is its own
		}
		if inUse != nil && inUse(was) {
			continue // parked, but another lane's row still names it
		}
		if dir != "" {
			return "", ""
		}
		dir, cwd = filepath.Join(store, n), was
	}
	return dir, cwd
}

// transcriptOrigin is the cwd and branch a transcript RECORDS, from its newest
// session file.
//
// Claude writes one JSON object per line and the first few are session
// metadata with no cwd on them, so this reads a bounded prefix rather than the
// first line — and stops the moment it has both. Decoded off the stream rather
// than scanned by line because a single line routinely runs past any sane line
// buffer (a tool result is one object).
func transcriptOrigin(dir string) (cwd, branch string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}
	var newest string
	var stamp time.Time
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(stamp) {
			newest, stamp = filepath.Join(dir, ent.Name()), info.ModTime()
		}
	}
	if newest == "" {
		return "", ""
	}
	f, err := os.Open(newest)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	for i := 0; i < 64; i++ {
		var rec struct {
			Cwd       string `json:"cwd"`
			GitBranch string `json:"gitBranch"`
		}
		if err := dec.Decode(&rec); err != nil {
			break
		}
		if cwd == "" {
			cwd = rec.Cwd
		}
		if branch == "" {
			branch = rec.GitBranch
		}
		if cwd != "" && branch != "" {
			break
		}
	}
	return cwd, branch
}

// adoptChat puts a stranded conversation at the path the lane lives at now.
//
// A COPY, never a move, and never over a conversation that is already there.
// The match above is strong evidence, not proof, so the failure direction is
// invariant 1's: if it is wrong, nothing was lost and the line resume prints
// names the directory it came from, which is one `rm -rf` to undo. Staged in a
// sibling directory and renamed into place, because a half-copied transcript at
// the real name is one `agentHasChat` reads as a conversation.
func adoptChat(from, to string) error {
	if _, err := os.Stat(to); err == nil {
		return fmt.Errorf("%s already holds a conversation", to)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(to), ".scruff-adopting-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging) // a no-op once the rename below has emptied it
	tmp := filepath.Join(staging, filepath.Base(to))
	if err := copyTree(from, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, to)
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, 0o755)
		case !d.Type().IsRegular():
			return nil // a transcript is plain files; a link is not ours to follow
		default:
			return copyFile(p, dst)
		}
	})
}

func copyFile(from, to string) error {
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// strandedChatOf is strandedChat for one discovered lane: a detector, with no
// side effects, so `scruff doctor` can report what it would take without
// taking it.
func (e *Env) strandedChatOf(agent string, entry Entry) (dir, was string) {
	if !agentProbeable(agent) {
		return "", ""
	}
	rows, _ := e.Reg.Load()
	claimed := make(map[string]bool, len(rows))
	for _, row := range rows {
		// This lane's OWN row is skipped: when the checkout moved, the row is
		// routinely the thing still naming the old path (discover corrects the
		// entry against git, the row keeps its guess until resume rewrites it).
		// Counting it would make every real case look like somebody else's.
		if row.Main == entry.Main && row.Branch == entry.Branch {
			continue
		}
		claimed[filepath.Clean(row.Path)] = true
	}
	return strandedChat(projStore(), e.Base, entry.Path, entry.Branch, func(p string) bool { return claimed[p] })
}

// recoverChat brings a lane's stranded conversation to the path the lane lives
// at now, and reports where it came from. "" is "there was nothing to do",
// which is the ordinary answer.
func (e *Env) recoverChat(agent string, entry Entry) string {
	dir, was := e.strandedChatOf(agent, entry)
	if dir == "" {
		return ""
	}
	if err := adoptChat(dir, projDir(entry.Path)); err != nil {
		ui.Warn("this lane's conversation is at %s and could not be copied here (%v) — `cp -R %s %s` does it by hand",
			was, err, dir, projDir(entry.Path))
		return ""
	}
	return was
}

// agentForPath is the recorded client for a lane, resolved BEFORE a parked
// checkout is re-registered: a five-column registry row predates the client
// field and is therefore Claude forever, even if the machine's default changed.
func (e *Env) agentForPath(path string) string {
	if row, ok := e.Reg.Find(path); ok && registry.KnownAgent(row.Agent) {
		return row.Agent
	}
	return e.Agent
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
