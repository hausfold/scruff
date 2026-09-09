package commands

import (
	"strings"
	"testing"
)

// The gate. Everything sanitizeName lets through becomes a branch name and a
// path segment, so the interesting cases are all the ones that must NOT.
func TestSanitizeName(t *testing.T) {
	own := []string{"scruff", "hausfold"}
	cases := []struct {
		in, want, why string
	}{
		{"mobile-nav-jitter", "mobile-nav-jitter", "the ordinary answer"},
		{"  fix-mobile  \n", "fix-mobile", "surrounding whitespace"},
		{"`draft-pr-grey`", "draft-pr-grey", "models fence one-word answers"},
		{"\"reap-ledger-race\"", "reap-ledger-race", "and quote them"},
		{"fix mobile nav", "fix-mobile-nav", "spaces instead of hyphens"},
		{"Mobile-Nav-Jitter", "mobile-nav-jitter", "case is not the model's to choose"},
		{"scruff-reap-ledger", "reap-ledger", "the repo names itself"},
		{"hausfold-scruff-parking", "parking", "…in either half of the slug"},
		{"one-two-three-four-five", "one-two-three", "more than three words"},
		{"absolutely-magnificent-recalibration", "absolutely-magnificent", "the word that would break the length cap is dropped"},

		{"Here is the name:", "", "a preamble line is never a name"},
		{"fix-mobile — this captures the task", "", "an answer with commentary after it"},
		{"I'd call it fix-mobile.", "", "prose that happens to contain one"},
		{"", "", "an empty line"},
		{"   ", "", "a blank line"},
		{"-rf", "", "a flag"},
		{"--force", "", "a long flag"},
		{"..", "", "the parent directory"},
		{"../../etc/passwd", "", "a traversal"},
		{"/tmp/evil", "", "an absolute path"},
		{"a;rm -rf ~", "", "shell metacharacters"},
		{"worktree-thing$(whoami)", "", "a substitution"},
		{"scruff", "", "the repo and nothing else"},
		{"lane_name", "", "an underscore is not scruff's separator"},
		{"名前", "", "no non-ascii reaches a branch name"},
		{"42", "", "a bare number"},
		{"12-34", "", "…or several"},
		{"ab", "", "too short to mean anything"},
	}
	for _, c := range cases {
		if got := sanitizeName(c.in, own, namerMaxLen); got != c.want {
			t.Errorf("sanitizeName(%q) = %q, want %q — %s", c.in, got, c.want, c.why)
		}
	}
}

// A namer told to answer with one word sometimes answers with one word and a
// paragraph about it. The first line that IS a name wins; a preamble can never
// become one, because it is rejected whole rather than sanitized into a word.
func TestSlugFromReadsPastChatter(t *testing.T) {
	own := []string{"scruff"}
	cases := []struct{ out, want string }{
		{"mobile-nav-jitter\n", "mobile-nav-jitter"},
		{"Here is the name:\n\nmobile-nav-jitter\n", "mobile-nav-jitter"},
		{"mobile-nav-jitter\n\nThis captures the fix without naming the repo.\n", "mobile-nav-jitter"},
		{"\n\n  reap-ledger-race  \n", "reap-ledger-race"},
		{"I can't name this without more detail about what you want changed.\n", ""},
		{"", ""},
		{strings.Repeat("Sure! Here is my reasoning, step by step:\n", 20) + "late-answer-here\n", ""},
	}
	for _, c := range cases {
		if got := slugFrom(c.out, own, namerMaxLen); got != c.want {
			t.Errorf("slugFrom(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

// The taken names are in the request because scruff's own collision handling is a
// numeric suffix: two lanes named from similar tasks become `fix-mobile` and
// `fix-mobile-2`, which is correct and unreadable. A namer that can see the
// neighbours picks a different word instead.
func TestNamingRequestCarriesRepoTaskAndNeighbours(t *testing.T) {
	req := namingRequest("hausfold/scruff", "the bar shows a draft PR in the merged colour", []string{"tart-backend", "reap-ledger"})
	for _, want := range []string{
		"hausfold/scruff",
		"the bar shows a draft PR in the merged colour",
		"tart-backend, reap-ledger",
		"2 or 3 lowercase words",
	} {
		if !strings.Contains(req, want) {
			t.Errorf("the naming request never mentions %q:\n%s", want, req)
		}
	}
}

// A repo with no remote has no slug, and a lane with no neighbours has no list.
// Neither is a reason to skip naming, so neither line appears at all.
func TestNamingRequestWithoutRepoOrNeighbours(t *testing.T) {
	req := namingRequest("", "do the thing", nil)
	if strings.Contains(req, "The repo is") {
		t.Errorf("a repo with no remote must not claim to be named:\n%s", req)
	}
	if strings.Contains(req, "Already taken") {
		t.Errorf("an empty neighbour list must not be offered as one:\n%s", req)
	}
	if !strings.Contains(req, "do the thing") {
		t.Errorf("the task itself is missing:\n%s", req)
	}
}

// A handoff can be pages long. The objective is at the top, and the rest is
// tokens paid for nothing.
func TestClipTruncatesOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", 4000) // two bytes each: a naive cut lands mid-rune
	got := clip(long, namerMaxPrompt)
	if len(got) > namerMaxPrompt+len("\n\n(truncated)") {
		t.Errorf("clip returned %d bytes, over the cap", len(got))
	}
	if !strings.HasSuffix(got, "(truncated)") {
		t.Error("a clipped brief must say it was clipped, so the namer knows it is reading the top of one")
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Error("clip cut a rune in half")
	}
	if short := clip("  fix the notch  ", namerMaxPrompt); short != "fix the notch" {
		t.Errorf("clip(short) = %q, want it trimmed and otherwise untouched", short)
	}
}

func TestLastLineOfStderr(t *testing.T) {
	if got := lastLine("starting\nfailed to reach the api\n\n"); got != ": failed to reach the api" {
		t.Errorf("lastLine = %q, want the last non-empty line", got)
	}
	if got := lastLine("   \n\n"); got != "" {
		t.Errorf("lastLine(blank) = %q, want empty", got)
	}
	if got := lastLine(strings.Repeat("x", 300)); !strings.HasSuffix(got, "…") || len(got) > 140 {
		t.Errorf("a runaway stderr line must be bounded, got %d bytes", len(got))
	}
}
