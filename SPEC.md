# scruff — design spec

**The worktree-lifecycle substrate.** A rewrite of haus's old
worktree tool (`haus/modules/den/wt.sh`, 1295 lines of bash) as a
standalone, repo-agnostic, client-agnostic Go binary — a dev-focused sister to
pounce / perch / trill, with `haus` and `bench` demoted to consumers.

This is the design doc; it stays the authority on *what* scruff is even as the code
lands beside it. **Update, post-cutover:** the bash predecessor has since been
retired entirely (haus#245) — every caller this doc describes as
transitional now points at `scruff` alone, with no fallback to roll back to.

Status: ejected from the workshop incubator to its own repo (2026-08-03), with
its history intact. Implementation progress is
measured against the ported acceptance suite — `make score` — and reported in
[`README.md`](README.md), not here. Any section below that the code has since
contradicted is a bug in the code or a bug in this file; say which.

---

## 0. Thesis

Five agents in five worktrees is now normal. Every vendor ships worktree spawning
(`claude --worktree`, Claude Agent SDK `isolation: worktree`, Cursor, Copilot CLI)
and every one of them stops at *create*. What nobody owns is the rest of the life:
the branch that's still alive after the pane died, the checkout nobody is sitting
in, the tree with 40 uncommitted minutes in it, the branch whose PR merged
yesterday and which has kept committing since.

scruff's product is not "make worktrees". It's the **state machine and its safety
invariants**:

```
        create ──▶ live ──▶ parked ──▶ live ──▶ landed ──▶ reaped
                    │         │                    ▲
                    └─────────┴────────────────────┘
                        (branch is the durable artifact;
                         the checkout dir is disposable)
```

Three invariants, in priority order. Everything else in this document is
subordinate to them:

1. **Never lose work.** Every destructive path parks first. The failure direction
   is always "a branch lingers", never "a tree vanished".
2. **Never reap something in use.** Occupied, dirty, or not-provably-landed ⇒
   keep. Uncertainty resolves to *keep*, always, including when the forge is
   unreachable.
3. **The registry is the source of truth, and it is locked.** Not the filesystem,
   not `git worktree list` — those are derived and lie (stray dirs, half-removed
   checkouts, parked branches with no dir at all).

The actions at each transition — what to build, what to test, what to deploy —
belong to the user. scruff has no opinion about your build system, and states so in
the README.

### Non-goals (say these in the README's second paragraph)

No scheduling. No agent supervision or restart. No fullscreen TUI. No hosted
anything. No knowledge of your build system, package manager, or CI. No merge
conflict *resolution*. No opinion about which agent you should run.

### The moat, stated plainly

Against first-party worktree support: vendors will never ship cross-client (no
one is going to support codex **and** opencode **and** claude in one registry),
never ship cross-repo parentage (`scruff child` has no equivalent anywhere), and
treat the lifecycle invariants as an afterthought because losing *your* work
isn't *their* problem. Park, PR-verified reap, occupancy detection, and
post-merge drift detection are the product.

### Vocabulary: a **lane**

The thing that state machine moves through its states is a **lane** — one
agent's branch, checkout and pane, from `create` to `reaped`. Every command,
message and `--json` field means *lane* when it says lane.

Three words that were doing this job are now reserved, because each already
means something narrower and the overload was the bug:

| word | means, and only this |
|---|---|
| **worktree** | git's — the checkout on disk. A *parked* lane has none, so "worktree" cannot name the unit; §0's whole point is that the branch is the durable artifact and the directory is disposable. |
| **agent** | the **client**: `claude`, `codex`, `opencode`, `pi`. Registry field 6, `--json` `agent`, `--agent`, `SCRUFF_AGENT`. Frozen (§2.1) — a lane *runs* an agent, it is not one. |
| **session** | somebody else's: zellij's session, and each client's own transcript/resume session. scruff never names its own unit this. |

`pane` stays available for the terminal pane a lane is (or isn't) occupied by.
It is what `occupied` is *for*, but not what `occupied` **observes**: the built-in
provider sees a process with its cwd in the checkout, and a dev server, a
watcher or a daemon orphaned to pid 1 is not a pane. So `occupied` reports
"something is standing here", and `occupied_by` (§2.2) names it — a refusal
that says "a pane is open" about a stray daemon sends a user looking for a
window that does not exist.

---

## 1. Name, license, distribution

| | |
|---|---|
| Name | **`scruff`** — the loose skin a mother cat carries a kitten by: the kitten goes limp and is never dropped, which is invariant 1 in one image. Shipped as `holt` (an otter's den) through 0.5.0 and renamed at 1.0.0; the old packages stay published, deprecated in place, and the old GitHub names stay unclaimed so their redirects live. |
| Why not `wt` | Already worktrunk's binary name, and Windows Terminal's. Non-negotiable rename. |
| Language | **Go.** Subprocess orchestrator, zero CPU-bound work — Rust/Zig buy nothing. CGo-free cross-compilation dominates for prebuilt-binary distribution. `x/sys/unix` has `Clonefileat` + `FICLONE` so reflink needs no CGo. charm (`fang`, `huh`) makes `doctor` good, and styled output is `hausfold/snug` — charm's `x/ansi` under the family's own role vocabulary, without lipgloss's styling engine. Bun `--compile` measured 60 MB / 9 ms — startup fine, size not. |
| License | **MIT.** A commercial GUI must be able to embed the substrate (that's the thesis) — MIT grants that with the fewest strings, and it's the family's standard. |
| Install CTA | `bun i -g scruff` — an npm wrapper that downloads a prebuilt binary (the esbuild/biome pattern), **not** a bun-runtime tool. Also `brew install hausfold/tap/scruff`, `curl … | sh`, and `go install`. |
| Tests | The bash predecessor's `haus/test/wt.bats` (1026 lines, 77 tests) is black-box — it drives the CLI with shim `gh`/`lsof` on `PATH`, and already has a `WT_UNDER_TEST` seam for pointing it at another implementation. It becomes scruff's acceptance suite on day one, and is the single best de-risking asset in the extraction. **Not quite unchanged:** four call sites drop `bash` (a Go binary isn't sourced), and three assertions on user-facing strings carry the new command name. No test *body* changes — which is the property that matters, because it means the contract is unmoved. |

---

## 2. Public contracts — freeze these before anyone pins them

Everything in this section is versioned and breaking-change-gated once 0.1 ships,
because `bench`, the haus statusline, and pounce's "Spawn Agent" command all
pin them within a day of cutover.

### 2.0 Lane-name resolution

Every verb that takes a lane name (`scruff <name>`, `drop`, `focus`, `reship`,
`runtime up|enter|down`) resolves through one matcher: `<name>` or
`<repo>/<name>`, exact first, then by a unique prefix of either part. The
prefix pass exists because the name a user (or an agent) types is the name the
listing showed, and the listing's cells are budgeted to the window — a cut cell
ends in `…`, and pasting it back must still resolve. A prefix matching several
lanes refuses, naming every lane it matched. Nothing fuzzy beyond the prefix:
no edit distance, no case folding, so a refusal or a hit is stable and
predictable.

### 2.1 Registry schema

Today (`$WT_BASE/registry.tsv`), one tab-separated line per lane, six fields:

```
name    main-checkout    branch    checkout-path    parent    agent
```

Field 4 (checkout path) is the primary key. Field 6 is the client id
(`claude|codex|opencode|pi`); **a row with fewer than 6 fields means `claude`** —
that's the already-shipped v0 migration case and it must survive. So does a
field 6 naming a client scruff no longer knows (`jcode`, accepted through
0.2.x): it reads as `claude`, and the next write persists that. Narrowing this
set is allowed; stranding a lane over it is not.

**Rule for 0.1: read the existing file unchanged.** No format migration on
cutover day. Julien's machine had live rows written by the bash predecessor;
scruff reads them, writes them back byte-compatibly, and only *then* earns the
right to propose v1.

Proposed v1 (post-cutover, opt-in, `scruff migrate`):

```toml
# $SCRUFF_STATE/registry/<sha256(checkout-path)[:12]>.toml   — one file per lane
schema   = 1
name     = "sparkle"
repo     = "hausfold/haus"   # remote slug — see §4
main     = "/Users/j/code/workshop/haus"
branch   = "worktree-sparkle"
path     = "/Users/j/.cache/scruff/hausfold-haus/sparkle"
parent   = "/Users/j/code/workshop"
agent    = "claude"
created  = 2026-08-03T10:04:00Z
```

Why one-file-per-row rather than a better TSV: it makes the lock story trivial
(create/rename is atomic on every filesystem scruff targets), it kills the
read-modify-write race that `reg_put` currently papers over with a temp file +
rename of the *whole* table, and it lets a corrupt row be quarantined instead of
poisoning the parse. `schema = N` on every row; unknown-higher schema ⇒ scruff
refuses to write and says which version to upgrade to.

**Locking.** 0.1 must take an exclusive `flock(2)` on `$SCRUFF_STATE/registry.lock`
for every mutation and a shared one for every read that will act on the result.
The bash version's TSV rewrite is a genuine lost-update race whenever two panes
close simultaneously; it's rare enough that it hasn't bitten, and that's luck.

### 2.2 `--json` output

Every listing/state command takes `--json`, and because bare `scruff` IS the
listing, **`scruff --json` is a synonym for `scruff list --json`** — byte-identical
output, pinned by the suite. Consumers should not have to know that `list` is
the implied verb. One envelope, so they can version-check without sniffing:

```json
{
  "scruff": "1.0.0",
  "schema": 2,
  "lanes": [
    {
      "name": "sparkle",
      "repo": "hausfold/haus",
      "main": "/Users/j/code/workshop/haus",
      "branch": "worktree-sparkle",
      "path": "/Users/j/.cache/scruff/hausfold-haus/sparkle",
      "parent": "/Users/j/code/workshop",
      "chat": "/Users/j/.cache/scruff/hausfold-haus/sparkle",
      "agent": "claude",
      "state": "live",
      "occupied": true,
      "occupied_by": [
        { "pid": 46864, "command": "node", "path": "/Users/j/.cache/scruff/hausfold-haus/sparkle", "via": "lsof" }
      ],
      "dirty": true,
      "ahead": 3,
      "behind": 12,
      "landed": { "verdict": "no", "via": null, "confidence": "certain" },
      "post_merge_ahead": { "commits": 0, "pr": 0, "diverged": false },
      "pr": { "number": 189, "state": "OPEN", "url": "https://…", "checks": "passing" },
      "overlap": ["frost"]
    }
  ],
  "warnings": ["forge unreachable: gh exited 4 — PR state is stale (cached 14m ago)"]
}
```

Contract points that matter:

- The array is **`lanes`**, not `worktrees` — a parked entry has no checkout on
  disk, so `worktrees` was never true of the whole set. `agent` inside each lane
  keeps its own meaning: the client, never the lane.
- `state` ∈ `live | parked | stray`. Closed set; additions are minor, removals major.
- `landed.verdict` ∈ `yes | no | fresh | contained` and `landed.via` ∈
  `never-diverged | ancestry | pr-head-oid | patch-equivalence | merge-tree-empty | null`
  — see §3. Consumers must treat an unknown `via` as `no`. `fresh` (§3.5) is an
  ADDITION, and a minor one by the same rule `state` follows: a consumer that
  doesn't know it falls back to "not landed", which is the safe direction.
- `chat` is the checkout whose conversation `scruff <name>` opens — `path` for a
  lane with its own chat, the parent's path for a spawned one (§5.3). It is an
  ADDITION, and the field a consumer that wants to hide lanes with no pane of
  their own must read: `parent` cannot answer that question, because a lane
  opened from inside another lane's pane is parented to that lane exactly as a
  `scruff child` is, and it has a pane, a panel and a chat. **`""` means
  undetermined and must be read as "show it"** — the same rule `occupied`
  follows below, and the reason `chat` is not simply `resume`'s answer:
  resume must always name a directory, so for a client whose transcripts scruff
  cannot probe it falls back to the parent. Published, that guess would hide
  every codex/opencode lane spawned from another pane, window and all. So the
  field is `""` for any client scruff cannot probe, and only ever load-bearing
  where it can. It is derived per call, not stored: it flips from the parent's
  path to the lane's own the first time an agent leaves a conversation in that
  checkout, which is a real change in the lane and a `watch` event worth having.
- `occupied`, `dirty`, `pr` are **nullable**: `null` means *not determined*
  (no `lsof`, no forge, cache miss), which is categorically different from
  `false`. Every consumer bug in the bash version's statusline came from
  conflating those two.
- `occupied_by[]` is the EVIDENCE behind `occupied: true` — `{pid, command,
  path, via}`, with `via` ∈ `lsof | leases`. Omitted entirely when nothing is
  standing there, so a consumer that never learns the key sees the same bytes it
  always did. It exists because `occupied` alone cannot be checked: a lane that
  reads busy for five days is either a long-running agent or a stray daemon
  nobody can see, and the two want opposite responses. `command` is best-effort
  (a lease knows the pid, never the name) and nothing in the safety model reads
  any of it — occupancy still resolves to *keep* regardless.
- `warnings[]` is where degraded-mode explanations go. Never silently degrade.
- **`schema` is `2` from 1.0.0.** Schema 1 spelled the version key `holt`;
  the 1.0.0 rename made it `scruff`, and renaming a required envelope field is
  exactly the break this counter exists to announce. A consumer pinned to
  schema 1 reads the old key and should refuse a payload it doesn't know —
  the five SDKs move with the CLI, on one version number.
- Field additions are non-breaking; consumers must ignore unknown keys.
- `post_merge_ahead.diverged` disambiguates a case `commits` alone cannot: the
  same nonzero count means "committed after the merge" (reshippable) when the
  branch's history includes what merged, and "this tip never built on what
  merged" (a stale or sideways checkout — a second local copy of the same
  branch, an amend) when it does not. `scruff reship` refuses the latter rather
  than pushing content the merge already superseded; the CLI marks it `~N` in
  the state column, distinct from `+N`. **A rebase is not that case**, whatever
  it does to the SHAs: the question is settled first by asking whether
  `origin/<base>` is an ancestor of the tip, because everything that merged is
  on the default branch by definition and a *squash* merge puts it there under a
  new SHA that neither ancestry against `headRefOid` nor `git cherry` can match.
  Squash-merge, rebase onto the default branch, keep working — the lane that did
  exactly the right thing — read as `~N` until #60, and `~N`'s remedy is "delete
  this checkout".
- `post_merge_ahead.pr` is not only the merged PR. An **open** PR standing at
  this exact tip covers everything since the merge, so the whole marker
  collapses to `{0, 0, false}` and `+N` comes down: the lane is simply in
  flight. Compared by OID, never by mere existence, because a commit made after
  that push is genuinely uncovered and is the case the marker exists for. Before
  #60 the map behind this listed MERGED PRs only, so the follow-up PR `scruff
  reship` had just opened was invisible and the lane went on being told to
  reship, forever.
- **A branch name is not a lane, and the forge only knows the name.** scruff
  coins lane names from a small word list and a task name gets reused outright —
  one repo's `worktree-continue-factory-docs` has carried seven PRs — so the
  newest merged PR on a name may belong to a lane that was reaped weeks ago. A
  PR counts as this branch's only when one of two facts says so: its
  `headRefOid` is reachable from the tip (**ancestry**, asked first, and it
  settles a rebase-and-merge or squash the SHAs alone cannot), or its `closedAt`
  is not before the branch came into being (**date**). Everything else keeps the
  PR — no OID, no `closedAt`, no datable branch — because a marker that stays up
  is noise while a marker that never comes up is un-shipped work nobody is told
  about. The branch's birth is the OLDEST reflog entry (`branch: Created from
  …`; git deletes a branch's reflog with the branch, so it dates the incarnation
  rather than the name), falling back to the oldest **author** date in
  `base..branch` — never committer, which a rebase rewrites to now and which
  would date a rebased lane after its own merge. Allow a few minutes of grace:
  the two stamps come off different clocks. **OPEN PRs are exempt**: the head
  ref *is* this name, so a push from this lane lands on that PR whoever opened
  it.

### 2.3 Hook protocol

scruff is the target of Claude Code's `WorktreeCreate` / `WorktreeRemove` hooks and
must stay tolerant of their drift. Today's bash `hook_field` accepts *either*
`name`/`worktree_name` and *either* `base_path`/`cwd` because the docs and 2.1.x
disagree. Keep that: **accept a set of aliases per logical field, first hit
wins**, and log (to `$SCRUFF_STATE/log`) which alias fired so a future CC bump is
diagnosable rather than silent.

```
scruff hook create   < JSON on stdin  → the new checkout path on stdout, NOTHING else
scruff hook remove   < JSON on stdin  → human text on stderr, nothing on stdout
```

The "only the path on stdout" rule is load-bearing (`cd "$(scruff child …)"` and the
CC hook both depend on it). Every diagnostic goes to stderr. This is a contract,
not a style choice, and needs a test.

A generic `scruff hook` also lets non-Claude clients wire in: the same JSON on
stdin from a Codex/OpenCode plugin gets the same behaviour.

### 2.4 Exit codes

The bash version has exactly two (0 / 1-via-`die`). Consumers need more:

| Code | Meaning |
|---|---|
| 0 | success — including "nothing to do" |
| 1 | usage / precondition error (bad args, not a git repo) |
| 2 | **refused for safety** — occupied, dirty, or not provably landed |
| 3 | degraded — the operation completed but a signal was unavailable (forge down, no `lsof`); pairs with a `warnings[]` entry |
| 4 | conflict found (`scruff overlap`, `scruff batch`) — a finding, not an error |
| 5 | lock contention / another scruff holds the registry |

`2` vs `1` is the one that matters: a wrapper script must be able to distinguish
"you asked wrong" from "I declined to destroy something".

And an argument a verb cannot explain is `1` — never a run of that verb with the
argument dropped. `scruff reap --help` used to sweep: help was spelled only at
the top level, `Reap` never looked at its arguments, and the flag you type to
ask a question about an unfamiliar verb ran the one verb that deletes. The rule
that fixes it belongs to every verb rather than to that flag, because the next
typo is one nobody has thought of yet: each verb answers `-h`/`--help` with its
own usage lines and does nothing else, and refuses anything it does not
recognise. Invariant 1's failure direction is "nothing happened".

---

## 3. "Landed" — the merge-strategy matrix

This is the predicate the whole safety story rests on: it decides whether a branch
**dies**. Getting it wrong in the permissive direction destroys work. The bash
version already handles more of this than most tools; the spec is to keep every
existing signal and close the remaining holes explicitly.

| How the work got to the default branch | Tip is ancestor of default? | Forge record | Detected by |
|---|---|---|---|
| Nothing — the branch never committed at all | ✅ (trivially: it *is* the default branch) | none | **not a landing → never-diverged (§3.5)**, reported as `verdict: fresh` |
| Fast-forward | ✅ | any | `merge-base --is-ancestor` |
| Merge commit | ✅ | MERGED | ancestry |
| Rebase-and-merge (forge button) | ❌ (new SHAs) | MERGED, `headRefOid` = pre-rebase tip = local tip | `headRefOid == local tip` |
| Squash-and-merge | ❌ | MERGED, `headRefOid` = local tip | `headRefOid == local tip` |
| Merged, then more commits on the branch | ❌ | MERGED, `headRefOid` ≠ tip | `post_merge_ahead` → `+N`, `scruff reship` |
| Merged, then the lane caught up on the default branch | ❌ | MERGED, `headRefOid` ≠ tip | `+N` counts `headRefOid..branch` **`--not` default** — a rebase onto, or a merge from, the default branch drags its already-landed commits past `headRefOid`, and billing those to the lane read `live+131` for two commits of its own |
| Branch amended/rebased *after* its merge | ❌ | MERGED, `headRefOid` unreachable | count falls back to 1 — "at least one commit here didn't land" |
| A previous lane wore this branch name | ❌ | MERGED, but the PR is somebody else's | **not a landing at all** — the PR is this branch's only if `headRefOid` is reachable from the tip, or it closed after the branch was born (§2.2). A fresh lane on a reused name otherwise inherits the old lane's merge and reads `+N` for nothing |
| Merged into a release branch, later to default | eventually ✅ | maybe | ancestry, once it arrives |
| Local `git merge --squash` + direct push, no PR | ❌ | none | **gap → merge-tree-empty (§3.2)** |
| Cherry-picked commit-by-commit | ❌ | none | **gap → patch-equivalence (§3.1)** |
| Merged from a fork | ❌ | `headRefName` may be `owner:branch` | **gap → §3.3** |
| PR merged >100 PRs ago | ❌ | outside the repo-wide `--limit 100` map | already safe: `branch_landed` keeps its own precise per-branch query, so the horizon only costs an annotation, never a wrong reap |
| Forge unreachable / no `gh` / offline | ❌ | unknown | **not landed** — keep. Correct, and stays correct. |
| PR closed unmerged, or the repo archived | ❌ | `CLOSED` / `isArchived` | **not landed, and never will be** — `reap` names it and keeps it; `scruff drop` is the only thing that takes it (§6.4b) |

The existing division of labour is right and must survive the port: the **listing**
uses one repo-wide `merged_map` query (a per-branch query costs ~0.5 s each, which
turns a 0.3 s listing into seconds with eight lanes), while **`branch_landed`
keeps its own exact per-branch query** because it decides whether a branch dies and
must not inherit the listing's horizon.

Likewise `default_branch` must keep resolving `refs/remotes/origin/HEAD` and never
`symbolic-ref HEAD` — the main checkout's current branch is not the branch a PR
lands on, and measuring against it once made a branch merged into a side branch
read as landed.

### 3.1 New signal: patch-equivalence

`git cherry <default> <branch>` marks every commit whose patch-id already exists
upstream with `-`. **All commits `-` ⇒ the work is upstream**, regardless of SHA.
Closes cherry-picks and rebases-done-elsewhere. Offline, no forge. Does *not*
close squashes (one squashed commit has no matching per-commit patch-id).

### 3.2 New signal: merge-tree-empty (`landed: contained`)

Strategy-agnostic and offline:

```
T = git merge-tree --write-tree <default> <branch>
T == tree-of(<default>)  ⇒  the branch adds nothing to the default branch
```

That's true for a squash merge, a manual re-implementation, and an empty branch
alike — which is exactly why it must **not** be a reap trigger by default. Spec:

- surfaced as `landed.verdict = "contained"`, `via = "merge-tree-empty"`,
  `confidence = "heuristic"`
- shown in `scruff list` as `landed?` (with the `?`)
- `scruff reap` ignores it unless given `--contained`, and even then requires
  clean + unoccupied + at least one commit not in default

Same primitive as §7 — one `merge-tree` implementation serves both features.

### 3.3 Fork PRs

`headRefName` for a cross-repo PR can arrive as `owner:branch`. `merged_map`'s
`$1==b` comparison silently never matches, so a fork-merged branch reads as
unlanded forever (safe, but it means the `+N` marker and the reap sweep both go
blind). Fix: match on the branch suffix after the last `:` **and** require
`headRepositoryOwner` to be a known remote before trusting an OID.

### 3.4 Degraded mode is a first-class state

If the forge is unreachable, scruff must say so — `exit 3`, a `warnings[]` entry,
and a visible marker in the listing — not quietly report every branch as unlanded.
Silent degradation is how a user learns to distrust the tool.

---

### 3.5 Never-diverged (`landed: fresh`)

Ancestry (§3, rung 1) answers "is the tip already in the default branch?", and a
lane cut from main five seconds ago answers **yes** — trivially, because it *is*
main. So the freshest possible branch carried the same `verdict: yes, via:
ancestry, confidence: certain` as one whose PR merged an hour ago, and every
consumer that renders a verdict rendered a brand-new lane as **merged**. The
haus's paw pill and its statusline ⏏ are the two that were seen doing it.

`fresh` is that state given its own word. Two offline facts, both required:

```
git rev-list --count <default>..<branch>  == 0   # nothing here now
git reflog show --format=%gs <branch>     has no `commit…` entry   # nothing ever
```

The reflog is the half that does the work: a branch that committed and then
merged by fast-forward has no commits of its own left to count either, and only
its reflog remembers that it ever did anything. Uncertainty resolves to the old
answer — a repo with `core.logAllRefUpdates=false`, or entries aged out by gc,
prints nothing and stays `via: ancestry`.

The test is **inverted on purpose**: it requires that *nothing but creation* ever
happened, rather than enumerating what "something happened" looks like. That
enumeration is a trap — `commit:` covers `git commit` and `--amend`, and nothing
else: cherry-pick writes `cherry-pick:`, revert `revert:`, rebase
`rebase (finish):`, `reset --hard` `reset: moving to`, `branch -f`
`branch: Reset to`. A prefix list calls every one of those fresh, which is this
same bug pointing the other way.

**It is a LABEL, not a gate.** `Landed` stays true, so `reap`, the parked sweep
and the remove hook behave exactly as they did: a never-committed branch has
nothing to lose, and this spec is about what a reader is told, not what the state
machine does.

## 4. Repo identity: the remote slug, not the directory basename

Today the bucket under `$WT_BASE` is `basename "$main"`, with one special case in
the bash predecessor's `child` command that falls back to the owner-repo slug **only** when the child's
basename collides with the spawning pane's. That's a patch on a specific collision
(a workshop dir vs the repo `hausfold/haus`), and the original "fix" was
renaming a directory on one machine — which does not survive contact with
strangers' filesystems, where two `api` checkouts under different orgs are the
common case, not the exotic one.

**0.1: key every repo on its remote slug, always.**

```
identity = owner/name   from `git remote get-url origin`, scheme/user/host stripped
path key = owner-name   (slug with '/' → '-')
$SCRUFF_HOME/<owner-name>/<worktree-name>
```

- No `origin`? Try `upstream`, then the first remote alphabetically, then fall
  back to `local/<basename>` and record `repo = null` in the registry — degraded,
  works, and `scruff doctor` tells you to add a remote.
- Multiple remotes disagreeing (fork workflows): `origin` wins; `scruff doctor`
  reports the ambiguity.
- The bucket directory is **cosmetic**; every command re-derives a lane's main
  checkout from the checkout itself (`git rev-parse --git-common-dir`), exactly as
  `resume_rows` does today. Never parse identity out of a path.

**Migration:** existing rows keep their existing `path`. scruff reads them, resolves
them, and never rewrites a path under a live row — new lanes get slug buckets,
old ones stay where they are. One `scruff doctor --relocate` can offer to move them
later. Cutover day changes nothing on disk.

---

## 5. Adapters — one template-variable set, four kinds

Everything variable becomes a TOML file in a directory: **agent clients**, **forges**,
**runtime-isolation backends**, **namers**. Built-ins ship as the same TOML, embedded via
`go:embed`, with no privileged code path — the built-in claude adapter is exactly
the file a user would write.

### 5.1 Resolution order

```
1. ~/.config/scruff/adapters/<kind>/<id>.toml     — user
2. built-in (embedded)                          — shipped
```

**Repo-local adapters are forbidden in 0.1.** A repo contributing command
templates makes `git clone` + worktree-create a remote-code-execution path;
worktrunk hit the same wall and disables `--execute` in project hook bodies.
Per-user adapters cover every real case today. Loosening later behind a
direnv-style content-hash trust prompt is a *minor* release; tightening after
teams have committed `.scruff/adapters/` is *breaking*. Ship tight.

A user adapter with the same id as a built-in shadows it wholesale (no merging —
merged config is unpredictable and undebuggable).

### 5.2 The shared template-variable set

Every adapter kind, every command template, gets the same variables. One table to
learn, one table to document.

| Variable | Meaning |
|---|---|
| `{{.Path}}` | the lane's checkout path |
| `{{.Main}}` | the main checkout path |
| `{{.Repo}}` | remote slug, `owner/name` |
| `{{.Name}}` | worktree name (branch minus the `worktree-` prefix) |
| `{{.Branch}}` | full branch name |
| `{{.Base}}` | default branch of the repo |
| `{{.Parent}}` | the spawning pane's cwd, or empty |
| `{{.Agent}}` | client id recorded for this lane |
| `{{.Prompt}}` | initial prompt, when starting a client — and the naming request, for a namer (§5.6) |
| `{{.Image}}` | path to an attached image, or empty |
| `{{.Port}}` | the deterministically allocated base port (§6) |
| `{{.Env}}` | map of the resolved environment |

Templates are `text/template` with **no** shell interpretation: each entry is an
argv slice, executed directly. No string-splitting, no quoting bugs, no injection
via a branch name containing a space.

### 5.3 Agent client — six lines

```toml
kind    = "agent"
id      = "amp"
start   = ["amp", "--cwd", "{{.Path}}", "--prompt={{.Prompt}}"]
resume  = ["amp", "--cwd", "{{.Path}}", "--sessions"]   # the client's PICKER
last    = ["amp", "--cwd", "{{.Path}}", "--continue"]   # newest here, no picker
has_chat = ["test", "-d", "{{.Path}}/.amp"]     # exit 0 ⇒ a transcript exists
image_flag = "--image"                           # optional; omitted ⇒ name the file in the prompt
```

**`start` must end its client's option parsing before `{{.Prompt}}`** — with a
`--` for a positional prompt, or the `--flag={{.Prompt}}` spelling for a valued
one. Prompts are routinely markdown lists, so the first character is a dash, and
a bare `{{.Prompt}}` argv element is read as a flag: the client exits with
`unknown option '- …'` before the pane draws. The built-in three do this today.

`has_chat` replaces the hardcoded "only Claude exposes a cheap cwd → transcript
test" special case: an adapter that omits it simply answers "unknown", and scruff
falls back to the client's own cwd-filtered picker, which is today's behaviour for
Codex and OpenCode.

**`resume` and `last` are two rungs, and which one fires is scruff's decision, not
the adapter's.** A lane's own checkout is a directory only that lane's agent ever
ran in, so "the newest conversation here" *is* the lane's chat — presenting a
picker there asks the user to answer a question with one answer, from a list
whose entries are indistinguishable at a glance. `scruff <name>` therefore runs
`last` whenever the chat lives in the lane's own checkout, and `resume` only for
the one case where scruff genuinely cannot name the conversation: a spawned lane
(`scruff child`, a nested spawn) whose chat lives in a SHARED parent checkout full
of unrelated sessions. `scruff <name> --pick` forces the picker for when the newest
isn't the one wanted. An adapter that omits `last` keeps the picker everywhere —
`last` is the addition, not the default.

Note what this deliberately does *not* do: record a session id. The id is
derivable — it is whatever the client itself calls newest-in-this-cwd — and a
recorded one goes stale the moment a session is forked or compacted, which is a
worse failure than the picker because it fails silently into the wrong chat. If
a future client makes newest-in-cwd unknowable, that is an adapter key
(`session_of = [...]`) and a v1 registry field (§2.1), never a seventh TSV
column.

### 5.4 Forge — six lines

Detected from the remote host, so `gh`/`glab`/`tea`/`bb` is chosen automatically.
**scruff never implements an auth flow of its own** — it delegates to whatever forge
CLI is on `PATH`, and falls back to the git-only merge-base check when none is.

```toml
kind  = "forge"
id    = "github"
hosts = ["github.com"]
probe = ["gh", "auth", "status"]
pr_for_branch  = ["gh", "pr", "list", "-R", "{{.Repo}}", "--head", "{{.Branch}}", "--state", "merged", "--limit", "1", "--json", "number,state,headRefOid"]
open_prs       = ["gh", "pr", "list", "-R", "{{.Repo}}", "--state", "open", "--limit", "100", "--json", "number,headRefName,headRefOid,title,url"]
```

Adapters declare **JSON-emitting** commands and scruff maps them through a small
per-adapter key mapping (`number`/`state`/`head_oid`/`head_ref`), so `glab`'s
different field names are a config concern, not a code concern. Every forge call
goes through the existing 6-second timeout + on-disk cache — a stalled network
must never hang pane teardown.

### 5.5 Runtime isolation — six lines

Default backend is `none` + deterministic port/env allocation (§6). Containers are
**at most one optional backend, never the mechanism**.

**One backend ships built in: `tart`.** `--backend tart` with no adapter file
clones an image, boots the guest headless with the lane shared in, and waits
for a shell to answer on the guest's address — an address alone is not enough,
because `tart ip` answers on a DHCP lease, most of a minute before sshd does.
`SCRUFF_TART_BASE` names the image, `SCRUFF_TART_USER` the account `enter` sshes
in as, `SCRUFF_TART_SSH_WAIT` how many seconds the shell gets (180). The guest's
console goes to a boot log under the state dir, never to the caller's stdout: a
`tart run` lives as long as the guest, and one that inherits the caller's pipe
holds it open until teardown. It is built in because its `setup` is four
commands and two waits, which one argv slot cannot hold, so the file-only rule
made every user write the same script before the verb worked at all. It changes nothing about
the mechanism: a `tart.toml` on disk still wins, nothing is automatic, and
`scruff runtime eject tart` prints the file to start from. It exists because an
agent lane that has to *see* a desktop change work should take a disposable
macOS rather than the screen its user is sitting at.

```toml
kind  = "runtime"
id    = "apple-container"
setup = ["container", "run", "-d", "--name", "scruff-{{.Name}}", "-v", "{{.Path}}:/work", "IMAGE"]
enter = ["container", "exec", "-it", "scruff-{{.Name}}", "bash"]
teardown = ["container", "rm", "-f", "scruff-{{.Name}}"]
```

### 5.6 Namer — one line

A lane opened on a first-turn task (`--prompt`/`--prompt-file`) with no name given
can be named after that task instead of after an animal. `mobile-nav-jitter` is
worth more in a listing than `cozy-otter`, and the brief is right there.

```toml
kind = "namer"
id   = "claude"
name = ["claude", "-p", "--model", "haiku", "--strict-mcp-config", "--disable-slash-commands", "--", "{{.Prompt}}"]
```

Selected by a top-level `namer = "<id>"` config key, and **absent by default**:
no key, no process, and an unnamed lane keeps taking a random word pair, exactly
as it did before this kind existed.

`{{.Prompt}}` here is scruff's whole naming REQUEST — the instruction, the repo,
the lane names already taken and the task, composed by scruff — and the command
answers on stdout with the name and nothing else. scruff owns that wording so that
name quality is scruff's problem rather than every adapter file's; an adapter that
disagrees wraps a script and reshapes the text it was handed.

**scruff never talks to a model.** It runs one argv and reads a word off stdout, so
there is no HTTP client here, no vendor and no API key for scruff to hold. The
built-in runs the `claude` binary a machine spawning agents already has, which
means the naming call is authenticated the same way the agents are; pointing the
same key at a local model, or at a script with no model in it at all, is a file.
`claude` is built in for the same reason `tart` is (§5.5) — it is the one every
install already has a reason to have — and a `claude.toml` on disk still wins.

**It is cosmetic, and it may never cost a lane.** No adapter, an uninstalled
namer, a timeout, prose instead of a name: every one of them is a warning and a
fall back to the random pair. The output is a model's text on its way to becoming
a branch name and a path, so it is not trusted either — what reaches `git
worktree add` is rebuilt from scratch as one to three `[a-z0-9-]` words, and a
candidate that is not already shaped like a name is rejected whole rather than
cleaned up.

### 5.7 `name_max` — a name the backend can carry

A lane name is a branch and a directory, both of which take almost anything. It
is also an identity a lane BACKEND has to hold, and a backend can have a ceiling
the filesystem does not. haus renders the key scruff writes for a lane —
`scruff/<repo>/<lane>` — as a zmx session name, and zmx names a unix socket after
it, so on macOS the whole key has to fit in 46 bytes. Past that the session
cannot be created, and the failure lands where nothing can act on it: the lane
exists, its window dies before the client starts, and the only error is one the
terminal emulator prints.

The machine that knows its ceiling states it:

```toml
# ~/.config/scruff/config.toml
name_max = "46"   # the longest `scruff/<repo>/<lane>` key this machine can hold
```

**Absent by default** — no key, no cap, exactly as every install behaved before
it existed. It is read quoted or bare (`name_max = 46`). The cap is on the whole
key rather than on the name because the repo is half of what has to fit: 46
leaves 27 bytes for a lane in `hausfold.co` and 35 for one in `nix`.

It is enforced at the one moment the name can still change, which is when it is
chosen, and the two halves are deliberately asymmetric:

- A name **scruff chose** — a namer's answer, the random pair — is built to fit.
  The budget reaches `sanitizeName`, so the namer stops on a whole word
  (`docs-displays-expansion`) instead of a name being cut afterwards
  (`docs-displays-expansion-sl`).
- A name the caller **typed** is refused, with the number it had, the number it
  gets and where the rest went. Trimming it silently would put their work on a
  branch they did not ask for and never tell them.
- The `-2` a collision adds counts against the budget too, and the same split
  holds: a chosen base gives those bytes back, a typed one is refused instead.
  Trimming there is the silent rename with the consequence hidden —
  `fix-perch-drag-and-drop-lag` would land as `fix-perch-drag-and-drop-2`, one
  suffix away from a different lane.

A repo leaving fewer than three bytes gets no budget rather than an impossible
one: nothing scruff would call a name fits anyway, and a lane nobody can name is
worse than one that might not open. The backend's own error is the backstop
there, and for `scruff hook create`, where the CLIENT owns the name and has
already made the branch — that path can only warn.

---

## 6. Bootstrap & lifecycle hooks

A fresh worktree is useless if `node_modules` isn't there, `.env` isn't there, and
the dev server wants port 3000 that four other worktrees already want.

### 6.1 Hook points

Two kinds, and the difference is what they are allowed to change.

**Lifecycle hooks** run *around* a transition scruff is going to make anyway:
`pre-create`, `post-create`, `pre-park`, `post-unpark`, `pre-reap`, `post-reap`.
Each runs argv-slices (no shell), with the §5.2 variables, and a non-zero exit on
a `pre-*` hook aborts the transition (exit 2 — refused).

**Policy seams** (§6.5) run *instead of* a decision scruff would have made. They
are the answer to the question the lifecycle hooks can't reach: not "do
something extra when a branch is reaped", but "no — *this* is what reapable
means here."

### 6.2 Config file

`~/.config/scruff/config.toml` for the machine-wide defaults; `<repo>/.scruff.toml`
for the repo. **The split on repo-local config is by execution, not by file:**

The machine config's implemented top-level default is `agent = "claude"` (or
`codex` / `opencode` / `pi`), plus the `[hooks]` table of §6.5. Agent resolution is
`SCRUFF_AGENT`, then the `agent` **hook**, then this key, then the legacy
`HAUS_AGENT_DEFAULT` environment fallback, then Claude. This keeps the
default stable for long-running callers while retaining a one-invocation
override for standalone use — and the hook rung exists because "which client"
is a decision on some machines and a constant on most.

| Repo-local key | Allowed? | Why |
|---|---|---|
| `copy`, `link`, `reflink` | ✅ | declarative paths, no execution |
| `ports`, `env` | ✅ | declarative values |
| `secrets` | ✅ | declarative paths, never contents |
| `run` / any hook body | ❌ unless trusted | it's execution — same RCE reasoning as §5.1 |

`scruff trust` records a content-hash of `<repo>/.scruff.toml`; a changed hash
re-prompts (direnv's model, applied to exactly the one dangerous key). Untrusted
`run` entries are *listed* by `scruff doctor` — "this repo wants to run X; `scruff
trust` to allow" — not silently dropped.

### 6.3 Built-in steps

```toml
[bootstrap]
reflink = ["node_modules", ".venv", "target", "vendor"]   # heavy gitignored dirs
copy    = [".env.local"]
link    = ["../shared-fixtures"]
ports   = { web = 3000, api = 8080 }
secrets = [".env.local", ".npmrc"]
run     = ["pnpm", "install", "--offline"]                # trust-gated
```

**reflink is the headline.** `cp -c` on APFS, `cp --reflink=auto` on btrfs/xfs,
`Clonefileat`/`FICLONE` via `x/sys/unix` with no CGo. It is strictly better than
both alternatives for the `node_modules` case:

| | cost | correctness |
|---|---|---|
| copy | seconds–minutes, GBs of disk | correct |
| symlink | instant, no disk | **broken** — pnpm and anything resolving `realpath` escapes into the source tree |
| **reflink** | instant, near-zero disk until write | correct, COW-isolated |

Fall back to copy with a `warnings[]` entry when the filesystem can't reflink;
never silently symlink.

**Ports:** `base = 20000 + (crc32(branch) mod 10000)`, then offset per named port.
Deterministic, so the same branch gets the same port every rebuild, and
collision-checked against the registry's other live allocations.

**Defer, don't fight.** If `.envrc` (direnv), `mise.toml`, `flake.nix` +
`.envrc`, or `.devcontainer/` is present, scruff's default is to **do nothing** and
say so: those tools already own environment materialisation, and racing them
produces two half-configured environments. `scruff doctor` reports "direnv detected
— scruff is deferring; add `[bootstrap] force = true` to override."

**Secrets:** files listed under `secrets` are `chmod 600` on create and
shredded (overwrite + unlink) on reap. They're never copied into a park commit —
a `.gitignore`d secret must stay ignored, and `scruff park` must refuse to `git add
-A` a file matching a `secrets` entry even if it's untracked-but-not-ignored.
That's a 5/5 failure mode and needs a test.

### 6.4 `scruff doctor`

Two halves under one verb, and they are not the same size.

```
scruff doctor            # ✅ diagnose this machine and this repo
scruff doctor --json     # ✅ the same, as data
scruff doctor --write    # ⏳ 0.2 — write a proposed .scruff.toml
```

**Diagnose — shipped.** It reports the base and its resolution; git, the forge
CLI *and whether it is authenticated*, occupancy (`lsof` / heartbeat leases),
whether the filesystem the checkouts land on supports reflink, and the machine
config with its hooks; then, for the repo it was run in, submodules / LFS /
sparse-checkout (§8) and **which rung answered the default-branch question** —
`origin-head` | `conventional` | `head`, weakest last, because the weakest rung
moves when somebody checks out a side branch in the main checkout and that is
the branch every landed verdict is measured against. Then the findings: stale
registry rows, stray checkouts, orphan branches, and disk used per repo
(`du`-equivalent, walked in Go, counting allocated blocks so a reflinked tree
reads as the near-nothing it costs).

Three decisions worth stating, because each is load-bearing:

- **It exits 0, findings and all.** A finding is doctor working. Exit 3 would be
  defensible for "a signal was unavailable" and is wrong here — the absences
  *are* the diagnosis, so a machine merely lacking `gh` would exit non-zero on a
  healthy run and the command would be unusable under `set -e`. `--json` carries
  every finding as data for a caller that wants to gate on one. `--migrate-base`
  keeps its own 2 and 3.
- **It fixes nothing.** `scruff` the listing sweeps parked lanes as it goes;
  doctor deliberately does not, and reports what a sweep *would* prune instead.
  It is the output a stranger is asked to paste into a bug report, and a
  diagnostic that repairs what it was asked to describe destroys the evidence.
  The one exception is the reflink probe, which clones a two-byte file in a temp
  dir under the base and removes it — nothing but trying it on that filesystem
  answers the question, and the probe runs the same `cp -c` / `cp --reflink`
  §6.3 will.
- **`--json` shares the §2.2 envelope's HEADER — `scruff`, `schema`, `warnings`
  — and carries no `lanes` key.** There, `lanes` is an array of lane objects;
  reusing the name for doctor's counts would be a meaning change in the one
  field the freeze is most specific about, so the counts live under `summary`.
  The nullable rule travels with the header: `forge.authenticated` is `null`
  with no `gh` to ask and `false` when `gh` was asked and said no, `reflink.supported`
  is `null` with nowhere to test, `repo` is `null` outside a git repo, and
  `disk.bytes` is `null` when the walk failed. None of those are `false`.

**Propose — 0.2.** Writing a `.scruff.toml` needs a per-repo config layer that
does not exist: `internal/config` reads `~/.config/scruff/config.toml` and
nothing else, so there is nothing for a proposal to be a proposal *of*, and
§6.2's `run` key arrives gated on `scruff trust`. `--write` therefore refuses
with exit 1 naming that, rather than writing a file scruff cannot read back.
When it lands it detects: package manager and its heavy dirs; `.env*` files that
are gitignored (candidates for `copy`/`secrets`); ports in
`docker-compose.yml` / `vite.config` / `package.json` scripts; and
direnv/mise/nix/devcontainer presence — the onboarding lever, because the
alternative is reading a TOML reference.

### 6.4b Dead ends, `drop`, and the reap ledger

Two lanes can never land, and neither is "not landed yet":

| shape | forge record | scruff's answer |
|---|---|---|
| The branch's PR was **closed unmerged** | latest PR `CLOSED`, none `MERGED` | named by `reap`, never swept — the work was *rejected*, and those commits are the only copy |
| The repo is **archived** on the forge | `isArchived: true` | named by `reap`, never swept — nothing can be submitted anywhere any more |

Both read exactly like an in-flight branch, so before this they outlived
everything around them with no signal at all. The obvious fix — let `reap` take
them — is the wrong one, and the asymmetry is the whole design: **`reap` is
automatic, so it may only ever take landed work; `drop` is a human typing one
lane's name, so it may take anything.** Widening the automatic sweep to delete
rejected work is precisely the thing scruff exists to never do.

`deadEnd` costs two forge calls, so it is asked **only of lanes a sweep has
already declined to reap** — the listing (which the statusline runs several
times a minute) never pays for it.

**The ledger.** Every branch deletion — `reap`, the parked sweep, the remove
hook, `drop` — writes one line to `$STATE/reaped.log` *before* the delete:
`when, repo, name, branch, sha, via, pr`, tab-separated, append-only field
order. `scruff reaped` reads it back with the recovery command spelled out.

This exists because a branch deletion destroys its own evidence: `git branch -D`
takes the branch's reflog with it and `git worktree remove` takes
`.git/worktrees/<name>`, so a lane that vanished between two listings left a
repo where the only honest answer was "something deleted it, and the record of
what died with the thing it recorded". `watch` emits a `reaped` event, but only
to whoever happened to be streaming at that instant. The recorded SHA outlives
git's own reflog entry, which makes every reap **reversible**, not merely
attributable.

### 6.5 Policy seams — the hardcoded facts, and how to disagree with them

scruff grew out of one machine's desktop, and it inherited that machine's answers to
questions that only *look* universal. "Landed" means merged into the default
branch. "Reapable" means landed, clean and unoccupied. "Resume" means become the
client process. Every one of those is a house rule wearing a universal name, and
every one of them is wrong somewhere: a shop that merges into a release train, a
machine that can enumerate its own panes better than `lsof` can, a multiplexer
user who wants a new pane rather than a hijacked one.

The fix is not more configuration keys. It is to name each decision, ship scruff's
answer as the *default* rather than the *mechanism*, and let a consumer replace
it. That is the difference between a tool and a substrate: haus should be
able to say "here is what resuming means on my machine" without scruff having ever
heard of zellij.

#### The protocol

A seam is a program, not an expression language. scruff execs the hook's argv and
reads the answer off the exit code.

| exit | predicate seam | action seam |
|---|---|---|
| `0` | yes | handled — scruff does nothing further |
| `1` | no | failed |
| `2` | no, **refused for safety** | refused — propagates as scruff's exit 2 |
| `3` | **no opinion — run the built-in** | **declined — run the built-in** |
| anything else, or wouldn't exec | defer, **and warn** | defer, **and warn** |

0/1/2 mean what they mean in scruff's own exit-code table (§2.4), so a hook and a
wrapper script speak one language. `3` is the only addition, and it is
deliberately not 0/1/2 so that the ways a script dies by accident — `1` from
`set -e`, `126` from a lost `+x` bit, `127` from a typo — can never be mistaken
for an opinion. **Every failure mode defers**: a broken hook costs you the
override, never the operation, because scruff is in the path of every pane open
and a stale store path must not be able to close that door. It costs you the
override *loudly* — a policy that silently stopped applying is worse than one
that never existed, because the operator still believes it is in force.

The situation arrives twice, so a seam can be a program with a JSON parser or
three lines of shell without either having to become the other:

- **stdin** — a JSON object, for predicates. (Action seams inherit stdin; they
  may be interactive.)
- **environment** — `SCRUFF_HOOK`, plus `SCRUFF_<FIELD>` for every §5.2 variable:
  `SCRUFF_PATH`, `SCRUFF_MAIN`, `SCRUFF_REPO`, `SCRUFF_NAME`, `SCRUFF_BRANCH`,
  `SCRUFF_PARENT`, `SCRUFF_CWD`. Three collisions to know about, all the same
  shape — scruff's own environment got to the name first, and a hook leaks its
  environment into every pane it spawns, so a field spelled as one of these
  hands scruff back its own input: `SCRUFF_BASE` is the lane base *directory*, so
  the repo's default branch is **`SCRUFF_BASE_BRANCH`**; `SCRUFF_STATE` is the
  state *directory* (§9.1) and `SCRUFF_AGENT` is the one-invocation client
  override (§5.3), so the lane's are **`SCRUFF_LANE_STATE`** and
  **`SCRUFF_LANE_AGENT`**.

`focus` is `resume`'s narrower sibling and the seam a desktop wants most: the
lane is already running, and the question is only which window to raise. scruff
cannot answer that — the join from a lane to a window belongs to whatever opened
it — so the built-in is `resume`, and a consumer that knows its own windows
overrides exactly this step. A `focus` hook that **defers** (exit 3) means "no
window of mine holds that lane", and scruff falls back to resume, which opens one:
a detached lane is running, not gone. Its usual caller is not a human — trill
runs `scruff focus <name>` when a lane's banner is clicked — which is why the
no-hook path still has to land somewhere useful.

The two action seams that open a session — `resume` and `open` — carry three
more, because a hook that spawns a pane has to reproduce a decision scruff already
made: `SCRUFF_CHAT` (the cwd the conversation lives in, which for a spawned lane
is NOT `SCRUFF_PATH` — getting that wrong is how a resumed child lane opens an
empty session), `SCRUFF_LANE_STATE`, and **`SCRUFF_COMMAND`** — the exact client
invocation scruff was about to exec, already resolved to continue-the-newest or
open-the-picker per §5.3. A hook that re-derives it instead lands its new pane
on the picker scruff just spared the user.

`SCRUFF_COMMAND` is a command STRING, **shell-quoted per argument**, and a hook
runs it through a shell rather than word-splitting it. That was invisible while
every invocation was one or two bare words; `new`/`spawn --prompt` put a whole
task in there ({{.Prompt}}, §5.2), and a brief spans lines and holds quotes and
`$`.

A predicate may print a JSON object on stdout to enrich its yes/no — a `landed`
hook naming its own rule, so a reap stays attributable in `--json` (`via:
"release-train"` beats `via: "hook"` when you are working out why a branch went
away). Prose on stdout is not an error; the exit code already answered.

Action seams get the terminal, but their **stdout is redirected to stderr**.
Both are the same tty for an interactive hook, so a TUI still draws — and scruff's
stdout carries data under a contract other programs parse (§2.3), which a hook
must not be able to break.

```toml
# ~/.config/scruff/config.toml
name_max = "46"                                             # §5.7

[hooks]
resume   = "/nix/store/…-scruff-on-resume"                  # a bare program
landed   = ["/nix/store/…-scruff-landed", "--release-train"] # or an argv
```

#### Shipped seams

| Seam | Kind | Answers | Built-in |
|---|---|---|---|
| `agent` | predicate | which client a new lane opens in | the `agent` key, then `SCRUFF_AGENT`, then claude |
| `landed` | predicate | has this branch's work reached the default branch? | the §3 ladder |
| `preserve` | predicate | does this dirty tree need a wip commit before removal? | yes, unless it's untracked scratch on a landed branch |
| `resume` | action | reopen this lane's session | chdir + exec the client — continuing the newest conversation there, or its picker for a shared parent (§5.3) |
| `open` | action | open a session in a freshly-created lane | chdir + exec the client |
| `focus` | action | put the window this lane is already running in in front | `resume` — the only go-to scruff has without a window layer |

Two things are **not** seams and will not become them, because they are about
scruff not sawing off the branch it is sitting on rather than about policy: the
checkout scruff is being **run from** is never swept, and a **stray** is never
swept, only reported.

**`reapable` is deliberately absent.** It is the obvious next seam and it was
built, tested and pulled back out: reapability reaches through *three* of scruff's
inherited opinions at once — occupancy, dirtiness, landedness — and a seam over
the lot of them is a bigger commitment than the five above, because a `yes` on a
dirty tree is the one answer that destroys work. It waits for the architecture
those three settle into. Overriding `landed` already moves the rung that matters
most; the rest stays scruff's until the shape is known.

#### Still hardcoded — the roadmap

The seams above are the ones with a consumer waiting. These are the rest of
scruff's inherited opinions, in the order they are worth prising out:

| Fact | Where | Shape |
|---|---|---|
| the `worktree-` branch prefix | `create.go`, `new.go`, `park.go` | a `branch` seam, or a config template |
| how each client is started / resumed | `agent.go` | adapter TOML (§5.3) — already specced |
| `gh`, and GitHub's argv | `landed.go` | forge adapter (§5.4) — already specced |
| what makes a lane **reapable** | `sweep.go` | a `reapable` seam — see above; blocked on the three opinions it spans |
| occupancy = `lsof` cwd prefix | `sweep.go` | a provider list (§9) |
| `$BASE/<bucket>/<name>` layout | `new.go` | a `path` seam |
| the `wip:` commit message and park semantics | `park.go`, `remove.go` | a `park` seam |
| Claude's trust-file seeding | `agent.go` | `post-create` (§6.1) |
| transcript-directory layout | `agent.go` | adapter `has_chat` (§5.3) |
| the two-word random name | `new.go` | a `name` seam |

The test for whether one belongs here: would a *reasonable* machine answer it
differently? "Never delete a branch that isn't landed" is scruff's product and
stays a floor. "Landed means merged into `main`" is a guess, and guesses get
seams.

---

## 7. `overlap` — conflict prediction, and the Clash question

### 7.1 What Clash is

[clash-sh/clash](https://github.com/clash-sh/clash): Rust, MIT, 63 stars, created
Feb 2026, last push mid-July. `clash check <file>` / `clash status` / `clash
watch`. It discovers worktrees via `git worktree list`, finds the merge base for
each pair, runs `git merge-tree` (via `gix`) in memory, and reports conflicting
files as a matrix. 100% read-only. `--json`, exit `0`/`2`/`1`. Ships a Claude Code
plugin that wires `clash check` as a **blocking `PreToolUse` hook on
`Write|Edit|MultiEdit`**.

### 7.2 Verdict: build a slim version inside scruff; don't adopt Clash

The *idea* is correct and worth having. The *dependency* isn't, for four reasons:

1. **It's one git primitive.** `git merge-tree --write-tree A B` (git ≥ 2.38) does
   the whole thing and exits non-zero on conflict. That's ~200 lines of Go
   including the matrix rendering. Taking a second Rust binary, a second config
   file, and a second Claude plugin for that flatly contradicts scruff's own "single
   binary, no runtime dependencies" pitch.
2. **It's structurally blind to exactly the lanes that matter to scruff.** Clash
   enumerates `git worktree list` — so it sees live checkouts only. scruff's
   **parked** branches have no checkout on disk at all, and those are precisely
   the ones you've forgotten about and that have been rotting against `main` for a
   week. scruff has a registry; it can merge-test a parked branch that Clash cannot
   see. That's not a bug in Clash, it's a capability scruff has and Clash can't get.
3. **The blocking-hook integration is the wrong shape.** A `PreToolUse` prompt on
   every `Write|Edit` is a high-frequency interruption for a low-frequency event —
   and Clash's own README notes that Claude Code doesn't render
   `permissionDecisionReason`, so what you actually get is a bare permission
   prompt with no explanation of why. Across five agents that's unusable.
4. **Traction says it's a weekend idea, not infrastructure.** 63 stars, 1 fork,
   both Show HN posts sitting at 1 point, single maintainer. Fine tool. Not a
   dependency to hang a differentiator on.

MIT license means there is nothing to negotiate about implementing the same
approach — and the approach is one paragraph of public documentation, not IP.

### 7.3 What scruff builds instead

```
scruff overlap [--json] [--committed-only] [--pair A B]
```

- Pairwise `git merge-tree --write-tree` across **every registry lane,
  including parked branches**, using each pair's own merge base.
- **Uncommitted work counts.** Clash's `has_active_changes` is a bare dirty
  boolean — it doesn't merge-test what the agents are currently typing, which in
  agent lanes is *most of the interesting content*. scruff builds a throwaway
  tree per lane with `GIT_INDEX_FILE=$tmp git add -A && git write-tree` (the
  real index is untouched) and merge-tests those. That's the difference between
  "these branches will conflict eventually" and "your two running agents are
  fighting over `src/auth.ts` right now". `--committed-only` skips the worktree
  stat for speed.
- Output: conflicting file list per pair, plus the matrix. Exit 4 on conflicts
  found (a finding, not an error).
- **Passive surfaces, not blocking ones.** An `overlap` column in `scruff list` and
  a token in the haus statusline. Optionally a *non-blocking* advisory hook
  that prints to the transcript. A blocking `PreToolUse` gate is available but is
  strictly opt-in and never the documented default.

Scaling is a non-issue and should be stated so: N is 3–8, merge-tree is
milliseconds, 12 lanes is 66 pairs and still sub-second. Cache keyed on the
pair's `(tipA, tipB, mergeBase)` triple; the temp-tree path additionally keys on
worktree mtime.

### 7.4 The real payoff: `overlap` is stage 0 of `batch`

Conflict prediction as a standalone novelty is a demo. Conflict prediction as the
**free, offline prefilter for the expensive integration test** is architecture.
See §8.

---

## 8. `batch` — the differentiating feature

Generalises haus's `bench try-batch`. The problem it solves: the normal
review flow is merge-then-test, which puts unverified code on `main` before anyone
has felt it. `batch` inverts that.

```
scruff batch [--verify "CMD"] [--json] [--land] [--exclude PR…]
```

Pipeline:

1. **Collect** open PRs via the forge adapter (`open_prs`), plus local
   `worktree-*` branches with no PR (opt-in via `--include-local`).
2. **Prefilter** with §7's pairwise merge-tree. Free, offline. Pairs that can't
   co-merge are recorded now, before a single integration worktree is created.
3. **Integrate**: a throwaway worktree off the default branch, merging candidates
   in a deterministic order (PR number ascending — reproducible, so the cache key
   means something), recording each conflicting pair as it's hit rather than
   aborting.
4. **Verify**: run the user-supplied command in the integration worktree. scruff has
   no opinion about what it is. Nonzero ⇒ the set is red.
5. **Bisect the queue.** This is the feature. When verify fails on the full set,
   binary-search the *merge set* — not the commits — to name the culprit PR, or
   the culprit **pair** when no single PR is red alone. Halt at ~log₂(N) verify
   runs. Report "PR #182 alone is green; #182 + #177 is red" — that sentence is
   the entire value proposition, and no other tool in this space produces it.
6. **Report**: a tick-off checklist for humans, `--json` for machines. Each entry
   carries the PR's own Verify block from its body, so a failure sends you
   somewhere useful.
7. **`--land`** (opt-in, never default): merge only the green set, via the forge's
   PR merge — never a local merge + push.

**Cache** keyed on the merge-set hash (sorted tip OIDs + base OID + verify command
string). Re-running after one PR moves re-verifies only what changed.

Invariant: `main` is never touched. The integration worktree is disposable and
removed on completion, including on failure (behind `--keep` for debugging).

---

## 9. Portability gaps to close

| Gap | Today | 0.1 |
|---|---|---|
| **Occupancy** | ~~one `lsof -d cwd` dump~~ — **done**, see §9.1 | `lsof` is one provider among several; leases cover the rest. Still open: `/proc/*/cwd` as a third provider on Linux. |
| **Forge** | `gh` hardcoded, GitHub-shaped | §5.4 forge adapters; git-only merge-base fallback when none is present |
| **Submodules** | not initialised in a new worktree — `git worktree add` doesn't recurse | detect `.gitmodules`; `bootstrap.submodules = "recursive" \| "none"`; default `none` with a `doctor` warning, because recursing can be minutes |
| **LFS** | pointers, not files, unless a smudge runs | detect `.gitattributes` filter=lfs; offer `git lfs pull` as a bootstrap step; warn loudly rather than silently handing over pointer files |
| **Sparse-checkout** | not inherited from the main checkout | copy the main checkout's sparse patterns into the new worktree by default (`--no-inherit-sparse` to opt out) — inheriting is nearly always what's meant |
| **Disk accounting** | ~~none~~ — `scruff doctor` reports per-repo usage in allocated blocks (§6.4) | `scruff list --disk` for the per-lane breakdown; flag when reflink fell back to copy and the tree is >1 GB |
| **`python3` dependency** | `hook_field` shells out to python3 to parse hook JSON | gone — Go has `encoding/json` |
| **Registry race** | whole-table temp-file rewrite | per-row files + `flock` (§2.1) |
| **Windows** | not attempted | out of scope for 0.1; state it. Path handling should not gratuitously preclude it. |

### 9.1 Occupancy is a provider set — and only some providers may say "empty"

Shipped. `internal/occupancy` folds N providers into one `Report`, under a single
rule that is worth stating as an invariant in its own right:

> **A provider may always assert PRESENCE. Only a provider that can enumerate
> every possible occupant may assert ABSENCE.**

`lsof` dumps every process's cwd, so a path missing from that dump is real
evidence nobody is there — it vouches for absence. A **lease** is the opposite:
it is written only by clients that opted in, so "no lease" means "nobody told
me", never "nobody is here". A lease can therefore save a checkout from `reap`
and can never condemn one. Reading an unleased checkout as free would reap the
worktree of anyone who simply `cd`'d in without telling scruff — invariant 2,
broken by the tool whose entire purpose is invariant 2.

`Report.Known()` is true only when *some* provider vouched for absence, and
unknown still resolves to **keep**, exactly as when `lsof` was the only answer.

**The lease.** `$SCRUFF_STATE/live/<sha256(path)[:12]>`, containing `pid<TAB>path`.
`$SCRUFF_STATE` is `$XDG_STATE_HOME/scruff` (default `~/.local/state/scruff`) — pointedly
*not* `$BASE`, which is globbed for checkouts, and pointedly not where the
registry lives, because no state-dir knob may be able to relocate the file
cutover day has to read (§10).

**The ask markers.** `$SCRUFF_STATE/asks/<key>`, one empty file per banner
`scruff hook notify` has up, named for the key trill knows it by with the slashes
flattened to dots (`scruff/<repo>/<lane>` → `scruff.<repo>.<lane>`). It is a cache
and only a cache: it exists so that the resume events — which fire on every
tool call in every pane — can answer "is anything waiting?" with one directory
read instead of a registry load and a launch of trill's binary. Nothing may
treat it as the truth about what is on screen, because anything can take a
banner down without telling scruff.

Two shapes of marker never get the tool call that would clear them — a lane
blocked on you when its pane closed, and a pane outside every lane whose
session has ended — so the sweep prunes both: reaping a lane clears its marker
(and resolves its banner, which now names somewhere that no longer exists), and
anything older than a day goes regardless. Without that the directory is never
empty again and the cheap answer becomes permanently the expensive one.

⚠️ **A desktop may read this directory** to find out whether any lane is
waiting before doing something costlier of its own; hausfold/haus does exactly
that. The path, the flattening and "empty means nothing is waiting" are
therefore a contract with more than scruff in it, and changing the naming breaks
a reader that cannot be seen from here.

A **relative** `$SCRUFF_STATE` is refused — scruff warns and uses the default. This
state is machine-global, so resolving it against the process cwd scatters the
lease and the ledger into whatever directory scruff was run from, routinely a git
checkout, where they surface as an untracked dir and can be swept into a `wip:`
commit by scruff's own park path. Nobody has ever meant that; an operator who
wants state elsewhere says so absolutely.

```
scruff heartbeat [path]            take or refresh; held by the CALLING process
scruff heartbeat [path] --pid N    held by pid N instead (0 = no watchable process)
scruff heartbeat [path] --release  drop it
```

A named pid settles liveness outright, in both directions: the kernel is a
better witness than any timestamp, it answers the instant a client is killed
rather than TTL-later, and a client holding a lease across an eight-hour session
never has to prove it is still there. The 90 s TTL exists only for `--pid 0` —
a holder on the far side of a container or a socket, where freshness is the only
evidence on offer. Dead leases are unlinked on sight.

The default pid is the **calling** process, not scruff's: an embedder exec's scruff,
scruff exits immediately, and a lease watching an exited process is a lease that
was never taken.

**`SCRUFF_OCCUPANCY=lease`** is the one deployment entitled to let leases answer
for absence too — an orchestrator that owns every session it serves, where a
lane nobody leased genuinely is a lane nobody is in. Opt-in by explicit env
var, never inferred from (say) the absence of `lsof`.

**How this relates to §6.5's seams.** §6.5 names "a machine that can enumerate
its own panes better than `lsof` can" as a motivating case, so an `occupied`
hook is clearly coming. These are not two competing inversions: a hook is **one
more `occupancy.Provider`** — the one that shells out — and it folds through
`Collect` under exactly the rule above. Which means the hook contract has to
answer *two* questions, not one: "is this lane held?" and "can you see every
possible occupant?". A seam that answers only the first is positive-only, like a
lease. Getting that second answer into the protocol is the design work; the
provider set is already the right shape to receive it.

---

## 10. The haus consumer story

### Keeps

- `modules/den/` keeps the **statusline** (it's haus-specific presentation) — it
  just consumes `scruff list --json` instead of parsing TSV by hand.
- The `WorktreeCreate`/`WorktreeRemove` hook wiring, retargeted to `scruff hook
  create` / `scruff hook remove`.
- `bench` keeps `try`, `try-batch`, `ship`, `release`. `try-batch` becomes a thin
  wrapper over `scruff batch --verify "bench try"` — bench supplies the nix
  knowledge, scruff supplies the queue mechanics and the bisect.
- The zellij keybinds, pounce's "Spawn Agent" command, the `⌘A` agent-spawn seam.

### Deletes

- `modules/den/wt.sh` (1295 lines) in its entirety. **Done** — deleted in
  haus#245, along with its test suite and the nix package that put it on
  `PATH`.
- `test/wt.bats` moves *out* of haus and into scruff (it's scruff's acceptance
  suite now; haus keeps only integration smoke tests for the hook wiring).
- The hand-maintained shell-side client list.

### `modules/lib/agents.nix` stops being a duplicate

Before the rewrite, `agents.nix` was the Nix-side list and `agent_known()` in
the bash predecessor was a hand-maintained shell-side copy of the same set —
its own comment admitted a fourth client meant editing both, because a shell
script can't read Nix.

Fix: **Nix generates adapter TOML into a directory scruff reads.**

```nix
# modules/den/scruff.nix
environment.etc."scruff/adapters".source = pkgs.runCommand "scruff-adapters" { } ''
  mkdir -p $out
  ${lib.concatMapStrings (c: "cp ${adapterFor c} $out/${c}.toml\n") agentClients}
'';
```

with `SCRUFF_ADAPTER_PATH` including `/etc/scruff/adapters`. `agents.nix` stays the
single source; the shell copy disappears; a fourth client is one list entry.

This also generalises: any Nix user gets declarative adapters for free, which is a
genuinely nice line in scruff's README and costs nothing to support (it's just
another directory on the adapter path — see §5.1, extended to
`$SCRUFF_ADAPTER_PATH` entries between user and built-in).

### Cutover safety on Julien's machine

At the time this section was written, the bash predecessor was load-bearing:
two Claude Code hooks, the statusline refresher, pounce's Spawn Agent command,
and `bench`. The plan was:

1. **scruff reads the existing `registry.tsv` unchanged.** No format migration on
   cutover day (§2.1). This is the hard requirement.
2. The hook switch is **one haus option** —
   `haus.agents.worktreeBackend = "wt" | "scruff"` — so the revert is
   `haus rollback`, not a code change.
3. Run both for a week: `scruff` installed, option still on the bash predecessor,
   and a `scruff list --json` diff check in `bench status`. Flip when they agree.
4. Only after the flip does `scruff migrate` (registry v1) become available, and it
   backs up the TSV first.

**What actually happened:** the cutover skipped the dual-run option entirely —
every caller (the terminal room's `⌘A`, pounce's Spawn Agent, the Claude Code hooks) was
repointed straight at `scruff` in haus#201, with the bash predecessor left
on `PATH` as an unused rollback. That rollback was never needed and the bash
predecessor has since been deleted outright (haus#245); there is no
`worktreeBackend` option and no fallback to revert to.

---

## 11. Delta since the passoff — what landed in the bash predecessor in the last few days

The passoff describes a 1092-line bash predecessor. It's **1295** now (and,
since retired, frozen at that count). These changes were all general-purpose
and belong in scruff 0.1, not just in haus:

| Change | PR | Impact on this spec |
|---|---|---|
| `reship` + `+N` post-merge-ahead marker | #189 | New lifecycle state: *landed-but-moved-on*. Must be in the state machine (§0), the `--json` shape (`post_merge_ahead`, §2.2), and the merge-strategy table (§3). This is a genuinely novel state no competitor models. |
| Client-agnostic bar / tab badge / agent-spawn bind | #170 | Confirms the agent-adapter seam (§5.3) is the right cut. |
| One client list in `modules/lib/agents.nix` | #171 | Directly motivates §10's "Nix generates adapter TOML". |
| Column sizing to pane / widest agent id | #168, 3a0d6d1 | Presentation belongs in the *consumer*; scruff's job is `--json` + a good default renderer. |
| Worktree in a repo with **no commits yet** | #166 | `git worktree add --orphan`. A real edge case with a real fix — port it, and keep the test. |
| Codex + OpenCode support | #162 | Ditto §5.3. |
| Registry field 6 (`agent`), v0 rows = claude | #162 | The migration case that §2.1 must preserve forever. |

---

## 12. Milestones

| | Scope | Done when |
|---|---|---|
| **0.1** | Everything in §2 (contracts), §3 (landed, incl. patch-equivalence), §4 (slug identity), §5 (adapters), §10 (cutover). Commands: `list`, `new`, `child`, `spawn`, `resume`, `park`, `unpark`, `reap`, `reship`, `hook create/remove`, `doctor`. | The ported acceptance suite passes unmodified against `scruff`; every haus caller is repointed at it (done, haus#201/#245) with no bash predecessor left to fall back to. |
| **0.2** | §6 bootstrap (reflink, ports, secrets, trust), §7 `overlap`. | `scruff doctor --write` produces a usable `.scruff.toml` on a stranger's Node repo; `overlap` sees parked branches. |
| **0.3** | §8 `batch` with queue bisection; `bench try-batch` becomes a wrapper. | It names a culprit *pair* on a real red queue. |
| **0.4** | §14 SDKs: `scruff watch --json`, then TS, then Python/Swift, plus §14.5's `scruff skill --json` + adapter `instructions_file` + `bootstrap.agent_instructions`. scruff stays a binary — SDKs shell out. | A third party ships an agent UI whose only worktree logic is `scruff` — and whose spawned agent knows `scruff child`/`scruff park` without a hand-written CLAUDE.md stanza. |
| later | Runtime backends, GUI-embeddable library split, §14.3 step 5 (remote transport). | — |

---

## 13. Open questions

None blocking. The one thing to get right in the README's opening paragraph is the
positioning against first-party worktree support — see §0's "moat, stated plainly".

---

## 14. Embedding: SDKs, and why the registry does not go on a URL

The thesis (§0) is that scruff is a *substrate* — something other people's UIs and
orchestrators are built on. That means SDKs in TS, Python, Swift and whatever
else, and it settles a question that looks unrelated: whether the registry can
live at a URL instead of a file.

**It cannot, and it shouldn't.** The question was "can `$SCRUFF_REGISTRY` be an
https:// address", and the answer is that a URL should address *scruff*, not the
registry file. Object storage holds a blob. It cannot run an occupancy provider,
cannot emit a lifecycle event, and cannot enforce a single one of the three
invariants. SDKs written against a shared blob would each reimplement park,
landed-detection and reap — four copies of the logic that *is* the product, and
four copies of its bugs. The blob path also fails on its own terms:

| | Why a bare blob breaks |
|---|---|
| **Machine-local rows** | `path`/`main`/`parent` are absolute paths on one machine. Machine B's `pruneRegistry` runs `branchAlive` against a `main` that does not exist locally, gets false, and deletes machine A's rows. Invariant 1, violated silently. |
| **No `flock` over HTTP** | Mutation is read-modify-write. Remote needs compare-and-swap (`If-Match`, `ifGenerationMatch`, non-fast-forward reject); a plain `PUT` loses an update every time two panes close at once — precisely the race the Go rewrite exists to kill (§2.1). |
| **Hot path** | The statusline shells `scruff list --json` on every bar refresh and both hooks touch it on pane open/close. A network RTT per redraw is not a cost, it's a defect. |
| **Offline** | Invariant 2 says uncertainty resolves to *keep*. Pane close must not fail because wifi dropped. |

Note also that a remote registry wants the **v1 one-object-per-row layout**
(§2.1), not the TSV: one object per checkout gives per-key CAS for free and
retires whole-table lost-update entirely. Building it on the TSV would mean
inventing a distributed lock for a format that is already scheduled for
replacement. So remote is a post-v1 question, gated behind the same
`scruff migrate`.

### 14.1 The shape SDKs actually take

**scruff stays a binary.** SDKs shell out; there is no daemon, no port, no auth,
no supervisor, and no socket semantics to pin before a single consumer exists.
Adding `scruff serve` later is purely additive, because the protocol is the same
either way.

```
scruff-core (Go)        invariants, git, registry. Knows nothing about lsof or zellij.
  providers           occupancy: lsof | leases | /proc     forge: gh | glab
                      adapter: claude | codex | opencode | pi
  transports          exec + --json  ·  watch/NDJSON  ·  (later) unix socket · HTTP
SDKs (ts/py/swift)    thin. Speak the wire schema. Hold a lease. That is all.
```

A connection string selects a **transport**, not a file format — `scruff("<path
or url>")` means "exec the local binary" or "speak to a scruff over HTTP", with
one schema behind both, so an SDK written against local works remote unchanged.

The frozen contract is therefore **§2.2's `--json` envelope**, not the registry
file. Generate SDK types from it. The single most important thing to carry into
every language is the nullable discipline: `occupied`/`dirty`/`pr` are
three-state, and every consumer bug in the shell statusline came from collapsing
`null` into `false`. TS and Swift optionals make that easy to get right; do not
let a generator flatten them.

### 14.2 Callbacks invert into leases

The obvious SDK shape has scruff calling back into the host program —
`isStillActiveInPane: () => boolean`, consulted mid-sweep. That needs
bidirectional RPC in every language, and it makes the sweep's correctness depend
on a stranger's event loop being responsive.

Invert it. The client *reports*; scruff consumes. That is exactly §9.1's lease,
and it dissolves the problem: a lease is a file, every language can write one,
and a lease naming a live pid is self-maintaining. It also generalises past the
zellij case that `lsof` was built around — an embedder's "session" is a
connection, not a cwd, and only the embedder can see it.

The callback shape does have a legitimate home, though: as a §6.5 **seam**, an
`occupied` hook scruff execs, which is a program rather than an in-process
closure and therefore works identically from every language. See §9.1 — a hook
is one more provider, and it inherits the presence/absence asymmetry along with
everything else. Leases are for a client reporting on *itself*, per session, at
connection speed; a hook is for a machine that can answer for *everyone* at once
and wants to replace `lsof` outright. An SDK wants both, for different things.

`onOpen` and friends do **not** require a daemon either. They require a stream:
`scruff watch --json` emitting NDJSON on stdout is a lifecycle feed any language
can consume over a subprocess pipe. That promotes `watch` from a §12 "later"
item to the thing the SDKs are built on.

### 14.3 Order

1. **Occupancy provider seam + lease/heartbeat** (§9.1) — *shipped*. Unblocks the
   SDKs and closes the container/Linux portability hole at the same time.
2. **`scruff watch --json`** — *shipped*. fsnotify on the registry, NDJSON out on
   stdout: a `hello` header (`scruff`, `schema`, `capabilities`), then `sync` for
   every lane already alive, `ready`, then `created` / `parked` / `resumed` /
   `reaped` / `changed` as things change. This is `onOpen`.
   `created`/`resumed`/`reaped` are registry mutations, caught instantly.
   `parked` mostly isn't — an unlanded pane closing only touches the
   filesystem (`park.go` commits; the WorktreeRemove hook drops the registry
   row only when landed) — so `watch` also re-scans on a plain local timer
   (3s) as a backstop; still no forge call beyond the existing 120s disk
   cache. `landed` / `post_merge_ahead` are the one family deliberately NOT
   in v1 — they change at the forge, and folding a `gh` poll into a stream
   meant to run for hours, across however many lanes and repos one embedder
   holds leases on, is a rate-limit generator waiting to happen (see §14.4's
   fork). A consumer that wants landedness still polls `--json`. `source` on
   every event and `capabilities` on `hello` exist so a forge-derived family
   can be added later without a schema bump — see §14.4.
3. **TS SDK** — *shipped* (`sdk/ts`). Subprocess + the two above: `list()`,
   `watch()`, `child`/`spawn` (create a lane without attaching an agent —
   the orchestrator primitive), leases, and `newInteractive`/
   `resumeInteractive` for a terminal app that wants to hand off the screen.
   Let it find what the schema is missing before three more languages pin
   the gaps.
4. `scruff skill --json` + the adapter `instructions_file` field + the
   `bootstrap.agent_instructions` step (§14.5) — ship alongside the TS SDK, so
   "an embedder's only worktree logic is scruff" (§12's 0.4 exit bar) is true of
   the *agent's* knowledge too, not just the UI's. `scruff skill` itself has
   landed; what remains here is the envelope and the two wiring seams.
5. Python, Swift, Go, Rust — mechanical once TS has proven the wire schema.
   *shipped* (`sdk/python`, `sdk/swift`, `sdk/go`, `sdk/rust`). Go is the one
   language where this repo's own toolchain is already present, so its SDK
   is a nested module (`sdk/go`'s own `go.mod`) rather than a copy-out —
   `go get` resolves it straight from this git repo, no publish step or
   package-manager account needed at all, unlike npm/PyPI/SwiftPM/crates.io.
   Rust is async (tokio) rather than a mechanical port of Go's synchronous
   shape, the same call the Python SDK made and for the same reason
   (§14.1's first real consumer is a long-running server).
6. Remote transport, as an HTTP server speaking the same protocol. Only here does
   the machine-local-rows problem above need solving, and by then a server knows
   which client each row came from, which is most of the answer.

### 14.4 Why `watch` doesn't poll the forge, and the schema headroom that decision bought

The design question step 2 actually had to answer: fsnotify on the registry
gives `created` / `resumed` / `reaped` for free — those are registry
mutations, so the file changing is a complete signal. `parked` turned out to
need its own backstop (a plain local re-scan timer, still v1's "registry and
filesystem only" scope — see the note on step 2 above) because the common
case, an unlanded pane closing, never touches the registry at all. `landed`
and `post_merge_ahead` are the harder gap: they change at the forge, and
nothing LOCAL — registry or filesystem — fires when a PR merges. Three shapes
were on the table for that one —

- **(a) registry-derived only.** A consumer that cares about landedness polls
  `scruff --json` itself, at whatever cadence it can afford.
- **(b) a forge poll on a TTL, folded into the same stream.**
- **(c) both, with an event `kind` distinguishing which produced it.**

**(a).** The first real consumer (§14, the web-server case) is ONE long-running
`watch` per box, holding leases across however many lanes and repos it's
serving. A poll baked into `watch` itself — option (b), or the half of (c) that
does the polling — multiplies by every one of those for as long as the process
runs, which is indefinitely. That turns a lifecycle stream into a background
`gh` rate-limit generator, silently, on a schedule nobody chose per-repo. (a)
has a real cost — landedness is stale between polls — but it's a cost the
CONSUMER chooses and can tune, rather than one `watch` imposes on every forge
it happens to be watching.

The schema is built so (b)/(c) are additive whenever they're worth it, not
foreclosed: every event carries `source` (`"registry"` today, `"forge"`
reserved), and `hello` carries `capabilities` so an SDK can ask "will this
stream ever emit a forge-derived event?" instead of guessing from which kinds
happen to have shown up. Adding a `landed` kind with `source: "forge"` later
costs zero consumers a rewrite. That headroom — a couple of fields that do
nothing in v1 — is deliberate: the whole point of freezing a wire schema this
early (§14.1) is not needing a v2 of it for a long time, and the fields that
save that are far cheaper to ship now, unused, than to retrofit once three
languages have generated types against their absence.

### 14.5 Teaching the agent scruff exists — embedders with no CLAUDE.md

§14.1–14.3 make scruff *reachable* from any language. They say nothing about
whether the **agent** an embedder spawns knows to use it. On Julien's machine
that's solved by accident, not by design: a global `~/.claude/CLAUDE.md`,
Nix-rendered, gets injected into every Claude Code session regardless of repo,
and happens to carry a hand-written `scruff child`/`scruff park` stanza. A
third-party TUI built on the TS SDK has no such file, no Nix, no
global-instructions injection point at all — and an agent that doesn't know
`scruff child` exists will run `git worktree add`, and one that doesn't know
`scruff park` exists will run `git stash`, landing exactly on the
shared-stash-stack footgun the README already spends a whole section warning
about. A substrate whose whole pitch is owning worktree-lifecycle invariants
can't leave "does the agent know the invariants exist" as an exercise for
every embedder.

**The instructions are not per-lane data.** They don't need §5.2's template
variables — "use `scruff child`, not `git worktree add`" is the same sentence on
every repo, every lane, every machine. That means they aren't a lifecycle hook
(§6.1) an embedder has to wire themselves; they're closer to a §6.3 built-in
bootstrap step, and the text itself is a Markdown asset compiled into the
binary (`go:embed`), not a network fetch or a template render.

That asset is `ai/SKILL.md`, and the verb that prints it is **`scruff skill`** —
*shipped* (`internal/commands/skill.go`, embedded by `skills.go`):

```
scruff skill [<name>]        print an agent skill — scruff's own, or handoff
scruff skill install         write them all into every agent client found
scruff skill --json          {"version": …, "body": …}          (not yet)
```

⚠️ **This section used to reserve the name `scruff docs agent
[--format=md|json]`.** It lost, deliberately and once: `skill` is what the
family standard (the workshop's `docs/agent-surface.md` §A3) makes every
hausfold tool answer to, every client calls these things skills, and in this
family `agent` already means a launchd agent. One spelling across five tools
beats a private one in the substrate. Don't reintroduce `docs agent` as an
alias — an alias nobody documents is surface that can never be removed,
because you cannot know who found it.

What has **not** landed is the envelope. `--json` returns
`{"version": "...", "body": "..."}` so an embedder can detect drift against a
copy it already spliced in, rather than diffing prose. `version` bumps only when
`body` changes — the same discipline as §2.2's envelope, so a pinned embedder
never gets silently rewritten instructions under it. The verb losing its old
name costs nothing here: the envelope is orthogonal to the spelling, and hangs
off `skill` exactly as it would have off `docs agent`.

**Where it lands is a per-client fact, so it's an adapter field, not a new
concept.** §5.3's agent adapters already carry client-specific behavior in six
lines; add one:

```toml
kind    = "agent"
id      = "claude"
...
instructions_file = "CLAUDE.md"      # codex/opencode/pi/amp: "AGENTS.md"
```

**Injection is opt-in and idempotent**, wired the same way §6.3's `copy` and
`reflink` steps are:

```toml
[bootstrap]
agent_instructions = true   # append `scruff skill`'s body into instructions_file
```

Idempotent means scruff looks for a marker —
`<!-- scruff:agent-instructions v3 -->` … `<!-- /scruff:agent-instructions -->` —
in the target file and rewrites only that span, leaving whatever the embedder
already wrote above and below it untouched. Same discipline as the trust-file
seeding in the README's "Workspace trust is inherited, never invented": scruff
propagates a decision, it never invents or overwrites one. A repo with no
instructions file yet gets one created containing just the marked block.

`scruff doctor` gets one more finding: an agent adapter with
`instructions_file` set but no marker present in that file for the repo being
inspected — "this agent won't know about `scruff`; `scruff doctor --write` to add
it."

**TS SDK surface is a single call:** `scruff.agentInstructions()` execs
`scruff skill --json` and returns the typed envelope — no new transport, no new
invariant, consistent with §14.1's "SDKs are thin."

None of this is mandatory: an embedder can ignore it and hand-write their own
stanza, the way haus does today. What it buys the ones who don't want to
is the same thing adapters buy for clients — one canonical, versioned copy
instead of N drifting hand-copied ones.
