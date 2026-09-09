---
name: scruff
description: Manage git worktrees for parallel coding agents — list what's open, resume a parked lane, set work aside without git stash, sweep merged branches, recover something reaped. Use when the user says "what agents do I have running", "what worktrees are open", "resume that session", "park this", "stash this", "clean up merged worktrees", "I lost a branch", "where did that work go", or asks to start an agent on another repo. In a repo of theirs, prefer `scruff park` over `git stash` — always.
---

# scruff — the worktree-lifecycle substrate

scruff gives every coding agent its own git worktree — its own checkout, its own
branch, its own pane — so parallel agents never fight over one working tree. One
of those is a **lane**: a branch, a checkout and a pane, from create to reaped.

Its product is not "make worktrees". It is the **state machine and its safety
invariants**, and those are what you must not undercut:

1. **Never lose work.** Every destructive path parks first. The failure
   direction is always "a branch lingers", never "a tree vanished".
2. **Never reap something in use.** Occupied, dirty, or not-provably-landed ⇒
   keep. Uncertainty resolves to *keep*.
3. **The registry is the truth.** Not the filesystem, not `git worktree list` —
   those are derived, and they lie.

scruff is repo-agnostic and client-agnostic. It knows nothing about the build
system, the package manager or which agent is running.

## Verbs

| do this | run this |
|---|---|
| what's open, everywhere | `scruff` |
| …machine-readable | `scruff --json` |
| resume a parked lane | `scruff <name>` (or `scruff <repo>/<name>`); a unique prefix of either part works — type what the listing shows even where it cut the cell |
| a lane on this repo | `cd "$(scruff new [name])"` |
| …and open an agent in it | `scruff new [name] --open [agent]` |
| …opened on a task, not a blank pane | `--prompt '<task>'`, or `--prompt-file <file>` |
| a lane on *another* repo | `scruff child <repo>` |
| a lane on any repo, with a task, from a spawner with no pane | `scruff spawn <repo> <name> --prompt-file <file>` |
| set the tree aside | `scruff park [label]` |
| put it back | `scruff unpark` |
| sweep landed lanes | `scruff reap` |
| what got reaped, and the SHA to undo it | `scruff reaped` |
| retire a lane that will never land | `scruff drop <name>` |
| push commits that outran a merged PR | `scruff reship [name]` |
| stand up/enter/tear down a lane's VM or container | `scruff runtime up\|enter\|down <name> --backend <id>` |
| **see a desktop change work without touching the user's screen** | `scruff runtime up <name> --backend tart` — built in, needs `tart` + `SCRUFF_TART_BASE`. Boots a headless macOS with the lane shared in and returns once a shell answers; then drive it over `ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null admin@$(tart ip scruff-<name>)` (both options, always — vmnet gives the next clone the same address): `screencapture -x` there returns real pixels, `osascript` sends the keystrokes. Prefer this over asking to drive the machine you are on |
| what state is this machine in (a bug report, "why is nothing being reaped") | `scruff doctor`, or `--json` — forge auth, occupancy, reflink, stray checkouts, orphan branches, disk per repo. It changes nothing and exits 0 even with findings |
| watch lifecycle events | `scruff watch --json` (NDJSON, one object per line) |
| put this skill into another agent client | `scruff skill install [--client codex]` — writes what the installed binary ships; exit 2 means it refused to overwrite, never that it broke |
| everything, exhaustively | `scruff --help` (prints to **stderr**); `scruff <verb> --help` is just that verb's lines, and no verb does its work on a help flag |

`scruff --json` returns `{scruff, schema, warnings, lanes:[…]}`. Each lane carries
`name`, `repo`, `main`, `branch`, `path`, `parent`, `chat`, `agent`, `state`,
`occupied`, `occupied_by`, `dirty`, `landed:{verdict,via,confidence}`,
`post_merge_ahead:{commits,pr,diverged}` and `last_commit`.

What an agent gets wrong about that payload:

- **`state` is a closed set: `live`, `parked`, `stray`.** A `stray` is a husk
  with no usable checkout — `scruff <name>` moves it aside and rebuilds.
- **`landed.verdict` is `yes`, `no`, `fresh` or `contained` — and `fresh` is not
  `yes`.** A lane cut from the default branch that has never committed carries
  nothing of its own, so ancestry alone finds it already contained there.
  `fresh` is that case given its own word: report it as *nothing yet*, never as
  *merged*. A verdict or `via` you don't recognise resolves to not-landed.
- **`occupied` and `dirty` are nullable, and `null` means *not determined*, not
  false.** Reading `null` as falsy is how you tell a user a lane is clean when
  scruff could not tell. Every consumer bug in the predecessor's status bar came
  from exactly this.
- **`chat`, not `parent`, says whether a lane has a pane of its own.** A lane
  spawned by `scruff child` — or opened from inside another lane's pane — is
  parented to that lane either way, and only the second has a window and a
  conversation. `chat` is the checkout `scruff <name>` would resume into: equal
  to `path` when the lane holds its own chat, the parent's path when it doesn't.
  `""` means scruff could not tell (a client whose transcripts it cannot probe)
  and must be read as *show it*. Filter a picker on this; never hide a lane on
  `parent` alone, and never hide one that is listed as `live`.
- **`occupied` says a process is standing there, not that a *pane* is.** A dev
  server or an orphaned daemon holds a lane exactly as hard as a live agent.
  `occupied_by[]` names it (`{pid, command, path, via}`, absent when free) — use
  it before telling a user to go and close a window that may not exist.
- **`warnings` is the only place a degraded run explains itself.** Under
  `--json` the human-readable notes are suppressed, so if you ignore
  `warnings` you will silently report a partial sweep as a complete one.

And `scruff`/`scruff --json` are **not read-only**: every listing also sweeps landed
parked branches. Harmless — the invariants still hold — but don't poll it under
the impression that you are only looking.

## Exit codes — check these, they mean different recoveries

| | meaning | what to do |
|---|---|---|
| 0 | success, **including "nothing to do"** | report what happened |
| 1 | usage or precondition error | fix the invocation |
| 2 | **refused for safety** — occupied, dirty, or not provably landed | this is scruff working. Don't force it. |
| 3 | degraded — it completed, but a signal was unavailable | report the caveat |
| 4 | conflict found | the user resolves it |
| 5 | registry locked by another scruff | another scruff is mid-operation; retry shortly |

Exit 2 is the one to respect. It means scruff decided keeping was safer than
removing, and the correct response is to say why, not to reach for `git
worktree remove`.

## When to reach for this

- "what am I working on / what's still open?" → `scruff`, then read the states out
- "resume that session from yesterday" → `scruff <name>`; it rebuilds the checkout
  and reopens the agent it was made with
- "park this, I need a clean tree" → `scruff park`
- "clean up the merged ones" → `scruff reap`, then `scruff reaped` to show what went
- "I lost a branch" → `scruff reaped` has the reason and the SHA to get it back
- "start an agent on the other repo" → `scruff child <repo>` from this pane
- "spawn an agent to do X" → `scruff spawn <repo> <name> --prompt-file <brief>`;
  the `handoff` skill is how you write the brief. Exit 3 means the lane exists
  but this machine has no `[hooks] open` to put a window on it — report the
  command scruff printed, don't retry. **Keep the name short.** Some machines
  cap it, because a lane backend has to carry `scruff/<repo>/<lane>` as one
  string; a name over the budget is refused before the branch exists, and the
  error says how many characters that repo leaves. Three or four
  identity-carrying words is the shape either way.

## When NOT to

- **Never `git stash` in a repo the user owns — use `scruff park`.** The stash
  stack lives in the shared `.git` dir, so every worktree of a repo *and* the
  main checkout push and pop the *same* stack; parallel agents routinely pop
  each other's entries into a tree that never asked for them. `scruff park`
  commits the dirty tree as one `wip:` commit on the branch only this pane has
  checked out. This is the single most valuable thing in this file.
- **Never `git worktree add` directly** when a lane is what's wanted. A raw add
  skips the registry, so nothing downstream — status bars, `reap`, the parent
  pane — ever learns the worktree exists.
- **Never `git worktree remove` to "clean up".** That is what `reap` and `drop`
  are for, and they park first.
- **Don't ask scruff to run, schedule or supervise an agent.** It is substrate,
  not orchestrator: no scheduling, no restarts, no opinion about which agent
  you run. The actions at each transition belong to the user.
- **Don't ask it about CI, merges or conflicts beyond detecting them.** It
  resolves nothing.

## Traps

- **A lane with zero commits is instantly sweepable.** A brand-new `scruff
  new`/`scruff child` checkout has no commits, so another session's `scruff reap`
  can take it on ancestry. Commit something immediately.
- **`scruff reap` deliberately spares a branch whose PR merged but which kept
  committing.** GitHub deletes the head branch on merge, so those later commits
  have no remote — that's `scruff reship`, not a bug.
- **`scruff unpark` refuses a wip commit you already pushed.** By design: it can
  never turn into a force-push.
- **The checkout is disposable; the branch is not.** Closing a pane parks and
  deletes the tree. Nothing is lost — say that rather than treating a missing
  directory as data loss.
