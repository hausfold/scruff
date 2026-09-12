# AGENTS.md

**scruff** — the worktree-lifecycle substrate for parallel coding agents. This
file is for an agent working *on* scruff from a checkout; per-client wiring is
[`.agents/README.md`](./.agents/README.md).

[`SPEC.md`](./SPEC.md) is the design of record,
[`docs/releasing.md`](./docs/releasing.md) the release, and the user manual is
[hausfold.co/docs/scruff](https://hausfold.co/docs/scruff/) (`hausfold.co`'s
`content/docs/scruff/`), never here. A verb, flag, exit code or `--json` key that
moves changes that tree and `ai/SKILL.md` in the same PR; `ai/handoff/SKILL.md`
too for `--prompt`, `--prompt-file`, `--agent`, `--image` or spawn's exit 3.

The mark is [`assets/`](./assets/README.md): three SVGs are the sources of
record and their PNGs render from them with `resvg`, the two banners are raster
with no source here, and every nebelung hex is **baked in**, so a palette change
is swapped in all three SVGs and re-rendered. What the mark may be — the ears
used verbatim, one hue, radius 24, the geometry written out as text — is the
brand kit's `docs/design.md`, and a change to it lands here **first** and in the
workshop second.

## Invariants

In priority order; trading one away is wrong even with a green suite.

1. **Never lose work.** Every destructive path parks first.
2. **Never reap something in use.** Occupied, dirty, not-provably-landed or
   forge-unreachable means keep.
3. **The locked registry is the source of truth**, not the filesystem or
   `git worktree list`.

Non-goals are `SPEC.md` §0: substrate, not orchestrator.
**Repo- and client-agnostic**: no `bench`, no haus paths, nothing new that
assumes the family (adapters: `SPEC.md` §5). Two grandfathered exceptions, never
a third: `HAUS_AGENT_DEFAULT` as a fallback rung in `defaultAgent`
(`internal/commands/env.go`), and `SPEC.md` §10.

## Vocabulary

A **lane** is the unit, `create` → `reaped`; `pane` is what `occupied` reports.
**worktree** is git's checkout, **agent** the client (`claude`, `codex`,
`opencode`, `pi`), **session** somebody else's — never scruff's unit, except in a
quoted user phrase in a skill `description`.

## Frozen contracts

`SPEC.md` §2 — registry schema, `--json` output, hook protocol, exit codes — plus
command names and flags. Downstreams pin them (haus, `bench status`, pounce,
every SDK, both `ai/` skills); changing one is a semver **major** conversation.

## The agent surface (`ai/`)

Two skills, [`ai/SKILL.md`](./ai/SKILL.md) (`scruff`) and
[`ai/handoff/SKILL.md`](./ai/handoff/SKILL.md) (`handoff`, `/handoff`), to the
workshop's `docs/agent-surface.md`: ≤150 lines, a `description` naming the
phrases a user says.

- **`scruff park`, never `git stash`** — said so it loads on "stash".
- **Exit 2 is scruff working, not failing** — never answer it with
  `git worktree remove`.
- Its `--json` traps are SPEC 2.2's (`state`, `landed.verdict`, nullable
  `occupied`/`dirty`, `warnings`).
- `handoff` hands work *over* and stops; spawning takes an explicit word.

`nix/skill.nix` ships both as `pkgs.scruff-skill` (`$out/scruff/SKILL.md`,
`$out/handoff/SKILL.md`), `src = ./.` unfiltered, so a prose edit moves scruff's
drvPath. `script/check-skills.sh` guards both, there and in CI.

**`scruff skill` is the verb** (`internal/commands/skill.go`), printing what
`skills.go` embeds (`//go:embed ai`, the one Go file outside `cmd/` and
`internal/`). `install` writes every skill it discovers — A3 of that same
standard: a differing or unwritable file is exit 2, a symlink is named and exit
0, `--dir` with `--client` or a missing flag value is exit 1, all before
anything is written. Never add a `docs agent` alias (`SPEC.md` §14.5).

## The five SDKs are one product

`sdk/{ts,python,rust,swift,go}` share one version and one wire format — the
`sdks` CI job proves it.

- **`sdk/swift` is the source; [`hausfold/scruff-swift`](https://github.com/hausfold/scruff-swift)
  is a generated mirror** (`git subtree split`) — never edit it. `release.yml`
  runs `sdk/swift/sync-mirror.sh --tag <version>` at every `v*` tag, and that
  tag is the SwiftPM release, so never hand-run it after one; the bare form
  mirrors `main`, only from `main`.
- **The Go module path is `github.com/hausfold/scruff`** (from `v1.0.0`) in
  `go.mod`, `sdk/go/go.mod` and the `Makefile`'s `LDFLAGS`; a mismatch builds
  fine and reports the wrong version. Older tags import
  `github.com/nebelhaus/holt` (≤ `v0.2.8`) or `github.com/hausfold/holt`
  (≤ `v0.5.0`), resolvable at those tags only; never "fix" them.

## Verify by running it

```sh
make check        # gofmt -w + go vet + go test ./... + bats
make test         # the suites
make build        # ./scruff
```

- **`make check` covers the CLI only** — `sdk/go` has its own `go.mod`, the
  rest no Make target. Run the SDK's own suite (`bun test`, `pytest`,
  `cargo test`, `swift test`, `go test ./...`) from its directory, as CI's
  `sdks` and `swift-sdk` jobs do.
- `fmt` is `gofmt -w` and rewrites your tree; CI gates on `gofmt -l`.
- `test/scruff.bats` is black-box (built binary, shim `gh`/`lsof` on `PATH`);
  `go test ./...` covers rewriting another tool's file (`~/.claude.json`). CI
  runs both on macOS and Linux; one OS is not done.
- **This repo is what the machine's worktree hooks call**:
  `~/.claude/settings.json` points `WorktreeCreate`/`WorktreeRemove` at
  `scruff hook create` / `scruff hook remove` (`scruff hook notify` is the
  third), and haus's ⌘↵ chord runs `scruff new`, the only path that asks
  `[hooks] open` — the seam a lane's own window arrives through. A broken hook
  breaks every agent pane, yours included — exercise hook changes by piping
  JSON to the built binary, never by opening a pane.

## Releasing

**Never tag by hand.** Cut from the workshop, gated on the user:

```sh
bench release scruff <X.Y.Z>      # stamps every manifest, commits, tags, watches CI
```

Semver is forced (three immutable registries). Read
`git diff <last-tag>..main -- sdk/` against the published SDK surface
(`docs/releasing.md`; the bump taxonomy is the workshop's `/release` skill),
propose the number, never run it unprompted.

## Landing work

- **Commit, push and open the PR without asking** — standing permission.
  Merging is the user's call unless they say ship/land/merge.
- A PR, never a push or local `git merge` into `main`.
- PR body: What / Why / Verify / Watch out; **Watch out** names the invariant or
  frozen contract you went near.
- Another repo from a scruff pane: `scruff child <repo>`, never a raw
  `git worktree add` (it skips the registry).
- scruff is in the workshop's `FAMILY`: a merged commit reaches haus only when
  `bench ship` bumps that lock.
