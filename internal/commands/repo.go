package commands

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/hausfold/scruff/internal/gitx"
)

// This file holds the one answer to "which repo is this?", because scruff used
// to have two spellings of it and they disagreed.

// repoSlug is a repo's identity: `owner/name` off its remote (SPEC.md §4).
//
// Pointedly NOT the directory basename, which is what the bucket, the listing's
// repo cell and the `<repo>/<name>` selector all used to be. Two checkouts
// named `api` under different orgs is the common case rather than the exotic
// one, and on the basename those two shared one bucket under $BASE, printed the
// same repo cell, and answered to the same selector — so `scruff api/dup`
// resolved to whichever lane discover reached first and rebuilt THAT checkout,
// in the other org, with nothing the user could type to say which they meant.
// Reproduced with antirez/kilo and gitlab-org/cli cloned side by side as
// `orgA/api` and `orgB/api` (2026-09-12).
//
// No remote to take an identity from is `local/<basename>` — the degraded
// answer `--json` has always reported, honest about why the identity is weak.
// `scruff doctor` is where "add a remote" gets said.
func repoSlug(main string) string {
	if main == "" {
		return ""
	}
	if v, ok := repoSlugs.Load(main); ok {
		return v.(string)
	}
	slug, err := gitx.RemoteSlug(main)
	if err != nil || slug == "" {
		slug = "local/" + filepath.Base(main)
	}
	repoSlugs.Store(main, slug)
	return slug
}

// repoSlugs memoises the answer for one invocation, because deriving it shells
// out to git and every listing asks it once per lane. A remote URL that changes
// under a long-lived `scruff watch` keeps the slug it started with, which costs
// a stale cell in a listing and nothing else.
var repoSlugs sync.Map // main checkout path → owner/name

// repoKey is the slug with its slash flattened (`hausfold/scruff` →
// `hausfold-scruff`): the bucket directory under $BASE, and the canonical
// `<repo>` half of a selector.
//
// Flattened rather than nested because a selector is split on its slash — a
// bucket of `hausfold/scruff` would make `<repo>/<name>` three segments, and
// every consumer of §2.0's matcher would have to learn that the repo half can
// carry one. It also keeps $BASE exactly two levels deep, which is what
// discover globs for live checkouts.
//
// It is NOT the lane KEY. `scruff/<repo>/<lane>` — what askKey writes and what
// haus renders as a zmx session name — stays on the basename on purpose; see
// laneID for why that one may not move alone.
func repoKey(main string) string { return strings.ReplaceAll(repoSlug(main), "/", "-") }

// repoName is the slug's name half — `scruff` out of `hausfold/scruff`. Taken
// off the SLUG rather than by cutting the key at its last `-`, because that cut
// cannot tell `gitlab-org/cli` from `gitlab/org-cli`.
func repoName(main string) string {
	slug := repoSlug(main)
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		return slug[i+1:]
	}
	return slug
}

// repoCells is the repo column's spelling for one listing: the repo's NAME
// where that is unique among the lanes on screen, its full key where it is not.
//
// The column is the one place the full key is the wrong answer. It is what the
// user reads to decide what to type, and a table where every row spends nine
// characters on `hausfold-` cuts to `hausf…` in a narrow pane — strictly less
// than the basename it replaced told you. Shortening is safe because it is
// PRESENTATION, not identity: the bucket is always the key, and repoMatches
// answers to either spelling always, so a cell is still something you can paste
// straight back. The ambiguous case — two repos genuinely both named `api` — is
// exactly where the cells grow to `orgA-api` and `orgB-api` and stay legible.
func repoCells(entries []Entry) map[string]string {
	mains := map[string]bool{}
	for _, e := range entries {
		mains[e.Main] = true
	}
	shared := map[string]int{}
	for m := range mains {
		shared[repoName(m)]++
	}
	cells := make(map[string]string, len(mains))
	for m := range mains {
		if shared[repoName(m)] > 1 {
			cells[m] = repoKey(m)
			continue
		}
		cells[m] = repoName(m)
	}
	return cells
}

// repoMatches answers the `<repo>` half of a lane selector: exact first, then a
// unique prefix, per SPEC.md §2.0. It lives here rather than as three `==`
// scattered across drop, focus and park — which were already spelled
// differently from one another before any of this.
//
// Three forms match. The key and the name are the two spellings a listing ever
// prints. The basename is compatibility: it is what `laneID` still writes into
// a trill banner's `scruff focus <repo>/<name>` action, and it is what anyone
// typing from muscle memory reaches for. Accepting extra spellings can never
// resolve to the WRONG repo — two repos that answer to the same word both
// match, and an ambiguous match is refused with every lane named, which is the
// whole bug being fixed here.
func repoMatches(main, want string) bool {
	if want == "" {
		return true
	}
	for _, form := range []string{repoKey(main), repoName(main), filepath.Base(main)} {
		if strings.HasPrefix(form, want) {
			return true
		}
	}
	return false
}
