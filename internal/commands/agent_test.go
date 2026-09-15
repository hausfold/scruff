package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trustWorktree edits a file scruff does not own — Claude Code's ~/.claude.json,
// ~180KB of that client's own state — so the thing under test is as much what it
// LEAVES ALONE as what it writes. The re-encode is the sharp edge: decoding JSON
// into map[string]any turns every number into a float64 unless you ask for
// json.Number, and marshalling that back writes `1.778838900185e+12` where a
// millisecond timestamp used to be. That corruption would be silent, in the
// user's client config, on a code path whose entire purpose is convenience.

const claudeConfig = `{
  "numStartups": 1202,
  "installMethod": "native",
  "oauthAccount": {"accountUuid": "abc-123"},
  "projects": {
    "/repo": {
      "hasTrustDialogAccepted": true,
      "lastSessionModified": 1778838900185,
      "lastFpsAverage": 6.07,
      "history": [{"display": "hello"}]
    },
    "/untrusted": {"hasTrustDialogAccepted": false}
  }
}`

func withHome(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func readProjects(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("wrote invalid JSON: %v", err)
	}
	projects, _ := doc["projects"].(map[string]any)
	return projects
}

func trusted(projects map[string]any, dir string) bool {
	entry, _ := projects[dir].(map[string]any)
	ok, _ := entry["hasTrustDialogAccepted"].(bool)
	return ok
}

func TestTrustWorktreeInheritsFromTrustedParent(t *testing.T) {
	path := withHome(t, claudeConfig)
	trustWorktree("claude", "/repo", "/wt/feature")

	projects := readProjects(t, path)
	if !trusted(projects, "/wt/feature") {
		t.Error("worktree of a trusted repo should not face the trust dialog")
	}
	if !trusted(projects, "/repo") {
		t.Error("the parent's own trust must survive the rewrite")
	}
}

// The one thing this must never do: decide on the user's behalf.
func TestTrustWorktreeRefusesUntrustedParent(t *testing.T) {
	path := withHome(t, claudeConfig)
	trustWorktree("claude", "/untrusted", "/wt/feature")

	if projects := readProjects(t, path); trusted(projects, "/wt/feature") {
		t.Error("granted trust the user never gave the parent repo")
	}
}

// ~/.claude.json is Claude Code's, and only the claude arm may write into it.
// pi is in this loop even though it DOES have a trust model, because the thing
// being asserted is that its decision lands in pi's own file and never here.
func TestTrustWorktreeWritesClaudesFileOnlyForClaude(t *testing.T) {
	path := withHome(t, claudeConfig)
	for _, agent := range []string{"codex", "opencode", "pi", ""} {
		trustWorktree(agent, "/repo", "/wt/"+agent)
	}
	projects := readProjects(t, path)
	for _, agent := range []string{"codex", "opencode", "pi", ""} {
		if _, ok := projects["/wt/"+agent]; ok {
			t.Errorf("%q wrote into Claude Code's ~/.claude.json", agent)
		}
	}
}

// The corruption test. A large integer must come back as the same integer, not
// as the float64 a naive map round-trip would produce.
func TestTrustWorktreePreservesEverythingElse(t *testing.T) {
	path := withHome(t, claudeConfig)
	trustWorktree("claude", "/repo", "/wt/feature")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		`1778838900185`,       // an int must not become 1.778838900185e+12
		`6.07`,                // …and a float must stay a float
		`"numStartups": 1202`, // untouched top-level keys survive
		`"accountUuid": "abc-123"`,
		`"display": "hello"`, // nested arrays/objects survive
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rewrite lost or mangled %s\n--- got ---\n%s", want, text)
		}
	}
	if os.Getenv("CI") == "" {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
			t.Errorf("a file holding credentials must stay 0600, got %o", fi.Mode().Perm())
		}
	}
}

// Every failure mode is a no-op, because the cost of getting this wrong (a
// clobbered client config) dwarfs the cost of not doing it (one trust prompt).
func TestTrustWorktreeSurvivesABadConfig(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		path := withHome(t, "")
		trustWorktree("claude", "/repo", "/wt/feature")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("must not conjure a config Claude Code never wrote")
		}
	})

	for name, body := range map[string]string{
		"unparseable":   `{"projects": {`,
		"no projects":   `{"numStartups": 3}`,
		"not an object": `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			path := withHome(t, body)
			trustWorktree("claude", "/repo", "/wt/feature")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != body {
				t.Errorf("rewrote a config it could not understand:\n%s", raw)
			}
		})
	}
}

// Re-running a spawn (or spawning twice into the same path) must not churn the
// file — this is the guard on "don't rewrite 180KB for nothing".
func TestTrustWorktreeIsIdempotent(t *testing.T) {
	path := withHome(t, claudeConfig)
	trustWorktree("claude", "/repo", "/wt/feature")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trustWorktree("claude", "/repo", "/wt/feature")
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("a second call rewrote a file it had nothing to add to")
	}
}

// ── the prompt is data, not flags ────────────────────────────────────────────
//
// The regression this pins: a Spawn Agent prompt pasted as a markdown list
// starts with `- `, and a bare argv element starting with a dash is an OPTION to
// every one of these clients. Pounce's box produced exactly that and the pane
// died on `error: unknown option '- https://…'` before the agent ever ran.

func TestStartArgvEndsOptionParsingBeforeThePrompt(t *testing.T) {
	const dashed = "- update the README\n- and its footer"

	cases := []struct {
		agent string
		image string
		want  []string
	}{
		{"claude", "", []string{"claude", "--", dashed}},
		{"codex", "", []string{"codex", "--", dashed}},
		{"codex", "/tmp/shot.png", []string{"codex", "-i", "/tmp/shot.png", "--", dashed}},
		{"opencode", "", []string{"opencode", "--prompt=" + dashed}},
		{"pi", "", []string{"pi", "--", dashed}},
		// pi's attachment is a message element (`@path`), not a flag, so it
		// rides AFTER the `--` — which is why the property test below asks
		// whether SOMETHING earlier ended option parsing rather than only the
		// element immediately before the prompt.
		{"pi", "/tmp/shot.png", []string{"pi", "--", "@/tmp/shot.png", dashed}},
	}
	for _, tc := range cases {
		spec, ok := specFor(tc.agent)
		if !ok {
			t.Fatalf("no spec for %q", tc.agent)
		}
		got := spec.start(tc.image, dashed)
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("%s start argv = %q, want %q", tc.agent, got, tc.want)
		}
	}
}

// Whatever the prompt, it must never arrive as an argv element a parser could
// still read as a flag — the property, not the four spellings above. Checked
// with an image as well as without, because pi's attachment sits between the
// terminator and the prompt and an earlier version of this test would have
// called that argv unsafe.
func TestStartNeverHandsAClientABareDashedPrompt(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "opencode", "pi"} {
		spec, _ := specFor(agent)
		for _, image := range []string{"", "/tmp/shot.png"} {
			argv := spec.start(image, "-x")
			for i, arg := range argv {
				if arg != "-x" {
					continue
				}
				if !optionsTerminatedBefore(argv[:i]) {
					t.Errorf("%s: prompt at argv[%d] of %q is still option-parsed", agent, i, argv)
				}
			}
		}
	}
}

// Did anything earlier in the argv end option parsing? A bare `--` does it for
// everything after it, however many elements intervene — `--prompt=<text>`
// carries the prompt inside one element and never exposes it at all, which is
// why the loop above finds nothing to check for opencode.
func optionsTerminatedBefore(before []string) bool {
	for _, arg := range before {
		if arg == "--" {
			return true
		}
	}
	return false
}

// ── pi's half of the same favour ─────────────────────────────────────────────
//
// pi's trust file is flat and INHERITED, so these tests are about the walk: the
// nearest saved decision to the main checkout wins, and only a yes is copied.

func withPiTrust(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "trust.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func readPiTrust(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]bool
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("wrote invalid JSON: %v", err)
	}
	return doc
}

// The case every lane on this machine hits: `~/code` is trusted, the repo is
// under it, and the worktree is not.
func TestTrustWorktreePiInheritsFromAnAncestor(t *testing.T) {
	path := withPiTrust(t, `{"/home/code": true}`)
	trustWorktree("pi", "/home/code/workshop", "/cache/worktrees/workshop/lane")

	if !readPiTrust(t, path)["/cache/worktrees/workshop/lane"] {
		t.Error("worktree of a trusted repo still faces pi's trust prompt")
	}
}

// The nearest decision wins, and a `no` is propagated by writing nothing.
func TestTrustWorktreePiHonoursANearerRefusal(t *testing.T) {
	path := withPiTrust(t, `{"/home/code": true, "/home/code/vendor": false}`)
	trustWorktree("pi", "/home/code/vendor/thing", "/cache/worktrees/thing/lane")

	if _, ok := readPiTrust(t, path)["/cache/worktrees/thing/lane"]; ok {
		t.Error("granted trust under a folder the user explicitly refused")
	}
}

// scruff never grants trust the user never gave.
func TestTrustWorktreePiRefusesUnknownParent(t *testing.T) {
	path := withPiTrust(t, `{"/home/code": true}`)
	trustWorktree("pi", "/elsewhere/repo", "/cache/worktrees/repo/lane")

	if _, ok := readPiTrust(t, path)["/cache/worktrees/repo/lane"]; ok {
		t.Error("granted trust for a repo with no saved decision")
	}
}

// No trust file at all is the first-run state, and it must cost a prompt, not a
// crash or an invented file.
func TestTrustWorktreePiWithNoFileIsANoOp(t *testing.T) {
	path := withPiTrust(t, "")
	trustWorktree("pi", "/home/code/workshop", "/cache/worktrees/workshop/lane")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("invented a trust file pi never wrote")
	}
}

// Everything else in the file survives, including a refusal we must not flip.
func TestTrustWorktreePiPreservesEverythingElse(t *testing.T) {
	path := withPiTrust(t, `{"/home/code": true, "/home/other": false}`)
	trustWorktree("pi", "/home/code/workshop", "/cache/worktrees/workshop/lane")

	doc := readPiTrust(t, path)
	if !doc["/home/code"] {
		t.Error("dropped the decision it read")
	}
	if v, ok := doc["/home/other"]; !ok || v {
		t.Errorf(`"/home/other" = %v, %v; want false, true`, v, ok)
	}
}

// The file is pi's, and scruff must hand it back the way it found it — an
// os.CreateTemp default of 0600 carried through the rename would tighten a
// file scruff does not own.
func TestTrustWorktreePiKeepsPisFileMode(t *testing.T) {
	path := withPiTrust(t, `{"/home/code": true}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	trustWorktree("pi", "/home/code/workshop", "/cache/worktrees/workshop/lane")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644", got)
	}
}

// ── a conversation the checkout moved out from under (#129) ──────────────────
//
// The matcher is the whole safety of the recovery: it copies somebody's
// conversation into a lane, so every case below is about what it must REFUSE.
// `scruff child` names a child lane after the pane that spawned it, so two
// lanes of one name in different buckets is an ordinary machine, not a
// contrived one.

// plantChat writes a transcript the way Claude Code would: a directory named
// for the cwd, holding a .jsonl whose first records are metadata with no cwd on
// them at all.
func plantChat(t *testing.T, store, cwd, branch string) string {
	t.Helper()
	dir := filepath.Join(store, projEnc(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"mode","sessionId":"s1"}` + "\n" +
		`{"type":"summary","summary":"a lane"}` + "\n" +
		`{"type":"user","sessionId":"s1","cwd":"` + cwd + `","gitBranch":"` + branch + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStrandedChatFindsTheLanesOwnOldPath(t *testing.T) {
	store, base := t.TempDir(), t.TempDir()
	old := filepath.Join(base, "workshop", "gallery")
	now := filepath.Join(base, "hausfold-hausfold.co", "gallery")
	want := plantChat(t, store, old, "worktree-gallery")

	dir, cwd := strandedChat(store, base, now, "worktree-gallery", nil)
	if dir != want || cwd != old {
		t.Fatalf("strandedChat = (%q, %q), want (%q, %q)", dir, cwd, want, old)
	}
}

// Everything the matcher must walk past, one reason each.
func TestStrandedChatRefusesEverythingItCannotBeSureOf(t *testing.T) {
	branch := "worktree-gallery"
	cases := []struct {
		why   string
		setup func(t *testing.T, store, base, now string) func(string) bool
	}{
		{"a conversation whose checkout is still standing there", func(t *testing.T, store, base, now string) func(string) bool {
			other := filepath.Join(base, "hausfold-haus", "gallery")
			if err := os.MkdirAll(other, 0o755); err != nil {
				t.Fatal(err)
			}
			plantChat(t, store, other, branch)
			return nil
		}},
		{"a parked lane of the same name that a registry row still claims", func(t *testing.T, store, base, now string) func(string) bool {
			other := filepath.Join(base, "hausfold-haus", "gallery")
			plantChat(t, store, other, branch)
			return func(p string) bool { return p == other }
		}},
		{"a transcript recorded on another branch", func(t *testing.T, store, base, now string) func(string) bool {
			plantChat(t, store, filepath.Join(base, "workshop", "gallery"), "worktree-something-else")
			return nil
		}},
		{"two old paths, which is a question only the user can answer", func(t *testing.T, store, base, now string) func(string) bool {
			plantChat(t, store, filepath.Join(base, "workshop", "gallery"), branch)
			plantChat(t, store, filepath.Join(base, "hausfold.co", "gallery"), branch)
			return nil
		}},
		{"a lane of another name under the same bucket", func(t *testing.T, store, base, now string) func(string) bool {
			plantChat(t, store, filepath.Join(base, "workshop", "other-lane"), branch)
			return nil
		}},
		{"a path outside the base entirely", func(t *testing.T, store, base, now string) func(string) bool {
			plantChat(t, store, filepath.Join(t.TempDir(), "workshop", "gallery"), branch)
			return nil
		}},
		{"a deeper path whose encoded name matches by accident", func(t *testing.T, store, base, now string) func(string) bool {
			plantChat(t, store, filepath.Join(base, "workshop", "nested", "gallery"), branch)
			return nil
		}},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			store, base := t.TempDir(), t.TempDir()
			now := filepath.Join(base, "hausfold-hausfold.co", "gallery")
			inUse := c.setup(t, store, base, now)
			if dir, cwd := strandedChat(store, base, now, branch, inUse); dir != "" || cwd != "" {
				t.Fatalf("adopted %s (%s) — %s", cwd, dir, c.why)
			}
		})
	}
}

// The lossy encoding, pointed at the matcher: `hausfold.co` and `hausfold-co`
// are one directory name, so a candidate is only ever confirmed by the cwd the
// transcript itself recorded.
func TestStrandedChatConfirmsAgainstTheRecordedCwd(t *testing.T) {
	store, base := t.TempDir(), t.TempDir()
	now := filepath.Join(base, "hausfold-hausfold.co", "gallery")
	old := filepath.Join(base, "hausfold.co", "gallery")
	want := plantChat(t, store, old, "worktree-gallery")
	// A directory of the right shape whose file says nothing: unusable, and it
	// must not make the real one ambiguous.
	empty := filepath.Join(store, projEnc(filepath.Join(base, "quiet", "gallery")))
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}

	dir, cwd := strandedChat(store, base, now, "worktree-gallery", nil)
	if dir != want || cwd != old {
		t.Fatalf("strandedChat = (%q, %q), want (%q, %q)", dir, cwd, want, old)
	}
}

// A transcript that predates gitBranch still resumes: the cwd already pins the
// base and the lane name, and refusing on a field the client used not to write
// would strand exactly the oldest lanes this recovers.
func TestStrandedChatAcceptsATranscriptWithNoBranchRecorded(t *testing.T) {
	store, base := t.TempDir(), t.TempDir()
	old := filepath.Join(base, "workshop", "gallery")
	dir := filepath.Join(store, projEnc(old))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","cwd":"` + old + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, cwd := strandedChat(store, base, filepath.Join(base, "acme-alpha", "gallery"), "worktree-gallery", nil); got != dir {
		t.Fatalf("strandedChat = (%q, %q), want %q", got, cwd, dir)
	}
}

func TestAdoptChatCopiesAndNeverOverwrites(t *testing.T) {
	store, base := t.TempDir(), t.TempDir()
	old := filepath.Join(base, "workshop", "gallery")
	from := plantChat(t, store, old, "worktree-gallery")
	// Claude keeps a per-session subdirectory beside the file; the copy is a
	// tree, not a file.
	if err := os.MkdirAll(filepath.Join(from, "s1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "s1", "note.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(store, projEnc(filepath.Join(base, "acme-alpha", "gallery")))

	if err := adoptChat(from, to); err != nil {
		t.Fatalf("adoptChat: %v", err)
	}
	if _, err := os.Stat(filepath.Join(to, "s1.jsonl")); err != nil {
		t.Fatalf("the transcript did not arrive: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(to, "s1", "note.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("the session directory did not arrive: %q %v", b, err)
	}
	// A copy: the original is still where it was, so a wrong guess costs a
	// `rm -rf` and never a conversation.
	if _, err := os.Stat(filepath.Join(from, "s1.jsonl")); err != nil {
		t.Fatalf("the original was moved, not copied: %v", err)
	}
	// Nothing staged is left behind beside it.
	ents, err := os.ReadDir(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		if strings.HasPrefix(ent.Name(), ".scruff-adopting-") {
			t.Fatalf("staging directory left behind: %s", ent.Name())
		}
	}
	// And a second run never writes over the conversation that is now there.
	if err := adoptChat(from, to); err == nil {
		t.Fatal("adoptChat wrote over a conversation that already existed")
	}
}
