package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The doctor's parsers read OTHER tools' output, which is exactly the surface a
// black-box suite covers badly: the shim in test/scruff.bats emits one shape
// each, and the shapes that break these are the ones a real `gh` or `git`
// emitted two releases ago on somebody else's machine.

// versionToken has to find the number without knowing which tool wrote the line,
// because every one of these spells the preamble differently.
func TestVersionTokenAcrossRealShapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"git version 2.51.0", "2.51.0"},
		{"git version 2.39.5 (Apple Git-154)", "2.39.5"},
		{"gh version 2.63.2 (2026-01-01)\nhttps://github.com/cli/cli/releases", "2.63.2"},
		{"", ""},
		// No number anywhere: keep the line rather than invent a version.
		{"some tool, unversioned", "some tool, unversioned"},
	}
	for _, c := range cases {
		if got := versionToken(c.in); got != c.want {
			t.Errorf("versionToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// `gh auth status` changed its wording — "Logged in to github.com as <user>"
// became "… account <user>" — and both are still in the wild.
func TestGhAccountReadsBothWordings(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  ✓ Logged in to github.com account octocat (keyring)", "octocat"},
		{"  ✓ Logged in to github.com as octocat (oauth_token)", "octocat"},
		{"You are not logged into any GitHub hosts.", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := ghAccount(c.in); got != c.want {
			t.Errorf("ghAccount(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The nullable rule, rendered: a size scruff could not measure must not print as
// a number, because "0 B" is a claim about the disk and null is the absence of
// one. json.go's header is the bug this rule came from.
func TestHumanBytesNullIsNotZero(t *testing.T) {
	if got := humanBytes(nil); got != "?" {
		t.Errorf("an unmeasured size rendered as %q — null must never read as a number", got)
	}
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1000, "1.0 kB"},
		{2_202_124_288, "2.2 GB"},
	} {
		n := c.in
		if got := humanBytes(&n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The reflink probe tests the filesystem the base will live on, which on a fresh
// install does not exist yet — so it walks up to the nearest directory that
// does rather than reporting "not determined" on every new machine.
func TestNearestExistingWalksUpToARealDirectory(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "not", "created", "yet")
	if got := nearestExisting(deep); got != root {
		t.Errorf("nearestExisting(%q) = %q, want %q", deep, got, root)
	}
	if got := nearestExisting(root); got != root {
		t.Errorf("an existing directory must answer for itself: got %q", got)
	}
	// A file is not a directory to probe in — keep walking.
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := nearestExisting(file); got != root {
		t.Errorf("nearestExisting on a file = %q, want its directory %q", got, root)
	}
}

// "1 lane(s)" is the tell that nobody read the output, and this output goes into
// bug reports.
func TestPlural(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{{0, "0 lanes"}, {1, "1 lane"}, {2, "2 lanes"}} {
		if got := plural(c.n, "lane"); got != c.want {
			t.Errorf("plural(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// SPEC.md §4's two remote situations. Both are silent everywhere else in scruff
// — identity resolves, every command runs, nothing errors — so a doctor that
// says "none — nothing here needs a human" in either is the whole bug.
func TestRepoFindingsNamesBothRemoteSituations(t *testing.T) {
	fork := &diagRepo{
		Slug: "julienmartel/kilo",
		Main: "/checkouts/kilo",
		Remotes: []diagRemote{
			{Name: "origin", Slug: "julienmartel/kilo"},
			{Name: "upstream", Slug: "antirez/kilo"},
		},
	}
	got := repoFindings(fork, false)
	if len(got) != 1 || got[0].Kind != "fork-remotes" {
		t.Fatalf("a fork's disagreeing remotes went unreported: %+v", got)
	}
	// The finding has to name the remote scruff is NOT asking; "there is an
	// ambiguity" without the other slug leaves the reader with no next move.
	if !strings.Contains(got[0].Detail, "antirez/kilo") {
		t.Errorf("the finding never named the other remote: %q", got[0].Detail)
	}

	// Same slug through a different protocol is a mirror, not a disagreement —
	// ParseSlug normalises scp-style and https to one identity, and a finding
	// here would fire on half the repos in the world.
	mirror := &diagRepo{
		Slug: "acme/alpha",
		Main: "/checkouts/alpha",
		Remotes: []diagRemote{
			{Name: "origin", Slug: "acme/alpha"},
			{Name: "gh", Slug: "acme/alpha"},
		},
	}
	if got := repoFindings(mirror, false); len(got) != 0 {
		t.Errorf("two spellings of one remote read as a fork: %+v", got)
	}

	bare := &diagRepo{Slug: "local/alpha", Main: "/checkouts/alpha", Remotes: []diagRemote{}}
	got = repoFindings(bare, true)
	if len(got) != 1 || got[0].Kind != "no-remote" {
		t.Fatalf("a remote-less repo went unreported: %+v", got)
	}

	// No remedy anywhere may send a reader to `git worktree remove` — invariant
	// 2 defeated from the outside, by a report that was meant to help.
	for _, f := range append(repoFindings(fork, false), repoFindings(bare, true)...) {
		if strings.Contains(f.Remedy, "git worktree") {
			t.Errorf("a remedy pointed at raw git worktree surgery: %q", f.Remedy)
		}
	}
}

// The remotes line is the one place a URL could reach a report that exists to
// be pasted into a public issue, so it carries slugs and never URLs.
func TestRemotesLineIsTheDisagreementAndNeverAURL(t *testing.T) {
	fork := &diagRepo{
		Slug: "julienmartel/kilo",
		Remotes: []diagRemote{
			{Name: "origin", Slug: "julienmartel/kilo"},
			{Name: "upstream", Slug: "antirez/kilo"},
		},
	}
	if got, want := remotesLine(fork), "origin julienmartel/kilo · upstream antirez/kilo"; got != want {
		t.Errorf("remotesLine = %q, want %q", got, want)
	}
	bare := &diagRepo{Slug: "local/alpha", Remotes: []diagRemote{}}
	if got := remotesLine(bare); !strings.Contains(got, "local/alpha") {
		t.Errorf("a remote-less repo's line didn't name the fallback identity: %q", got)
	}
}
