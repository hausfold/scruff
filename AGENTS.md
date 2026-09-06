# AGENTS.md

**scruff** — the worktree-lifecycle substrate for parallel coding agents. A single
Go binary plus five SDKs, published from this one repo under one version number.

**This file is the one set of instructions, for every agent** — Claude Code,
Codex, OpenCode, Cursor, Copilot alike, directly or through a one-line pointer.
Per-client wiring lives in that client's own file; the wiring map is
[`.agents/README.md`](./.agents/README.md).

Deep detail lives in the docs, not here — [`SPEC.md`](./SPEC.md) is the design
of record and [`docs/releasing.md`](./docs/releasing.md) the six-artifact
release. **The user-facing manual is not in this repo**: the lifecycle, config,
exit codes, the `--json` payload, the seams and the SDKs are all at
[hausfold.co/docs/scruff](https://hausfold.co/docs/scruff/), written in
[`hausfold/hausfold.co`](https://github.com/hausfold/hausfold.co) under
`content/docs/scruff/`. A change here that moves a verb, a flag, an exit code or
a `--json` key changes that tree too, in the same breath as `ai/SKILL.md`.

## What scruff is, and what it must never become

scruff's product is **not** "make worktrees" — every vendor ships creation and
stops there. It is the **state machine and its safety invariants**. Three of
them, in priority order; everything else in this repo is subordinate:

1. **Never lose work.** Every destructive path parks first. The failure direction
   is always "a branch lingers", never "a tree vanished".
2. **Never reap something in use.** Occupied, dirty, or not-provably-landed ⇒
   keep. Uncertainty resolves to *keep* — including when the forge is unreachable.
3. **The registry is the source of truth, and it is locked.** Not the filesystem,
   not `git worktree list` — those are derived, and they lie (stray dirs,
   half-removed checkouts, parked branches with no dir at all).

A change that trades any of these for convenience is wrong even when it passes
the suite. Say which invariant your change touches in the PR body.

**Non-goals, and they are load-bearing:** no scheduling, no agent supervision or
restart, no fullscreen TUI, no hosted anything, no knowledge of your build system
/ package manager / CI, no merge-conflict *resolution*, no opinion about which
agent you run. "Substrate, not orchestrator" is the whole thesis — the actions at
each transition belong to the user.

**scruff is repo-agnostic and client-agnostic.** No repo-local adapters, no
`bench`, no haus paths, no *new* code that assumes the consumer is the family.
Adapters are template-driven (`SPEC.md` §5); if a need can only be met by
hardcoding one caller, it belongs in that caller.

The rule binds new behavior, not history. **Two grandfathered exceptions exist
on purpose — don't "enforce" them away**: `HAUS_AGENT_DEFAULT` is still read as
a fallback rung in `defaultAgent` (`internal/commands/env.go`), because older
haus builds set it and deleting it breaks their default agent; and `SPEC.md` §10
is that one consumer's cutover story. Adding a *third* is the thing to argue
hard against.

## Vocabulary (the overload was the bug)

A **lane** is the unit — one agent's branch, checkout and pane, `create` →
`reaped`. Three words are reserved and mean only their narrow thing:

| word | means, and only this |
|---|---|
| **worktree** | git's — the checkout on disk. A *parked* lane has none, so it can't name the unit. |
| **agent** | the **client**: `claude`, `codex`, `opencode`, `pi`. A lane *runs* an agent; it is not one. |
| **session** | somebody else's — the multiplexer's, or a client's transcript. scruff never names its own unit this. |

`pane` is available, and is exactly what `occupied` reports.

## Frozen contracts — check before you touch

`SPEC.md` §2 is versioned and breaking-change-gated: the **registry schema**,
`--json` **output**, the **hook protocol**, and **exit codes**. Downstreams pin
them — haus's statusline, the workshop's `bench status`, pounce's Spawn
Agent, and every SDK. Changing one is a semver **major** conversation, not a
refactor. The same goes for user-facing command names and flags.

**The `ai/` skills quote two of those four** — `--json` output and exit codes —
plus the frozen command names and flags, so they are now downstream of the same
freeze. See below.

## The agent surface (`ai/`) — two skills

**Don't confuse it with this file.** `AGENTS.md` is for an agent working **on**
scruff, from a checkout. `ai/` is for an agent **using** it — on a stranger's
machine, with no checkout, when their human says *"what worktrees do I have
open?"* or *"park this"*.

| file | skill | teaches |
|---|---|---|
| [`ai/SKILL.md`](./ai/SKILL.md) | `scruff` | the lifecycle: verbs, the six exit codes, the `--json` fields, and when the answer is to do nothing |
| [`ai/handoff/SKILL.md`](./ai/handoff/SKILL.md) | `handoff` | the one thing with no verb — how to write the brief a cold session can act on, ending on the clipboard or in `scruff spawn --prompt-file` |

**Why `handoff` is scruff's and not a consumer's.** The lane it opens is scruff's
unit, and `--prompt` is scruff's flag; the brief is that flag's argument, and an
argument nobody knows how to write is a flag nobody uses well. It stays inside
the non-goals: it describes handing work **over** and stops there — no
scheduling, no supervision, no opinion about what the receiving agent then does.
The day it starts telling that agent how to do the work, it has become an
orchestrator and belongs somewhere else.

`handoff` is also the one skill here that a user invokes by name (`/handoff`),
so its `description` carries both phrase families — *"make me a handoff"* and
*"spawn an agent for this"* — and its default ending is the harmless one.
Spawning costs a branch, a window and another agent's context, so it takes an
explicit word; a skill that spawned on an ambiguous ask would be the wrong kind
of surprise.

The rest of this section is about both files.

It is bound by the family standard, [the workshop's
`docs/agent-surface.md`](https://github.com/hausfold/workshop/blob/main/docs/agent-surface.md) —
≤150 lines, no flag dumps (that's `scruff --help`), and the `description`
frontmatter names **the phrases a user says**, not the features scruff has. A
description written as a feature summary is true, well written, and never loads.

The vocabulary rule above still binds it: scruff's own unit is a **lane**, never a
"session". The exception is the quoted user phrases — *"resume that session from
yesterday"* is what a person says, and matching it is the whole job of the
`description`.

Two things it carries that no other family skill does, and both are the whole
point of scruff existing:

- **`scruff park`, never `git stash`.** The stash stack is shared across every
  worktree of a repo, so parallel agents pop each other's entries. The skill's
  `description` says this out loud precisely so it loads on the word "stash".
- **Exit 2 is scruff working, not scruff failing.** An agent that reads "refused for
  safety" as an error and reaches for `git worktree remove` has defeated
  invariant 2 from the outside. The skill says so twice.

It also spends space on the `--json` payload's traps — `state`'s closed set,
`landed.verdict`'s `fresh` not meaning `yes`, `occupied`/`dirty` being nullable
with `null` meaning *undetermined*, and `warnings` being the only channel a
degraded run has under `--json`. Those are SPEC 2.2's own warnings, aimed at the
newest consumer.

`nix/skill.nix` ships both as `pkgs.scruff-skill` (`$out/scruff/SKILL.md` and
`$out/handoff/SKILL.md`, one directory per skill) — its own derivation so a
consumer can install them with no Go toolchain and no binary. It does **not** isolate the Go build's hash: `src = ./.` is unfiltered,
so `ai/` is inside that closure and a prose edit moves scruff's drvPath either way
(measured, not assumed). The build runs the same guards over each: it fails if the
frontmatter is missing or unterminated, if `name:` disagrees with the directory,
or if the file passes 150 lines — each of those produces a skill that installs,
lists, and is never loaded.

**`scruff skill` is the verb** (`internal/commands/skill.go`), and what it prints
is EMBEDDED. `skills.go` at the repo root holds the `//go:embed ai`, and that is
the only reason a Go file lives outside `cmd/` and `internal/`: an embed pattern
cannot escape its own package directory, and `ai/` is at the root because the
family standard puts it there. `install` writes every skill it *discovers* —
never just the tool's own — and never over a file that exists and differs or
sits behind a symlink. A file that differs, or one it cannot write, is exit 2,
the same "declined for safety" the rest of the CLI means by it. A symlink is
named and left alone but is **not** a refusal: on a haus machine haus.ai.skill
has already installed every skill as a read-only store symlink, and that is the
end state holding, so the run says so and exits 0 rather than sending every
agent on such a machine back with force against a store path. `--dir` with
`--client`, and either flag with a missing or empty value, are usage (exit 1)
before anything is written. All three rules are A3 of the workshop's
`docs/agent-surface.md`.

`SPEC.md` §14.5 reserved this capability as `scruff docs agent
[--format=md|json]` first. `skill` won — five tools in this family answer to it,
and here `agent` already means a launchd agent — and §14.5 now records that and
why. Its `{version, body}` envelope is still unbuilt and arrives as `scruff
skill --json`. Don't add a `docs agent` alias.

**Every claim in them must be runnable.** A verb, flag, exit code or `--json` key
that changes changes `ai/SKILL.md` in the same PR — and `ai/handoff/SKILL.md`
too when it is one of the flags that file spells out (`--prompt`,
`--prompt-file`, `--agent`, `--image`, and spawn's exit 3). Those files quote the
frozen contracts above, so they are not merely documentation that drifted — they
are downstream consumers of them.

## The five SDKs are one product

`sdk/{ts,python,rust,swift,go}` all wrap the same CLI and **share one version
number**. Five clients agreeing about one wire format is the invariant the SDK CI
job exists to protect, so a change to one SDK's surface is a change to all five.

- **`sdk/swift` is the source; [`hausfold/scruff-swift`](https://github.com/hausfold/scruff-swift)
  is a generated mirror** (`git subtree split`). Never edit the mirror — changes
  there are overwritten on the next sync. `release.yml` runs
  `sdk/swift/sync-mirror.sh --tag <version>` at every `v*` tag, and a tag on the
  mirror *is* the SwiftPM release, so don't hand-run it after a release. The
  bare form (no `--tag`) mirrors `main` only, and exists for one case: getting
  an unreleased change in front of a consumer pinning a branch.
- **The Go module path is `github.com/hausfold/scruff`**, in `go.mod`,
  `sdk/go/go.mod` and the `Makefile`'s `LDFLAGS`. Keep the three spellings in
  step — a mismatch between `go.mod` and `LDFLAGS` builds fine and reports the
  wrong version. ⚠️ Go's proxy is immutable and this path has moved **twice** —
  `github.com/nebelhaus/holt` through `v0.2.8` (the org rename),
  `github.com/hausfold/holt` through `v0.5.0` (the scruff rename), and
  `github.com/hausfold/scruff` from `v1.0.0`. Each old path stays resolvable at
  the tags published under it forever, and at nothing after. An importer on one
  either edits its import line or pins that path's last tag.

## Verify by running it

```sh
make check        # gofmt -w + go vet + go test ./... + the bats acceptance suite
make test         # just the suites
make build        # ./scruff
```

⚠️ **`make check` covers the CLI only — never the SDKs.** `sdk/go` has its own
`go.mod`, so `go test ./...` structurally can't see it, and ts/python/rust/swift
have no Make target at all. CI runs two more jobs (`sdks` on Linux, `swift-sdk`
on macOS), so a green `make check` on an SDK edit means nothing. Run that SDK's
own suite from its directory — `bun test`, `pytest`, `cargo test`, `swift test`,
`go test ./...` — before you open the PR. Note also that `make check`'s `fmt`
step is `gofmt -w`: it **rewrites** your tree, while CI gates on `gofmt -l`. A
suddenly-dirty tree after `make check` is that, not a bug.

The acceptance suite (`test/scruff.bats`) is **black-box**: it drives the built
binary with shim `gh`/`lsof` on `PATH`, inherited from the bash predecessor so
the contract is provably unmoved. `go test ./...` covers the one thing a
black-box suite structurally can't — code that rewrites a file belonging to
another tool (Claude Code's `~/.claude.json`), where most of the assertion is
about what survived untouched. Both run in CI on macOS and Linux; a change that
only passes on one is not done.

## Releasing — always the user's call

**Never tag by hand.** Releases are cut from the workshop:

```sh
bench release scruff <X.Y.Z>      # stamps every manifest, commits, tags, watches CI
```

scruff is the family's **one semver repo**, and it's forced, not chosen: three of
the five registries are immutable and already hold published numbers, so the
version is a compatibility contract rather than a date. Picking the bump means
reading `git diff <last-tag>..main -- sdk/` against the *published SDK surface* —
that judgement is [`docs/releasing.md`](./docs/releasing.md), and the release
itself is gated on the user. Propose the number and the evidence; don't run it
unprompted.

## Landing work

Standing rules for any agent working here:

- **Commit, push and open the PR without asking** — that's standing permission.
  Merging the PR is the user's call unless they say ship/land/merge.
- **Never push to `main` or `git merge` into it locally.** Parallel agents doing
  that have clobbered each other; a PR is atomic and conflict-detected.
- Give the PR a **What / Why / Verify / Watch-out** body — the session that wrote
  the code is gone by the time anyone feel-tests it, so `gh pr view` has to carry
  the recovery context. For this repo, **Verify** means the command someone else
  can run, and **Watch out** names the invariant or frozen contract you went near.
- Working on another repo from a scruff pane? `scruff child <repo>` — never a raw
  `git worktree add`, which skips the registry (scruff's own dogfooding rule, and
  the reason the statusline can see a child's PR at all).

## Where scruff sits in the family

The layer ([hausfold/haus](https://github.com/hausfold/haus)) takes scruff as a
flake input and ships it on `PATH`. Its **⌘↵** lane chord runs `scruff new` for
every client — claude, codex, opencode and pi alike; `claude --worktree` is
deliberately not the path it takes, because that runs the client in the pane it
was launched from and never asks `[hooks] open`, the seam a lane's own window
arrives through. The `WorktreeCreate`/`WorktreeRemove` hooks still call `scruff
hook create` / `scruff hook remove`, so a hand-run `--worktree` lands in the
registry too. Either way this repo is the plumbing.

scruff IS a family repo for *shipping* purposes — it's in the workshop's `FAMILY`,
so a merged commit here is invisible to haus until `bench ship` bumps that lock.
But the dependency runs **one way only**: haus consumes scruff, and new code here
doesn't get to know haus exists (the two grandfathered exceptions are named
above). The day scruff needs a *third* hausfold-shaped special case is the day the
abstraction was wrong.
