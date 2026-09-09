package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hausfold/scruff/internal/config"
	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/ui"
)

// Naming a lane after its task, for the one case where scruff has a task to read:
// a lane opened on a first-turn prompt (`--prompt`/`--prompt-file`) with no name
// given. `mobile-nav-jitter` is worth more in a listing than `cozy-otter`, and
// the brief is right there.
//
// Three things keep this from becoming something scruff shouldn't be:
//
//   - It is OFF unless `namer = "<id>"` is in the config. No key, no processes,
//     no behaviour change from before it existed. A lane that was going to be
//     called `cozy-otter` still is.
//   - scruff does not talk to a model. It runs ONE argv from an adapter file
//     (SPEC.md §5.6) and reads a word off stdout — so there is no HTTP client
//     here, no vendor, and no API key for scruff to hold. The built-in runs the
//     `claude` binary that a machine spawning agents already has, which means
//     the naming call is authenticated the same way the agents are. Pointing it
//     at a local model, or at a script with no model at all, is a file.
//   - It is COSMETIC and it cannot fail the lane. Every failure — no adapter,
//     a namer that isn't installed, a timeout, prose instead of a name — is a
//     warning and a fall back to randomName(). A lane scruff could not name is
//     still a lane; refusing to create it would trade invariant 1 for a nicety.
//
// The output is UNTRUSTED text on its way to becoming a branch name and a
// filesystem path, and slugFrom is where that is dealt with: nothing reaches
// `git worktree add` that isn't one to three plain [a-z0-9-] words.

const (
	// namerTimeout bounds the one call. Measured on a warm machine, the
	// built-in returns in 8-12s (most of it the client's own start-up, not the
	// model), so this is generous on purpose: a slow answer that still arrives
	// beats a random name, and the ceiling only has to stop a hang.
	namerTimeout = 30 * time.Second
	// namerMaxPrompt is how much of the brief the namer is shown. A handoff can
	// be pages; the objective is always near the top, and the rest is cost.
	namerMaxPrompt = 2000
	// namerMaxWords/namerMaxLen are the shape of a name. Three words beat two:
	// two spends one slot on a near-free verb ("fix", "add") and leaves one for
	// the subject, which is how you get `fix-mobile`. The third word is where
	// `mobile-nav-jitter` becomes distinguishable from the other mobile lane.
	namerMaxWords = 3
	namerMaxLen   = 24
)

// nameForNewLane is the name an unnamed lane takes: the namer's answer when
// there is a namer and a task to read, and the throwaway word pair otherwise.
//
// Deliberately not `laneNameFor` — that one (notify.go) answers the opposite
// question, "which existing lane is this cwd in?". One is a lookup, this is a
// choice, and they are one letter apart in the wrong direction.
func (e *Env) nameForNewLane(main, prompt string) string {
	max := e.nameLenFor(main)
	if name := e.nameFromPrompt(main, prompt, max); name != "" {
		return name
	}
	return fitName(randomName(), max)
}

// nameLenFor is how long a name scruff CHOOSES may be here: its own shape rule,
// tightened by what the machine's lane backend can carry (new.go's
// laneNameBudget) when that is the smaller of the two.
//
// Building the name to fit is better than trimming one afterwards, which is why
// the number goes all the way down to sanitizeName: a name assembled word by
// word stops at `docs-displays-expansion`, while a cut one stops mid-word.
func (e *Env) nameLenFor(main string) int {
	max := namerMaxLen
	if b := e.laneNameBudget(main); b > 0 && b < max {
		max = b
	}
	return max
}

// nameFromPrompt asks the configured namer to name this task. "" means it
// couldn't, for any reason at all, and the caller falls back.
func (e *Env) nameFromPrompt(main, prompt string, max int) string {
	if e.Cfg == nil || e.Cfg.Namer == "" || strings.TrimSpace(prompt) == "" {
		return ""
	}
	adapter, err := config.LoadNamerAdapter(e.Cfg.Namer)
	if err != nil {
		e.Warn(fmt.Sprintf("%v — naming this lane at random instead", err))
		return ""
	}
	slug, _ := gitx.RemoteSlug(main)
	own := repoWords(main, slug)
	argv, err := config.RenderArgv(adapter.Name, config.TemplateVars{
		Main:   main,
		Repo:   slug,
		Base:   gitx.DefaultBranch(main),
		Agent:  e.Agent,
		Prompt: namingRequest(slug, prompt, e.laneNamesIn(main)),
	})
	if err != nil {
		e.Warn(fmt.Sprintf("rendering the %s namer's command: %v — naming this lane at random instead", adapter.ID, err))
		return ""
	}

	ui.Say("naming the lane from the task, via %s …", adapter.ID)
	out, err := runNamer(argv, main)
	if err != nil {
		e.Warn(fmt.Sprintf("the %s namer (%s) %v — naming this lane at random instead", adapter.ID, argv[0], err))
		return ""
	}
	name := slugFrom(out, own, max)
	if name == "" {
		e.Warn(fmt.Sprintf("the %s namer answered with something that isn't a name — naming this lane at random instead", adapter.ID))
		return ""
	}
	return name
}

// runNamer runs the namer's argv and returns its stdout.
//
// Three deliberate choices about the child's file descriptors and lifetime:
//
//   - fd 0 is /dev/null (a nil Stdin), never scruff's own. Inheriting it would
//     let a client that waits on a non-tty stdin add seconds to every spawn,
//     and `scruff new --prompt-file -` has ALREADY drained fd 0 to read the brief
//     — a namer must not be able to touch what the client is about to be given
//     back (see restoreStdin).
//   - stderr is captured rather than inherited: a namer's progress chatter is
//     not the user's business on a path they didn't ask a question on. Its last
//     line comes back attached to the failure, which is when it matters.
//   - WaitDelay bounds the wait for a killed process's grandchildren to let go
//     of the pipes. The clients this runs are node and their subprocesses
//     outlive them; without it a timeout could still hang scruff on the read.
func runNamer(argv []string, cwd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), namerTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Stdin = nil
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("took longer than %s", namerTimeout)
	}
	if err != nil {
		var notFound *exec.Error
		if errors.As(err, &notFound) {
			return "", fmt.Errorf("isn't on PATH")
		}
		return "", fmt.Errorf("failed: %v%s", err, lastLine(errs.String()))
	}
	return out.String(), nil
}

// namingRequest is the whole prompt the namer is given: what to answer, what
// not to say, what is already taken, and the task.
//
// The taken names are in there because scruff's own collision handling is a
// numeric suffix — `freeName` turns a repeat into `fix-mobile-2`, which is
// correct and useless to read. A namer that can see the neighbours picks a
// different third word instead, and that is the whole reason a listing of six
// lanes stays scannable.
func namingRequest(repo, prompt string, taken []string) string {
	var b strings.Builder
	b.WriteString("Name this coding task for a git branch: 2 or 3 lowercase words joined by hyphens.\n\n")
	b.WriteString("Reply with the name and nothing else — no quotes, no explanation, no preamble.\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Name the WORK, not the repo")
	if repo != "" {
		b.WriteString(". The repo is " + repo + " and is already known; never use its name as a word")
	}
	b.WriteString(".\n")
	b.WriteString("- Prefer 3 words when the third one distinguishes this task from a similar one; never more than 3.\n")
	b.WriteString("- Say what changes, not that something changes: never \"fix\", \"update\", \"improve\", \"misc\", \"task\" or \"changes\" as the whole name.\n")
	b.WriteString("- Lowercase letters, digits and hyphens only.\n")
	if len(taken) > 0 {
		b.WriteString("- Already taken in this repo — do not repeat one or come close to one: " + strings.Join(taken, ", ") + ".\n")
	}
	b.WriteString("\nThe task:\n")
	b.WriteString(clip(prompt, namerMaxPrompt))
	b.WriteString("\n")
	return b.String()
}

// slugFrom reads a lane name out of whatever the namer printed.
//
// This is the gate. The text on the other side is a model's output on its way
// to `git worktree add` and a path under the lane base, so nothing here trusts
// it: a candidate is rejected outright unless it is already shaped like a name,
// and what survives is rebuilt from scratch out of [a-z0-9-] rather than
// "cleaned up". Wrong-but-safe (a random name) is the failure direction.
//
// It reads line by line because a namer told to answer with one word sometimes
// answers with one word and a sentence about it. The first line that IS a name
// wins; a preamble line can never become one, because a line with a colon,
// a comma or five words in it is thrown away whole rather than sanitized into
// `here-is-the`.
func slugFrom(out string, own []string, max int) string {
	for i, line := range strings.Split(out, "\n") {
		if i >= 8 {
			break // an answer this far down is prose about an answer
		}
		if name := sanitizeName(line, own, max); name != "" {
			return name
		}
	}
	return ""
}

// sanitizeName turns one candidate line into a lane name, or "" if it isn't one.
// `max` is the longest name this repo can take — namerMaxLen, or less where the
// machine's lane backend says so.
func sanitizeName(line string, own []string, max int) string {
	raw := strings.TrimSpace(line)
	raw = strings.Trim(raw, "`\"'*.")
	if raw == "" {
		return ""
	}
	fields := strings.Fields(raw)
	if len(fields) > namerMaxWords {
		return "" // a sentence, not a name
	}
	var words []string
	for _, f := range fields {
		if !plainWord(f) {
			return "" // punctuation means prose: "Here's the name:" is not a name
		}
		for _, w := range strings.Split(strings.ToLower(f), "-") {
			if w == "" || isOwn(w, own) {
				continue
			}
			words = append(words, w)
		}
	}

	var name string
	for _, w := range words {
		next := w
		if name != "" {
			next = name + "-" + w
		}
		if len(next) > max || strings.Count(next, "-") >= namerMaxWords {
			break
		}
		name = next
	}
	if len(name) < 3 || strings.Trim(name, "0123456789-") == "" {
		return "" // empty, a fragment, or a bare number: not a name
	}
	return name
}

// plainWord reports whether a field is a bare word or hyphenated word run —
// letters, digits and interior hyphens, starting on an alphanumeric.
//
// The leading-alphanumeric rule is doing real work: it is what stops `-rf`,
// `..` and `/etc` from ever being considered, before any of them could become
// a branch name or a path segment.
func plainWord(s string) bool {
	if s == "" || !alnum(rune(s[0])) {
		return false
	}
	for _, r := range s {
		if !alnum(r) && r != '-' {
			return false
		}
	}
	return true
}

func alnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// isOwn reports whether a word is the repo naming itself. The repo is already
// a property of the lane — every listing shows it — so spending one of three
// words on it says nothing.
func isOwn(word string, own []string) bool {
	for _, o := range own {
		if word == o {
			return true
		}
	}
	return false
}

// repoWords is the set of words that mean "this repo": its checkout's basename,
// and both halves of its owner/name slug.
func repoWords(main, slug string) []string {
	words := []string{strings.ToLower(filepath.Base(main))}
	for _, part := range strings.Split(strings.ToLower(slug), "/") {
		if part != "" {
			words = append(words, part)
		}
	}
	return words
}

// laneNamesIn is every lane this repo already has, for the namer to steer away
// from. A registry that won't load is not worth failing a name over — the
// namer just picks without knowing the neighbours.
func (e *Env) laneNamesIn(main string) []string {
	rows, err := e.Reg.Load()
	if err != nil {
		return nil
	}
	var names []string
	for _, row := range rows {
		if row.Main == main && row.Name != "" {
			names = append(names, row.Name)
		}
	}
	return names
}

// clip truncates on a rune boundary and says that it did, so the namer knows
// it is looking at the top of a brief rather than all of one.
func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "\n\n(truncated)"
}

// utf8Start reports whether a byte begins a UTF-8 rune (i.e. is not a
// continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// lastLine is the tail of a failed namer's stderr, bounded, for the warning.
func lastLine(s string) string {
	for _, line := range reversed(strings.Split(strings.TrimSpace(s), "\n")) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 120 {
			line = line[:120] + "…"
		}
		return ": " + line
	}
	return ""
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}
