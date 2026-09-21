package commands

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The porcelain reaching dirtyNote has been through gitx.Run's TrimSpace, so
// the first line of an unstaged-only change arrives one column short. Both
// shapes have to parse, and a rename's tail has to survive whole.
func TestPorcelainPath(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{" M modules/core/haus.sh", "modules/core/haus.sh"}, // as git prints it
		{"M modules/core/haus.sh", "modules/core/haus.sh"},  // after TrimSpace ate column 0
		{" D gone.txt", "gone.txt"},                         //
		{"D gone.txt", "gone.txt"},                          //
		{"?? untracked.txt", "untracked.txt"},               // both columns filled
		{"MM staged-and-not.txt", "staged-and-not.txt"},     //
		{"M  staged.txt", "staged.txt"},                     //
		{"R  old.txt -> new.txt", "old.txt -> new.txt"},     // the tail is kept
		{" M a file with spaces.txt", "a file with spaces.txt"},
		{`?? "caf\303\251.txt"`, "café.txt"}, // C-quoting is undone
	} {
		if got := porcelainPath(c.line); got != c.want {
			t.Errorf("porcelainPath(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

// The grace window's sentence is the only place a duration reaches a reader, and
// it is a line about whether to WAIT — so "4m13.2318s" is the wrong answer even
// though it is the true one.
func TestRoughly(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "under a minute"},
		{59 * time.Second, "under a minute"},
		{time.Minute, "1m"},
		{4*time.Minute + 13*time.Second, "4m"}, // floored: the window is a floor too
		{59*time.Minute + 59*time.Second, "59m"},
		{time.Hour, "1h"},
	} {
		if got := roughly(c.d); got != c.want {
			t.Errorf("roughly(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// laneAge dates the checkout by the `.git` pointer, and an unreadable one
// resolves to the YOUNGEST possible lane — which is what keeps it, like every
// other uncertainty in the sweep.
func TestLaneAgeUnreadableIsYoungest(t *testing.T) {
	if got := laneAge(filepath.Join(t.TempDir(), "no-such-checkout")); got != 0 {
		t.Errorf("laneAge of a missing checkout = %s, want 0 (and so inside the grace)", got)
	}
	if laneGrace <= 0 {
		t.Fatalf("laneGrace is %s — an unreadable checkout would be swept", laneGrace)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, ".git"), old, old); err != nil {
		t.Fatal(err)
	}
	if got := laneAge(dir); got < laneGrace {
		t.Errorf("laneAge of a three-hour-old checkout = %s, want at least %s", got, laneGrace)
	}
}
