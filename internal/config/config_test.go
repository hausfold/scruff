package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func load(t *testing.T, body string) (*Config, []string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "scruff"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scruff", "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load()
}

func TestLoadNoFileIsEveryDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, warnings := Load()
	if cfg.Agent != "" || len(cfg.Hooks) != 0 || len(warnings) != 0 {
		t.Fatalf("a missing config must be silent and empty, got %+v / %v", cfg, warnings)
	}
	if cfg.Defined(HookLanded) {
		t.Fatal("no config must mean no hooks")
	}
}

func TestLoadParsesKeysAndHooks(t *testing.T) {
	cfg, warnings := load(t, `
# the client a new worktree opens in
agent = "codex"   # trailing comment

[hooks]
resume   = "/usr/local/bin/scruff-resume"
landed   = ["/usr/local/bin/scruff-landed", "--strict", "a,b"]
preserve = '/usr/local/bin/scruff-preserve'
`)
	if len(warnings) != 0 {
		t.Fatalf("clean config produced warnings: %v", warnings)
	}
	if cfg.Agent != "codex" {
		t.Fatalf("Agent = %q, want codex (the trailing comment must not survive)", cfg.Agent)
	}
	if got := cfg.Hooks["resume"]; !reflect.DeepEqual(got, []string{"/usr/local/bin/scruff-resume"}) {
		t.Fatalf("a bare string hook must become a one-element argv, got %q", got)
	}
	want := []string{"/usr/local/bin/scruff-landed", "--strict", "a,b"}
	if got := cfg.Hooks["landed"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("Hooks[landed] = %q, want %q (a comma inside quotes is data)", got, want)
	}
	if !cfg.Defined(HookPreserve) {
		t.Fatal("single-quoted hook value was dropped")
	}
}

// A typo in the config must cost you the line, not the pane. scruff runs on the
// path of every worktree open, so a hard parse failure is a machine that won't
// spawn.
func TestLoadBadLineWarnsAndContinues(t *testing.T) {
	cfg, warnings := load(t, "agent = codex\n\n[hooks]\nlanded = \"/bin/true\"\n")
	if len(warnings) != 1 {
		t.Fatalf("want exactly one warning for the unquoted value, got %v", warnings)
	}
	if cfg.Agent != "" {
		t.Fatalf("an unparseable value must leave the key unset, got %q", cfg.Agent)
	}
	if !cfg.Defined(HookLanded) {
		t.Fatal("a bad line must not stop the rest of the file being read")
	}
}

// ── the hook protocol ────────────────────────────────────────────────────────

func hook(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Config{Hooks: map[string][]string{"landed": {path}}}
}

func TestAskExitCodes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    Answer
		refused bool
		warns   bool
	}{
		{"0 is yes", "#!/bin/sh\nexit 0\n", Yes, false, false},
		{"1 is no", "#!/bin/sh\nexit 1\n", No, false, false},
		{"2 is a safety refusal", "#!/bin/sh\nexit 2\n", No, true, false},
		{"3 is no opinion", "#!/bin/sh\nexit 3\n", Defer, false, false},
		// The accidental deaths. A hook killed by a typo'd command (127) or a
		// lost +x bit must never read as an opinion, and must never be silent.
		{"127 defers loudly", "#!/bin/sh\nexit 127\n", Defer, false, true},
		{"a crash defers loudly", "#!/bin/sh\nkill -SEGV $$\n", Defer, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := hook(t, tc.body).Ask("landed", map[string]string{"branch": "worktree-x"})
			if res.Answer != tc.want || res.Refused != tc.refused {
				t.Fatalf("Answer = %v refused=%v, want %v/%v", res.Answer, res.Refused, tc.want, tc.refused)
			}
			if (res.Warning != "") != tc.warns {
				t.Fatalf("Warning = %q, want warning: %v", res.Warning, tc.warns)
			}
		})
	}
}

func TestAskNoHookDefers(t *testing.T) {
	cfg := &Config{}
	if res := cfg.Ask("landed", nil); res.Answer != Defer || res.Warning != "" {
		t.Fatalf("an unconfigured hook must defer silently, got %+v", res)
	}
}

// Both channels carry the same situation, because a hook may be a program with
// a JSON parser or three lines of shell, and neither should have to become the
// other.
func TestAskDeliversPayloadOnStdinAndInEnv(t *testing.T) {
	cfg := hook(t, `#!/bin/sh
read -r body
[ "$SCRUFF_HOOK" = "landed" ] || { echo "SCRUFF_HOOK=$SCRUFF_HOOK" >&2; exit 1; }
[ "$SCRUFF_BRANCH" = "worktree-x" ] || { echo "SCRUFF_BRANCH=$SCRUFF_BRANCH" >&2; exit 1; }
[ "$SCRUFF_BASE_BRANCH" = "main" ] || { echo "SCRUFF_BASE_BRANCH=$SCRUFF_BASE_BRANCH" >&2; exit 1; }
case "$body" in *'"branch":"worktree-x"'*) ;; *) echo "stdin=$body" >&2; exit 1 ;; esac
echo '{"via": "release-train", "confidence": "certain"}'
exit 0
`)
	res := cfg.Ask("landed", map[string]string{"branch": "worktree-x", "base": "main"})
	if res.Answer != Yes {
		t.Fatalf("Answer = %v, want Yes (the hook asserts its own inputs and exits 1 on mismatch)", res.Answer)
	}
	if res.Data["via"] != "release-train" {
		t.Fatalf("stdout JSON not carried through: %+v", res.Data)
	}
}

// A predicate that prints prose has still answered with its exit code. Parsing
// is an enrichment, never a requirement.
func TestAskIgnoresNonJSONStdout(t *testing.T) {
	res := hook(t, "#!/bin/sh\necho looks landed to me\nexit 0\n").Ask("landed", nil)
	if res.Answer != Yes || res.Data != nil {
		t.Fatalf("got %+v, want Yes with no data", res)
	}
}

// A hook's env must never hand a lane's lifecycle state to scruff as its state
// DIRECTORY. It did once: the open seam exported SCRUFF_STATE=live, the pane it
// spawned inherited it, and every scruff run in that pane wrote its machine-global
// state to the relative path "live" under the cwd — routinely a git checkout.
func TestHookEnvRenamesStateAwayFromScruffsOwnStateDir(t *testing.T) {
	env := hookEnv("open", map[string]string{
		"state": "live",
		"agent": "claude",
		"base":  "main",
		"path":  "/tmp/lane",
	})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	// The collision is a property of the NAME and not of the spelling, so the
	// test pins the spelling the collision is documented under.
	for _, k := range []string{"SCRUFF_STATE"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s is the state DIRECTORY — a hook must not set it; got %q", k, got[k])
		}
	}
	if got["SCRUFF_LANE_STATE"] != "live" {
		t.Errorf("SCRUFF_LANE_STATE = %q, want %q", got["SCRUFF_LANE_STATE"], "live")
	}
	for _, k := range []string{"SCRUFF_AGENT"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s is the one-invocation client override — a hook must not set it; got %q", k, got[k])
		}
	}
	if got["SCRUFF_LANE_AGENT"] != "claude" {
		t.Errorf("SCRUFF_LANE_AGENT = %q, want %q", got["SCRUFF_LANE_AGENT"], "claude")
	}
	if got["SCRUFF_BASE_BRANCH"] != "main" {
		t.Errorf("SCRUFF_BASE_BRANCH = %q, want %q", got["SCRUFF_BASE_BRANCH"], "main")
	}
	if got["SCRUFF_PATH"] != "/tmp/lane" {
		t.Errorf("SCRUFF_PATH = %q, want %q", got["SCRUFF_PATH"], "/tmp/lane")
	}
}

// Every hook variable goes out under exactly one spelling. The old-name pair
// shipped through 0.5.0 ended at 1.1.0; this test is now the proof it ended —
// any reappearance of HOLT_* in the hook environment fails here.
func TestHookEnvSpeaksOneName(t *testing.T) {
	env := hookEnv("open", map[string]string{
		"name":  "lane",
		"state": "live",
		"agent": "claude",
		"base":  "main",
	})
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOLT_") {
			t.Errorf("the hook environment still speaks the old name: %s", kv)
		}
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for scruffKey, want := range map[string]string{
		"SCRUFF_HOOK":        "open",
		"SCRUFF_NAME":        "lane",
		"SCRUFF_LANE_STATE":  "live",
		"SCRUFF_LANE_AGENT":  "claude",
		"SCRUFF_BASE_BRANCH": "main",
	} {
		if got[scruffKey] != want {
			t.Errorf("%s = %q, want %q", scruffKey, got[scruffKey], want)
		}
	}
}

// name_max is a byte count, spelled as a string because that is all this
// parser reads. A value that is not one leaves lane names uncapped and says so
// — the same "a typo must not stop a pane opening" rule the rest of Load keeps.
func TestLoadNameMax(t *testing.T) {
	cfg, warnings := load(t, "name_max = \"45\"\n")
	if cfg.NameMax != 45 {
		t.Fatalf("name_max = %d, want 45", cfg.NameMax)
	}
	if len(warnings) != 0 {
		t.Fatalf("a good name_max must be silent, got %v", warnings)
	}

	for _, bad := range []string{"forty-five", "-1"} {
		cfg, warnings := load(t, "name_max = \""+bad+"\"\n")
		if cfg.NameMax != 0 {
			t.Fatalf("name_max = %q must leave the cap unset, got %d", bad, cfg.NameMax)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "name_max") {
			t.Fatalf("name_max = %q must warn by name, got %v", bad, warnings)
		}
	}

	cfg, _ = load(t, "agent = \"codex\"\n")
	if cfg.NameMax != 0 {
		t.Fatalf("no name_max is no cap, got %d", cfg.NameMax)
	}

	// The natural TOML spelling of a byte count is a bare integer, and a
	// hand-written config will say it that way. parseValue reads strings and
	// lists of strings only, so this key is read before it.
	cfg, warnings = load(t, "name_max = 46\n")
	if cfg.NameMax != 46 {
		t.Fatalf("a bare integer name_max = %d, want 46", cfg.NameMax)
	}
	if len(warnings) != 0 {
		t.Fatalf("a bare integer must be silent, got %v", warnings)
	}
}
