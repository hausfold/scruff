<div align="center">

# scruff

**the worktree-lifecycle substrate for parallel coding agents**

never loses work · never reaps what's in use · the registry, not the filesystem, is truth

</div>

---

Every vendor ships worktree *creation* now — `claude --worktree`, the Claude
Agent SDK's `isolation: worktree`, Cursor, Copilot CLI — and every one of them
stops there. Nobody owns the rest of the life: **the branch still alive after
the pane died**, **the checkout nobody is sitting in**, **the tree with 40
uncommitted minutes in it**, **the branch whose PR merged yesterday and has
kept committing since**.

If you've ever `git worktree list`'d through a graveyard trying to remember
which of those you can safely delete — that's the problem scruff makes go away.

```sh
cd "$(scruff new fix-flaky-test)"  # a lane on this repo: new checkout, new branch, path on stdout
scruff                             # every lane you've got going, live or parked, across every repo
scruff fix-flaky-test              # back later — rebuild the checkout, reopen the agent, where you left off
scruff reap                        # sweep every lane whose branch landed and nobody's standing in
```

**And never `git stash` again.** The stash stack lives in the shared `.git`
dir, so every worktree of a repo *and* the main checkout push and pop the same
one — parallel agents routinely pop each other's work into a tree that never
asked for it. `scruff park` commits your dirty tree as a single `wip:` commit on
the branch only this pane has checked out; `scruff unpark` puts it back.

## install

```sh
nix run github:hausfold/scruff                            # try it
nix profile install github:hausfold/scruff                # keep it
go install github.com/hausfold/scruff/cmd/scruff@latest   # or, with Go 1.26+

scruff skill install                                      # teach your agent it exists
```

Needs `git`. `gh` and `lsof` are optional and make it sharper; without them
scruff degrades toward *keep*, never toward *delete*. On
[haus](https://github.com/hausfold/haus) it's already on `PATH`, wired to ⌘↵.

## the promise

Not "makes worktrees" — every vendor does that. scruff's product is the state
machine and three invariants, in this priority order:

1. **Never lose work.** Every destructive path parks first. The failure
   direction is always *a branch lingers*, never *a tree vanished*.
2. **Never reap what's in use.** Occupied, dirty, or not provably landed ⇒
   keep. Uncertainty resolves to keep — including when GitHub is unreachable.
3. **The registry is truth.** Not the filesystem, not `git worktree list` —
   those are derived, and they lie.

So a command exiting **`2` — refused for safety** is scruff working, not scruff
failing. Don't reach for `git worktree remove`; ask it why.

## the manual

📖 **[hausfold.co/docs/scruff](https://hausfold.co/docs/scruff/)** — and it only
lives there: [installing](https://hausfold.co/docs/scruff/install/),
[working in lanes](https://hausfold.co/docs/scruff/lanes/),
[parking](https://hausfold.co/docs/scruff/parking/),
[landing and cleanup](https://hausfold.co/docs/scruff/landing/),
[runtimes](https://hausfold.co/docs/scruff/runtimes/),
[config](https://hausfold.co/docs/scruff/config/),
[hooks and seams](https://hausfold.co/docs/scruff/seams/),
[every verb and exit code](https://hausfold.co/docs/scruff/cli/),
[the `--json` payload](https://hausfold.co/docs/scruff/json/), and
[the five SDKs](https://hausfold.co/docs/scruff/sdks/).

Inside a haus machine it's [the AI
room](https://hausfold.co/docs/haus/rooms/ai/), which puts scruff on your `PATH`
and binds ⌘↵ to it.

## SDKs

Five clients over the same CLI, sharing one version number:
[`sdk/ts`](sdk/ts) · [`sdk/python`](sdk/python) · [`sdk/rust`](sdk/rust) ·
[`sdk/go`](sdk/go) · [`sdk/swift`](sdk/swift) (published from a generated
mirror, [`hausfold/scruff-swift`](https://github.com/hausfold/scruff-swift) —
send changes here, never there). Install lines, the two shapes of usage and
leases: [hausfold.co/docs/scruff/sdks](https://hausfold.co/docs/scruff/sdks/).

## in this repo

- [`ai/SKILL.md`](ai/SKILL.md) — the agent surface: your agent drives scruff correctly first try. It ships *inside the binary*, so `scruff skill install` puts it in front of every agent client on the machine (a haus box already has it)
- [`ai/handoff/SKILL.md`](ai/handoff/SKILL.md) — the companion skill: write a brief a cold session can act on, and `scruff spawn --prompt-file` it into its own lane
- [`AGENTS.md`](./AGENTS.md) — hacking on scruff: the invariants, the frozen contracts, what `make check` doesn't cover
- [`THANKS.md`](./THANKS.md) — git, the Go libraries scruff leans on, and the people who ran it before it was public
- [`SPEC.md`](SPEC.md) — the design of record, and the contracts that are frozen
- [`docs/releasing.md`](docs/releasing.md) — how the CLI and all five SDKs are cut from one number
- `scruff --help` — the exhaustive flag list

---

<div align="center">

<sub>Nothing here takes something it can't give back — a dirty tree is parked as a commit, and only merged, unoccupied lanes are ever reaped. That's the intent, not a warranty: keep a backup, and [tell us what breaks](https://github.com/hausfold/scruff/issues).</sub>

<sub>MIT · one of the [hausfold](https://github.com/hausfold) repos — [haus](https://github.com/hausfold/haus) rebuilds the Mac, this is what its agent panes stand on</sub>

<a href="https://hausfold.co">⌂ hausfold</a>

</div>
