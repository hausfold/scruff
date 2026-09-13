package commands

import (
	"os/exec"
	"strconv"
	"strings"

	"github.com/hausfold/scruff/internal/exitcode"
	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/ui"
)

// Reship pushes a branch that outran its merged PR, and opens the follow-up.
//
// The other half of the +N story. After a squash merge the forge deletes the
// head branch, so the commits a lane makes afterwards have no remote and no
// PR — `git push` alone re-creates the branch but leaves the work unreviewed and
// invisible. This does both, from the MAIN checkout, so it works whether the
// lane is live, parked, or long gone.
func (e *Env) Reship(want string) error {
	main, branch, err := e.reshipTarget(want)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return exitcode.Degradedf("gh is unavailable — install it, or push and open the PR by hand.")
	}
	slug, err := gitx.RemoteSlug(main)
	if err != nil || slug == "" {
		return exitcode.Usagef("that repo has no origin remote — nothing to push to.")
	}
	base := gitx.DefaultBranch(main)

	// "Nothing to ship" is the answer whenever the branch adds nothing to the
	// base — true both for a fully-landed branch and for one that never
	// diverged. Asked BEFORE the push, so a no-op can't leave a pushed branch
	// with no PR behind it.
	if countCommits(main, base+".."+branch) == 0 {
		return exitcode.Refusedf("'%s' has nothing the %s branch doesn't already have.", branch, base)
	}

	// reship's whole contract is "push the commits that outran a MERGED PR", and
	// a lane that never had a PR of any kind has outrun nothing. Without this the
	// verb went straight to the push and let the REMOTE be the one to say no —
	// which it does in raw git, with no guidance ("Permission to antirez/kilo.git
	// denied", "could not read Username for 'https://gitlab.com'"), and only
	// because those two remotes happened to refuse. On a repo the user can write
	// to, the push succeeds and origin grows a branch on a precondition that was
	// never true. An OPEN PR is the one other thing worth pushing to, so it
	// counts here too: that is the in-flight lane below, whose push IS the job.
	// Asked with the others, BEFORE the push, so a refusal can never leave a
	// pushed branch behind it.
	openURL := e.openPRFor(slug, branch)
	if merged, _ := e.mergedMapLookup(main, branch); merged == "" && openURL == "" {
		return exitcode.Refusedf(
			"'%s' has no merged PR to ship past — reship pushes the commits a lane made "+
				"after its PR merged, and this branch has no PR at all. "+
				"If it simply needs to go up, that is `git push -u origin %s` then `gh pr create`.",
			branch, branch,
		)
	}

	// A branch whose tip does not build on its own merged PR is not "ahead" of
	// it, it is STALE or SIDEWAYS: a second checkout of the same branch name
	// that never pulled, a rebase, an amend. Pushing it would recreate a
	// deleted remote branch and open a real PR whose diff reintroduces content
	// the merge already superseded — confusing at best, a step backward at
	// worst. Refuse and say why, rather than doing the wrong kind of "help".
	if _, _, diverged := e.postMergeAhead(main, branch); diverged {
		return exitcode.Refusedf(
			"'%s' has already-merged content, but its tip does not build on that merged PR "+
				"— this looks like a stale or sideways checkout, not new work. "+
				"If this really is new work, rebase onto the merged commit first. "+
				"If it's stale, the fix is removing the checkout, not reshipping it.",
			branch,
		)
	}

	ui.Say("pushing %s → origin (%s)", branch, slug)
	if _, err := gitx.Run(main, "push", "-u", "origin", branch); err != nil {
		return exitcode.Usagef("push failed — resolve it, then re-run: scruff reship (%v)", err)
	}

	// An OPEN PR already covers these commits; the push above was the whole job.
	// Answered from the lookup made before the push: a PR cannot open on a branch
	// between those two moments, and the question costs ~0.5 s of forge round-trip.
	if openURL != "" {
		ui.Say("an open PR already covers this branch — pushed to it: %s", openURL)
		return nil
	}

	ahead, prNum, _ := e.postMergeAhead(main, branch)
	title := gitx.Subject(main, branch)
	if title == "" {
		title = "follow-up on " + branch
	}

	url, err := ghCreatePR(slug, branch, base, title, reshipBody(main, base, branch, prNum))
	if err != nil {
		return exitcode.Usagef("gh pr create failed: %v", err)
	}
	// The listing reads open PRs through a 120 s disk cache. Drop this repo's
	// entry, or the very next `scruff` still shows the lane as +N and still says
	// "covered by no PR" about the PR whose URL we are about to print.
	e.forgetForge(openMapKey(slug))
	suffix := ""
	if ahead > 0 {
		suffix = " for the " + strconv.Itoa(ahead) + " commit(s) past the merge"
	}
	ui.Say("follow-up PR open%s: %s", suffix, url)
	return nil
}

// reshipTarget resolves which (main, branch) to reship: a named lane, or — with
// no name — the branch of the checkout we are standing in.
func (e *Env) reshipTarget(want string) (main, branch string, err error) {
	if want == "" {
		main, err = gitx.MainCheckout(e.Cwd)
		if err != nil {
			return "", "", exitcode.Usagef("not in a git repo — name a lane instead: scruff reship <name>")
		}
		branch = gitx.CurrentBranch(e.Cwd)
		if branch == "" {
			return "", "", exitcode.Refusedf("HEAD is detached — check out a branch first.")
		}
		return main, branch, nil
	}

	// The same resolver every other verb types lane names into — prefix
	// resolution included — with reship's own wording on its refusals.
	entry, err := e.matchLane(want, "scruff reship")
	if err != nil {
		return "", "", err
	}
	return entry.Main, entry.Branch, nil
}

// reshipBody is a PR body scruff can write HONESTLY: what this PR carries, and
// what it follows. The What / Why / Verify / Watch-out a reviewer is owed is
// prompted for, not faked — scruff did not write the code and has nothing true to
// say about why it exists.
func reshipBody(main, base, branch string, prNum int) string {
	after := ""
	if prNum > 0 {
		after = "PR #" + strconv.Itoa(prNum) + " "
	}
	log, _ := gitx.Run(main, "log", "--format=- %s", base+".."+branch)
	lines := gitx.Lines(log)
	if len(lines) > 20 {
		lines = lines[:20]
	}
	return "Commits on `" + branch + "` that landed after " + after + "merged:\n\n" +
		strings.Join(lines, "\n") +
		"\n\n_Opened by `scruff reship` — add What / Why / Verify / Watch-out._\n"
}

func (e *Env) openPRFor(slug, branch string) string {
	out, err := exec.Command("gh", "pr", "list", "-R", slug, "--head", branch,
		"--state", "open", "--limit", "1", "--json", "url", "--jq", ".[0].url // empty").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func ghCreatePR(slug, head, base, title, body string) (string, error) {
	out, err := exec.Command("gh", "pr", "create", "-R", slug,
		"--head", head, "--base", base, "--title", title, "--body", body).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return "", errorText(text, err)
	}
	// gh prints progress before the URL; the URL is the last line.
	if lines := gitx.Lines(text); len(lines) > 0 {
		return lines[len(lines)-1], nil
	}
	return text, nil
}

func countCommits(dir, rng string) int {
	out, err := gitx.Run(dir, "rev-list", "--count", rng)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0
	}
	return n
}

func errorText(text string, err error) error {
	if text != "" {
		return &plainError{text}
	}
	return err
}

type plainError struct{ msg string }

func (e *plainError) Error() string { return e.msg }
