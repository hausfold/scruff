package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/occupancy"
	"github.com/hausfold/scruff/internal/ui"
)

// The DIAGNOSE half of `scruff doctor` (SPEC.md §6.4). The other half — propose
// a `.scruff.toml` and `--write` it — is 0.2 work and waits on a per-repo config
// layer that does not exist yet; see doctor.go's dispatch for the refusal.
//
// Three properties shape everything below.
//
//  1. **It is a report, so it exits 0.** A finding is doctor working, not doctor
//     failing. Exit 3 would be defensible for "a signal was unavailable", and it
//     is wrong here: the absences ARE the diagnosis, so a machine with no `gh`
//     would exit non-zero on a perfectly healthy run and become unusable under
//     `set -e`. `--json` carries every finding as data for a script that wants to
//     gate on one. The migrate half keeps its own 2 and 3.
//
//  2. **It does not fix anything, and it barely touches anything.** `scruff` the
//     listing sweeps parked lanes as it goes; doctor deliberately does not — it
//     is the one command that looks without changing what it is looking at, which
//     is the whole reason it is safe to ask a stranger to paste its output into a
//     bug report. It reports what a sweep WOULD prune rather than pruning it.
//     The single exception is the reflink probe, which writes two bytes into a
//     temp dir under the base and removes them: the question is "can THIS
//     filesystem clone", and nothing but trying it on that filesystem answers it.
//
//  3. **The output is meant to be pasted.** It goes to stdout (the report is what
//     the user ran the command for — SPEC.md §2.3's rule for the listing table,
//     same reasoning), it is plain aligned text rather than a width-budgeted
//     table, and every line is short enough to survive a GitHub issue box. The
//     version, schema and platform on line one are there because they are the
//     three things every bug report is missing.

// diagnosis is the whole report, and the shape of `scruff doctor --json`.
//
// It shares the frozen envelope's HEADER — `scruff`, `schema`, `warnings` — so a
// consumer can version-check any scruff payload the same way (SPEC.md §2.2). It
// deliberately does NOT carry a `lanes` key: there, `lanes` is an array of lane
// objects, and reusing that name for doctor's counts would be a meaning change
// in the one field name the frozen contract is most specific about. The counts
// live under `summary` instead, and every other key here is new, so nothing a
// consumer already reads can move underneath it.
//
// The nullable rule is json.go's, applied to a new set of unknowables: a *bool
// that is null means scruff could not determine the fact, which is categorically
// different from determining that it is false. `forge.authenticated` is null
// with no `gh` installed; `reflink.supported` is null when there is no directory
// to test against; `repo` is null when doctor was not run inside a git repo;
// `disk.bytes` is null when the walk failed. None of those are `false`.
type diagnosis struct {
	Scruff      string          `json:"scruff"`
	Schema      int             `json:"schema"`
	Base        diagBase        `json:"base"`
	Environment diagEnvironment `json:"environment"`
	Repo        *diagRepo       `json:"repo"`
	Summary     diagSummary     `json:"summary"`
	Findings    []diagFinding   `json:"findings"`
	Disk        diagDisk        `json:"disk"`
	Warnings    []string        `json:"warnings"`
}

type diagBase struct {
	Path     string `json:"path"`
	Resolved string `json:"resolved"` // SCRUFF_BASE | CLAUDE_WT_BASE | default | legacy | elsewhere
	Registry string `json:"registry"`
	Rows     int    `json:"registry_rows"`
	State    string `json:"state_dir"`
	// Migrate is true while the LEGACY base still holds the registry — the one
	// base fact that has a verb attached to it (`doctor --migrate-base`).
	Migrate bool `json:"migrate_pending"`
}

type diagEnvironment struct {
	OS        string        `json:"os"`
	Arch      string        `json:"arch"`
	Git       diagTool      `json:"git"`
	Forge     diagForge     `json:"forge"`
	Occupancy diagOccupancy `json:"occupancy"`
	Reflink   diagReflink   `json:"reflink"`
	Config    diagConfig    `json:"config"`
}

type diagTool struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Path      string `json:"path,omitempty"`
}

// diagForge is the forge CLI and whether it can actually answer. The two are
// separate facts and the difference is the whole diagnosis: an installed-but-
// unauthenticated `gh` fails every PR query, and scruff resolves that to "not
// landed" — correct, safe, and indistinguishable from "your branches really
// aren't merged" unless something says so out loud.
type diagForge struct {
	CLI           string `json:"cli"`
	Available     bool   `json:"available"`
	Version       string `json:"version,omitempty"`
	Authenticated *bool  `json:"authenticated"` // null: no CLI to ask, or it wouldn't say
	Account       string `json:"account,omitempty"`
}

// diagOccupancy is the answer to "can `scruff reap` sweep a LIVE checkout on
// this machine?". Determined is the load-bearing one: without a provider that
// vouches for absence the sweep degrades to parked-only, which reads as scruff
// forgetting about lanes rather than as scruff refusing to guess.
type diagOccupancy struct {
	LSOF       bool   `json:"lsof"`
	Determined bool   `json:"determined"`
	Leases     int    `json:"leases"`
	LeaseDir   string `json:"lease_dir"`
	LeasesSole bool   `json:"leases_sole"` // SCRUFF_OCCUPANCY=lease
}

type diagReflink struct {
	Supported *bool  `json:"supported"` // null: nowhere to test it
	TestedAt  string `json:"tested_at,omitempty"`
	Method    string `json:"method,omitempty"`
}

type diagConfig struct {
	Path  string   `json:"path"` // "" when there is no config file, which is the common case
	Agent string   `json:"agent"`
	Namer string   `json:"namer,omitempty"`
	Hooks []string `json:"hooks"`
}

// diagRepo is the repo doctor was RUN IN, not every repo scruff knows about.
// The §6.4 facts in here are properties of one checkout — a `.gitmodules`, an
// LFS filter, a sparse cone — and answering them for eight repos at once would
// bury the one the user is standing in.
type diagRepo struct {
	Slug             string       `json:"slug"`
	Main             string       `json:"main"`
	Checkout         string       `json:"checkout"`
	Lane             string       `json:"lane,omitempty"` // set when this checkout IS a scruff lane
	Remotes          []diagRemote `json:"remotes"`        // [] when there are none, which is a fact, not an unknown
	DefaultBranch    string       `json:"default_branch"`
	DefaultBranchVia string       `json:"default_branch_via"` // origin-head | conventional | head | none
	Submodules       int          `json:"submodules"`
	LFS              bool         `json:"lfs"`
	LFSCLI           bool         `json:"lfs_cli"`
	SparseCheckout   bool         `json:"sparse_checkout"`
}

// diagRemote is one remote and the identity it would give this repo.
//
// The URL itself is deliberately NOT carried. Doctor's output exists to be
// pasted into a bug report, and an https remote routinely holds a credential
// (`https://x-access-token:ghp_…@github.com/o/r`). ParseSlug drops the userinfo
// on its way to owner/name, so the slug is the whole diagnostic fact with none
// of the secret — the same reason `Slug` and not `URL` is what the registry
// keys on.
type diagRemote struct {
	Name string `json:"name"`
	Slug string `json:"slug"` // "" when the URL parsed to nothing usable
}

type diagSummary struct {
	Repos  int `json:"repos"`
	Lanes  int `json:"lanes"`
	Live   int `json:"live"`
	Parked int `json:"parked"`
	Stray  int `json:"stray"`
}

// diagFinding is one thing worth doing something about. Kind is a closed set,
// and it is what a script matches on; Detail and Remedy are for the human.
//
// A Remedy is a scruff verb wherever scruff owns the fix, and it NEVER names
// `git worktree remove` — that is the exact move that defeats invariant 2 from
// the outside, and a report that suggests it does the damage at one remove. The
// one shape that escapes the verb is a fix that is genuinely not scruff's to
// make: a repo with no remote needs `git remote add`, which SPEC.md §4 says in
// as many words, and inventing a `scruff remote` wrapper for it would be scruff
// growing a git porcelain it has no reason to own.
type diagFinding struct {
	Kind   string `json:"kind"` // legacy-base | stale-row | stray-checkout | orphan-branch | orphan-chat | fork-remotes | no-remote
	Repo   string `json:"repo,omitempty"`
	Name   string `json:"name,omitempty"`
	Branch string `json:"branch,omitempty"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail"`
	Remedy string `json:"remedy"`
}

type diagDisk struct {
	Base  string         `json:"base"`
	Bytes *int64         `json:"bytes"` // the sum of the repos below, null when nothing could be walked
	Repos []diagDiskRepo `json:"repos"`
}

type diagDiskRepo struct {
	Repo  string `json:"repo"`
	Main  string `json:"main"`
	Lanes int    `json:"lanes"`
	Bytes *int64 `json:"bytes"` // null: every walk for this repo failed
}

// Diagnose is the bare `scruff doctor`: gather, then render or encode.
func (e *Env) Diagnose(asJSON bool) error {
	d := e.gather()
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(d)
	}
	e.renderDiagnosis(d)
	return nil
}

// gather runs every probe. Order matters in exactly one place: the reflink probe
// creates and removes a directory under the base, so it finishes before
// discover() globs that base.
func (e *Env) gather() diagnosis {
	d := diagnosis{
		Scruff:   Version,
		Schema:   2,
		Findings: []diagFinding{},
	}
	base, baseFindings := e.diagBase()
	d.Base = base
	d.Findings = append(d.Findings, baseFindings...)
	d.Environment = e.diagEnvironment()
	repo, repoFinds := e.diagRepo()
	d.Repo = repo
	d.Findings = append(d.Findings, repoFinds...)

	// discover() returns everything it can reach, INCLUDING entries a dead
	// registry row points at. The findings pass wants all of that; the counts
	// and the disk walk want what `scruff` itself would list, which is the
	// branchAlive subset — a doctor whose lane count disagrees with the listing
	// is a doctor nobody believes.
	entries := e.discover()
	var alive []Entry
	repos := map[string]bool{}
	for _, entry := range entries {
		if !e.branchAlive(entry) {
			continue
		}
		alive = append(alive, entry)
		repos[entry.Main] = true
		d.Summary.Lanes++
		switch entry.State {
		case Live:
			d.Summary.Live++
		case Parked:
			d.Summary.Parked++
		case Stray:
			d.Summary.Stray++
		}
	}
	d.Summary.Repos = len(repos)
	d.Findings = append(d.Findings, e.laneFindings(entries)...)
	d.Disk = e.diskUsage(alive)
	// Last, so a warning raised by any probe above is in the payload. Under
	// --json this is the ONLY channel a degraded run has (SPEC.md §2.2).
	d.Warnings = append([]string{}, e.Warnings...)
	return d
}

// ── the base ─────────────────────────────────────────────────────────────────

func (e *Env) diagBase() (diagBase, []diagFinding) {
	var findings []diagFinding
	rows, _ := e.Reg.Load()
	out := diagBase{
		Path:     e.Base,
		Registry: e.Reg.Path(),
		Rows:     len(rows),
		State:    stateDir(),
		Resolved: "elsewhere",
	}
	newBase, legacy := defaultBaseCandidates()
	switch {
	case os.Getenv("SCRUFF_BASE") != "":
		out.Resolved = "SCRUFF_BASE"
	case os.Getenv("CLAUDE_WT_BASE") != "":
		out.Resolved = "CLAUDE_WT_BASE"
	case e.Base == newBase:
		out.Resolved = "default"
	case e.Base == legacy:
		out.Resolved = "legacy"
		out.Migrate = true
		findings = append(findings, diagFinding{
			Kind:   "legacy-base",
			Path:   e.Base,
			Detail: "the base is still the LEGACY path (~/.cache/claude-worktrees) and it holds the registry",
			Remedy: "scruff doctor --migrate-base",
		})
	}
	return out, findings
}

// ── the machine ──────────────────────────────────────────────────────────────

func (e *Env) diagEnvironment() diagEnvironment {
	env := diagEnvironment{
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Git:     probeTool("git", "--version"),
		Forge:   probeForge(),
		Reflink: e.probeReflink(),
		Config: diagConfig{
			Path:  e.Cfg.Path,
			Agent: e.Agent,
			Namer: e.Cfg.Namer,
			Hooks: hookNames(e.Cfg.Hooks),
		},
	}

	// Occupancy through the same fold the sweep uses, so the doctor cannot
	// report a capability reap does not have.
	occ := e.Occupancy()
	leases, _ := occupancy.Leases(e.LeaseDir, e.LeaseSole).Scan()
	_, lsofErr := exec.LookPath("lsof")
	env.Occupancy = diagOccupancy{
		LSOF:       lsofErr == nil,
		Determined: occ.Known(),
		Leases:     len(leases),
		LeaseDir:   e.LeaseDir,
		LeasesSole: e.LeaseSole,
	}
	return env
}

func hookNames(hooks map[string][]string) []string {
	out := []string{}
	for name, argv := range hooks {
		if len(argv) > 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// probeTimeout bounds every external probe. A doctor that hangs is worse than
// one that reports "couldn't tell" — the whole point of running it is that
// something is already wrong.
const probeTimeout = 5 * time.Second

// probeTool asks a program for its version, and reports where it came from.
// The PATH entry is worth printing: "git 2.39" from /usr/bin when you installed
// 2.51 elsewhere is a diagnosis on its own.
func probeTool(name string, args ...string) diagTool {
	path, err := exec.LookPath(name)
	if err != nil {
		return diagTool{}
	}
	out, _, ok := runBounded(name, args...)
	t := diagTool{Available: true, Path: path}
	if ok {
		t.Version = versionToken(out)
	}
	return t
}

// versionToken pulls the number out of a `<tool> version X.Y.Z (…)` line
// without caring which tool wrote it — the shape every one of these uses.
func versionToken(out string) string {
	line := out
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	for _, f := range strings.Fields(line) {
		if f != "" && f[0] >= '0' && f[0] <= '9' {
			return strings.TrimSuffix(f, ",")
		}
	}
	return strings.TrimSpace(line)
}

// probeForge asks whether PR state is answerable on this machine at all.
//
// `gh auth status` rather than a PR query on purpose: it is the question with no
// repo in it, so the answer is the same whichever directory doctor was run from,
// and it never populates the forge cache with a repo-specific answer some later
// command would then trust.
func probeForge() diagForge {
	f := diagForge{CLI: "gh"}
	if _, err := exec.LookPath("gh"); err != nil {
		return f // Authenticated stays nil: there is nothing to ask, not a "no"
	}
	f.Available = true
	if out, _, ok := runBounded("gh", "--version"); ok {
		f.Version = versionToken(out)
	}
	out, errOut, ok := runBounded("gh", "auth", "status")
	f.Authenticated = &ok
	f.Account = ghAccount(out + "\n" + errOut)
	return f
}

// ghAccount pulls the login out of `gh auth status`, whose wording has moved —
// "Logged in to github.com as <user>" became "… account <user>". Both, and
// neither is fatal to miss.
func ghAccount(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if (f == "account" || f == "as") && i+1 < len(fields) {
				return strings.Trim(fields[i+1], "()")
			}
		}
	}
	return ""
}

// probeReflink answers "can the filesystem the checkouts land on clone a file?"
// by cloning a file on it. Every cheaper answer — the mount type, the OS, a
// syscall probe — answers a DIFFERENT question than the one that matters, which
// is whether the copy `scruff` will actually run (SPEC.md §6.3's `cp -c` /
// `cp --reflink=always`) succeeds here. So it runs that exact command.
//
// This is doctor's only write, and it is confined to a temp directory that is
// removed before anything else looks at the base. When the base does not exist
// yet — a fresh install — it walks up to the nearest directory that does, which
// is the filesystem the base is about to be created on.
func (e *Env) probeReflink() diagReflink {
	method := "cp --reflink=always"
	if runtime.GOOS == "darwin" {
		method = "cp -c"
	}
	r := diagReflink{Method: method}

	root := nearestExisting(e.Base)
	if root == "" {
		return r // Supported stays nil: nowhere to try, so nothing is known
	}
	r.TestedAt = root

	dir, err := os.MkdirTemp(root, ".scruff-reflink-")
	if err != nil {
		return r
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("scruff"), 0o644); err != nil {
		return r
	}
	args := []string{"--reflink=always", src, filepath.Join(dir, "dst")}
	if runtime.GOOS == "darwin" {
		args = []string{"-c", src, filepath.Join(dir, "dst")}
	}
	_, _, ok := runBounded("cp", args...)
	r.Supported = &ok
	return r
}

// nearestExisting walks up from p to the first directory that exists.
func nearestExisting(p string) string {
	for p != "" && p != "/" && p != "." {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return p
	}
	return ""
}

// runBounded runs a probe with a deadline and hands back both streams. ok is
// "exited 0" — for a probe, the exit code IS the answer, so a non-zero exit is
// data rather than an error to propagate.
func runBounded(name string, args ...string) (stdout, stderr string, ok bool) {
	cmd := exec.Command(name, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return "", "", false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), errb.String(), err == nil
	case <-time.After(probeTimeout):
		_ = cmd.Process.Kill()
		<-done
		return out.String(), errb.String(), false
	}
}

// ── the repo you are standing in ─────────────────────────────────────────────

func (e *Env) diagRepo() (*diagRepo, []diagFinding) {
	main, err := gitx.MainCheckout(e.Cwd)
	if err != nil || main == "" {
		return nil, nil
	}
	top, err := gitx.Toplevel(e.Cwd)
	if err != nil {
		top = e.Cwd
	}
	// repoSlug, not a second derivation of it: repo.go exists because scruff had
	// two spellings of "which repo is this?" and they disagreed, and a doctor
	// that reports an identity no other command uses is that bug with a report
	// attached.
	//
	// `degraded` is SPEC.md §4's remote-less repo, where that slug is
	// `local/<basename>` and the registry records no repo. It is read off the
	// remotes rather than off the slug's shape — `local/foo` is a legal owner
	// and name on a forge, and sniffing the prefix would call a real repo
	// degraded. Nothing here is an error: everything still works, which is
	// exactly why it needs saying out loud.
	remotes := remoteIdentities(main)
	degraded := true
	for _, rem := range remotes {
		if rem.Slug != "" {
			degraded = false
			break
		}
	}
	branch, via := gitx.DefaultBranchDetail(main)
	r := &diagRepo{
		Slug:             repoSlug(main),
		Main:             main,
		Checkout:         top,
		Remotes:          remotes,
		DefaultBranch:    branch,
		DefaultBranchVia: via,
		Submodules:       countSubmodules(top),
		LFS:              usesLFS(top),
		SparseCheckout:   sparseCheckout(top),
	}
	if _, err := exec.LookPath("git-lfs"); err == nil {
		r.LFSCLI = true
	}
	// Naming the lane matters more than the boolean: it is what you type to get
	// back here, and a doctor run from inside a lane is the common case.
	if row, ok := e.Reg.Find(top); ok {
		r.Lane = row.Name
	}
	return r, repoFindings(r, degraded)
}

// remoteIdentities resolves every remote to the slug it would give this repo.
//
// Read off `main` rather than the checkout: a worktree shares its remote config
// through the common dir, so the two agree, and `main` is what RemoteSlug was
// asked — a second source here could disagree with the identity beside it.
func remoteIdentities(main string) []diagRemote {
	out := []diagRemote{}
	for _, name := range gitx.Remotes(main) {
		url, err := gitx.Run(main, "remote", "get-url", name)
		if err != nil || url == "" {
			continue
		}
		out = append(out, diagRemote{Name: name, Slug: gitx.ParseSlug(url)})
	}
	return out
}

// repoFindings is SPEC.md §4's two remote situations, and doctor is the command
// that names them. Both are otherwise silent: identity resolves, every command
// runs, nothing errors, and the only symptom is lanes that are never swept.
//
// Neither finding proposes that scruff go looking for a better remote. `origin`
// winning is §4's decision and the landing check follows identity on purpose —
// picking a different remote to believe would be scruff GUESSING which of N
// remotes is the forge of record, in the one code path that deletes branches,
// and invariant 2 does not guess. So the blindness gets a name and a human gets
// the choice.
func repoFindings(r *diagRepo, degraded bool) []diagFinding {
	if degraded {
		return []diagFinding{{
			Kind: "no-remote", Repo: r.Slug, Path: r.Main,
			Detail: "this repo has no remote to take an identity from, so scruff keys it on the directory name and records no repo for its lanes — another checkout called " + filepath.Base(r.Main) + " under a different org shares the same bucket, and no PR can ever be found for a branch here, so nothing is reaped on PR evidence",
			Remedy: "git remote add origin <url> — new lanes key on the slug from then on, and the rows you already have keep the paths they have",
		}}
	}
	var others []string
	for _, rem := range r.Remotes {
		if rem.Slug != "" && rem.Slug != r.Slug {
			others = append(others, rem.Name+" is "+rem.Slug)
		}
	}
	if len(others) == 0 {
		return nil
	}
	return []diagFinding{{
		Kind: "fork-remotes", Repo: r.Slug, Path: r.Main,
		Detail: "this repo's remotes disagree about who it is: identity is " + r.Slug + ", and " + strings.Join(others, ", ") + ". scruff keys on origin (SPEC.md §4), so every PR query asks " + r.Slug + " — if your pull requests land somewhere else, scruff never sees one merge and a squash-merged lane stays kept rather than swept",
		Remedy: "nothing, if your PRs land in " + r.Slug + ". Otherwise scruff drop <name> takes a lane by hand, and a [hooks] landed entry (SPEC.md §6.5) teaches the sweep where they really land",
	}}
}

// countSubmodules reads `.gitmodules` through git rather than by hand, so an
// include or a nonstandard spelling counts the same way git counts it.
// `git worktree add` does not recurse, so a nonzero count here is the whole
// explanation for a lane whose vendored dependency directory is empty.
func countSubmodules(top string) int {
	if _, err := os.Stat(filepath.Join(top, ".gitmodules")); err != nil {
		return 0
	}
	out, err := gitx.Run(top, "config", "-f", ".gitmodules", "--name-only", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil {
		return 0
	}
	return len(gitx.Lines(out))
}

// usesLFS looks for an LFS filter in the repo's own `.gitattributes`. False
// here is an honest "nothing declares LFS", not an undetermined — an
// unreadable or absent file means the repo asked for no smudge.
func usesLFS(top string) bool {
	b, err := os.ReadFile(filepath.Join(top, ".gitattributes"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "filter=lfs")
}

func sparseCheckout(top string) bool {
	out, err := gitx.Run(top, "config", "--bool", "core.sparseCheckout")
	return err == nil && out == "true"
}

// ── findings ─────────────────────────────────────────────────────────────────

// laneFindings is the §6.4 diagnose set that is about lanes: stray checkouts,
// orphan branches, and registry rows that no longer mean anything.
//
// It REPORTS what a sweep would do and does none of it. `scruff` the listing
// prunes stale rows on its way past; doctor deliberately does not, because a
// diagnostic that repairs the thing it was asked to describe destroys the
// evidence somebody was about to paste into a bug report.
func (e *Env) laneFindings(entries []Entry) []diagFinding {
	var out []diagFinding

	registered := map[string]bool{}
	rows, _ := e.Reg.Load()
	for _, row := range rows {
		registered[row.Main+"\x00"+row.Branch] = true
	}

	for _, entry := range entries {
		if !e.branchAlive(entry) {
			continue // a dead row's entry: the stale-row pass below owns it, once
		}
		repo := repoKey(entry.Main)
		if entry.State == Stray {
			out = append(out, diagFinding{
				Kind: "stray-checkout", Repo: repo, Name: entry.Name(),
				Branch: entry.Branch, Path: entry.Path,
				Detail: "a directory is there but git has disowned it — a `git worktree remove` that died part-way. The work in it is intact.",
				Remedy: "scruff " + entry.Name() + " (moves the husk aside and rebuilds the checkout)",
			})
		}
		if !registered[entry.Main+"\x00"+entry.Branch] {
			out = append(out, diagFinding{
				Kind: "orphan-branch", Repo: repo, Name: entry.Name(),
				Branch: entry.Branch, Path: entry.Path,
				Detail: "an agent branch with no registry row — its work is safe and it still resumes, but scruff has lost which client it belongs to and which pane spawned it",
				Remedy: "scruff " + entry.Name() + " (resumes it and writes the row back)",
			})
		}
		// A conversation left at a path this lane has moved off. Invisible
		// otherwise — the branch, the checkout and the row all look right, and
		// the only symptom is an empty pane that exits 1 the next time somebody
		// resumes it. Asked only of a lane that would open its OWN checkout: one
		// resuming into a parent's pane (a `scruff child`) has no conversation
		// here by design, and saying so would be a finding on every child lane.
		if agent := e.agentForPath(entry.Path); agentProbeable(agent) &&
			!agentHasChat(agent, entry.Path) && e.chatHome(agent, entry.Path) == entry.Path {
			if _, was := e.strandedChatOf(agent, entry); was != "" {
				out = append(out, diagFinding{
					Kind: "orphan-chat", Repo: repo, Name: entry.Name(),
					Branch: entry.Branch, Path: entry.Path,
					Detail: "the checkout moved and its conversation did not — the transcript is at " + was +
						", and the client keys history on the exact path, so resuming here opens an empty session",
					Remedy: "scruff " + entry.Name() + " (copies the conversation to where the lane lives now)",
				})
			}
		}
	}

	// A row whose branch no longer means anything. `branchAlive` is the same
	// test `pruneRegistry` uses, so this names exactly what the next sweep will
	// drop — and nothing else.
	for _, row := range rows {
		entry := Entry{Main: row.Main, Branch: row.Branch, Path: row.Path, State: checkoutState(row.Path)}
		if e.branchAlive(entry) {
			continue
		}
		detail := "the branch no longer exists, so the row can resume nothing"
		if _, err := gitx.MainCheckout(row.Main); err != nil {
			detail = "the main checkout it points at (" + row.Main + ") is not a git repo any more"
		}
		out = append(out, diagFinding{
			Kind: "stale-row", Repo: repoKey(row.Main), Name: row.Name,
			Branch: row.Branch, Path: row.Path,
			Detail: detail,
			Remedy: "any `scruff` listing prunes it — nothing to do by hand",
		})
	}
	return out
}

// ── disk ─────────────────────────────────────────────────────────────────────

// diskUsage walks each LIVE lane checkout and sums it per repo.
//
// Per repo rather than per bucket directory: two repos with the same basename
// share a bucket under the base, and the registry knows which lane belongs to
// which repo while the directory name does not.
//
// It measures allocated BLOCKS, not apparent size, which is the only accounting
// that tells the truth about the feature scruff sells — a reflinked
// `node_modules` reports its full apparent size while occupying nearly nothing,
// so apparent size would make the reflink path look identical to the copy it
// replaced (SPEC.md's disk-accounting row, §12).
func (e *Env) diskUsage(entries []Entry) diagDisk {
	type acc struct {
		main    string
		lanes   int
		bytes   int64
		counted bool
	}
	order := []string{}
	byRepo := map[string]*acc{}

	for _, entry := range entries {
		if entry.State == Parked {
			continue // no checkout on disk; the branch costs the repo's object store, not a tree
		}
		slug, err := gitx.RemoteSlug(entry.Main)
		if err != nil {
			slug = "local/" + filepath.Base(entry.Main)
		}
		a, ok := byRepo[slug]
		if !ok {
			a = &acc{main: entry.Main}
			byRepo[slug] = a
			order = append(order, slug)
		}
		a.lanes++
		if n, err := treeBytes(entry.Path); err == nil {
			a.bytes += n
			a.counted = true
		}
	}

	out := diagDisk{Base: e.Base, Repos: []diagDiskRepo{}}
	var total int64
	var anyCounted bool
	for _, slug := range order {
		a := byRepo[slug]
		r := diagDiskRepo{Repo: slug, Main: a.main, Lanes: a.lanes}
		if a.counted {
			n := a.bytes
			r.Bytes = &n
			total += n
			anyCounted = true
		}
		out.Repos = append(out.Repos, r)
	}
	// Biggest first: the reason anyone reads this section is to find what to
	// remove, and a repo with no measurable size is never that.
	sort.SliceStable(out.Repos, func(i, j int) bool {
		return bytesOrZero(out.Repos[i].Bytes) > bytesOrZero(out.Repos[j].Bytes)
	})
	if anyCounted {
		out.Bytes = &total
	}
	return out
}

func bytesOrZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// treeBytes is a `du`-equivalent walk. A file it cannot stat is skipped rather
// than fatal: a permission wall in one directory must not turn the whole
// section into "unknown".
func treeBytes(root string) (int64, error) {
	if _, err := os.Stat(root); err != nil {
		return 0, err
	}
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: count what we can, skip what we can't
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += allocatedBytes(info)
		return nil
	})
	return total, err
}

// ── rendering ────────────────────────────────────────────────────────────────

const diagLabel = 17 // the widest label below, plus a space

// plural is the difference between a report and a form letter. "1 lane(s)" is
// the tell that nobody read the output before shipping it, and this one is
// going into bug reports.
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func (e *Env) renderDiagnosis(d diagnosis) {
	out := func(format string, a ...any) { ui.Out(format+"\n", a...) }
	field := func(label, format string, a ...any) {
		out("  %-*s%s", diagLabel, label, fmt.Sprintf(format, a...))
	}
	// note is a line with nothing in the label column — used where there is one
	// thing to say and no field to hang it on.
	note := func(format string, a ...any) { out("  "+format, a...) }

	out("scruff %s · schema %d · %s/%s", d.Scruff, d.Schema, d.Environment.OS, d.Environment.Arch)
	out("")

	out("base")
	field("path", "%s", d.Base.Path)
	field("resolved from", "%s", baseResolution(d.Base))
	field("registry", "%s — %s", d.Base.Registry, plural(d.Base.Rows, "row"))
	field("state", "%s", d.Base.State)
	out("")

	env := d.Environment
	out("environment")
	field("git", "%s", toolLine(env.Git))
	field("forge", "%s", forgeLine(env.Forge))
	field("occupancy", "%s", occupancyLine(env.Occupancy))
	field("leases", "%d held · %s%s", env.Occupancy.Leases, env.Occupancy.LeaseDir, soleNote(env.Occupancy))
	field("reflink", "%s", reflinkLine(env.Reflink))
	field("config", "%s", configLine(env.Config))
	out("")

	if d.Repo == nil {
		out("repo")
		note("%s", "not inside a git repo — the per-repo facts are skipped")
		out("")
	} else {
		r := d.Repo
		out("repo   %s", r.Slug)
		field("checkout", "%s%s", r.Checkout, laneNote(r))
		field("main", "%s", r.Main)
		field("remotes", "%s", remotesLine(r))
		field("default branch", "%s — %s", r.DefaultBranch, branchViaNote(r.DefaultBranchVia))
		field("submodules", "%s", submoduleNote(r.Submodules))
		field("LFS", "%s", lfsNote(r))
		field("sparse-checkout", "%s", onOff(r.SparseCheckout))
		out("")
	}

	s := d.Summary
	out("lanes  %d across %s — %d live, %d parked, %d stray",
		s.Lanes, plural(s.Repos, "repo"), s.Live, s.Parked, s.Stray)
	out("")

	out("findings")
	if len(d.Findings) == 0 {
		note("%s", "none — nothing here needs a human")
	}
	for _, f := range d.Findings {
		// A lane finding is headed by its name with the repo in parentheses; a
		// finding ABOUT the repo has no lane, so the repo stands alone rather
		// than degrading to a bare path nobody can read at a glance.
		head := f.Name
		switch {
		case head != "" && f.Repo != "":
			head += " (" + f.Repo + ")"
		case head == "":
			head = f.Repo
		}
		if head == "" {
			head = f.Path
		}
		field(findingLabel(f.Kind), "%s", head)
		out("  %-*s%s", diagLabel, "", f.Detail)
		out("  %-*s→ %s", diagLabel, "", f.Remedy)
	}
	out("")

	out("disk   lane checkouts under %s", d.Disk.Base)
	if len(d.Disk.Repos) == 0 {
		note("%s", "no live checkouts to measure")
	}
	for _, r := range d.Disk.Repos {
		field(humanBytes(r.Bytes), "%s — %s", r.Repo, plural(r.Lanes, "lane"))
	}
	if d.Disk.Bytes != nil && len(d.Disk.Repos) > 1 {
		field(humanBytes(d.Disk.Bytes), "%s", "total")
	}
}

func baseResolution(b diagBase) string {
	switch b.Resolved {
	case "SCRUFF_BASE", "CLAUDE_WT_BASE":
		return b.Resolved + " — the default-path decision is overridden"
	case "default":
		return "the default (~/.cache/scruff)"
	case "legacy":
		// The word LEGACY and the verb are both pinned by the suite, and both
		// earn it: this is the one base state with something to do about it.
		return "the LEGACY path (~/.cache/claude-worktrees) — it holds the registry.\n" +
			fmt.Sprintf("  %-*s`scruff doctor --migrate-base` moves it to ~/.cache/scruff", diagLabel, "")
	}
	return "an unrecognised path — scruff is using it as given"
}

func toolLine(t diagTool) string {
	if !t.Available {
		return "MISSING — scruff shells out to git for everything; nothing works without it"
	}
	if t.Version == "" {
		return t.Path
	}
	return t.Version + " · " + t.Path
}

func forgeLine(f diagForge) string {
	if !f.Available {
		return "no `" + f.CLI + "` on PATH — PR state is unknown, so nothing is ever reaped on that basis"
	}
	line := f.CLI
	if f.Version != "" {
		line += " " + f.Version
	}
	switch {
	case f.Authenticated == nil:
		line += " — could not tell whether it is authenticated"
	case *f.Authenticated && f.Account != "":
		line += " — authenticated as " + f.Account
	case *f.Authenticated:
		line += " — authenticated"
	default:
		line += " — NOT authenticated: every PR query fails, and scruff resolves that to `not landed` (safe, and it looks like nothing ever merges)"
	}
	return line
}

func occupancyLine(o diagOccupancy) string {
	if o.Determined {
		which := "leases"
		if o.LSOF {
			which = "lsof"
		}
		return which + " vouches for absence — `scruff reap` can sweep live checkouts"
	}
	return "UNDETERMINED (no lsof) — reap degrades to parked-only, and nothing live is ever swept"
}

func soleNote(o diagOccupancy) string {
	if o.LeasesSole {
		return " · SCRUFF_OCCUPANCY=lease — a lane nobody leased counts as empty"
	}
	return ""
}

func reflinkLine(r diagReflink) string {
	switch {
	case r.Supported == nil:
		return "not determined — no directory to test against"
	case *r.Supported:
		return "supported — `" + r.Method + "` clones on " + r.TestedAt
	}
	return "NOT supported on " + r.TestedAt + " — heavy directories would be copied in full"
}

func configLine(c diagConfig) string {
	line := c.Path
	if line == "" {
		line = "none (~/.config/scruff/config.toml) — every default"
	}
	line += " · agent=" + c.Agent
	if c.Namer != "" {
		line += " · namer=" + c.Namer
	}
	if len(c.Hooks) > 0 {
		line += " · hooks: " + strings.Join(c.Hooks, ", ")
	}
	return line
}

func laneNote(r *diagRepo) string {
	if r.Lane == "" {
		return ""
	}
	return " (the `" + r.Lane + "` lane)"
}

// remotesLine names every remote and the identity it would give, origin first
// because origin is the one that wins. The point of the line is the DISAGREEMENT
// — a fork's `origin julienmartel/kilo · upstream antirez/kilo` is the whole
// explanation for a repo whose lanes never sweep — so it prints even when there
// is only one, which is the reading that makes "only one" informative.
func remotesLine(r *diagRepo) string {
	if len(r.Remotes) == 0 {
		return "none — identity falls back to " + r.Slug + ", and no PR can be found for any branch here"
	}
	parts := make([]string, 0, len(r.Remotes))
	for _, rem := range r.Remotes {
		slug := rem.Slug
		if slug == "" {
			slug = "?"
		}
		parts = append(parts, rem.Name+" "+slug)
	}
	return strings.Join(parts, " · ")
}

func branchViaNote(via string) string {
	switch via {
	case "origin-head":
		return "from refs/remotes/origin/HEAD, which the repo asserts"
	case "conventional":
		return "guessed from the name; there is no origin/HEAD to ask"
	case "head":
		return "guessed from whatever the main checkout has out — this MOVES if somebody checks out a side branch there"
	}
	return "unresolved"
}

// submoduleWhy is the whole explanation for an empty vendored directory in a
// fresh lane, and it is a const so that it is said with the SAME words wherever
// it is said: doctor predicts it for a repo, `new`/`child`/`spawn` report it for
// the lane they just made (sayCreated). Two phrasings of one cause is how a
// reader ends up believing they are two causes.
const submoduleWhy = "`git worktree add` does not recurse, so a new lane's submodule dirs are empty until you init them"

func submoduleNote(n int) string {
	if n == 0 {
		return "none"
	}
	return fmt.Sprintf("%d — %s", n, submoduleWhy)
}

func lfsNote(r *diagRepo) string {
	if !r.LFS {
		return "nothing in .gitattributes declares it"
	}
	if r.LFSCLI {
		return "declared in .gitattributes · git-lfs is installed"
	}
	return "declared in .gitattributes but git-lfs is NOT installed — a new lane gets pointer files, not content"
}

func onOff(b bool) string {
	if b {
		return "on — a new lane does not inherit the cone; it checks out everything"
	}
	return "off"
}

func findingLabel(kind string) string {
	switch kind {
	case "legacy-base":
		return "legacy base"
	case "stale-row":
		return "stale row"
	case "stray-checkout":
		return "stray checkout"
	case "orphan-branch":
		return "orphan branch"
	case "orphan-chat":
		return "orphan chat"
	case "fork-remotes":
		return "fork remotes"
	case "no-remote":
		return "no remote"
	}
	return kind
}

// humanBytes renders a nullable size. null is "?", never 0 — the whole nullable
// rule in one character.
func humanBytes(p *int64) string {
	if p == nil {
		return "?"
	}
	n := *p
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTP"[exp])
}
