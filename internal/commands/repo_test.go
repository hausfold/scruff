package commands

import "testing"

// seedSlug pins a main checkout's slug without a git repo behind it — the memo
// is the seam, and these are pure decisions about a slug once you have one.
func seedSlug(t *testing.T, main, slug string) string {
	t.Helper()
	repoSlugs.Store(main, slug)
	t.Cleanup(func() { repoSlugs.Delete(main) })
	return main
}

// The name half comes off the SLUG, never off the flattened key, because
// cutting `gitlab-org-cli` at its last `-` gives `cli` and cutting it at the
// first gives `org-cli`, and neither knows which one is the owner.
func TestRepoKeyAndName(t *testing.T) {
	for _, c := range []struct{ main, slug, key, name string }{
		{"/x/api", "antirez/kilo", "antirez-kilo", "kilo"},
		{"/y/api", "gitlab-org/cli", "gitlab-org-cli", "cli"},
		{"/z/hausfold.co", "hausfold/hausfold.co", "hausfold-hausfold.co", "hausfold.co"},
		{"/w/nowhere", "local/nowhere", "local-nowhere", "nowhere"},
	} {
		seedSlug(t, c.main, c.slug)
		if got := repoKey(c.main); got != c.key {
			t.Errorf("repoKey(%s) = %q, want %q", c.slug, got, c.key)
		}
		if got := repoName(c.main); got != c.name {
			t.Errorf("repoName(%s) = %q, want %q", c.slug, got, c.name)
		}
	}
}

// The column shows the name until two repos on screen answer to it, and then
// both grow — not just the newcomer, which would read as the same repo twice
// under two spellings.
func TestRepoCellsGrowOnlyWhereTheyCollide(t *testing.T) {
	kilo := seedSlug(t, "/orgA/api", "antirez/kilo")
	cli := seedSlug(t, "/orgB/api", "gitlab-org/cli")
	one := seedSlug(t, "/one/api", "orgA/api")
	two := seedSlug(t, "/two/api", "orgB/api")

	cells := repoCells([]Entry{{Main: kilo}, {Main: cli}})
	if cells[kilo] != "kilo" || cells[cli] != "cli" {
		t.Errorf("two repos that need no telling apart got %q and %q", cells[kilo], cells[cli])
	}
	cells = repoCells([]Entry{{Main: one}, {Main: two}, {Main: kilo}})
	if cells[one] != "orgA-api" || cells[two] != "orgB-api" {
		t.Errorf("two repos genuinely named api got %q and %q", cells[one], cells[two])
	}
	if cells[kilo] != "kilo" {
		t.Errorf("an uninvolved repo grew too: %q", cells[kilo])
	}
}

// Every spelling a listing prints resolves, and so does the basename a trill
// banner's focus action still carries. What must never happen is the basename
// picking ONE of two repos that share it: both match, and matchLane refuses an
// ambiguous match rather than guessing.
func TestRepoMatchesEverySpellingItPrints(t *testing.T) {
	kilo := seedSlug(t, "/orgA/api", "antirez/kilo")
	cli := seedSlug(t, "/orgB/api", "gitlab-org/cli")
	for _, want := range []string{"", "antirez-kilo", "antirez", "kilo", "api", "ap"} {
		if !repoMatches(kilo, want) {
			t.Errorf("repoMatches(antirez/kilo at /orgA/api, %q) = false", want)
		}
	}
	if !repoMatches(cli, "api") {
		t.Fatal("the other api must match too, or the basename silently picks one")
	}
	for _, want := range []string{"kilo", "antirez"} {
		if repoMatches(cli, want) {
			t.Errorf("gitlab-org/cli answered to %q", want)
		}
	}
}
