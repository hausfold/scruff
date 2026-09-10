---
name: handoff
description: Turn a piece of work into a self-contained prompt a FRESH agent session can act on cold — copied to the clipboard, or spawned straight into a new scruff lane with its own checkout, branch and window. Use when the user says "hand this off", "make me a handoff", "write a prompt for a new session/pane/agent", "I want to start this somewhere else", "spawn an agent to do this", "kick this off in <repo>", or when work has to leave this session and survive the transition.
---

# handoff — a prompt a cold agent can act on

The next session knows **nothing**. Not this repo, not what was tried, not what
"it" refers to. A handoff is not a summary of what happened here — it is the
smallest brief that lets someone with zero context take the next action
correctly. Anything that doesn't change what they'd DO is noise; anything they'd
have to rediscover is a bug.

## Two endings, one brief

| the user said | ending |
|---|---|
| "hand this off", "make me a handoff", `/handoff` | **clipboard** — copy it, print it, stop |
| "spawn an agent for this", "kick this off in <repo>", `/handoff spawn` | **lane** — `scruff spawn`, which opens a checkout, a branch and a window on it |

**The clipboard is the default; the lane ending needs the explicit word.**
Spawning costs a branch, a window and another agent's context — so when the ask
is ambiguous, take the clipboard and print the `scruff spawn` line underneath.

## The steps

0. **Source it, and take the aim from the user.** A paste is the usual case and
   **is the source of truth**: distill it, don't augment it. Nothing pasted means
   hand off THIS session. Words after `/handoff` say what the next session is
   FOR — write toward that, keeping the reasoning that bears on it rather than
   flattening the session evenly.
1. **Resolve the facts you can.** `git rev-parse --show-toplevel`,
   `git branch --show-current`, `gh pr view --json number,url` if a PR exists —
   cheap and read-only. Never guess a path, a branch or a command; if the paste
   names a file, `ls` it before writing it down.
2. **Write the prompt to a file**, don't compose it in your head:
   `<scratchpad>/handoff-<hh-mm-ss>.md`, or `/tmp` if there is none. A fresh name
   each time — a second handoff must not overwrite the first, and the file is
   what the lane ending hands to `scruff`.
3. **Take the ending the user asked for** — below.
4. **Print it** between the markers, exactly as shown, then stop — no follow-up
   offer, no "let me know if". If something essential was missing, one line
   after the closing marker naming exactly what; see Gaps.

### Ending A — clipboard

`pbcopy < <that file>` on macOS, `wl-copy`/`xclip -selection clipboard` on
Linux. If none exists, say so rather than pretending it worked. Then print, with
the ready-made lane command underneath.

### Ending B — a lane

```sh
# The MAIN checkout, never this worktree: `--git-common-dir` resolves to it
# from inside a lane too.
repo="$(dirname "$(git rev-parse --path-format=absolute --git-common-dir)")"
scruff spawn "$repo" <name> --prompt-file <that file>
```

- **`<name>` comes from the objective, not its first words.** Three or four
  identity-carrying words, kebab-case — `bar-pill-flickers`, not
  `look-into-why-the`; scruff suffixes a taken name, and refuses one over a
  machine's cap on the key `scruff/<repo>/<lane>` instead of trimming it, so a
  refusal wants a word fewer and costs nothing. Omit it only where the machine
  sets `namer` — you wrote the brief, so you are the better namer.
- **`--prompt-file`, never `--prompt "$(cat …)"`.** A brief is multi-line and
  routinely holds quotes, backticks and `$`; the file form never crosses a shell.
- **Another repo?** Pass its main checkout as `repo`, which is why the path is
  an argument: "kick this off in <other repo>" stays one call.
- `--agent claude|codex|opencode|pi` picks the client (default: the machine's);
  `--image <file>` puts a screenshot in front of the first turn — attached where
  the client can do that, named in the prompt where it can't.

Read the exit code: **0** the lane opened (its path is on stdout — report the
lane and branch); **2** the open hook declined and **3** nothing opened it —
neither is a failure, the lane exists: report the command scruff printed, and
soon, since a lane with no commits is sweepable by any pane's `scruff reap`;
**1** the invocation was wrong, usually a repo path that isn't a main checkout.

Then stop — **the handoff is done the moment the lane opens.** This session need
not be: forking a side task off a live thread is the strongest use of that ending.

## Shape of the prompt

Aim for **150 words, hard cap 250**. Every line earns its place; drop any
heading with nothing real under it rather than writing "N/A".

```
<One sentence: what the next session must accomplish. Imperative, not a topic.>

Where: <repo> · branch <branch> · <repo-relative/path:line>, <path:line>
State: <2-4 sentences — what is already true, what was tried and rejected and
        why, what decision is already made. Past tense, load-bearing only.>
Verified: <what was actually run, and its result. "not verified" if it wasn't.>
Next: <the first concrete action, as a command or an edit at a path:line.>
Watch out: <the one gotcha that costs 20 minutes if they don't know it.>
Read first: <the doc, rule or skill it needs before editing. Omit if none.>
```

## Rules that make it usable cold

- **No pronouns without antecedents.** "it", "the file", "that approach" become
  the actual name. This is the single most common way a handoff fails.
- **Verbatim commands and real branch names**, never a paraphrase. Paths outside
  any repo go absolute — a fresh session may start in a different cwd.
- **Name the repo and the branch, not just the checkout path.** A lane's
  checkout at `~/.cache/…/<repo>/<name>` is deleted when its pane closes (work
  is parked on the branch first); the branch is the durable handle, and `scruff
  <name>` rebuilds the checkout around it. Paths inside the repo go repo-relative.
- **Point at what is already written, never copy it.** A spec, an issue, a PR, a
  diff, a rule in AGENTS.md — give the path or the URL. Two copies drift.
- **No secrets.** The brief lands on disk and in another agent's prompt: name
  the env var or the vault item, never paste a key or a token.
- **Say what's unproven.** The next session treats the brief as a contract and
  won't re-check it, so "builds, not feel-tested" beats a confident "done".
- **Carry the constraints, not the history.** "We decided X over Y because Y
  breaks Z" — one line. Nobody needs the path walked to get there.
- **Never invent, and don't re-derive.** No plausible file names, no assumed
  test commands, no imagined next steps — unknown is a fact, and gets written
  down as one. Where the paste already has a good finding, keep its wording.
- No greeting, no "you are an agent that…", no praise, no closing pleasantry.

## Print format — exact

The clipboard or the lane is how it travels; this print is so the user can see
where it starts and stops in a wall of transcript. Markers verbatim, own lines:

````
━━━━━━━━━ HANDOFF BEGINS ━━━━━━━━━ (copied to clipboard)

<the prompt, exactly as it was copied>

━━━━━━━━━ HANDOFF ENDS ━━━━━━━━━
````

Say which ending ran on the BEGINS line — `(copied to clipboard)` or
`(spawned as <repo>/<lane>)`. Wrap the prompt in a fenced block *inside* those
markers so its own formatting survives; if the prompt itself contains a fence,
use a longer one on the outside. After the ENDS line, first the file's path —
the clipboard moves on, so it is the only handle left — then any Gaps note.

## Gaps

Too thin to ground — no repo, no path, no objective? Produce the best handoff
the material supports, mark holes inline as `UNKNOWN: <what>`, and name after
the closing marker the one thing that would most improve it. Don't interrogate.

**A brief with an UNKNOWN in its objective or its Next line does not get the
lane ending.** Take the clipboard, and say which hole blocked it. Spawning an
agent onto a task nobody can start burns a context window and lands nothing.
