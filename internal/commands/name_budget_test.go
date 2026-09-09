package commands

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The budget is what the repo leaves over, because the key a backend carries is
// `scruff/<repo>/<lane>` and the repo is half of it. These are the numbers this
// machine's zmx backend actually produces, so a change to askKeyPrefix that
// moves them fails here rather than at the next lane that will not open.
func TestLaneNameBudgetIsWhatTheRepoLeaves(t *testing.T) {
	e := envWith(t, "name_max = \"46\"\n")
	for _, c := range []struct {
		main string
		want int
	}{
		{"/Users/x/code/workshop/hausfold.co", 27}, // 46 - len("scruff/") - 11 - 1
		{"/Users/x/code/workshop/nix", 35},
		{"/Users/x/code/joshua-whatman", 24},
	} {
		if got := e.laneNameBudget(c.main); got != c.want {
			t.Errorf("laneNameBudget(%s) = %d, want %d", c.main, got, c.want)
		}
		key := askKey(laneID(c.main, strings.Repeat("a", c.want)), nil)
		if len(key) != 46 {
			t.Errorf("a name at the budget makes a %d-byte key for %s, want exactly 46", len(key), c.main)
		}
	}
}

// No `name_max` is every install that came before this key, and a repo whose own
// name eats the cap gets no budget rather than an impossible one.
func TestLaneNameBudgetOffByDefault(t *testing.T) {
	if got := envWith(t, "").laneNameBudget("/Users/x/code/haus"); got != 0 {
		t.Fatalf("no name_max must be no cap, got %d", got)
	}
	if got := envWith(t, "name_max = \"12\"\n").laneNameBudget("/Users/x/code/homebrew-tap"); got != 0 {
		t.Fatalf("a repo that overruns the cap on its own must not cap names, got %d", got)
	}
}

// A name scruff chose is trimmed at a word boundary; a name the caller typed is
// refused, because a lane is a branch and a checkout too.
func TestFitNameTrimsOnAWordBoundary(t *testing.T) {
	for _, c := range []struct {
		in, want string
		budget   int
	}{
		{"docs-displays-expansion-slim", "docs-displays-expansion", 27},
		{"docs-displays-expansion-slim", "docs-displays-expansion", 23},
		{"docs-displays-expansion-slim", "docs-displays", 22},
		{"docs-displays-expansion-slim", "docs-displays-expansion-slim", 0},
		{"verylongsinglewordname", "verylongsinglew", 15},
	} {
		if got := fitName(c.in, c.budget); got != c.want {
			t.Errorf("fitName(%q, %d) = %q, want %q", c.in, c.budget, got, c.want)
		}
	}
}

func TestRefuseLongNameNamesTheNumbers(t *testing.T) {
	e := envWith(t, "name_max = \"46\"\n")
	main := "/Users/x/code/workshop/hausfold.co"

	if err := e.refuseLongName(main, "docs-displays-expansion-slim"); err == nil {
		t.Fatal("a 28-character name in hausfold.co must be refused")
	} else {
		msg := err.Error()
		for _, want := range []string{"docs-displays-expansion-slim", "28", "27", "hausfold.co"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal must say %q — got %q", want, msg)
			}
		}
	}
	if err := e.refuseLongName(main, "docs-displays-expansion"); err != nil {
		t.Fatalf("a name inside the budget must be taken as typed: %v", err)
	}
	if err := envWith(t, "").refuseLongName(main, strings.Repeat("a", 200)); err != nil {
		t.Fatalf("no name_max must refuse nothing: %v", err)
	}
}

// The namer builds to fit rather than being cut afterwards, so the tighter of
// its own shape rule and the machine's budget is what reaches sanitizeName.
func TestNamerBuildsToTheSmallerBudget(t *testing.T) {
	e := envWith(t, "name_max = \"46\"\n")
	if got := e.nameLenFor("/Users/x/code/workshop/nix"); got != namerMaxLen {
		t.Fatalf("a roomy repo keeps the namer's own %d, got %d", namerMaxLen, got)
	}
	tight := "/Users/x/code/a-repo-with-a-long-name"
	want := e.laneNameBudget(tight)
	if got := e.nameLenFor(tight); got != want || got >= namerMaxLen {
		t.Fatalf("a tight repo must hand the namer its budget %d, got %d", want, got)
	}
	if got := sanitizeName("docs displays expansion", nil, 22); got != "docs-displays" {
		t.Fatalf("sanitizeName must stop on a whole word inside the budget, got %q", got)
	}
}

// The collision suffix counts against the budget, and what happens then depends
// on whose name it is. Trimming a TYPED base to make room would rename someone's
// branch behind their back — the exact thing refuseLongName exists to stop, and
// worse here for landing a byte away from a different lane's name.
func TestFitNameNeverEatsAWordToMakeRoomForASuffix(t *testing.T) {
	// A chosen name gives the bytes back off its own base.
	if got := fitName("docs-displays-expansion", 21); got != "docs-displays" {
		t.Errorf("a chosen base must shorten for its suffix, got %q", got)
	}
	// A budget tighter than the suffix still yields a name rather than "" — a
	// lane still needs one, and `worktree--2` is not it.
	for _, c := range []struct {
		in     string
		budget int
	}{
		{"docs-displays", 1}, {"ab-cd", 3}, {"a-b-c-d", 2},
	} {
		if got := fitName(c.in, c.budget); got == "" {
			t.Errorf("fitName(%q, %d) = \"\" — a lane still needs a name", c.in, c.budget)
		}
	}
	// The one input with nothing to keep. freeName answers it by choosing again
	// rather than by asking fitName for something that isn't there.
	if got := fitName("---", 2); got != "" {
		t.Errorf("fitName(\"---\", 2) = %q, want the empty string freeName tests for", got)
	}
}

// The budget is bytes because the ceiling is a socket path, but a cut lands on a
// rune. A derived name — `scruff child` inheriting its parent's — is whatever a
// person once typed, and half a rune in a branch name is not recoverable.
func TestFitNameCutsOnARuneBoundary(t *testing.T) {
	for _, c := range []struct {
		in     string
		budget int
	}{
		{"日本語のレーン", 7}, {"aöööööö", 4}, {"café-très-longue", 9},
	} {
		got := fitName(c.in, c.budget)
		if !utf8.ValidString(got) {
			t.Errorf("fitName(%q, %d) = %q — not valid UTF-8", c.in, c.budget, got)
		}
		if len(got) > c.budget {
			t.Errorf("fitName(%q, %d) = %q, %d bytes — over budget", c.in, c.budget, got, len(got))
		}
	}
}
