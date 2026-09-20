package commands

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hausfold/scruff/internal/registry"
)

// notifyEnv is an Env with a registry holding one lane, for the cwd → lane
// name resolution the notify hook does.
func notifyEnv(t *testing.T) (*Env, registry.Row) {
	t.Helper()
	dir := t.TempDir()
	reg, err := registry.Open(filepath.Join(dir, "registry.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	row := registry.Row{
		Name: "sparkle", Main: filepath.Join(dir, "repo"),
		Branch: "worktree-sparkle",
		Path:   filepath.Join(dir, "wtbase", "repo", "sparkle"),
		Agent:  "claude",
	}
	if err := reg.Put(row); err != nil {
		t.Fatal(err)
	}
	return &Env{Reg: reg}, row
}

func TestTrillSendArgsMapsNotificationToAsk(t *testing.T) {
	e, row := notifyEnv(t)
	args, ok := e.trillSendArgs(map[string]any{
		"hook_event_name": "Notification",
		"cwd":             row.Path,
		"message":         "Claude needs your permission to use Bash",
	})
	if !ok {
		t.Fatal("a Notification event must produce a send")
	}
	joined := strings.Join(args, " ")
	if !slices.Contains(args, "ask") {
		t.Fatalf("want --kind ask, got %q", joined)
	}
	if !slices.Contains(args, row.Name) {
		t.Fatalf("want the lane name %q in the argv, got %q", row.Name, joined)
	}
	// The payload's message is conversation content — it must never reach trill.
	if strings.Contains(joined, "permission") {
		t.Fatalf("the payload message leaked into the argv: %q", joined)
	}
}

func TestTrillSendArgsMapsStopToDone(t *testing.T) {
	e, row := notifyEnv(t)
	// cwd is a SUBDIRECTORY of the lane — the resolution is containment, not
	// equality, because a session cds around its checkout.
	args, ok := e.trillSendArgs(map[string]any{
		"hook_event_name": "Stop",
		"cwd":             filepath.Join(row.Path, "internal", "deep"),
	})
	if !ok {
		t.Fatal("a Stop event must produce a send")
	}
	if !slices.Contains(args, "done") || !slices.Contains(args, row.Name) {
		t.Fatalf("want --kind done for lane %q, got %q", row.Name, strings.Join(args, " "))
	}
}

// A Stop mid-stop-hook-loop is not a finished turn; one banner per iteration
// would be noise.
func TestTrillSendArgsSkipsActiveStopHook(t *testing.T) {
	e, row := notifyEnv(t)
	if _, ok := e.trillSendArgs(map[string]any{
		"hook_event_name":  "Stop",
		"cwd":              row.Path,
		"stop_hook_active": true,
	}); ok {
		t.Fatal("stop_hook_active must suppress the send")
	}
}

func TestTrillSendArgsDeclinesUnknownEvents(t *testing.T) {
	e, _ := notifyEnv(t)
	for _, event := range []string{"", "PreToolUse", "SessionEnd"} {
		if _, ok := e.trillSendArgs(map[string]any{"hook_event_name": event, "cwd": "/x"}); ok {
			t.Fatalf("event %q must not produce a send", event)
		}
	}
}

// A pane outside any lane still banners, named after its directory — and
// carries no click, because there is no lane for `scruff focus` to go to.
func TestTrillSendArgsFallsBackToDirectoryName(t *testing.T) {
	e, _ := notifyEnv(t)
	args, ok := e.trillSendArgs(map[string]any{
		"hook_event_name": "Stop",
		"cwd":             "/somewhere/else/mytool",
	})
	if !ok || !slices.Contains(args, "mytool") {
		t.Fatalf("want the cwd basename as the title, got %q", strings.Join(args, " "))
	}
	if slices.Contains(args, "--action") {
		t.Fatalf("a pane that is not a lane must offer no lane action, got %q", strings.Join(args, " "))
	}
}

// The banner is clickable, and the lane it names is qualified by repo — the
// same spelling `scruff focus` (and matchLane behind it) accepts, because one
// name can exist in two repos.
func TestTrillSendArgsOffersTheLaneAsAClick(t *testing.T) {
	e, row := notifyEnv(t)
	args, ok := e.trillSendArgs(map[string]any{
		"hook_event_name": "Notification",
		"cwd":             row.Path,
	})
	if !ok {
		t.Fatal("a Notification event must produce a send")
	}
	want := "Go to lane=lane:" + filepath.Base(row.Main) + "/" + row.Name
	i := slices.Index(args, "--action")
	if i < 0 || i+1 >= len(args) || args[i+1] != want {
		t.Fatalf("want --action %q, got %q", want, strings.Join(args, " "))
	}
}

// SCRUFF_TRILL set is authoritative: pointing at nothing means "no banners",
// never a fall-through to whatever else the machine has.
func TestTrillBinaryHonorsOverride(t *testing.T) {
	t.Setenv("SCRUFF_TRILL", filepath.Join(t.TempDir(), "absent"))
	if got := trillBinary(); got != "" {
		t.Fatalf("a missing SCRUFF_TRILL must resolve to nothing, got %q", got)
	}
}

// bundleAt stands a fake Trill.app where trillBundles will look, and answers
// with the path to its executable.
func bundleAt(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(root, "Trill.app", "Contents", "MacOS", "Trill")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// stubBundles swaps the two install locations for temp dirs and empties PATH,
// so the bundle list is reached at all and both halves of it can exist.
func stubBundles(t *testing.T) (system, home string) {
	t.Helper()
	dir := t.TempDir()
	system, home = filepath.Join(dir, "sys"), filepath.Join(dir, "home")
	real := trillBundles
	trillBundles = func(string) []string {
		return []string{
			filepath.Join(system, "Trill.app", "Contents", "MacOS", "Trill"),
			filepath.Join(home, "Trill.app", "Contents", "MacOS", "Trill"),
		}
	}
	t.Cleanup(func() { trillBundles = real })
	// Otherwise a real `trill` on the runner's PATH answers first and the list
	// is never reached — the case below would pass without asserting anything.
	t.Setenv("PATH", filepath.Join(dir, "empty-bin"))
	return system, home
}

// The pinned /Applications bundle beats a dev build in ~/Applications, and this
// is a precedence with NO error surface: both candidates are real, working
// Trills, so getting it backwards runs a stale daemon forever rather than
// failing. It used to be backwards. haus's wrapper (modules/core/trill.sh)
// carries the same order and the two must agree.
func TestTrillBinaryPrefersSystemBundle(t *testing.T) {
	system, home := stubBundles(t)
	want := bundleAt(t, system)
	bundleAt(t, home)

	if got := trillBinary(); got != want {
		t.Fatalf("want the /Applications bundle %q, got %q", want, got)
	}
}

// ...and home is a fallback rather than a demotion: alone, it still answers.
// That is what makes system-first the safe order — a candidate that is not on
// disk is skipped, so a user-scoped install loses nothing.
func TestTrillBinaryFallsBackToHomeBundle(t *testing.T) {
	_, home := stubBundles(t)
	want := bundleAt(t, home)

	if got := trillBinary(); got != want {
		t.Fatalf("want the ~/Applications bundle %q, got %q", want, got)
	}
}

// A fin nothing can name again is a fin that stacks: two permission prompts
// from one lane would hang two, and the ledge holds five.
func TestTrillSendArgsKeysTheFinByLane(t *testing.T) {
	e, row := notifyEnv(t)
	want := "scruff/" + filepath.Base(row.Main) + "/" + row.Name
	for _, event := range []string{"Notification", "Stop"} {
		args, ok := e.trillSendArgs(map[string]any{
			"hook_event_name": event, "cwd": row.Path, "session_id": "abc-123",
		})
		if !ok {
			t.Fatalf("%s must produce a send", event)
		}
		i := slices.Index(args, "--key")
		if i < 0 || i+1 >= len(args) || args[i+1] != want {
			t.Fatalf("%s: want --key %q, got %q", event, want, strings.Join(args, " "))
		}
	}
}

// A pane outside every lane has no lane identity to key by — and its directory
// is not one either, since a session can cd out of it. The client's session id
// is the only stable name it has.
func TestTrillSendArgsKeysANonLanePaneBySession(t *testing.T) {
	e, _ := notifyEnv(t)
	args, ok := e.trillSendArgs(map[string]any{
		"hook_event_name": "Notification", "cwd": "/somewhere/else/mytool",
		"session_id": "abc-123",
	})
	if !ok {
		t.Fatal("a Notification event must produce a send")
	}
	i := slices.Index(args, "--key")
	if i < 0 || i+1 >= len(args) || args[i+1] != "scruff/session/abc-123" {
		t.Fatalf("want the session key, got %q", strings.Join(args, " "))
	}
}

// Nothing to key by at all (an older client, no session id) is not a failure:
// the banner still goes up, it just can't be resolved later.
func TestTrillSendArgsOmitsTheKeyWhenThereIsNothingToName(t *testing.T) {
	e, _ := notifyEnv(t)
	args, ok := e.trillSendArgs(map[string]any{
		"hook_event_name": "Stop", "cwd": "/somewhere/else/mytool",
	})
	if !ok || slices.Contains(args, "--key") {
		t.Fatalf("want no --key, got %q", strings.Join(args, " "))
	}
}

// The gate the resume events read. Its whole job is to be cheap and honest:
// nothing outstanding anywhere → no registry read, no trill launch.
func TestAskMarkersGateTheResolvePath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	if anyAskOutstanding() {
		t.Fatal("a fresh state dir has no asks outstanding")
	}
	markAskOutstanding("scruff/alpha/sparkle")
	if !anyAskOutstanding() {
		t.Fatal("a marked ask must be outstanding")
	}
	if clearAskOutstanding("scruff/alpha/other") {
		t.Fatal("clearing another lane's key must report nothing cleared")
	}
	if !clearAskOutstanding("scruff/alpha/sparkle") {
		t.Fatal("clearing the marked key must report it cleared")
	}
	// Idempotent: a fin dismissed by hand leaves nothing behind to clear twice.
	if clearAskOutstanding("scruff/alpha/sparkle") || anyAskOutstanding() {
		t.Fatal("a cleared ask must stay cleared")
	}
}

// Keys become one filename each, and a key with a separator in it may not
// climb out of the state dir.
func TestAskMarkerStaysInsideTheStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	for _, key := range []string{"scruff/alpha/sparkle", "scruff/../../etc/passwd"} {
		if got := filepath.Dir(askMarker(key)); got != asksDir() {
			t.Fatalf("key %q escaped to %q", key, got)
		}
	}
}

// The leak the gate above could not survive: two shapes of marker never get
// the "next tool call" that clears them — a lane reaped while it was blocked,
// and a pane outside every lane whose session has ended. One each per
// abandoned question, and the dir is then never empty again, which makes the
// cheap answer permanently the expensive one.
func TestStaleAskMarkersArePruned(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")

	markAskOutstanding("scruff/alpha/sparkle")
	markAskOutstanding("scruff/session/7f3c")
	old := time.Now().Add(-askMarkerMaxAge - time.Hour)
	if err := os.Chtimes(askMarker("scruff/session/7f3c"), old, old); err != nil {
		t.Fatal(err)
	}

	pruneStaleAsks()

	if _, err := os.Stat(askMarker("scruff/session/7f3c")); !os.IsNotExist(err) {
		t.Fatal("a marker nothing will ever clear must not survive the sweep")
	}
	// And the live half is untouched: a lane blocked on you five minutes ago is
	// exactly what the gate is for.
	if !anyAskOutstanding() {
		t.Fatal("a fresh marker must survive the sweep")
	}
}

// A marker at the age boundary is kept, because the direction of the error
// matters: pruning early costs one fin that its own `done` replaces at the end
// of the turn, pruning late costs every pane the expensive path.
func TestAskMarkerPruneKeepsAnythingYoungerThanTheCutoff(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")

	markAskOutstanding("scruff/alpha/sparkle")
	young := time.Now().Add(-askMarkerMaxAge + time.Hour)
	if err := os.Chtimes(askMarker("scruff/alpha/sparkle"), young, young); err != nil {
		t.Fatal(err)
	}

	pruneStaleAsks()

	if !anyAskOutstanding() {
		t.Fatal("a marker inside the cutoff must survive")
	}
}

// A missing state dir is the ordinary case on a machine that has never had a
// fin, and the prune runs inside a sweep whose job is elsewhere.
func TestAskMarkerPruneSurvivesAMissingDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	pruneStaleAsks()
}

// The reap path spells a lane the same way the hook path does. If these two
// ever disagree, a reaped lane's fin outlives it in silence — nothing else on
// the machine can name that key.
func TestLaneIDMatchesTheHookPathsSpelling(t *testing.T) {
	if got := laneID("/Users/x/code/hausfold.co", "ci-main-branch"); got != "hausfold.co/ci-main-branch" {
		t.Fatalf("laneID = %q", got)
	}
	if got := askKey(laneID("/Users/x/code/haus", "sparkle"), nil); got != "scruff/haus/sparkle" {
		t.Fatalf("askKey = %q", got)
	}
	// A row that is missing either half names no lane, and must not become
	// `scruff//sparkle` — a key that would clear nothing and mark nothing.
	if laneID("", "sparkle") != "" || laneID("/Users/x/code/haus", "") != "" {
		t.Fatal("half a row is not a lane")
	}
}

// The resolve path clears the marker for the lane the event came from — the
// gate every later tool call reads, so a marker left behind turns the cheap
// check into a registry read and a Trill.app launch for the life of the pane.
func TestResolveAskClearsTheKey(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	t.Setenv("SCRUFF_TRILL", filepath.Join(t.TempDir(), "absent")) // no launch

	e, row := notifyEnv(t)
	markAskOutstanding(askKey(laneID(row.Main, row.Name), nil))

	e.resolveAsk(map[string]any{
		"hook_event_name": "PostToolUse", "cwd": row.Path, "session_id": "abc-123",
	})

	if anyAskOutstanding() {
		t.Fatal("the lane's marker must come down")
	}
}

// ── the background-work hold ─────────────────────────────────────────────────

// stopWith is a Stop payload carrying the client's in-flight background work.
func stopWith(tasks ...map[string]any) map[string]any {
	entries := make([]any, 0, len(tasks))
	for _, task := range tasks {
		entries = append(entries, task)
	}
	return map[string]any{"hook_event_name": "Stop", "background_tasks": entries}
}

func task(kind, status string) map[string]any {
	return map[string]any{"id": "t1", "type": kind, "status": status, "description": "a thing"}
}

// The banner the session earns is the one after its LAST answer. An answer
// delivered with agents still running is a turn that only looks over: you walk
// to the pane and find a progress list.
func TestStopIsHeldWhileASubagentIsInFlight(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	key := "scruff/alpha/sparkle"

	if !heldForBackgroundWork("Stop", stopWith(task("subagent", "running")), key) {
		t.Fatal("a Stop with a running subagent must not banner")
	}
	if !waitingOnAgents(key) {
		t.Fatal("the held Stop must leave the marker the idle ask reads")
	}
	// And the turn that really ends banners, and takes the marker with it.
	if heldForBackgroundWork("Stop", stopWith(), key) {
		t.Fatal("a Stop with nothing in flight must banner")
	}
	if waitingOnAgents(key) {
		t.Fatal("a finished turn must clear the marker")
	}
}

// Most of what can be in that list must never hold a banner back: a shell is
// routinely a dev server, a monitor never exits by definition. Holding on one
// silences the lane for the rest of its life.
func TestStopIsNotHeldByBackgroundWorkThatNeverEnds(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	for _, kind := range []string{"shell", "monitor", "teammate", "cloud session", "dream"} {
		if heldForBackgroundWork("Stop", stopWith(task(kind, "running")), "scruff/alpha/sparkle") {
			t.Fatalf("a %q task must not hold the banner", kind)
		}
	}
	// A subagent the client says is over holds nothing either — and the raw
	// discriminant is matched beside the friendly label, since the client falls
	// back to it for types it has no label for.
	if heldForBackgroundWork("Stop", stopWith(task("subagent", "completed")), "scruff/alpha/sparkle") {
		t.Fatal("a finished subagent must not hold the banner")
	}
	// A status this does not recognise falls out the same way: the client may
	// grow a `cancelled` or a `timed_out`, and an unreadable one must fire the
	// banner rather than hold it.
	if heldForBackgroundWork("Stop", stopWith(task("subagent", "cancelled")), "scruff/alpha/sparkle") {
		t.Fatal("an unknown status must not hold the banner")
	}
}

// Workflows are the other bounded thing the session is woken by, and both the
// friendly label and the raw discriminant reach this hook — the client falls
// back to the discriminant for any type it has no label for.
func TestStopIsHeldByEitherSpellingOfAgentWork(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	for _, kind := range []string{"subagent", "local_agent", "workflow", "local_workflow"} {
		for _, status := range []string{"running", "pending"} {
			if !heldForBackgroundWork("Stop", stopWith(task(kind, status)), "scruff/alpha/sparkle") {
				t.Fatalf("a %q task %q must hold the banner", kind, status)
			}
		}
	}
	// One held task in a list of things that hold nothing is still a hold.
	if !heldForBackgroundWork("Stop", stopWith(
		task("shell", "running"), task("monitor", "running"), task("subagent", "running"),
	), "scruff/alpha/sparkle") {
		t.Fatal("a subagent beside a dev server must still hold the banner")
	}
}

// An older client, or any other client wiring this hook up, sends no
// background_tasks at all. That is not "nothing is running" being asserted —
// it is nothing being said — and the answer to it is today's banner.
func TestStopBannersWhenTheClientSaysNothing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	for _, payload := range []map[string]any{
		{"hook_event_name": "Stop"},
		{"hook_event_name": "Stop", "background_tasks": nil},
		{"hook_event_name": "Stop", "background_tasks": "not a list"},
	} {
		if heldForBackgroundWork("Stop", payload, "scruff/alpha/sparkle") {
			t.Fatalf("payload %v must banner", payload)
		}
	}
}

// The sticky half. The idle ask cannot see what the Stop saw — its payload
// carries no task list — so it reads the marker that Stop left behind.
func TestIdleAskIsHeldButAQuestionIsNot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	key := "scruff/alpha/sparkle"
	heldForBackgroundWork("Stop", stopWith(task("subagent", "running")), key)

	idle := map[string]any{"hook_event_name": "Notification", "notification_type": "idle_prompt"}
	if !heldForBackgroundWork("Notification", idle, key) {
		t.Fatal("the idle ask must be held while agents are still running")
	}
	// A permission prompt during background work is a real question with a real
	// session blocked behind it, and an unrecognised type is treated as one.
	for _, kind := range []string{"permission_prompt", "agent_needs_input", "elicitation_dialog", "something_new", ""} {
		payload := map[string]any{"hook_event_name": "Notification", "notification_type": kind}
		if heldForBackgroundWork("Notification", payload, key) {
			t.Fatalf("notification_type %q must still banner", kind)
		}
	}
	// And with no held Stop behind it, the idle ask is just an idle ask.
	if heldForBackgroundWork("Notification", idle, "scruff/alpha/other") {
		t.Fatal("another lane's idle ask must not be held")
	}
}

// The one shape that leaks: a session that died while its agents ran, whose
// marker no later Stop will ever rewrite. The age is the whole recovery — and
// the cost of being wrong is one idle ask that does not fire.
func TestTheHoldAgesOut(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	key := "scruff/alpha/sparkle"
	markWaitingOnAgents(key)

	old := time.Now().Add(-waitMarkerMaxAge - time.Minute)
	if err := os.Chtimes(waitMarker(key), old, old); err != nil {
		t.Fatal(err)
	}
	if waitingOnAgents(key) {
		t.Fatal("a marker past the age must stop holding anything")
	}

	pruneStaleWaits()
	if _, err := os.Stat(waitMarker(key)); !os.IsNotExist(err) {
		t.Fatal("the sweep must drop it")
	}
}

// Keys become one filename each here too, and the two marker directories stay
// separate — one key is an ask in one and a hold in the other.
func TestWaitMarkerStaysInsideItsOwnDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	for _, key := range []string{"scruff/alpha/sparkle", "scruff/../../etc/passwd"} {
		if got := filepath.Dir(waitMarker(key)); got != waitsDir() {
			t.Fatalf("key %q escaped to %q", key, got)
		}
	}
	// A pane with nothing to key by cannot be held, and must not write the
	// directory itself as a file.
	markWaitingOnAgents("")
	if waitingOnAgents("") {
		t.Fatal("the empty key is not a key")
	}
}

// Events that are neither Stop nor Notification pass straight through — the
// hold is not a second place where this hook decides what to send.
func TestTheHoldIgnoresEveryOtherEvent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	key := "scruff/alpha/sparkle"
	markWaitingOnAgents(key)
	for _, event := range []string{"UserPromptSubmit", "PostToolUse", "SubagentStop", ""} {
		if heldForBackgroundWork(event, map[string]any{"hook_event_name": event}, key) {
			t.Fatalf("event %q must not be held", event)
		}
	}
}

// A held Stop leaves an outstanding ask exactly where it is. The marker is
// content-free, so nothing can tell a stale idle fin from a background
// worker's own permission prompt — and resolving THAT takes a live question
// off the ledge while the session it blocks waits for an answer nobody will
// be told about. A stale fin costs one `done` replacing it at the end of the
// wait; this would cost the question.
func TestAHeldStopLeavesAnOutstandingAskAlone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SCRUFF_STATE", "")
	t.Setenv("SCRUFF_TRILL", filepath.Join(t.TempDir(), "absent")) // no launch
	key := "scruff/alpha/sparkle"
	markAskOutstanding(key)

	if !heldForBackgroundWork("Stop", stopWith(task("subagent", "running")), key) {
		t.Fatal("the Stop must be held")
	}
	if !anyAskOutstanding() {
		t.Fatal("a held Stop must not resolve the lane's ask")
	}
}
