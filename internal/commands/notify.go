package commands

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// HookNotify implements the client-side notification hooks: the session's
// "blocked on the user" / "finished the turn" events become a trill banner for
// the lane — an `ask` that trill's ledge parks until answered, a `done` when
// the turn ends — and the events that mean the session RESUMED take that ask
// back down.
//
// Four events, two directions:
//
//	Notification      → an `ask` fin, keyed by lane
//	Stop              → a `done` banner, same key, so it replaces the fin
//	UserPromptSubmit  → the user answered: resolve the fin
//	PostToolUse       → a tool actually ran, so a permission prompt was
//	                    approved: resolve the fin
//
// The two resolve events fire constantly (PostToolUse, once per tool call), so
// the fast path has to be nearly free: with no ask outstanding anywhere this
// reads one directory and returns, without loading the registry or launching
// trill. See anyAskOutstanding.
//
// Unlike its create/remove siblings this hook changes NOTHING — no checkout,
// no registry row — so it must also never be the reason a session breaks. It
// exits 0 whatever happens: a payload that isn't JSON, an event it doesn't
// recognise, no trill binary anywhere, the daemon down (trill exit 2). Every
// one of those means "no banner", never an error the client surfaces
// mid-session.
func (e *Env) HookNotify(stdin io.Reader) error {
	payload, err := readHookPayload(stdin)
	if err != nil {
		return nil
	}
	event, _ := hookField(payload, "hook_event_name")
	switch event {
	case "UserPromptSubmit", "PostToolUse":
		e.resolveAsk(payload)
		return nil
	}
	args, ok := e.trillSendArgs(payload)
	if !ok {
		return nil
	}
	bin := trillBinary()
	if bin == "" {
		return nil
	}
	if runTrill(bin, args) != nil {
		// The daemon refused or isn't there. Nothing is on screen, so nothing
		// is outstanding — leaving a marker behind would arm the resolve path
		// for a fin that doesn't exist.
		return nil
	}
	// The fin is up (or, for a done, the ask it replaced is gone). Arm — or
	// disarm — the cheap check the resume events make.
	key, _ := e.askKeyFor(payload)
	if key == "" {
		return nil
	}
	if event == "Notification" {
		markAskOutstanding(key)
	} else {
		clearAskOutstanding(key)
	}
	return nil
}

// resolveAsk is the other direction: this lane's session is moving again, so
// the question its fin asks has been answered. `trill resolve` is idempotent —
// resolving something already dismissed prints 0 and exits 0 — so the only
// thing to be careful about here is COST, not correctness.
//
// PostToolUse fires on every tool call in every pane, and the work below would
// otherwise be a registry read plus a launch of Trill.app's binary each time.
// So the marker written when an ask went up is the gate: no marker, no work.
func (e *Env) resolveAsk(payload map[string]any) {
	if !anyAskOutstanding() {
		return
	}
	key, _ := e.askKeyFor(payload)
	if key == "" {
		return
	}
	// Nothing cleared at all means some OTHER lane is the one waiting.
	takeDownAsk(key)
}

// runTrill launches the CLI, bounded and quiet: a wedged daemon must not hang
// pane redraw, and trill's own stderr is not this hook's to surface.
func runTrill(bin string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

// trillSendArgs maps one hook payload to a `trill send` argv, or declines.
//
// The title and thread carry the lane name and nothing else — never the
// payload's message string, which is conversation content a banner has no
// business showing on a shared screen.
func (e *Env) trillSendArgs(payload map[string]any) ([]string, bool) {
	event, _ := hookField(payload, "hook_event_name")
	var kind, body, symbol string
	switch event {
	case "Notification":
		kind, body, symbol = "ask", "waiting on you", "questionmark.bubble"
	case "Stop":
		// A Stop with stop_hook_active is a stop hook steering the session
		// mid-loop, not a finished turn — a done banner per iteration is noise.
		if active, _ := payload["stop_hook_active"].(bool); active {
			return nil, false
		}
		kind, body, symbol = "done", "finished its turn", "checkmark.circle"
	default:
		return nil, false
	}
	name, lane := e.laneFor(payload)
	args := []string{"send",
		"--kind", kind,
		"--title", name,
		"--body", body,
		"--source", "claude",
		"--thread", name,
		"--symbol", symbol,
	}
	// The name anything else gets to call this fin by later — this hook's own
	// resolve path, a rebuild hook, the user typing `trill resolve`. Without
	// it every event is its own fin: two permission prompts from one lane
	// stack two, and trill's ledge only holds five. With it, an ask replaces
	// the lane's previous ask and a done replaces the ask it finished.
	if key := askKey(lane, payload); key != "" {
		args = append(args, "--key", key)
	}
	// A banner that names a lane and can't take you to it is the whole
	// friction this hook was supposed to remove: you still had to find the
	// window yourself. `lane:` is trill's focus_lane action — it runs
	// `scruff focus <name>` and nothing else — and it is qualified by repo
	// because two repos may hold the same lane name, which is exactly the
	// ambiguity `scruff focus` refuses to guess at.
	//
	// Only when a registry row matched: a pane outside every lane still gets
	// a banner (named after its directory), and there is nothing to focus.
	if lane != "" {
		args = append(args, "--action", "Go to lane=lane:"+lane)
	}
	return args, true
}

// laneFor names the lane a hook payload came from: the registry row whose
// checkout contains the payload's cwd. A pane outside any lane (the main
// checkout, a ~ pane) still gets a banner — named after its directory, which
// is the honest answer and still not conversation content.
//
// Two names come back. The first is what the banner says, short enough to read
// at a glance. The second is what `scruff focus` is given — `<repo>/<name>`, the
// same qualified spelling matchLane accepts — and it is empty for anything
// that isn't a lane, which is how the caller knows not to offer a click.
func (e *Env) laneFor(payload map[string]any) (name, lane string) {
	cwd, _ := hookField(payload, "cwd")
	if cwd == "" {
		return "claude", ""
	}
	if rows, err := e.Reg.Load(); err == nil {
		for _, row := range rows {
			if cwd == row.Path || strings.HasPrefix(cwd, row.Path+string(filepath.Separator)) {
				return row.Name, laneID(row.Main, row.Name)
			}
		}
	}
	return filepath.Base(cwd), ""
}

// laneID is how every fin, banner action and `scruff focus` argument spells one
// lane: qualified by the main checkout's basename, because `scruff child` puts
// one lane name in two repos. One function, because the reap path builds it
// from a registry row and the hook path from a payload's cwd, and the two must
// agree byte for byte or a fin outlives the lane it named.
func laneID(main, name string) string {
	if main == "" || name == "" {
		return ""
	}
	return filepath.Base(main) + "/" + name
}

// trillBundles is where a Trill.app install lands, in the order they are tried:
// SYSTEM BEFORE HOME. That order is a judgement, and it goes this way round
// because the two are not two spellings of the same thing — /Applications is
// where a cask, a drag-install and haus's notifications room all put the
// release, while ~/Applications is where trill's own scripts/dev-install.sh
// leaves a build you were testing. Home-first let that build outrank the
// release forever, with no error surface at all: both candidates are real,
// signed, working Trills, so the only difference is which daemon VERSION
// answers, and a stale one just quietly misbehaves. Measured 2026-09-02, a
// stray ~/Applications/Trill.app hung `trill history` at exactly 8192 bytes —
// the socket's send buffer — while the release beside it answered fine.
// SCRUFF_TRILL is still the way to aim at a branch build, and is tried first.
//
// haus's wrapper (modules/core/trill.sh) carries the same order, and the two
// have to agree: a machine where scruff picked one bundle and haus the other
// would drive two daemons off one ledge, with no way to see it.
//
// A var so a test can stand both locations in a temp dir. /Applications is
// absolute and root-owned, so a hermetic test cannot create the system half
// any other way — and the ORDER is the whole thing under test.
var trillBundles = func(home string) []string {
	return []string{
		"/Applications/Trill.app/Contents/MacOS/Trill",
		filepath.Join(home, "Applications", "Trill.app", "Contents", "MacOS", "Trill"),
	}
}

// trillBinary resolves the trill CLI without assuming anything about the
// machine: Trill.app is routinely installed while `trill` is on nobody's PATH,
// because the app binary IS the CLI. SCRUFF_TRILL is authoritative when set —
// including set to something missing, which is how a machine (or a test) says
// "no banners" — and an empty answer means exactly that, silently.
//
// The PATH lookup below finds haus's wrapper on a haus machine, which makes the
// bundle list a fallback rather than the answer there. It is NOT dead code: a
// launchd context that sets its own PATH has no `trill` to look up and lands in
// the list, and hook payloads reach this function from exactly such contexts.
func trillBinary() string {
	if p := os.Getenv("SCRUFF_TRILL"); p != "" {
		if isExecutable(p) {
			return p
		}
		return ""
	}
	if p, err := exec.LookPath("trill"); err == nil {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	for _, p := range trillBundles(home) {
		if isExecutable(p) {
			return p
		}
	}
	return ""
}

func isExecutable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// askKeyFor is askKey for a payload the caller hasn't resolved a lane from
// yet — the shape the resolve path needs, where nothing has looked one up.
func (e *Env) askKeyFor(payload map[string]any) (key, lane string) {
	_, lane = e.laneFor(payload)
	return askKey(lane, payload), lane
}

// askKey names a lane's fin, so something later can call it by name:
// `trill resolve <key>`, or another `trill send --key <key>` that replaces it.
//
// Keyed by the LANE, not the session. A lane whose agent was resumed is the
// same lane and the same question, so its fin should be replaced rather than
// joined by a second one — and the resolve path has to be able to name a fin
// that a *previous* session put up. A pane outside every lane has no such
// identity, so it falls back to the client's session id: a pane's directory
// can change under it mid-session, and its basename is not unique anyway.
//
// ⚠️ This prefix is half of a JOIN, so it moves in step with haus or not at
// all. haus's lane-seen.sh matches a zmx session named `scruff.<repo>.<lane>`
// against this key with the slashes flattened; the two spellings have to agree
// or the bar quietly stops resolving anything.
func askKey(lane string, payload map[string]any) string {
	if lane != "" {
		return askKeyPrefix + lane
	}
	if sid, _ := hookField(payload, "session_id"); sid != "" {
		return askKeyPrefix + "session/" + sid
	}
	return ""
}

// askKeyPrefix is the one spelling scruff writes, and now the only one it
// answers to.
const askKeyPrefix = "scruff/"

// takeDownAsk clears one key's marker and, when there was one, resolves its
// fin. It reports whether the marker was there — which is also the answer to
// "was this key the one waiting?", and the gate on launching trill at all.
func takeDownAsk(key string) bool {
	if !clearAskOutstanding(key) {
		return false
	}
	if bin := trillBinary(); bin != "" {
		_ = runTrill(bin, []string{"resolve", key})
	}
	return true
}

// ── the outstanding-ask marker ───────────────────────────────────────────────
//
// One empty file per fin this hook put up, under scruff's state dir. It exists
// for exactly one reason: PostToolUse fires on every tool call in every pane,
// and without a gate each of those would read the registry and launch
// Trill.app's binary to resolve a fin that is usually not there. With it, the
// ordinary tool call reads one directory and stops.
//
// It is a CACHE, not a record. Anything can take a fin down without telling
// scruff — the ✕, a pill, an eviction, a relaunch, a desktop that clears a lane's
// fin when you go and look at it — so a marker may outlive its fin. The cost of
// a stale one is a single no-op `trill resolve` (idempotent by design) the next
// time that lane runs a tool, after which the marker is gone. Nothing here may
// ever be treated as the truth about what is on screen.
//
// ── who cleans up when that "next tool call" never comes ────────────────────
// The sentence above quietly assumed every marker gets one. Two shapes never
// do, and both leak forever:
//
//   * a lane blocked on you when its pane was closed. It is answered by being
//     REAPED, not by running another tool, so its marker outlives the lane, the
//     branch and the checkout.
//   * a pane outside every lane, keyed by session id. When that session ends
//     there is nothing left that could ever match the key again.
//
// Left alone they accumulate one per abandoned question — 53 of them on the
// machine this was found on — and the dir is then never empty, which turns the
// whole gate into "yes, something is waiting" on every tool call in every pane
// for the life of the machine: exactly the registry read and Trill.app launch
// it exists to avoid. So the sweep that already runs on every listing prunes
// them, from both ends: reapSweep clears a lane's marker the moment it reaps
// that lane, and pruneStaleAsks drops anything older than askMarkerMaxAge,
// which is the only answer available for the session-keyed half.

func asksDir() string { return filepath.Join(stateDir(), "asks") }

// askMarker is one key's file. Keys are `scruff/<repo>/<lane>` or a session id,
// so the flattening below is lossless in practice; a lane named with a dot
// could in principle collide with another, and the consequence of that is one
// resolve firing a moment early for a lane that was about to be resolved
// anyway.
func askMarker(key string) string {
	flat := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		}
		return '.'
	}, key)
	return filepath.Join(asksDir(), flat)
}

func markAskOutstanding(key string) {
	if err := os.MkdirAll(asksDir(), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(askMarker(key), nil, 0o644)
}

// clearAskOutstanding drops one key's marker and reports whether there was one
// — which is also the answer to "was this lane the one waiting?".
//
// The empty key is not a key. `askMarker("")` is the asks DIRECTORY, so
// without this line an unnamed lane would delete the whole dir the moment it
// happened to be empty — and a consumer reading it (see the section header)
// would get an ENOENT where it expects "nothing is waiting". The two hook
// callers check before calling; the sweep's does not, because this is where
// the check belongs.
func clearAskOutstanding(key string) bool {
	if key == "" {
		return false
	}
	return os.Remove(askMarker(key)) == nil
}

// anyAskOutstanding is the cheap half of the gate: is ANY fin up, anywhere?
// Read before the registry, because the answer is almost always no.
func anyAskOutstanding() bool {
	dir, err := os.Open(asksDir())
	if err != nil {
		return false
	}
	defer dir.Close()
	names, err := dir.Readdirnames(1)
	return err == nil && len(names) > 0
}

// askMarkerMaxAge is how long a marker may sit unanswered before the sweep
// assumes nothing will ever answer it.
//
// A day, and the direction of the error is what picks it: pruning early costs
// one fin that stays on trill's ledge until its `done` replaces it at the end
// of the turn (same key, so it does), while pruning late costs every pane on
// the machine the expensive path. A question a whole day old is not one the
// resolve path is still racing to take down.
const askMarkerMaxAge = 24 * time.Hour

// pruneStaleAsks drops markers nothing is going to clear. See the section
// header: this is the backstop for the session-keyed half, and for any lane
// whose fin outlived the sweep that reaped it.
//
// Best-effort throughout. It runs inside a sweep whose job is elsewhere, and a
// marker dir that cannot be read is exactly the "no fin is up" answer the gate
// already gives. Entries are unlinked one by one and the directory itself is
// never touched — something else on the machine may be watching it.
func pruneStaleAsks() {
	dir := asksDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-askMarkerMaxAge)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}
