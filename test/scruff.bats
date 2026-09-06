#!/usr/bin/env bats
# Hermetic tests for `wt` — the agent-worktree manager (modules/den/wt.sh).
#
# Everything runs against throwaway repos in $BATS_TEST_TMPDIR: HOME, the
# worktree base (CLAUDE_WT_BASE, already an env knob) and every external tool
# wt shells out to (`gh`, `lsof`) are substituted, so the suite never touches
# the machine's real registry, real repos, or the network. `wt`'s bare-PATH
# rescue is APPENDED rather than prepended precisely so these shims win.
#
# What this pins down, roughly in the order a worktree lives:
#   create → park/unpark → list → resume → remove → reap → registry upkeep
#
# Nothing here is skipped — every contract the suite states holds. To see what a
# given revision of the script breaks, point the suite at it:
#
#   git show <rev>:modules/den/wt.sh > /tmp/wt.sh
#   WT_UNDER_TEST=/tmp/wt.sh bats test/wt.bats
#
# That is the intended way to demonstrate a bug: write the test, watch it fail
# against the old copy, fix, watch it pass against both the new copy and the
# whole suite. A test that passes against BOTH copies is not reproducing the bug
# you think it is — two of these did exactly that on the first attempt.

bats_require_minimum_version 1.5.0   # `run --separate-stderr`, used by the width test

setup() {
  # WT_UNDER_TEST lets you point the whole suite at another copy of the script —
  # an older revision, a candidate rewrite — to see exactly which contracts it
  # breaks. That is how the fixes below were shown to fix something.
  WT="${WT_UNDER_TEST:-$BATS_TEST_DIRNAME/../scruff}"
  # Fail LOUDLY when there is no binary to test, instead of letting every
  # invocation exit 127 and every fixture capture an empty path. That is not a
  # theoretical tidiness: with $WT missing, `dir="$(hook_create …)"` yields "",
  # `commit_in` then runs `git -C "" commit`, and `git -C ""` means THE CURRENT
  # DIRECTORY — so the suite committed twice into the real scruff checkout it was
  # being run from. `make test` builds first; running bats directly does not.
  [ -x "$WT" ] || {
    printf 'no scruff binary at %s — run `make test`, or set WT_UNDER_TEST\n' "$WT" >&2
    return 1
  }
  # macOS puts BATS_TEST_TMPDIR under /var/folders, a symlink to /private/var.
  # git resolves paths (`rev-parse --path-format=absolute`) while our fixtures
  # would carry the unresolved form, so registry rows and git's own answers
  # would never string-compare equal. Resolve once, up front.
  TMP="$(cd "$BATS_TEST_TMPDIR" && pwd -P)"

  # Hermetic git: the machine's global config (gpgsign, hooks, default branch,
  # user identity) must not leak in or the same test passes here and fails in CI.
  export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
  export GIT_AUTHOR_NAME=Test GIT_AUTHOR_EMAIL=t@example.com
  export GIT_COMMITTER_NAME=Test GIT_COMMITTER_EMAIL=t@example.com

  export HOME="$TMP/home"                 # wt_projdir + the WT_BASE default live here
  export XDG_CONFIG_HOME="$TMP/config"    # Scruff's persisted default lives here
  # Occupancy leases live under here. NOT $TMP/state — the hook tests use that
  # path as a scratch FILE to record that they ran, and a directory of the same
  # name makes their `cat` fail in a way that reads as a hook bug.
  export XDG_STATE_HOME="$TMP/xdg-state"
  # Scrub the env knobs: machine choices must not leak into the fixtures.
  unset SCRUFF_BASE
  unset SCRUFF_AGENT HAUS_AGENT_DEFAULT # machine choices must not leak in
  unset SCRUFF_STATE SCRUFF_OCCUPANCY   # the lease dir and its sole-provider switch
  unset SCRUFF_TRILL                    # the notify tests shim trill on PATH
  export CLAUDE_WT_BASE="$TMP/wtbase"
  REG="$CLAUDE_WT_BASE/registry.tsv"
  mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_STATE_HOME"

  BIN="$TMP/bin"; mkdir -p "$BIN"
  export PATH="$BIN:$PATH"

  # ── shim: gh ───────────────────────────────────────────────────────────────
  # wt asks gh four things, and the shim answers by shape:
  #   --head <branch>  the precise "did THIS branch's PR merge, at what SHA?"
  #                    (pr_merge_info, the gate branch_landed reaps on).
  #                    FAKE_GH_MERGED=1 → yes, with FAKE_GH_OID/FAKE_GH_PR.
  #   no --head        the repo-wide merged-PR map (merged_map — one call per
  #                    repo, feeds the +N annotations). Answers for the single
  #                    branch FAKE_GH_BRANCH, which is all any test needs.
  #                    FAKE_GH_MERGED_AT is that PR's closedAt — the stamp that
  #                    says whether it is THIS lane's or the last lane to wear
  #                    the name. Unset is "the forge didn't say", which is what
  #                    every test written before the gate relies on.
  #   --state open     two shapes, told apart by the fields asked for:
  #                    --json url  → "is a PR already open?" (reship) →
  #                    FAKE_GH_OPEN_URL. --json …headRefOid → the repo-wide
  #                    OPEN map (open_map, the other half of the +N answer) →
  #                    FAKE_GH_OPEN_BRANCH/_OID/_PR.
  #   --state closed   the dead-end question → FAKE_GH_CLOSED_PR, with
  #                    FAKE_GH_CLOSED_OID/_AT its head SHA and close date —
  #                    same ownership gate as the merged map.
  #   pr create        opens one → FAKE_GH_PR_URL
  # Printing nothing is a real gh's answer when offline or unauthenticated.
  cat >"$BIN/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"${FAKE_GH_LOG:-/dev/null}"
# `scruff doctor` asks two repo-less questions (internal/commands/diagnose.go):
# the version, and whether the CLI can talk to the forge at all. FAKE_GH_UNAUTH
# is the installed-but-logged-out case, which is a FALSE the doctor must keep
# distinct from the null it reports when there is no gh to ask.
case "$1" in
  --version) printf 'gh version 2.63.2 (2026-01-01)\n'; exit 0 ;;
esac
case "$1 $2" in
  "auth status")
    [ "${FAKE_GH_UNAUTH:-0}" = 1 ] && { printf 'You are not logged into any GitHub hosts.\n' >&2; exit 1; }
    printf '  github.com\n    Logged in to github.com account octocat (keyring)\n'; exit 0 ;;
esac
case "$1 $2" in
  "pr create") printf '%s\n' "${FAKE_GH_PR_URL:-https://github.com/acme/alpha/pull/9}"; exit 0 ;;
esac
case "$1 $2" in
  "repo view") printf '%s' "${FAKE_GH_ARCHIVED:-false}"; exit 0 ;;
esac
case " $* " in
  *" --state open "*)
    case " $* " in
      *headRefOid*)
        [ -n "${FAKE_GH_OPEN_BRANCH:-}" ] || exit 0
        printf '%s\t%s\t%s\n' "$FAKE_GH_OPEN_BRANCH" "${FAKE_GH_OPEN_OID:-}" "${FAKE_GH_OPEN_PR:-8}"
        exit 0 ;;
    esac
    printf '%s' "${FAKE_GH_OPEN_URL:-}"; exit 0 ;;
  *" --state closed "*)
    [ -n "${FAKE_GH_CLOSED_PR:-}" ] || exit 0
    printf '%s\t%s\t%s\n' "$FAKE_GH_CLOSED_PR" "${FAKE_GH_CLOSED_OID:-}" "${FAKE_GH_CLOSED_AT:-}"
    exit 0 ;;
  *" --head "*)
    [ "${FAKE_GH_MERGED:-0}" = 1 ] || exit 0
    printf 'MERGED %s %s\n' "${FAKE_GH_OID:-}" "${FAKE_GH_PR:-7}"; exit 0 ;;
esac
[ "${FAKE_GH_MERGED:-0}" = 1 ] && [ -n "${FAKE_GH_BRANCH:-}" ] || exit 0
printf '%s\t%s\t%s\t%s\n' "$FAKE_GH_BRANCH" "${FAKE_GH_OID:-}" "${FAKE_GH_PR:-7}" "${FAKE_GH_MERGED_AT:-}"
EOF

  # ── shim: lsof ─────────────────────────────────────────────────────────────
  # wt reads one dump of every process's cwd to decide "is a pane standing in
  # this worktree?". Always emit at least "/" so the dump is non-empty — an
  # EMPTY dump is wt's "lsof told me nothing, degrade to parked-only" signal,
  # which FAKE_LSOF_BROKEN=1 exercises deliberately.
  #
  # Real `-F pcn` FIELD SETS, not bare `n` lines: scruff now carries the pid and
  # command name into every refusal it prints, so a shim that emitted only
  # paths would let the parse regress without a single test noticing. Pids
  # start at 4001 and count up per cwd; FAKE_LSOF_CMD names them all.
  cat >"$BIN/lsof" <<'EOF'
#!/usr/bin/env bash
[ "${FAKE_LSOF_BROKEN:-0}" = 1 ] && exit 1
printf 'p1\ncinit\nfcwd\nn/\n'
pid=4000
for c in ${FAKE_LSOF_CWDS:-}; do
  pid=$((pid + 1))
  printf 'p%s\nc%s\nfcwd\nn%s\n' "$pid" "${FAKE_LSOF_CMD:-zsh}" "$c"
done
EOF

  chmod +x "$BIN/gh" "$BIN/lsof"
  export FAKE_GH_LOG="$TMP/gh.log"

  # Last: stand somewhere harmless. bats starts every test in the checkout it
  # was launched from — the REAL scruff repo — so a test whose fixture path came
  # back empty runs `cd ""`, which bash accepts as a no-op, and the mutating
  # verb on the next line lands here. That is not hypothetical: writing the
  # `park --help` test below parked this repo's own working tree as a `wip:`
  # commit on the branch it was being written on. Every test that needs a repo
  # cd's into one; from $TMP the same slip finds no git repo and says so.
  cd "$TMP" || return 1
}

# ── fixtures ─────────────────────────────────────────────────────────────────

mkrepo() { # mkrepo <name> — a main checkout on `main`, with a GitHub origin
  local name="$1" main="$TMP/repos/$1"
  mkdir -p "$main"
  git -C "$main" init -q -b main
  git -C "$main" config commit.gpgsign false
  echo hello >"$main/README.md"
  git -C "$main" add -A
  git -C "$main" commit -qm init
  # repo_slug parses this for `gh -R`; a real-looking URL keeps that path honest.
  git -C "$main" remote add origin "https://github.com/acme/$name.git"
  printf '%s' "$main"
}

wt_run() { run "$WT" "$@"; }

hook_create() { # hook_create <main> <name> — drive the WorktreeCreate hook
  printf '{"name":"%s","cwd":"%s"}' "$2" "$1" | "$WT" create
}

hook_remove() { # hook_remove <worktree-path>
  printf '{"worktree_path":"%s"}' "$1" | "$WT" remove
}

commit_in() { # commit_in <checkout> <file> <msg> — give a branch real history
  # Guard the path. `git -C ""` is not an error to git — it means the current
  # directory — so an empty $1 here silently retargets every command below at
  # whatever repo the suite is being RUN from. That is not hypothetical: it
  # happened, and it left two commits in the real scruff repo.
  [ -n "$1" ] && [ -d "$1" ] || { printf 'commit_in: refusing an empty/missing checkout path\n' >&2; return 1; }
  echo "$RANDOM" >"$1/$2"
  git -C "$1" add -A
  git -C "$1" -c commit.gpgsign=false commit -qm "$3"
}

# A worktree with one commit of its own, so it is NOT ancestry-merged into main
# and therefore survives the self-heal sweep that every `wt` listing runs.
mkwt() { # mkwt <main> <name> — echo the checkout path
  local dir; dir="$(hook_create "$1" "$2")"
  [ -n "$dir" ] && [ -d "$dir" ] || { printf 'mkwt: create gave no usable path (%s)\n' "$dir" >&2; return 1; }
  commit_in "$dir" work.txt "work on $2"
  printf '%s' "$dir"
}

# awk, not `grep -c`: grep prints "0" AND exits 1 on an empty file, so the
# obvious `grep -c . "$REG" || echo 0` emits "0\n0" and every -eq blows up.
reg_rows() { awk 'NF' "$REG" 2>/dev/null | wc -l | tr -d ' '; }

fail() { printf '%s\n' "$*" >&2; return 1; }   # not a bats builtin

# ── create (WorktreeCreate hook) ─────────────────────────────────────────────

@test "create: makes <base>/<name> on worktree-<name> and prints ONLY the path" {
  local main; main="$(mkrepo alpha)"
  run bash -c "printf '{\"name\":\"sparkle\",\"cwd\":\"$main\"}' | '$WT' create 2>/dev/null"
  [ "$status" -eq 0 ]
  [ "$output" = "$CLAUDE_WT_BASE/alpha/sparkle" ]
  [ -e "$output/.git" ]
  [ "$(git -C "$output" branch --show-current)" = worktree-sparkle ]
}

@test "create: accepts the documented key names too (worktree_name/base_path)" {
  local main; main="$(mkrepo alpha)"
  run bash -c "printf '{\"worktree_name\":\"doc\",\"base_path\":\"$main\"}' | '$WT' create 2>/dev/null"
  [ "$status" -eq 0 ]
  [ "$(git -C "$output" branch --show-current)" = worktree-doc ]
}

@test "create: records main, branch, path, parent, and its Claude client in the registry" {
  local main dir; main="$(mkrepo alpha)"; dir="$(hook_create "$main" sparkle)"
  run cat "$REG"
  [ "$output" = "$(printf 'sparkle\t%s\tworktree-sparkle\t%s\t%s\tclaude' "$main" "$dir" "$main")" ]
}

@test "create: a name whose branch already exists fails instead of half-creating" {
  local main; main="$(mkrepo alpha)"
  hook_create "$main" dup >/dev/null 2>&1
  rm -rf "$CLAUDE_WT_BASE/alpha/dup"
  run bash -c "printf '{\"name\":\"dup\",\"cwd\":\"$main\"}' | '$WT' create"
  [ "$status" -ne 0 ]
  # NOTE: today this is a raw `git worktree add` error. cmd_child has friendly
  # collision guards; cmd_create does not. See the create-guard gap.
}

@test "create: a garbage hook payload fails loudly, naming the keys it wanted" {
  run bash -c "printf '{\"nope\":1}' | '$WT' create"
  [ "$status" -ne 0 ]
  [[ "$output" == *"none of"* ]]
}

# ── park ─────────────────────────────────────────────────────────────────────

@test "park: dirty tree becomes one wip: commit and the tree goes clean" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" p1)"
  echo edited >"$dir/README.md"
  echo new >"$dir/untracked.txt"
  cd "$dir"; wt_run park "mid refactor"
  [ "$status" -eq 0 ]
  [[ "$output" == *"parked 2 change(s)"* ]]
  [ -z "$(git -C "$dir" status --porcelain)" ]
  [[ "$(git -C "$dir" log -1 --format=%s)" == "wip: mid refactor (parked "* ]]
  # Untracked files are swept in too — that is the point of "set the tree aside".
  git -C "$dir" show --name-only --format= HEAD | grep -qx untracked.txt
}

@test "park: with no label still parks, under a generic subject" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" p2)"
  echo x >>"$dir/README.md"
  cd "$dir"; wt_run park
  [ "$status" -eq 0 ]
  [[ "$(git -C "$dir" log -1 --format=%s)" == "wip: parked "* ]]
}

@test "park: a clean tree is a no-op, not an empty commit" {
  local main dir head; main="$(mkrepo alpha)"; dir="$(mkwt "$main" p3)"
  head="$(git -C "$dir" rev-parse HEAD)"
  cd "$dir"; wt_run park
  [ "$status" -eq 0 ]
  [[ "$output" == *"nothing to park"* ]]
  [ "$(git -C "$dir" rev-parse HEAD)" = "$head" ]
}

@test "park: refuses on detached HEAD — the commit would be unreachable" {
  local main dir head; main="$(mkrepo alpha)"; dir="$(mkwt "$main" p4)"
  git -C "$dir" checkout -q --detach
  head="$(git -C "$dir" rev-parse HEAD)"
  echo x >>"$dir/README.md"
  cd "$dir"; wt_run park
  [ "$status" -ne 0 ]
  [[ "$output" == *"detached"* ]]
  [ "$(git -C "$dir" rev-parse HEAD)" = "$head" ]
  [ -n "$(git -C "$dir" status --porcelain)" ]   # the edit is untouched
}

@test "park: on a non-agent branch it still parks, but warns not to push it" {
  local main; main="$(mkrepo alpha)"
  echo x >>"$main/README.md"
  cd "$main"; wt_run park
  [ "$status" -eq 0 ]
  [[ "$output" == *"isn't an agent branch"* ]]
}

@test "park: outside a git repo dies without touching anything" {
  mkdir -p "$TMP/notarepo"; cd "$TMP/notarepo"
  wt_run park
  [ "$status" -ne 0 ]
  [[ "$output" == *"not in a git repo"* ]]
}

# ── unpark ───────────────────────────────────────────────────────────────────

@test "unpark: rewinds the wip commit and gives the files back, uncommitted" {
  local main dir base; main="$(mkrepo alpha)"; dir="$(mkwt "$main" u1)"
  base="$(git -C "$dir" rev-parse HEAD)"
  echo edited >"$dir/README.md"
  cd "$dir"; "$WT" park >/dev/null 2>&1
  wt_run unpark
  [ "$status" -eq 0 ]
  [ "$(git -C "$dir" rev-parse HEAD)" = "$base" ]
  [ "$(cat "$dir/README.md")" = edited ]
  [ -n "$(git -C "$dir" status --porcelain)" ]
}

@test "unpark: a parked UNTRACKED file comes back untracked, not staged" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" u2)"
  echo new >"$dir/fresh.txt"
  cd "$dir"; "$WT" park >/dev/null 2>&1
  wt_run unpark
  [ "$status" -eq 0 ]
  [ "$(git -C "$dir" status --porcelain fresh.txt)" = "?? fresh.txt" ]
}

@test "unpark: refuses when HEAD isn't a wip: commit" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" u3)"
  cd "$dir"; wt_run unpark
  [ "$status" -ne 0 ]
  [[ "$output" == *"isn't a parked commit"* ]]
}

@test "unpark: refuses to rewrite a wip commit that is already pushed" {
  local main dir head; main="$(mkrepo alpha)"; dir="$(mkwt "$main" u4)"
  echo edited >"$dir/README.md"
  cd "$dir"; "$WT" park >/dev/null 2>&1
  head="$(git -C "$dir" rev-parse HEAD)"
  # Stand in for "pushed": a remote-tracking ref that contains the wip commit.
  git -C "$dir" update-ref refs/remotes/origin/worktree-u4 HEAD
  wt_run unpark
  [ "$status" -ne 0 ]
  [[ "$output" == *"already pushed"* ]]
  [ "$(git -C "$dir" rev-parse HEAD)" = "$head" ]   # never force-push behind your back
}

@test "unpark: refuses when the wip commit is the branch's root commit" {
  local root; root="$TMP/repos/rootonly"
  mkdir -p "$root"; git -C "$root" init -q -b main
  git -C "$root" config commit.gpgsign false
  echo a >"$root/a.txt"
  cd "$root"; "$WT" park >/dev/null 2>&1
  wt_run unpark
  [ "$status" -ne 0 ]
  [[ "$output" == *"first commit"* ]]
}

@test "unpark: two parks need two unparks — one call rewinds only the newest" {
  local main dir base; main="$(mkrepo alpha)"; dir="$(mkwt "$main" u5)"
  base="$(git -C "$dir" rev-parse HEAD)"
  cd "$dir"
  echo one >"$dir/one.txt";  "$WT" park first  >/dev/null 2>&1
  echo two >"$dir/two.txt";  "$WT" park second >/dev/null 2>&1
  "$WT" unpark >/dev/null 2>&1
  [ "$(git -C "$dir" rev-parse HEAD)" != "$base" ]        # the first park is still committed
  [[ "$(git -C "$dir" log -1 --format=%s)" == "wip: first"* ]]
  "$WT" unpark >/dev/null 2>&1
  [ "$(git -C "$dir" rev-parse HEAD)" = "$base" ]
  [ -f "$dir/one.txt" ] && [ -f "$dir/two.txt" ]
}

# ── list ─────────────────────────────────────────────────────────────────────

@test "list: says so plainly when there is nothing parked" {
  wt_run list
  [ "$status" -eq 0 ]
  [[ "$output" == *"none parked"* ]]
}

@test "list: shows a live checkout as live and a removed one as parked" {
  local main a b; main="$(mkrepo alpha)"
  a="$(mkwt "$main" alive)"; b="$(mkwt "$main" gone)"
  git -C "$main" worktree remove --force "$b"
  wt_run list
  [ "$status" -eq 0 ]
  [[ "$output" == *"alive"*"live"* ]]
  echo "$output" | grep -Eq '^\s+alpha\s+gone\s+parked'
}

# An agent branch can be checked out somewhere `wt` never put it: a raw
# `git worktree add`, or another agent's own worktree feature (codex keeps its
# under ~/.codex/worktrees/). Such a branch is only ever discovered by the
# orphan scan, which SYNTHESIZES a path from the bucket convention — a path that
# does not exist. Trusting that guess files a very-much-live checkout as parked.
# (A rename INSIDE the base is not this case: the disk glob re-reads each live
# checkout's current branch, so it self-corrects.)
mk_stray() { # mk_stray <main> <name> — a worktree-<name> checkout outside WT_BASE
  local out="$TMP/elsewhere/$2"
  mkdir -p "$TMP/elsewhere"
  # wt discovers repos through the registry and the WT_BASE glob, so a repo whose
  # ONLY worktree is a stray is invisible entirely — a different (and correct)
  # behaviour. Anchor the repo with one ordinary worktree so the orphan scan runs
  # and the stray is actually reached. That is also the real-world shape.
  [ -n "$(awk -F'\t' -v m="$1" '$2==m' "$REG" 2>/dev/null)" ] || mkwt "$1" "${2}-anchor" >/dev/null
  git -C "$1" worktree add -q -b "worktree-$2" "$out" >/dev/null 2>&1
  commit_in "$out" stray.txt "work in an unregistered checkout"
  printf '%s' "$out"
}

@test "list: a branch checked out OUTSIDE the worktree base still reads as live" {
  local main out; main="$(mkrepo alpha)"; out="$(mk_stray "$main" manual)"
  wt_run list
  [ "$status" -eq 0 ]
  echo "$output" | grep -Eq '^\s+alpha\s+manual\s+live'
  [ -e "$out/.git" ]
}

@test "list: stays one line per worktree in a narrow pane" {
  command -v python3 >/dev/null || skip "no python3 to fork a pty with"
  local main; main="$(mkrepo alpha)"
  mkwt "$main" a-rather-long-worktree-name >/dev/null
  # A REAL pty, 48 columns wide. The listing is budgeted against the stream it
  # lands on, so neither COLUMNS nor `tput cols` is asked any more — the former
  # was never a promise the kernel made and the latter answers a static 80 in a
  # 40-column pane. A pipe is deliberately NOT truncated (a captured listing
  # keeps every name whole for whatever greps it), which is why measuring this
  # takes a window rather than an environment variable.
  #
  # Both streams land on the one pty here, which is the shape a person actually
  # sees: the banner folds to the same window as the table, so every line is
  # under the contract, not just the rows.
  run python3 - "$WT" <<'PYEOF'
import fcntl, os, pty, select, struct, sys, termios
cmd = sys.argv[1]
pid, fd = pty.fork()
if pid == 0:
    # From the CHILD, before exec: the parent cannot set the size without
    # racing the very first line scruff prints.
    fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 48, 0, 0))
    os.execvp(cmd, [cmd, "list"])
out = b""
while True:
    r, _, _ = select.select([fd], [], [], 10)
    if not r:
        break
    try:
        d = os.read(fd, 65536)
    except OSError:
        break
    if not d:
        break
    out += d
os.waitpid(pid, 0)
sys.stdout.write(out.decode("utf8", "replace"))
PYEOF
  [ "$status" -eq 0 ]
  local clean; clean="$(printf '%s' "$output" | sed $'s/\033\[[0-9;?]*[a-zA-Z]//g' | tr -d '\r')"
  while IFS= read -r l; do
    [ "${#l}" -le 48 ] || fail "line wider than the 48-column window: $l"
  done <<<"$clean"
  # And it is still a TABLE at that width, one line per lane — the stacked
  # fallback would put the repo and the name on lines of their own.
  echo "$clean" | grep -Eq '^\s+alpha\s+a-rather-lo.*live' || fail "no single-line row: $clean"
}

@test "list: self-heals — a parked branch already merged into main is reaped" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" landed)"
  git -C "$main" merge -q --no-edit worktree-landed
  git -C "$main" worktree remove --force "$dir"
  wt_run list
  [ "$status" -eq 0 ]
  [[ "$output" == *"swept 1 merged lane"* ]]
  run git -C "$main" show-ref -q --verify refs/heads/worktree-landed
  [ "$status" -ne 0 ]
}

# ── resume ───────────────────────────────────────────────────────────────────

@test "resume: rebuilds a parked checkout at its registered path" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" back)"
  git -C "$main" worktree remove --force "$dir"
  [ ! -e "$dir" ]
  wt_run resume back
  [ "$status" -eq 0 ]
  [ -e "$dir/.git" ]
  [ "$(git -C "$dir" branch --show-current)" = worktree-back ]
  [[ "$output" == *"claude --continue"* ]]   # no tty → prints the command, never execs
}

@test "resume: a lane's own chat is CONTINUED, never offered as a picker" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" solo)"
  git -C "$main" worktree remove --force "$dir"
  wt_run resume solo
  [ "$status" -eq 0 ]
  # One checkout, one lane, one newest conversation: asking which is a question
  # with a single answer, and scruff is the one holding it.
  [[ "$output" == *"claude --continue"* ]]
  [[ "$output" != *"--resume"* ]] || fail "the picker came back: $output"
}

@test "resume: --pick asks for the picker anyway, either side of the name" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" choosy)"
  git -C "$main" worktree remove --force "$dir"
  wt_run resume choosy --pick
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude --resume"* ]]
  wt_run resume --pick choosy
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude --resume"* ]]
}

@test "resume: a live worktree is reported live, not rebuilt" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" here)"
  wt_run resume here
  [ "$status" -eq 0 ]
  [[ "$output" == *"still live at $dir"* ]]
}

@test "resume: an ambiguous name across two repos demands a repo qualifier" {
  local a b; a="$(mkrepo alpha)"; b="$(mkrepo beta)"
  mkwt "$a" twin >/dev/null; mkwt "$b" twin >/dev/null
  wt_run resume twin
  [ "$status" -ne 0 ]
  [[ "$output" == *"more than one repo"* ]]
  wt_run resume alpha/twin
  [ "$status" -eq 0 ]
}

@test "resume: an unknown name dies pointing at the listing" {
  wt_run resume nope
  [ "$status" -ne 0 ]
  [[ "$output" == *"no lane named 'nope'"* ]]
}

@test "resume: a unique PREFIX of the name resolves — the listing cuts cells, so what you can see is what you type" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" frisky-vole)"
  git -C "$main" worktree remove --force "$dir"
  wt_run resume frisky
  [ "$status" -eq 0 ]
  [ -e "$dir/.git" ]
  [[ "$output" == *"matched by prefix"* ]]
}

@test "resume: a PASTED cut cell resolves — the trailing ellipsis is unwrapped, not taken as text" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" frisky-vole)"
  git -C "$main" worktree remove --force "$dir"
  wt_run resume "frisky-vole…"
  [ "$status" -eq 0 ]
  [ -e "$dir/.git" ]
  wt_run resume "frisky-vole..."   # the ASCII mangle a shell can hand back
  [ "$status" -eq 0 ]
}

@test "resume: an AMBIGUOUS prefix dies naming every lane it matched" {
  local main; main="$(mkrepo alpha)"
  mkwt "$main" frisky-vole >/dev/null
  mkwt "$main" frisky-vole-two >/dev/null
  wt_run resume frisky
  [ "$status" -ne 0 ]
  [[ "$output" == *"matches several lanes"* ]]
  [[ "$output" == *"alpha/frisky-vole"* ]]
  [[ "$output" == *"alpha/frisky-vole-two"* ]]
  wt_run resume frisky-vole-t   # a longer prefix narrows to one and resumes
  [ "$status" -eq 0 ]
}

@test "resume: the REPO cell may arrive cut too — a repo prefix qualifies" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" solo)"
  git -C "$main" worktree remove --force "$dir"
  wt_run resume "al/solo…"
  [ "$status" -eq 0 ]
  [ -e "$dir/.git" ]
}

# ── focus ────────────────────────────────────────────────────────────────────

@test "focus: the hook is handed the lane, and its yes ends it" {
  local main hook; main="$(mkrepo alpha)"; mkwt "$main" glance >/dev/null
  hook="$(mkhook focus 'printf "%s %s\n" "$SCRUFF_NAME" "$SCRUFF_MAIN" >"'"$TMP"'/focused"; exit 0')"
  setcfg "[hooks]
focus = \"$hook\""
  wt_run focus glance
  [ "$status" -eq 0 ]
  [ "$(cat "$TMP/focused")" = "glance $main" ]
  # Handled means handled: resume must not also run and open a second window
  # onto the session the hook just raised.
  [[ "$output" != *"claude --continue"* ]] || fail "resume ran behind the hook: $output"
}

@test "focus: a hook that defers falls back to resume — a lane with no window still opens one" {
  local main dir hook; main="$(mkrepo alpha)"; dir="$(mkwt "$main" detached)"
  git -C "$main" worktree remove --force "$dir"
  hook="$(mkhook focus 'exit 3')"
  setcfg "[hooks]
focus = \"$hook\""
  wt_run focus detached
  [ "$status" -eq 0 ]
  [ -e "$dir/.git" ]
  [[ "$output" == *"claude --continue"* ]]
}

@test "focus: with no hook at all it is resume — scruff's own answer to go-to-this-lane" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" plain)"
  git -C "$main" worktree remove --force "$dir"
  wt_run focus plain
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude --continue"* ]]
}

@test "focus: a live lane is answered from the registry, at a cost that doesn't grow with the machine" {
  local a b hook; a="$(mkrepo alpha)"; b="$(mkrepo beta)"
  mkwt "$a" glance >/dev/null; mkwt "$b" elsewhere >/dev/null
  hook="$(mkhook focus 'exit 0')"
  setcfg "[hooks]
focus = \"$hook\""

  # Count git subprocesses, because that is where this path spent its second:
  # matchLane's discover globs every checkout under the base and asks every main
  # checkout it reached for its worktree-* branches, so ONE click cost more on a
  # busier machine — over a hundred forks with a few dozen lanes open, MEASURED.
  # A banner click is the one caller that cannot afford it (trill's focus_lane
  # runs this), so a live, unambiguous lane is answered from the registry, which
  # invariant 3 already calls the source of truth.
  local realgit; realgit="$(command -v git)"
  cat >"$BIN/git" <<EOF
#!/usr/bin/env bash
echo . >>"$TMP/gitcalls"
exec "$realgit" "\$@"
EOF
  chmod +x "$BIN/git"

  : >"$TMP/gitcalls"
  wt_run focus glance
  [ "$status" -eq 0 ]
  local few; few="$(wc -l <"$TMP/gitcalls" | tr -d ' ')"
  [ "$few" -gt 0 ] || fail "the git shim never ran — this test is measuring nothing"

  # Six more lanes, the same click. The number is the contract, not the ceiling:
  # constant means the registry answered, and anything that grows here has put
  # the whole-machine walk back on the click path.
  local n; for n in one two three four five six; do mkwt "$a" "$n" >/dev/null; done
  : >"$TMP/gitcalls"
  wt_run focus glance
  [ "$status" -eq 0 ]
  local many; many="$(wc -l <"$TMP/gitcalls" | tr -d ' ')"
  [ "$many" -eq "$few" ] || fail "focus got dearer as lanes were added: $few git calls → $many"
}

@test "focus: a registry row the disk has outgrown is answered the thorough way" {
  local main dir hook; main="$(mkrepo alpha)"; dir="$(mkwt "$main" glance)"
  # The registry records where a checkout WAS. Moving it makes that row a lie of
  # exactly the kind discover exists to correct — so the fast path above has to
  # verify before it believes, or a click lands on a path that isn't there.
  git -C "$main" worktree move "$dir" "$TMP/moved"
  hook="$(mkhook focus 'printf "%s %s\n" "$SCRUFF_NAME" "$SCRUFF_PATH" >"'"$TMP"'/focused"; exit 0')"
  setcfg "[hooks]
focus = \"$hook\""
  wt_run focus glance
  [ "$status" -eq 0 ]
  [ "$(cat "$TMP/focused")" = "glance $TMP/moved" ]
}

@test "focus: an ambiguous or unknown name is refused, never guessed at" {
  local a b; a="$(mkrepo alpha)"; b="$(mkrepo beta)"
  mkwt "$a" twin >/dev/null; mkwt "$b" twin >/dev/null
  wt_run focus twin
  [ "$status" -ne 0 ]
  [[ "$output" == *"more than one repo"* ]]
  wt_run focus nope
  [ "$status" -ne 0 ]
  [[ "$output" == *"no lane named 'nope'"* ]]
  wt_run focus
  [ "$status" -ne 0 ]
}

@test "resume: a branch checked out OUTSIDE the base is reported live, not re-added" {
  local main out; main="$(mkrepo alpha)"; out="$(mk_stray "$main" manual)"
  wt_run resume manual
  [ "$status" -eq 0 ]
  # Trusting the synthesized path here means `git worktree add` on a branch that
  # is already checked out — which fails outright, so the worktree is unreachable.
  [[ "$output" == *"still live at $out"* ]]
}

# ── remove (WorktreeRemove hook) ─────────────────────────────────────────────

@test "remove: unmerged work survives — checkout gone, branch and registry kept" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" keep)"
  hook_remove "$dir"
  [ ! -e "$dir" ]
  git -C "$main" show-ref -q --verify refs/heads/worktree-keep
  [ "$(reg_rows)" -eq 1 ]
}

@test "remove: uncommitted edits are auto-parked as a wip commit, never dropped" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" dirty)"
  echo precious >"$dir/README.md"
  hook_remove "$dir"
  [[ "$(git -C "$main" log -1 --format=%s worktree-dirty)" == "wip: auto-saved on pane close"* ]]
  [ "$(git -C "$main" show worktree-dirty:README.md)" = precious ]
}

@test "remove: an ancestry-merged branch is reaped and its registry row dropped" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" done)"
  git -C "$main" merge -q --no-edit worktree-done
  hook_remove "$dir"
  run git -C "$main" show-ref -q --verify refs/heads/worktree-done
  [ "$status" -ne 0 ]
  [ "$(reg_rows)" -eq 0 ]
}

@test "remove: landed branch with ONLY untracked scratch reaps instead of parking" {
  # The regression that made merged worktrees pile up: WIP-committing build
  # scratch moves the tip past the merged PR's SHA, so the merge stops being
  # recognized and the worktree is falsely parked forever.
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" scratch)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)"
  mkdir -p "$dir/target"; echo junk >"$dir/target/o.o"
  hook_remove "$dir"
  run git -C "$main" show-ref -q --verify refs/heads/worktree-scratch
  [ "$status" -ne 0 ]
}

@test "remove: landed branch with TRACKED edits parks them and keeps the branch" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" late)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)"
  echo after-the-merge >"$dir/README.md"
  hook_remove "$dir"
  git -C "$main" show-ref -q --verify refs/heads/worktree-late
  [[ "$(git -C "$main" log -1 --format=%s worktree-late)" == "wip: auto-saved"* ]]
}

@test "remove: a squash-merged branch whose tip moved on is NOT reaped" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" moved)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)"
  commit_in "$dir" post.txt "work done after the PR merged"   # tip != headRefOid
  hook_remove "$dir"
  git -C "$main" show-ref -q --verify refs/heads/worktree-moved
}

# ── notify (Notification/Stop hook) ──────────────────────────────────────────
# The one hook that changes nothing: it forwards "blocked on the user" /
# "finished the turn" to trill as a banner. Its contract is exit 0 ALWAYS — a
# hook that can fail is a hook that can break the session it watches.

mktrill() { # a recording trill shim on PATH; FAKE_TRILL_EXIT fakes the daemon
  cat >"$BIN/trill" <<'EOF'
#!/usr/bin/env bash
printf 'trill %s\n' "$*" >>"${FAKE_TRILL_LOG:-/dev/null}"
exit "${FAKE_TRILL_EXIT:-0}"
EOF
  chmod +x "$BIN/trill"
  export FAKE_TRILL_LOG="$TMP/trill.log"
}

hook_notify() { # hook_notify <json> — drive the notify hook
  printf '%s' "$1" | "$WT" hook notify
}

@test "notify: a Notification becomes an ask banner titled with the lane name" {
  local main dir; main="$(mkrepo alpha)"; dir="$(hook_create "$main" sparkle)"
  mktrill
  run hook_notify "{\"hook_event_name\":\"Notification\",\"cwd\":\"$dir\",\"message\":\"needs permission to use Bash\"}"
  [ "$status" -eq 0 ]
  grep -q -- '--kind ask' "$FAKE_TRILL_LOG"
  grep -q -- '--title sparkle' "$FAKE_TRILL_LOG"
  # The payload message is conversation content — it must never reach trill.
  ! grep -q 'permission' "$FAKE_TRILL_LOG"
}

@test "notify: a Stop becomes a done banner" {
  local main dir; main="$(mkrepo alpha)"; dir="$(hook_create "$main" sparkle)"
  mktrill
  run hook_notify "{\"hook_event_name\":\"Stop\",\"cwd\":\"$dir\"}"
  [ "$status" -eq 0 ]
  grep -q -- '--kind done' "$FAKE_TRILL_LOG"
  grep -q -- '--title sparkle' "$FAKE_TRILL_LOG"
}

@test "notify: trill exit 2 (daemon down) is swallowed — the hook still exits 0" {
  local main dir; main="$(mkrepo alpha)"; dir="$(hook_create "$main" sparkle)"
  mktrill; export FAKE_TRILL_EXIT=2
  run hook_notify "{\"hook_event_name\":\"Stop\",\"cwd\":\"$dir\"}"
  [ "$status" -eq 0 ]
}

@test "notify: no trill binary anywhere is a silent no-op, exit 0" {
  local main dir; main="$(mkrepo alpha)"; dir="$(hook_create "$main" sparkle)"
  export SCRUFF_TRILL="$TMP/no-such-binary"   # authoritative when set: no fall-through
  run hook_notify "{\"hook_event_name\":\"Stop\",\"cwd\":\"$dir\"}"
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "notify: a garbage payload exits 0 — this hook must never break a session" {
  mktrill
  run hook_notify 'not json at all'
  [ "$status" -eq 0 ]
  run hook_notify '{"hook_event_name":"SomethingNew","cwd":"/x"}'
  [ "$status" -eq 0 ]
  ! grep -q 'trill' "$FAKE_TRILL_LOG"
}

@test "notify: a Notification is keyed by lane, so the next one replaces the fin" {
  local main dir; main="$(mkrepo alpha)"; dir="$(hook_create "$main" sparkle)"
  mktrill
  run hook_notify "{\"hook_event_name\":\"Notification\",\"cwd\":\"$dir\"}"
  [ "$status" -eq 0 ]
  grep -q -- '--key scruff/alpha/sparkle' "$FAKE_TRILL_LOG"
}

# The other half of the ask: the session moved again, so the question its fin
# asks has been answered. UserPromptSubmit is the user typing; PostToolUse means
# a tool actually ran, which is what approving a permission prompt leads to.
@test "notify: a resume event resolves the lane's fin" {
  local main dir; main="$(mkrepo alpha)"; dir="$(hook_create "$main" sparkle)"
  mktrill
  hook_notify "{\"hook_event_name\":\"Notification\",\"cwd\":\"$dir\"}"
  : >"$FAKE_TRILL_LOG"
  run hook_notify "{\"hook_event_name\":\"PostToolUse\",\"cwd\":\"$dir\"}"
  [ "$status" -eq 0 ]
  grep -q -- 'trill resolve scruff/alpha/sparkle' "$FAKE_TRILL_LOG"
}

# PostToolUse fires on every tool call in every pane. With no fin outstanding it
# must not launch trill at all — that binary is Trill.app's, and paying for it
# per tool call is the whole reason the marker gate exists.
@test "notify: a resume event with nothing outstanding launches no trill" {
  local main dir; main="$(mkrepo alpha)"; dir="$(hook_create "$main" sparkle)"
  mktrill
  run hook_notify "{\"hook_event_name\":\"UserPromptSubmit\",\"cwd\":\"$dir\"}"
  [ "$status" -eq 0 ]
  [ ! -s "$FAKE_TRILL_LOG" ]
}

# One resolve, not one per tool call: the fin is down after the first.
@test "notify: the fin resolves once, then the gate is shut again" {
  local main dir; main="$(mkrepo alpha)"; dir="$(hook_create "$main" sparkle)"
  mktrill
  hook_notify "{\"hook_event_name\":\"Notification\",\"cwd\":\"$dir\"}"
  hook_notify "{\"hook_event_name\":\"PostToolUse\",\"cwd\":\"$dir\"}"
  : >"$FAKE_TRILL_LOG"
  run hook_notify "{\"hook_event_name\":\"PostToolUse\",\"cwd\":\"$dir\"}"
  [ "$status" -eq 0 ]
  [ ! -s "$FAKE_TRILL_LOG" ]
}

# Another lane moving is not this lane being answered.
@test "notify: a resume event from a different lane leaves the fin up" {
  local main a b; main="$(mkrepo alpha)"
  a="$(hook_create "$main" sparkle)"; b="$(hook_create "$main" other)"
  mktrill
  hook_notify "{\"hook_event_name\":\"Notification\",\"cwd\":\"$a\"}"
  : >"$FAKE_TRILL_LOG"
  run hook_notify "{\"hook_event_name\":\"PostToolUse\",\"cwd\":\"$b\"}"
  [ "$status" -eq 0 ]
  [ ! -s "$FAKE_TRILL_LOG" ]
  # …and the fin is still there to be resolved when sparkle itself moves.
  hook_notify "{\"hook_event_name\":\"PostToolUse\",\"cwd\":\"$a\"}"
  grep -q -- 'trill resolve scruff/alpha/sparkle' "$FAKE_TRILL_LOG"
}

# A daemon that never took the ask leaves nothing armed: the next tool call must
# not pay for a resolve of a fin that was never on screen.
@test "notify: an ask trill refused arms no resolve" {
  local main dir; main="$(mkrepo alpha)"; dir="$(hook_create "$main" sparkle)"
  mktrill; export FAKE_TRILL_EXIT=2
  hook_notify "{\"hook_event_name\":\"Notification\",\"cwd\":\"$dir\"}"
  export FAKE_TRILL_EXIT=0
  : >"$FAKE_TRILL_LOG"
  run hook_notify "{\"hook_event_name\":\"PostToolUse\",\"cwd\":\"$dir\"}"
  [ "$status" -eq 0 ]
  [ ! -s "$FAKE_TRILL_LOG" ]
}

# ── reap ─────────────────────────────────────────────────────────────────────

@test "reap: removes a clean, landed, unoccupied checkout and its branch" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" sweepme)"
  git -C "$main" merge -q --no-edit worktree-sweepme
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped sweepme (alpha)"* ]]
  [ ! -e "$dir" ]
  [ "$(reg_rows)" -eq 0 ]
}

# ── the ask markers a reap is the only answer to ─────────────────────────────
# `scruff hook notify` keeps its resolve path cheap with one marker file per fin
# it put up, and clears that marker when the SESSION moves. Two shapes never
# move again: a lane blocked on you when its pane closed (it is answered by
# being reaped) and a pane outside every lane, keyed by session id. Left alone
# they accumulate one per abandoned question, the dir is never empty again, and
# the gate answers "yes, something is waiting" on every tool call in every pane
# for the life of the machine.

@test "reap: a lane reaped while it was blocked takes its fin down with it" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" sweepme)"
  git -C "$main" merge -q --no-edit worktree-sweepme
  mktrill
  local asks="$XDG_STATE_HOME/scruff/asks"
  mkdir -p "$asks"; : >"$asks/scruff.alpha.sweepme"

  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped sweepme (alpha)"* ]]
  # The marker is gone, and so is the fin: its `Go to lane` action would run
  # `scruff focus` against a lane that no longer exists.
  [ ! -e "$asks/scruff.alpha.sweepme" ]
  grep -q -- 'resolve scruff/alpha/sweepme' "$FAKE_TRILL_LOG"
  # And never the directory itself — something else on the machine watches it.
  [ -d "$asks" ]
}

@test "reap: an ordinary reap launches no trill at all" {
  local main; main="$(mkrepo alpha)"; mkwt "$main" sweepme >/dev/null
  git -C "$main" merge -q --no-edit worktree-sweepme
  mktrill
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  # The marker is the gate on the launch, not an afterthought to it: a lane
  # that ended its turn cleanly has nothing on the ledge, and a sweep of forty
  # of them must not start forty processes.
  [ ! -s "$FAKE_TRILL_LOG" ]
}

@test "list: a marker older than a day is dropped, whatever it named" {
  local asks="$XDG_STATE_HOME/scruff/asks"
  mkdir -p "$asks"
  : >"$asks/scruff.session.7f3c"        # a pane outside every lane; its session ended
  : >"$asks/scruff.alpha.fresh"
  touch -t 202001010000 "$asks/scruff.session.7f3c"

  cd "$TMP"; wt_run                   # the listing sweeps
  [ "$status" -eq 0 ]
  [ ! -e "$asks/scruff.session.7f3c" ]
  # Today's marker is exactly what the gate is for.
  [ -e "$asks/scruff.alpha.fresh" ]
}

@test "reap: keeps a landed checkout that a pane is still standing in" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" busy)"
  git -C "$main" merge -q --no-edit worktree-busy
  export FAKE_LSOF_CWDS="$dir"
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"kept busy (alpha) — something is standing in the checkout"* ]]
  [ -e "$dir/.git" ]
}

@test "reap: an occupied lane NAMES the process, because lsof sees a cwd not a pane" {
  # The bug this exists for: a lane was kept with "a pane is open in it" and
  # there was no window anywhere on the machine. lsof does not observe panes,
  # it observes cwds — a dev server, a watcher, a telemetry daemon orphaned to
  # pid 1 days ago all pin a lane exactly as hard as a live agent, and read
  # identically once the pid is discarded. The verdict is unchanged (occupied
  # ⇒ keep); what has to change is that the evidence survives the sweep.
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" haunted)"
  git -C "$main" merge -q --no-edit worktree-haunted
  export FAKE_LSOF_CWDS="$dir/node_modules/next" FAKE_LSOF_CMD=node
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"pid 4001 node"* ]] || fail "the refusal named no process: $output"
  # A cwd DEEPER than the checkout says which subdirectory, which is usually
  # the whole diagnosis.
  [[ "$output" == *"in node_modules/next"* ]] || fail "$output"
  [[ "$output" == *"$dir"* ]] || fail "the refusal named no checkout to go and look at: $output"
  [ -e "$dir/.git" ]
}

@test "reap: an occupied lane exposes its holders in --json" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" watched)"
  export FAKE_LSOF_CWDS="$dir" FAKE_LSOF_CMD=node
  cd "$TMP"; wt_run list --json
  [ "$status" -eq 0 ]
  [[ "$output" == *'"occupied": true'* ]] || fail "$output"
  [[ "$output" == *'"pid": 4001'* ]] || fail "$output"
  [[ "$output" == *'"command": "node"'* ]] || fail "$output"
  [[ "$output" == *'"via": "lsof"'* ]] || fail "$output"
}

@test "reap: an unoccupied lane carries no occupied_by key at all" {
  # occupied_by is an ADDITION to a frozen envelope. Omitted when empty, so a
  # consumer that never learns the key sees exactly what it saw before.
  local main; main="$(mkrepo alpha)"; mkwt "$main" quiet >/dev/null
  cd "$TMP"; wt_run list --json
  [ "$status" -eq 0 ]
  [[ "$output" == *'"occupied": false'* ]] || fail "$output"
  [[ "$output" != *"occupied_by"* ]] || fail "an empty holder list must not appear: $output"
}

@test "list --json: a lane that never committed reads as fresh, not merged" {
  # The bug: a lane cut from main seconds ago is trivially an ancestor of main,
  # so the ancestry rung called it landed and every consumer that renders a
  # verdict — haus's paw pill among them — labelled a brand-new lane
  # `merged`. "Nothing has happened here yet" is its own state.
  local main; main="$(mkrepo alpha)"; hook_create "$main" brandnew >/dev/null
  cd "$TMP"; wt_run list --json
  [ "$status" -eq 0 ]
  [[ "$output" == *'"verdict": "fresh"'* ]] || fail "$output"
  [[ "$output" == *'"via": "never-diverged"'* ]] || fail "$output"
}

@test "list --json: a branch whose commits really landed still reads as yes" {
  # The other half of the same rule: `fresh` must never swallow a real landing.
  # This one merged by fast-forward, so it has no commits of its own left to
  # count either — only its reflog remembers that it ever did anything.
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" done)"
  git -C "$main" merge -q --ff-only worktree-done
  cd "$TMP"; wt_run list --json
  [ "$status" -eq 0 ]
  [[ "$output" == *'"verdict": "yes"'* ]] || fail "$output"
  [[ "$output" == *'"via": "ancestry"'* ]] || fail "$output"
}

@test "list --json: work that arrived by cherry-pick is not 'fresh' either" {
  # The trap this test exists for: `commit:` is NOT every way git spells "this
  # branch did something". cherry-pick, revert, rebase and reset each write
  # their own reflog subject, so a rule that hunts for `commit…` calls all of
  # them fresh — this bug pointing the other way, a landed lane losing its
  # verdict. The rule is inverted instead: nothing but `branch: Created from`.
  local main dir; main="$(mkrepo alpha)"
  git -C "$main" checkout -q -b source
  commit_in "$main" side.txt "a commit made somewhere else"
  git -C "$main" checkout -q main
  dir="$(hook_create "$main" picked)"
  git -C "$dir" -c commit.gpgsign=false cherry-pick "$(git -C "$main" rev-parse source)" >/dev/null
  git -C "$main" merge -q --ff-only worktree-picked   # …and that is how it landed
  cd "$TMP"; wt_run list --json
  [ "$status" -eq 0 ]
  [[ "$output" != *'"verdict": "fresh"'* ]] || fail "a cherry-picked lane read as fresh: $output"
}

@test "list --json: chat names the pane a spawned lane resumes into, not its own checkout" {
  # `chat` is what a picker filters on to hide lanes with no pane of their own.
  # `parent` cannot answer that: a lane opened from inside another lane's pane
  # is parented to it exactly as a `scruff child` is, and it HAS a pane.
  local main sub parent child
  main="$(mkrepo alpha)"; sub="$(mkrepo beta)"
  parent="$(mkwt "$main" workshop)"
  mkdir -p "$HOME/.claude/projects/$(printf '%s' "$parent" | tr './' '--')"
  cd "$parent"; child="$("$WT" child "$sub" workshop 2>/dev/null)"
  cd "$TMP"; wt_run list --json
  [ "$status" -eq 0 ]
  [[ "$output" == *"\"path\": \"$child\""* ]] || fail "the spawned lane is missing: $output"
  [[ "$output" != *"\"chat\": \"$child\""* ]] \
    || fail "the spawned lane claims a chat of its own: $output"
  # Twice: the parent's chat is its own checkout, and the child's is the parent's.
  [ "$(printf '%s\n' "$output" | grep -c "\"chat\": \"$parent\"")" -eq 2 ] \
    || fail "chat should name \$parent for both lanes: $output"
}

@test "list --json: chat is EMPTY, not a guess, for a client scruff cannot probe" {
  # The trap `chat` exists to avoid, pointed the other way. resume must always
  # name a directory, so for codex/opencode it falls back to the parent — a
  # sensible guess when the next step is exec-ing a picker. PUBLISHED, that
  # guess says "no chat of my own" about a lane opened from inside another
  # pane, which has a window and an agent in it, and a picker filtering on the
  # field would hide a running agent. Undetermined is the only honest answer.
  local main sub parent child
  main="$(mkrepo alpha)"; sub="$(mkrepo beta)"
  parent="$(mkwt "$main" workshop)"
  mkdir -p "$HOME/.claude/projects/$(printf '%s' "$parent" | tr './' '--')"
  cd "$parent"; child="$("$WT" child "$sub" workshop 2>/dev/null)"

  # Claude can be probed, so the spawned lane answers with the parent's path.
  cd "$TMP"; wt_run list --json
  [ "$status" -eq 0 ]
  [ "$(printf '%s\n' "$output" | grep -c "\"chat\": \"$parent\"")" -eq 2 ] || fail "$output"

  # The same lane under a client whose transcripts scruff cannot see. `child`
  # is Claude Code's own hook, so the column is rewritten in place.
  awk -F'\t' -v OFS='\t' -v p="$child" '$4 == p { $6 = "codex" } 1' "$REG" >"$REG.new"
  mv "$REG.new" "$REG"
  cd "$TMP"; wt_run list --json
  [ "$status" -eq 0 ]
  [[ "$output" == *'"agent": "codex"'* ]] || fail "the client column did not change: $output"
  [ "$(printf '%s\n' "$output" | grep -c '"chat": ""')" -eq 1 ] \
    || fail "an unprobeable client must answer \"\", not a parent path: $output"

  # …and resume still gets its guess, which is a different question.
  wt_run resume beta/workshop
  [[ "$output" == *"spawned from a pane in $parent"* ]] || fail "resume lost its fallback: $output"
}

@test "list: a same-repo lane from another lane's pane is a SIBLING; only the cross-repo child nests" {
  # Both are parented to the pane that spawned them — `parent` is the cwd that
  # made the lane, and cannot tell the two apart (SPEC.md §2.2). Only the
  # cross-repo one is subordinate. A sibling has its own window and its own
  # branch off the SAME main, so filing it under whoever pressed the key buries
  # it under an unrelated task and eats the room a capped consumer keeps for
  # real children.
  local main sub lane sib
  main="$(mkrepo alpha)"; sub="$(mkrepo beta)"
  lane="$(mkwt "$main" opener)"
  shim_agent claude
  sib="$(cd "$lane" && "$WT" new sibling 2>/dev/null | tail -1)"
  cd "$lane"; "$WT" child "$sub" crossed >/dev/null 2>&1
  # The registry still RECORDS the spawning pane for both: the fix is what the
  # listing DOES with `parent`, never what goes in it.
  [ "$(awk -F'\t' -v p="$sib" '$4==p{print $5}' "$REG")" = "$lane" ] \
    || fail "the sibling lost its parent record: $(cat "$REG")"
  cd "$TMP"; wt_run list
  [ "$status" -eq 0 ]
  [[ "$output" == *"└ crossed"* ]] || fail "the cross-repo child must still nest: $output"
  [[ "$output" != *"└ sibling"* ]] || fail "a same-repo lane must not nest: $output"
}

@test "list: a spawned lane is drawn UNDER its parent, never dropped from the table" {
  # Its branch and its PR are its own, and closing the parent's pane does not
  # reap it — so the listing is where it has to stay visible. Nesting is the
  # answer to the noise, not omission.
  local main sub parent
  main="$(mkrepo alpha)"; sub="$(mkrepo beta)"
  parent="$(mkwt "$main" workshop)"
  cd "$parent"; "$WT" child "$sub" workshop >/dev/null 2>&1
  cd "$TMP"; wt_run list
  [ "$status" -eq 0 ]
  [[ "$output" == *"└ workshop"* ]] || fail "the spawned lane is not marked: $output"
  [ "$(printf '%s\n' "$output" | grep -n '^ *alpha' | cut -d: -f1)" \
    -lt "$(printf '%s\n' "$output" | grep -n '^ *beta' | cut -d: -f1)" ] \
    || fail "the spawned lane must follow the lane that spawned it: $output"
}

@test "reap: a fresh lane is still reapable — the new verdict is a label only" {
  # `fresh` splits what a reader is TOLD, never what the sweep does: there is
  # nothing on a never-committed branch to lose, exactly as before.
  local main dir; main="$(mkrepo alpha)"; dir="$(hook_create "$main" nothing)"
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped nothing (alpha)"* ]] || fail "$output"
  [ ! -e "$dir" ]
  [ "$(reg_rows)" -eq 0 ]
}

@test "reap: keeps a landed checkout with uncommitted changes" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" messy)"
  git -C "$main" merge -q --no-edit worktree-messy
  echo edit >"$dir/README.md"
  cd "$TMP"; wt_run reap
  [ -e "$dir/.git" ]
  git -C "$main" show-ref -q --verify refs/heads/worktree-messy
}

@test "reap: a dirty lane NAMES what is in the way instead of going quiet" {
  # The lane that prompted this was landed, unoccupied, and held back by a
  # single untracked directory a tool had dropped in it. Reap said nothing
  # about it at all and closed with the generic three-reason line, so the
  # lane read as one scruff had simply forgotten about.
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" fossil)"
  git -C "$main" merge -q --no-edit worktree-fossil
  mkdir -p "$dir/live"; echo junk >"$dir/live/reaped.log"
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"kept fossil (alpha)"* ]] || fail "the dirty lane went unnamed: $output"
  [[ "$output" == *"live/"* ]] || fail "the note didn't name the path in the way: $output"
  [[ "$output" == *"$dir"* ]] || fail "the note didn't say where to go look: $output"
  # And the generic line must NOT also fire — it reads as a second verdict.
  [[ "$output" != *"nothing to reap"* ]] || fail "the abstract line contradicted the concrete one: $output"
  [ -e "$dir/.git" ]
}

@test "reap: a dirty lane's note caps the paths it lists" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" littered)"
  git -C "$main" merge -q --no-edit worktree-littered
  local i; for i in 1 2 3 4 5; do echo x >"$dir/stray$i.txt"; done
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"+2 more"* ]] || fail "an uncapped note would be a screenful: $output"
}

@test "reap: keeps an unmerged checkout" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" unmerged)"
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"nothing to reap"* ]]
  [ -e "$dir/.git" ]
}

@test "reap: never removes the checkout it is being run from" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" self)"
  git -C "$main" merge -q --no-edit worktree-self
  cd "$dir"; wt_run reap
  [ -e "$dir/.git" ]
  git -C "$main" show-ref -q --verify refs/heads/worktree-self
}

@test "reap: without a usable lsof it degrades to parked-only and says so" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" cautious)"
  git -C "$main" merge -q --no-edit worktree-cautious
  export FAKE_LSOF_BROKEN=1
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"no lsof"* ]]
  [ -e "$dir/.git" ]     # a live checkout is never guessed at
}

@test "reap: a squash-merged branch is recognized via its merged PR" {
  local main dir tip; main="$(mkrepo alpha)"; dir="$(mkwt "$main" squashed)"
  tip="$(git -C "$dir" rev-parse HEAD)"
  git -C "$main" merge -q --squash worktree-squashed && git -C "$main" commit -qm "squash merge"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$tip"
  cd "$TMP"; wt_run reap
  [[ "$output" == *"reaped squashed (alpha)"* ]]
}

@test "reap: a merged PR whose SHA no longer matches the tip is left alone" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" ahead)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)"
  commit_in "$dir" post.txt "un-landed work"
  cd "$TMP"; wt_run reap
  [ -e "$dir/.git" ]
  git -C "$main" show-ref -q --verify refs/heads/worktree-ahead
}

# ── the branch that outran its merged PR ─────────────────────────────────────
# The sweep has always KEPT these (the test above), which is right — and said
# nothing about them, which is how they went unnoticed: the PR reads merged
# everywhere you look, so a worktree sitting on un-shipped commits is
# indistinguishable from one still in flight. These three pin the naming.

@test "reap: names the branch whose PR merged but whose tip moved on" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" outran)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-outran
  commit_in "$dir" post.txt "work done after the PR merged"
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"kept outran (alpha) — merged PR #12, 1 commit(s) since"* ]] \
    || fail "reap kept the branch but never said why: $output"
  [[ "$output" == *"scruff reship outran"* ]] || fail "reap named no way out of it"
}

# A second checkout of the same branch name pushed a merge before this one ever
# pulled: the local tip is real work, just not work built on top of what
# actually merged. Same nonzero commit count as "outran", opposite remedy —
# reshipping THIS tip would push content the merge already superseded.
@test "reap: names a branch whose merged PR the tip never built on, not 'outran'" {
  local main dir stale; main="$(mkrepo alpha)"; dir="$(mkwt "$main" diverged)"
  commit_in "$main" elsewhere.txt "landed via a different checkout entirely"
  stale="$(git -C "$main" rev-parse HEAD)"   # not reachable from worktree-diverged
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$stale" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-diverged
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"kept diverged (alpha) — merged PR #12, but the tip isn't built on what merged"* ]] \
    || fail "reap kept the branch but blamed the wrong cause: $output"
  [[ "$output" != *"scruff reship diverged"* ]] \
    || fail "reap pointed a diverged (stale) branch at reship, which would push it: $output"
  git -C "$main" show-ref -q --verify refs/heads/worktree-diverged \
    || fail "the branch was deleted despite Landed() correctly saying no"
}

# A squash merge puts the branch's content on main under a NEW sha, and the
# lane then rebases onto main and keeps working — the single most ordinary
# shape there is. Ancestry against the PR's pre-squash tip says "sideways", and
# the remedy for sideways is DELETE THE CHECKOUT, so this misread cost real
# commits. What settles it is that main is an ancestor of the tip.
@test "reap: a lane rebased past its squash-merged PR is 'outran', not diverged" {
  local main dir squashed; main="$(mkrepo alpha)"; dir="$(mkwt "$main" rebased)"
  squashed="$(git -C "$dir" rev-parse HEAD)"   # what the PR was opened at
  # main squash-merges it: same content, a sha the branch has never seen.
  git -C "$main" merge -q --squash worktree-rebased
  git -C "$main" commit -qm "squash merge of #12"
  # the lane rebases onto main, dropping its own copies, then keeps working
  git -C "$dir" rebase -q main
  commit_in "$dir" post.txt "work done after the PR merged"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$squashed" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-rebased
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"kept rebased (alpha) — merged PR #12, 1 commit(s) since"* ]] \
    || fail "a rebased-past-a-squash lane was not read as new work: $output"
  [[ "$output" != *"isn't built on what merged"* ]] \
    || fail "reap told a correctly rebased lane to delete itself: $output"
  [ -e "$dir/.git" ]
}

# The +N marker promises "commits no PR covers". It read merged PRs only, so
# the follow-up PR `scruff reship` had just opened was invisible to it and the
# lane went on demanding a reship that had already happened.
@test "list: an open PR at the tip clears the +N marker" {
  local main dir tip; main="$(mkrepo alpha)"; dir="$(mkwt "$main" shipped)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-shipped
  commit_in "$dir" post.txt "work done after the PR merged"
  tip="$(git -C "$dir" rev-parse HEAD)"
  cd "$TMP"; wt_run
  [[ "$output" == *"live+1"* ]] || fail "the un-shipped commit was not marked: $output"
  # …now reship it: an open PR stands at exactly that tip.
  export FAKE_GH_OPEN_BRANCH=worktree-shipped FAKE_GH_OPEN_OID="$tip" FAKE_GH_OPEN_PR=13
  rm -rf "$CLAUDE_WT_BASE/.cache"
  cd "$TMP"; wt_run
  [[ "$output" != *"live+1"* ]] \
    || fail "the marker still demanded a reship the open PR already covers: $output"
}

# …but only at the tip. A commit made after the push is genuinely uncovered,
# and that is the whole case the marker exists for.
@test "list: a commit made after the open PR's tip is still marked +N" {
  local main dir tip; main="$(mkrepo alpha)"; dir="$(mkwt "$main" ahead-of-pr)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-ahead-of-pr
  commit_in "$dir" post.txt "pushed, and the PR covers it"
  tip="$(git -C "$dir" rev-parse HEAD)"
  commit_in "$dir" post2.txt "committed after the push — nothing covers this"
  export FAKE_GH_OPEN_BRANCH=worktree-ahead-of-pr FAKE_GH_OPEN_OID="$tip" FAKE_GH_OPEN_PR=13
  cd "$TMP"; wt_run
  [[ "$output" == *"live+2"* ]] \
    || fail "an open PR behind the tip silenced the marker anyway: $output"
}

@test "list: a branch that outran its merged PR is marked +N" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" outran)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-outran
  commit_in "$dir" post.txt "one"
  commit_in "$dir" post2.txt "two"
  cd "$TMP"; wt_run
  [ "$status" -eq 0 ]
  [[ "$output" == *"live+2"* ]] || fail "the state column hid the un-shipped commits: $output"
  [[ "$output" == *"scruff reship"* ]] || fail "the +N marker was printed with no legend"
}

@test "list: a branch diverged from its merged PR is marked ~N, not +N" {
  local main dir stale; main="$(mkrepo alpha)"; dir="$(mkwt "$main" diverged)"
  commit_in "$main" elsewhere.txt "landed via a different checkout entirely"
  stale="$(git -C "$main" rev-parse HEAD)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$stale" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-diverged
  cd "$TMP"; wt_run
  [ "$status" -eq 0 ]
  [[ "$output" == *"live~1"* ]] || fail "a diverged tip was marked as though it outran its PR: $output"
  [[ "$output" != *"live+1"* ]] || fail "diverged and outran share a marker: $output"
  [[ "$output" == *"remove the checkout instead of reshipping"* ]] \
    || fail "the ~N marker was printed with no legend, or the wrong one: $output"
}

@test "list: a branch whose post-merge commits ALSO landed is not marked" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" landed)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-landed
  commit_in "$dir" post.txt "more"
  git -C "$main" merge -q --no-edit worktree-landed     # …and that landed too
  cd "$TMP"; wt_run
  [[ "$output" != *"+1"* ]] || fail "a branch fully in main was flagged as un-shipped: $output"
}

@test "list: +N counts THIS lane's un-shipped commits, not main's that it caught up on" {
  # The marker promises "commits no PR covers". A long-lived lane keeps catching
  # up on the default branch, and every commit that ride brings along is
  # reachable from the tip but NOT from the merged head — so a bare
  # `head..branch` bills other people's already-landed work to this lane. A real
  # one read `live+131` for two commits of its own, which reads as "unreviewable,
  # deal with it later" instead of "one PR, two commits".
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" longlived)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-longlived
  commit_in "$main" one.txt "someone else's work"      # three other PRs land
  commit_in "$main" two.txt "and more"
  commit_in "$main" three.txt "and more still"
  git -C "$dir" rebase -q main                          # the lane catches up
  commit_in "$dir" mine.txt "the commit that really is un-shipped"
  cd "$TMP"; wt_run
  [[ "$output" == *"live+2"* ]] \
    || fail "the marker billed main's landed commits to this lane: $output"
}

@test "list: the +N count agrees with the commit list reship would put in the PR" {
  # `reshipBody` already lists `base..branch`, so before this agreed, `reship`
  # announced "131 commit(s) past the merge" and opened a PR whose body listed
  # two. Same lane, same moment, two numbers — the marker is what has to move.
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" agrees)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-agrees
  commit_in "$main" one.txt "someone else's work"
  commit_in "$main" two.txt "and more"
  git -C "$dir" rebase -q main
  commit_in "$dir" mine.txt "un-shipped"
  local listed; listed="$(git -C "$main" rev-list --count main..worktree-agrees)"
  cd "$TMP"; wt_run
  [[ "$output" == *"live+$listed"* ]] \
    || fail "the marker says something other than the $listed commit(s) reship would list: $output"
}

# ── the name is not the lane ─────────────────────────────────────────────────
#
# The forge answers about a branch NAME, and scruff coins lane names from a small
# word list while a task name gets reused outright — one repo's
# `worktree-continue-factory-docs` has carried seven PRs. So a lane cut this
# morning inherited the reaped lane's merged PR, and every commit it made of its
# own was counted against a merge nobody here performed.

@test "list: a merged PR that closed before this lane existed is somebody else's" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" reused)"
  # The previous lane's tip: an OID this checkout has never heard of (its
  # objects went with it), merged long before this branch was cut.
  export FAKE_GH_MERGED=1 FAKE_GH_PR=12 FAKE_GH_BRANCH=worktree-reused
  export FAKE_GH_OID=0000000000000000000000000000000000000001
  export FAKE_GH_MERGED_AT=2020-01-01T00:00:00Z
  commit_in "$dir" post.txt "this lane's own work, covered by no PR of its own"
  cd "$TMP"; wt_run
  [ "$status" -eq 0 ]
  [[ "$output" != *"live+"* ]] || fail "a reaped lane's PR stuck to the lane wearing its name: $output"
  [[ "$output" != *"live~"* ]] || fail "the same PR came back as a diverged tip: $output"
  [[ "$output" == *"live"* ]] || fail "the row itself went missing: $output"
}

@test "list: a merged PR that closed AFTER the lane was cut is kept, unreachable SHA and all" {
  # The direction that must not over-fire. A lane that merged and then rebased
  # has a head SHA that is no longer reachable either, and that PR is very much
  # its own — only a PR predating the branch belongs to somebody else.
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" mine)"
  export FAKE_GH_MERGED=1 FAKE_GH_PR=12 FAKE_GH_BRANCH=worktree-mine
  export FAKE_GH_OID=0000000000000000000000000000000000000001
  export FAKE_GH_MERGED_AT=2099-01-01T00:00:00Z
  cd "$TMP"; wt_run
  # A marker at all is the assertion: a branch with no merged PR gets a bare
  # `live` cell (see "an ordinary in-flight branch", below), so `+1` is the PR
  # surviving the gate. Which marker it earns is the pre-existing outran /
  # diverged question and is settled elsewhere.
  [[ "$output" == *"live+1"* ]] || fail "this lane's own PR was dropped as somebody else's: $output"
}

@test "list: a lane with no reflog dates itself by its own oldest commit" {
  # Reflogs can be off (core.logAllRefUpdates=false) or aged out by gc, and the
  # gate still has to be able to fire: the commits a lane carries of its own are
  # the next-best "this branch did not exist before then".
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" nolog)"
  rm -f "$main/.git/logs/refs/heads/worktree-nolog"
  export FAKE_GH_MERGED=1 FAKE_GH_PR=12 FAKE_GH_BRANCH=worktree-nolog
  export FAKE_GH_OID=0000000000000000000000000000000000000001
  export FAKE_GH_MERGED_AT=2020-01-01T00:00:00Z
  cd "$TMP"; wt_run
  [[ "$output" != *"live~"* ]] || fail "with no reflog the stale PR came back: $output"
  [[ "$output" != *"live+"* ]] || fail "with no reflog the stale PR came back: $output"
}

@test "list: with no reflog, a lane that rebased after its merge keeps its own PR" {
  # The no-reflog fallback's boundary, in the direction that costs something. A
  # rebase rewrites every COMMITTER date to now, so dating the branch by those
  # would put its birth after its own merge and drop the PR that is genuinely
  # its own. Author dates survive the rebase, so they are what the fallback reads.
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" rebased)"
  git -C "$dir" -c commit.gpgsign=false commit -q --amend --no-edit \
    --date="2020-01-01T00:00:00Z"                        # author then, committer now
  rm -f "$main/.git/logs/refs/heads/worktree-rebased"    # …after the amend rewrote it
  export FAKE_GH_MERGED=1 FAKE_GH_PR=12 FAKE_GH_BRANCH=worktree-rebased
  export FAKE_GH_OID=0000000000000000000000000000000000000001
  export FAKE_GH_MERGED_AT=2020-06-01T00:00:00Z
  cd "$TMP"; wt_run
  [[ "$output" == *"live+1"* ]] || fail "the lane's own PR was dropped as somebody else's: $output"
}

@test "list: a merged PR reachable from the branch is this lane's, whatever the dates say" {
  # Ancestry is asked FIRST and settles it alone: the PR's head SHA being
  # reachable means this branch is what the PR was opened from. A forge clock
  # that disagrees with this Mac's must never be able to override that.
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" ancestry)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-ancestry FAKE_GH_MERGED_AT=1999-01-01T00:00:00Z
  commit_in "$dir" post.txt "after the merge"
  cd "$TMP"; wt_run
  [[ "$output" == *"live+1"* ]] || fail "an impossible date beat reachable ancestry: $output"
}

@test "reap: a PR closed unmerged before this lane existed is not its dead end" {
  # The same defect on the other forge question. "Nothing is going to land these
  # commits" is the worst thing scruff can say about work no one has reviewed
  # once, and `scruff drop` is what it says next.
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" innocent)"
  export FAKE_GH_CLOSED_PR=43
  export FAKE_GH_CLOSED_OID=0000000000000000000000000000000000000001
  export FAKE_GH_CLOSED_AT=2020-01-01T00:00:00Z
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" != *"closed unmerged"* ]] \
    || fail "a fresh lane inherited the last lane's rejection: $output"
  git -C "$main" show-ref -q --verify refs/heads/worktree-innocent
}

@test "list: an ordinary in-flight branch keeps a bare state column" {
  local main; main="$(mkrepo alpha)"; mkwt "$main" plain >/dev/null
  cd "$TMP"; wt_run
  [[ "$output" == *"live"* ]]
  [[ "$output" != *"live+"* ]] || fail "a branch with no merged PR was marked as outrunning one"
  [[ "$output" != *"reship"* ]] || fail "the legend printed on a listing that earned no marker"
}

@test "reap: 'landed' means landed on the DEFAULT branch, not whatever main has checked out" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" sidequest)"
  git -C "$main" checkout -qb detour
  git -C "$main" merge -q --no-edit worktree-sidequest   # landed on `detour`, NOT on main
  cd "$TMP"; wt_run reap
  git -C "$main" show-ref -q --verify refs/heads/worktree-sidequest
}

# ── dead ends: PR closed, repo archived ──────────────────────────────────────

@test "reap: a lane whose PR was CLOSED unmerged is named, not swept" {
  local main; main="$(mkrepo alpha)"; mkwt "$main" rejected >/dev/null
  export FAKE_GH_CLOSED_PR=43
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"PR #43 was closed unmerged"* ]] \
    || fail "a lane nothing will ever land said nothing about it: $output"
  [[ "$output" == *"scruff drop rejected"* ]] || fail "reap named no way out of it: $output"
  # The commits were REJECTED, not landed. An automatic sweep must never take them.
  git -C "$main" show-ref -q --verify refs/heads/worktree-rejected \
    || fail "reap deleted unlanded work because a PR was closed"
}

@test "reap: a lane in an ARCHIVED repo is named, not swept" {
  local main; main="$(mkrepo alpha)"; mkwt "$main" frozen >/dev/null
  export FAKE_GH_ARCHIVED=true
  cd "$TMP"; wt_run reap
  [[ "$output" == *"archived on the forge"* ]] \
    || fail "an unlandable lane in an archived repo said nothing: $output"
  git -C "$main" show-ref -q --verify refs/heads/worktree-frozen \
    || fail "reap deleted work that can no longer be submitted anywhere"
}

@test "reap: a merged PR still reads as reship, never as a dead end" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" both)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-both FAKE_GH_CLOSED_PR=43
  commit_in "$dir" post.txt "after the merge"
  cd "$TMP"; wt_run reap
  [[ "$output" == *"scruff reship both"* ]] || fail "the merged PR lost to the closed one: $output"
  [[ "$output" != *"closed unmerged"* ]] || fail "a branch that DID land was called a dead end: $output"
}

# ── drop + the reap ledger ───────────────────────────────────────────────────

@test "drop: takes an unlanded lane reap won't, and prints the undo" {
  local main; main="$(mkrepo alpha)"; mkwt "$main" doomed >/dev/null
  local sha; sha="$(git -C "$main" rev-parse worktree-doomed)"
  cd "$TMP"; wt_run drop doomed
  [ "$status" -eq 0 ]
  git -C "$main" show-ref -q --verify refs/heads/worktree-doomed \
    && fail "drop left the branch behind"
  [[ "$output" == *"${sha:0:12}"* ]] || fail "drop deleted a branch without printing the SHA back: $output"
  [[ "$output" == *"undo:"* ]] || fail "drop named no way back"
  [ "$(reg_rows)" -eq 0 ] || fail "drop left the registry row behind"
}

@test "drop: refuses a dirty checkout — an unlanded lane's dirt has no PR to fall back on" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" messy)"
  echo scratch >"$dir/uncommitted.txt"
  cd "$TMP"; wt_run drop messy
  [ "$status" -eq 2 ] || fail "expected a refusal (exit 2), got $status: $output"
  git -C "$main" show-ref -q --verify refs/heads/worktree-messy \
    || fail "a refused drop still deleted the branch"
  [ -f "$dir/uncommitted.txt" ] || fail "a refused drop still ate the working tree"
}

@test "drop: refuses a lane a pane is standing in, and names what is standing there" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" busy)"
  export FAKE_LSOF_CWDS="$dir" FAKE_LSOF_CMD=node
  cd "$TMP"; wt_run drop busy
  [ "$status" -eq 2 ] || fail "expected a refusal (exit 2), got $status: $output"
  # "close it first" is useless advice when the occupant is a dev server rather
  # than a window. A drop is a refusal a human must clear, so it has to say what.
  [[ "$output" == *"pid 4001 node"* ]] || fail "the refusal named no process: $output"
  git -C "$main" show-ref -q --verify refs/heads/worktree-busy || fail "drop yanked an occupied lane"
}

@test "drop: an unknown name dies pointing at the listing" {
  mkrepo alpha >/dev/null
  cd "$TMP"; wt_run drop nosuchlane
  [ "$status" -eq 1 ]
  [[ "$output" == *"scruff"* ]] || fail "the refusal named no way to find the real names"
}

@test "reaped: every reap leaves a line saying what died and how to get it back" {
  # The bug this exists for: a lane vanished mid-session and NOTHING could say
  # why — `git branch -D` takes the branch's reflog with it, `git worktree
  # remove` takes the admin dir, and scruff kept no record of its own most
  # destructive act.
  local main; main="$(mkrepo alpha)"; mkwt "$main" gone >/dev/null
  local sha; sha="$(git -C "$main" rev-parse worktree-gone)"
  git -C "$main" merge -q --no-edit worktree-gone     # landed by ancestry
  cd "$TMP"; "$WT" reap >/dev/null 2>&1
  wt_run reaped
  [ "$status" -eq 0 ]
  [[ "$output" == *"alpha/gone"* ]] || fail "the ledger doesn't name the lane: $output"
  [[ "$output" == *"ancestry"* ]] || fail "the ledger doesn't say which rung justified it: $output"
  [[ "$output" == *"${sha:0:12}"* ]] || fail "the ledger kept no SHA, so nothing is recoverable: $output"
  [[ "$output" == *"branch worktree-gone"* ]] || fail "the ledger names no recovery command: $output"
}

@test "reaped: the recorded SHA really does restore the branch" {
  local main; main="$(mkrepo alpha)"; mkwt "$main" undoable >/dev/null
  local sha; sha="$(git -C "$main" rev-parse worktree-undoable)"
  cd "$TMP"; wt_run drop undoable
  git -C "$main" show-ref -q --verify refs/heads/worktree-undoable && fail "drop didn't delete"
  # Read the SHA back out of the ledger, not out of the test's own variable —
  # that is the path a human actually walks.
  local recorded; recorded="$(awk -F'\t' '$3=="undoable"{print $5}' "$XDG_STATE_HOME/scruff/reaped.log")"
  [ "$recorded" = "$sha" ] || fail "ledger SHA $recorded != $sha"
  git -C "$main" branch worktree-undoable "$recorded"
  [ "$(git -C "$main" rev-parse worktree-undoable)" = "$sha" ] || fail "the branch came back wrong"
}

@test "reaped: an empty ledger says so and names its own path" {
  mkrepo alpha >/dev/null
  cd "$TMP"; wt_run reaped
  [ "$status" -eq 0 ]
  [[ "$output" == *"no lanes reaped"* ]] || fail "an empty ledger printed something else: $output"
  [[ "$output" == *"reaped.log"* ]] || fail "an empty ledger didn't say where it would live"
}

@test "reap: is idempotent — a second run finds nothing and changes nothing" {
  local main; main="$(mkrepo alpha)"
  mkwt "$main" twice >/dev/null
  git -C "$main" merge -q --no-edit worktree-twice
  cd "$TMP"; "$WT" reap >/dev/null 2>&1
  wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"nothing to reap"* ]]
}

# ── reship ───────────────────────────────────────────────────────────────────
# The way OUT of the state above. A real remote is needed here (the other tests
# only ever parse origin's URL), so these point origin at a bare repo on disk.

no_pr_created() { # 0 when gh was never asked to open a PR (the log may not exist at all)
  ! grep -q "pr create" "$FAKE_GH_LOG" 2>/dev/null
}

mkremote() { # mkremote <main> — give a repo a bare origin it can actually push to
  local bare="$TMP/remotes/$(basename "$1").git"
  mkdir -p "$(dirname "$bare")"
  git init -q --bare -b main "$bare"
  git -C "$1" remote set-url origin "$bare"
  printf '%s' "$bare"
}

@test "reship: pushes the branch and opens the follow-up PR" {
  local main dir bare; main="$(mkrepo alpha)"; dir="$(mkwt "$main" outran)"
  bare="$(mkremote "$main")"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-outran
  commit_in "$dir" post.txt "work done after the PR merged"
  cd "$TMP"; wt_run reship outran
  [ "$status" -eq 0 ]
  git -C "$bare" show-ref -q --verify refs/heads/worktree-outran \
    || fail "the branch was never pushed, so the follow-up PR would be empty"
  grep -q "pr create" "$FAKE_GH_LOG" || fail "no PR was opened: $output"
  [[ "$output" == *"follow-up PR open"* ]]
}

@test "reship: a PASTED cut cell resolves through the shared matcher" {
  local main dir bare; main="$(mkrepo alpha)"; dir="$(mkwt "$main" outran)"
  bare="$(mkremote "$main")"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-outran
  commit_in "$dir" post.txt "work done after the PR merged"
  cd "$TMP"; wt_run reship "outr…"
  [ "$status" -eq 0 ]
  [[ "$output" == *"matched by prefix"* ]]
  grep -q "pr create" "$FAKE_GH_LOG" || fail "no PR was opened: $output"
}

@test "reship: refuses a diverged tip instead of pushing stale content" {
  local main dir bare stale; main="$(mkrepo alpha)"; dir="$(mkwt "$main" diverged)"
  bare="$(mkremote "$main")"
  commit_in "$main" elsewhere.txt "landed via a different checkout entirely"
  stale="$(git -C "$main" rev-parse HEAD)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$stale" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-diverged
  cd "$TMP"; wt_run reship diverged
  [ "$status" -ne 0 ]
  [[ "$output" == *"does not build on that merged PR"* ]] \
    || fail "reship refused for the wrong reason, or didn't refuse: $output"
  git -C "$bare" show-ref -q --verify refs/heads/worktree-diverged \
    && fail "reship pushed a diverged tip despite refusing"
  no_pr_created || fail "a PR was opened for content the merge already superseded"
}

@test "reship: an already-open PR takes the push and no second PR" {
  local main dir bare; main="$(mkrepo alpha)"; dir="$(mkwt "$main" inflight)"
  bare="$(mkremote "$main")"
  export FAKE_GH_OPEN_URL="https://github.com/acme/alpha/pull/3"
  cd "$TMP"; wt_run reship inflight
  [ "$status" -eq 0 ]
  git -C "$bare" show-ref -q --verify refs/heads/worktree-inflight
  no_pr_created || fail "a second PR was opened over an open one"
  [[ "$output" == *"already covers this branch"* ]]
}

@test "reship: a branch with nothing past main refuses rather than opening an empty PR" {
  local main; main="$(mkrepo alpha)"
  hook_create "$main" empty >/dev/null      # a worktree, no commits of its own
  mkremote "$main" >/dev/null
  cd "$TMP"; wt_run reship empty
  [ "$status" -ne 0 ]
  [[ "$output" == *"nothing the main branch doesn't already have"* ]]
  no_pr_created || fail "an empty PR was opened"
}

@test "reship: an unknown name dies pointing at the listing" {
  mkrepo alpha >/dev/null
  cd "$TMP"; wt_run reship nosuch
  [ "$status" -ne 0 ]
  [[ "$output" == *"no lane named 'nosuch'"* ]]
}

# ── child ────────────────────────────────────────────────────────────────────

@test "child: worktrees another repo and registers THIS pane as the parent" {
  local a b dir; a="$(mkrepo alpha)"; b="$(mkrepo beta)"
  cd "$a"
  run bash -c "cd '$a' && '$WT' child '$b' cross 2>/dev/null"
  [ "$status" -eq 0 ]
  dir="$output"
  [ "$dir" = "$CLAUDE_WT_BASE/beta/cross" ]
  [ "$(git -C "$dir" branch --show-current)" = worktree-cross ]
  # 5th registry field is the spawning cwd — this is what the statusline reads.
  [ "$(awk -F'\t' -v p="$dir" '$4==p{print $5}' "$REG")" = "$a" ]
}

@test "child: defaults the name to this pane's own worktree name" {
  local a b dir; a="$(mkrepo alpha)"; b="$(mkrepo beta)"
  dir="$(mkwt "$a" shared)"
  run bash -c "cd '$dir' && '$WT' child '$b' 2>/dev/null"
  [ "$status" -eq 0 ]
  [ "$output" = "$CLAUDE_WT_BASE/beta/shared" ]
}

@test "child: a pane that is NOT in a lane gets a name, not its repo's" {
  # The cwd's basename is a REPO name — the same string for every child that
  # pane ever spawns — so it is never the default. See Child() in new.go.
  local a b dir; a="$(mkrepo alpha)"; b="$(mkrepo beta)"
  run bash -c "cd '$a' && '$WT' child '$b' 2>/dev/null"
  [ "$status" -eq 0 ]
  dir="$output"
  [ "$dir" != "$CLAUDE_WT_BASE/beta/alpha" ]
  [[ "$(basename "$dir")" =~ ^[a-z]+-[a-z]+$ ]]
}

@test "child: two unnamed children of one non-lane pane both land" {
  local a b one two; a="$(mkrepo alpha)"; b="$(mkrepo beta)"
  one="$(cd "$a" && "$WT" child "$b" 2>/dev/null)"
  two="$(cd "$a" && "$WT" child "$b" 2>/dev/null)"
  [ -d "$one" ] && [ -d "$two" ]
  [ "$one" != "$two" ]
}

@test "child: refuses a name whose branch or path already exists" {
  local a b; a="$(mkrepo alpha)"; b="$(mkrepo beta)"
  "$WT" child "$b" taken >/dev/null 2>&1
  run bash -c "cd '$a' && '$WT' child '$b' taken"
  [ "$status" -ne 0 ]
  [[ "$output" == *"already exists"* ]]
}

@test "child: refuses a path that isn't a repo, and a linked worktree" {
  run "$WT" child "$TMP/nope"
  [ "$status" -ne 0 ]
  [[ "$output" == *"no such directory"* ]]
  mkdir -p "$TMP/plain"
  run "$WT" child "$TMP/plain"
  [ "$status" -ne 0 ]
  [[ "$output" == *"isn't inside a git repo"* ]]
}

@test "child: a resumed child inherits its parent's chat, not an empty picker" {
  local a b dir cdir; a="$(mkrepo alpha)"; b="$(mkrepo beta)"
  dir="$(mkwt "$a" par)"
  mkdir -p "$HOME/.claude/projects/$(printf '%s' "$dir" | sed 's/[/.]/-/g')"
  cdir="$(cd "$dir" && "$WT" child "$b" 2>/dev/null)"
  commit_in "$cdir" c.txt "child work"
  git -C "$b" worktree remove --force "$cdir"
  wt_run resume beta/par
  [ "$status" -eq 0 ]
  [[ "$output" == *"spawned from a pane in $dir"* ]]
  # The parent is a SHARED checkout with many conversations in it, so "the
  # newest one" isn't an answer scruff is entitled to give — this is the one case
  # the picker is right.
  [[ "$output" == *"claude --resume"* ]] || fail "a shared parent needs the picker: $output"
}

# ── spawn ────────────────────────────────────────────────────────────────────

@test "spawn: names the worktree and parents it to the repo, not to a pane" {
  local b dir; b="$(mkrepo beta)"
  # No cd: the palette runs under launchd, from wherever it happens to be.
  run bash -c "'$WT' spawn '$b' fix-the-notch 2>/dev/null"
  [ "$status" -eq 0 ]
  dir="$output"
  [ "$dir" = "$CLAUDE_WT_BASE/beta/fix-the-notch" ]
  [ "$(git -C "$dir" branch --show-current)" = worktree-fix-the-notch ]
  # Parent is the repo's own main checkout — a pane sitting there lists it.
  [ "$(awk -F'\t' -v p="$dir" '$4==p{print $5}' "$REG")" = "$b" ]
}

@test "spawn: a taken name takes the next free suffix instead of dying" {
  local b first second; b="$(mkrepo beta)"
  first="$("$WT" spawn "$b" dupe 2>/dev/null)"
  run bash -c "'$WT' spawn '$b' dupe 2>/dev/null"
  [ "$status" -eq 0 ]
  second="$output"
  [ "$first" = "$CLAUDE_WT_BASE/beta/dupe" ]
  [ "$second" = "$CLAUDE_WT_BASE/beta/dupe-2" ]
  [ "$(git -C "$second" branch --show-current)" = worktree-dupe-2 ]
}

@test "spawn: a free path with a taken BRANCH still skips to a free name" {
  local b; b="$(mkrepo beta)"
  git -C "$b" branch worktree-held
  run bash -c "'$WT' spawn '$b' held 2>/dev/null"
  [ "$status" -eq 0 ]
  [ "$output" = "$CLAUDE_WT_BASE/beta/held-2" ]
}

@test "spawn: records its client, independently of the future default" {
  local b dir; b="$(mkrepo beta)"
  dir="$("$WT" spawn "$b" codex-task codex 2>/dev/null)"
  [ "$(awk -F'\t' -v p="$dir" '$4==p{print $6}' "$REG")" = codex ]
  HAUS_AGENT_DEFAULT=opencode run "$WT" resume codex-task
  [ "$status" -eq 0 ]
  [[ "$output" == *"codex resume"* ]]
  [[ "$output" != *"opencode --continue"* ]]
}

# The fourth client, end to end: the row records it, and `resume` picks pi's
# continue-the-newest rung rather than the machine default's.
@test "new: a pi lane records pi, and reopens with pi --continue" {
  local b dir; b="$(mkrepo beta)"
  shim_agent pi
  cd "$b"
  run "$WT" new pi-task pi
  [ "$status" -eq 0 ]
  dir="$CLAUDE_WT_BASE/beta/pi-task"
  [ "$(awk -F'\t' -v p="$dir" '$4==p{print $6}' "$REG")" = pi ]
  [[ "$output" == *"ran pi"* ]]

  HAUS_AGENT_DEFAULT=claude run "$WT" resume pi-task
  [ "$status" -eq 0 ]
  [[ "$output" == *"pi --continue"* ]]
  [[ "$output" != *"claude --continue"* ]]
}

@test "resume: pre-client registry rows remain Claude worktrees" {
  local main dir; main="$(mkrepo alpha)"; dir="$CLAUDE_WT_BASE/alpha/legacy"
  git -C "$main" branch worktree-legacy
  mkdir -p "$(dirname "$REG")"
  printf 'legacy\t%s\tworktree-legacy\t%s\t%s\n' "$main" "$dir" "$main" >"$REG"
  HAUS_AGENT_DEFAULT=codex run "$WT" resume legacy
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude --continue"* ]]
}

@test "resume: a row naming a retired client reopens in Claude" {
  local main dir; main="$(mkrepo alpha)"; dir="$CLAUDE_WT_BASE/alpha/retired"
  git -C "$main" branch worktree-retired
  mkdir -p "$(dirname "$REG")"
  printf 'retired\t%s\tworktree-retired\t%s\t%s\tjcode\n' "$main" "$dir" "$main" >"$REG"
  HAUS_AGENT_DEFAULT=codex run "$WT" resume retired
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude --continue"* ]]
}

@test "agent start: Codex receives a captured screenshot as an initial image" {
  cat >"$BIN/codex" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@"
EOF
  chmod +x "$BIN/codex"
  local image="$TMP/shot.png"; : >"$image"
  run "$WT" agent start codex --image "$image" -- "inspect this"
  [ "$status" -eq 0 ]
  [ "$output" = "$(printf '%s\n%s\n%s\n%s' -i "$image" -- "inspect this")" ]
}

# A prompt is DATA. Pounce's Spawn Agent box takes a paragraph, and a task typed
# there is very often a markdown list — so its first character is `-`, and a bare
# argv element starting with a dash is a FLAG to every one of these clients. This
# used to kill the pane on `error: unknown option '- …'` before the agent ran.
@test "agent start: a prompt starting with a dash reaches the client as text" {
  local prompt='- update the README
- and its footer'
  for client in claude codex opencode pi; do
    cat >"$BIN/$client" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@"
EOF
    chmod +x "$BIN/$client"
    run "$WT" agent start "$client" -- "$prompt"
    [ "$status" -eq 0 ]
    [[ "$output" == *"- update the README"* ]]
    [[ "$output" == *"- and its footer"* ]]
    # …and it arrived somewhere a parser can no longer read as an option.
    [[ "$output" == *"--"* ]]
  done
}

# ── new (the client-agnostic ⌘C) ─────────────────────────────────────────────
#
# `wt new` is what Super c runs when the default client isn't Claude Code — the
# only client that can make its own worktree. It must land the SAME checkout,
# branch and registry row the create hook does, parented to the pane's cwd, and
# then hand the pane over to the client. A shim client stands in for that exec.

shim_agent() { # shim_agent <name> — a client that prints how it was invoked
  cat >"$BIN/$1" <<EOF
#!/usr/bin/env bash
printf 'ran %s %s\n' "$1" "\$*"
EOF
  chmod +x "$BIN/$1"
}

@test "new: worktree of THIS repo, parented to the pane, then opens the client" {
  local b dir; b="$(mkrepo beta)"
  shim_agent opencode
  cd "$b"
  run "$WT" new notch-fix opencode
  [ "$status" -eq 0 ]
  dir="$CLAUDE_WT_BASE/beta/notch-fix"
  [ -e "$dir/.git" ]
  [ "$(git -C "$dir" branch --show-current)" = worktree-notch-fix ]
  # Parent is the PANE's cwd (as the create hook records it), not the repo, and
  # the row keeps the client so a later `wt notch-fix` reopens opencode.
  [ "$(awk -F'\t' -v p="$dir" '$4==p{print $5}' "$REG")" = "$b" ]
  [ "$(awk -F'\t' -v p="$dir" '$4==p{print $6}' "$REG")" = opencode ]
  [[ "$output" == *"ran opencode"* ]]
}

@test "new: by default it just makes the lane and prints the path" {
  local b dir; b="$(mkrepo beta)"
  shim_agent claude
  cd "$b"
  run "$WT" new quiet-one
  [ "$status" -eq 0 ]
  dir="$CLAUDE_WT_BASE/beta/quiet-one"
  [ -e "$dir/.git" ]
  # Only the path on stdout, so `cd "$(scruff new)"` works — and NO client ran:
  # a lane is a checkout, and what you open in it is your business.
  [ "$(printf '%s\n' "$output" | tail -1)" = "$dir" ]
  [[ "$output" != *"ran claude"* ]]
}

@test "new: --open hands the pane to the client, --cmd to anything else" {
  local b; b="$(mkrepo beta)"
  shim_agent codex
  cd "$b"
  run "$WT" new opened --open codex
  [ "$status" -eq 0 ]
  [[ "$output" == *"ran codex"* ]]

  run "$WT" new scripted --cmd 'echo ran-cmd-in "$PWD"'
  [ "$status" -eq 0 ]
  [[ "$output" == *"ran-cmd-in"* ]]
  [[ "$output" == *"beta/scripted"* ]]

  # The two endings contradict each other; saying both is a typo, not a plan.
  run "$WT" new both --open --cmd 'true'
  [ "$status" -eq 1 ]
  [[ "$output" == *"pick one"* ]]
}

@test "new: an unnamed spawn names itself, and a taken name takes a suffix" {
  local b first second; b="$(mkrepo beta)"
  shim_agent claude
  cd "$b"
  run "$WT" new
  [ "$status" -eq 0 ]
  first="$(awk -F'\t' 'NR==1{print $1}' "$REG")"
  [ -n "$first" ]
  run "$WT" new "$first"          # ask for the taken one on purpose
  [ "$status" -eq 0 ]
  second="$(awk -F'\t' -v n="$first" '$1!=n{print $1}' "$REG")"
  [ "$second" = "$first-2" ]
}

@test "new: outside a repo it refuses rather than leaving a stray worktree" {
  shim_agent claude
  mkdir -p "$TMP/plain"
  cd "$TMP/plain"
  run "$WT" new
  [ "$status" -ne 0 ]
  [[ "$output" == *"not inside a git repo"* ]]
  [ "$(reg_rows)" -eq 0 ]
}

@test "new: an uninstalled client is named, and the checkout survives to resume" {
  local b; b="$(mkrepo beta)"
  cd "$b"
  # "codex is not installed" has to be SIMULATED, and simulating it takes both
  # halves below, because either one alone is defeated by the other mechanism:
  #
  #   SCRUFF_PATH_RESCUE=0  — scruff appends a bare-PATH rescue for the hook case
  #                         (see internal/commands/path.go), and that rescue
  #                         re-adds the profile bindir a real codex lives in.
  #   PATH=$BIN:onlygit   — and the caller's PATH has to lose codex too, via a
  #                         directory holding nothing but a git symlink. Narrowing
  #                         to git's OWN directory is not enough: on a Nix box git
  #                         and codex share one profile bindir.
  #
  # Without both, this test passes in CI (no codex there) and silently fails on
  # any machine that has one — which is exactly what it did, in this suite and in
  # haus's copy, for as long as it existed.
  mkdir -p "$TMP/onlygit"
  ln -sf "$(command -v git)" "$TMP/onlygit/git"
  run env SCRUFF_PATH_RESCUE=0 PATH="$BIN:$TMP/onlygit" "$WT" new stranded codex
  [ "$status" -ne 0 ]
  [[ "$output" == *"codex is unavailable"* ]]
  [ -e "$CLAUDE_WT_BASE/beta/stranded/.git" ]
}

@test "spawn: refuses a missing path, a non-repo, and a missing name" {
  local b; b="$(mkrepo beta)"
  run "$WT" spawn "$TMP/nope" x
  [ "$status" -ne 0 ]
  [[ "$output" == *"no such directory"* ]]
  mkdir -p "$TMP/plain"
  run "$WT" spawn "$TMP/plain" x
  [ "$status" -ne 0 ]
  [[ "$output" == *"isn't inside a git repo"* ]]
  run "$WT" spawn "$b"
  [ "$status" -ne 0 ]
  [[ "$output" == *"usage: scruff spawn"* ]]
}

# ── dangling checkouts (husks) ───────────────────────────────────────────────
#
# `git worktree remove` deletes the repo's admin dir (.git/worktrees/<id>) BEFORE
# it deletes the working tree. When that second half fails — an ignored build dir
# it cannot unlink, a file another process holds — what's left is a directory
# whose .git file points at a gitdir that no longer exists. `[ -e "$wt/.git" ]`
# says "live"; every git command run inside says "fatal: not a git repository".
#
# husk() reproduces exactly that end state, which is all any caller can observe.

husk() { # husk <main> <checkout> — leave <checkout> on disk, unregistered
  local id; id="$(basename "$2")"
  rm -rf "$1/.git/worktrees/$id"
}

@test "husk: a checkout git has disowned lists as 'stray', not 'live'" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" ghosted)"
  husk "$main" "$dir"
  wt_run
  [ "$status" -eq 0 ]
  [[ "$output" == *ghosted* ]]
  # The whole point: the old `-e .git` test called this live, so the row lied and
  # `wt ghosted` refused to rebuild it — the branch was unreachable through wt.
  [[ "$output" != *"ghosted"*"live"* ]]
  [[ "$output" == *stray* ]]
}

@test "husk: the listing says what to do about it, and the sweep spares the branch" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" ghosted)"
  # Landed AND merged-by-PR: every reason the sweep has to reap, so the only thing
  # keeping the branch alive is the husk rule itself.
  git -C "$main" merge -q --no-ff -m merge worktree-ghosted
  husk "$main" "$dir"
  FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$main" rev-parse worktree-ghosted)" wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *dangling* ]]
  git -C "$main" show-ref -q --verify refs/heads/worktree-ghosted \
    || fail "the branch was reaped while its checkout was a husk — the husk's uncommitted files are now referenced by nothing"
  [ -d "$dir" ] || fail "the husk directory was deleted"
}

@test "husk: resume moves it aside — never deletes it — and rebuilds the checkout" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" ghosted)"
  # An edit that exists ONLY here: not committed, not on the branch. This is the
  # thing a husk can be holding, and the reason it is moved rather than removed.
  echo "only-copy" >"$dir/unsaved.txt"
  husk "$main" "$dir"
  wt_run resume ghosted
  [ "$status" -eq 0 ]
  # Rebuilt, and git can read it again.
  git -C "$dir" rev-parse --git-dir >/dev/null 2>&1 \
    || fail "resume left the husk in place instead of rebuilding the checkout"
  [ "$(git -C "$dir" branch --show-current)" = worktree-ghosted ]
  # And the old contents survive beside it, named in the output.
  local moved; moved="$(echo "$dir".stray-*)"
  [ -f "$moved/unsaved.txt" ] || fail "the husk's only copy of unsaved.txt is gone"
  [[ "$output" == *"$moved"* ]]
}

# A git whose `worktree remove` fails the way the real one does when the delete
# breaks down half-way: admin dir gone, working tree still standing, non-zero
# exit. Everything else passes straight through to the real git. Scoped to the
# one test that needs it — PATH is shim-first, and wt appends its rescue path
# precisely so a shim wins.
git_husk_shim() { # git_husk_shim — echoes a dir to put at the front of PATH
  local shim="$TMP/gitshim"
  mkdir -p "$shim"
  cat >"$shim/git" <<EOF
#!/usr/bin/env bash
if [ "\$3" = worktree ] && [ "\$4" = remove ]; then
  for a in "\$@"; do last="\$a"; done
  rm -rf "\$2/.git/worktrees/\$(basename "\$last")"
  exit 1
fi
exec $(command -v git) "\$@"
EOF
  chmod +x "$shim/git"
  printf '%s' "$shim"
}

@test "husk: the remove hook finishes the deletion git abandoned" {
  local main dir shim; main="$(mkrepo alpha)"; dir="$(mkwt "$main" messy)"
  echo "edit" >>"$dir/work.txt"          # dirty, so the wip-commit path runs too
  shim="$(git_husk_shim)"
  PATH="$shim:$PATH" hook_remove "$dir"
  # The uncommitted edit went to a wip commit as always, so nothing on disk was
  # irreplaceable — and only then is the hook allowed to finish what git started.
  [ ! -e "$dir" ] || fail "the hook left a husk at $dir; it would read 'live' forever and freeze the statusline"
  git -C "$main" show-ref -q --verify refs/heads/worktree-messy \
    || fail "the branch was dropped along with the residue"
  [[ "$(git -C "$main" log -1 --format=%s worktree-messy)" == wip:* ]] \
    || fail "the dirty tree wasn't parked before the checkout was deleted"
}

@test "husk: residue nothing can delete is reported, not died on" {
  [ "$(id -u)" != 0 ] || skip "root ignores the directory permissions this test uses"
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" stubborn)"
  # An IGNORED directory that cannot be unlinked (no write permission on its
  # parent) defeats git's delete — and ours. The contract is that the hook still
  # exits cleanly, keeps the branch, and names what it left behind.
  mkdir -p "$dir/scratch"
  echo build >"$dir/scratch/out.o"
  echo scratch/ >"$dir/.gitignore"
  git -C "$dir" add -A
  git -C "$dir" -c commit.gpgsign=false commit -qm ignore
  chmod 555 "$dir/scratch"
  run hook_remove "$dir"
  chmod 755 "$dir/scratch" 2>/dev/null || true
  [ "$status" -eq 0 ] || fail "the remove hook died on a checkout it couldn't delete"
  git -C "$main" show-ref -q --verify refs/heads/worktree-stubborn \
    || fail "the branch was dropped even though the checkout survived"
}

# ── registry upkeep ──────────────────────────────────────────────────────────

@test "registry: rows whose branch has vanished are pruned on the next listing" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" ghost)"
  git -C "$main" worktree remove --force "$dir"
  git -C "$main" branch -qD worktree-ghost
  [ "$(reg_rows)" -eq 1 ]
  "$WT" list >/dev/null 2>&1
  [ "$(reg_rows)" -eq 0 ]
}

@test "registry: a row pointing at a deleted main checkout doesn't invent a repo" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" orphan)"
  git -C "$main" worktree remove --force "$dir"
  rm -rf "$main"
  cd "$TMP"; wt_run list
  [ "$status" -eq 0 ]
  # The old bug listed the CURRENT repo's branches again under a repo named ".".
  ! [[ "$output" == *" . "* ]]
}

@test "registry: parallel creates must not lose rows to a read-modify-write race" {
  # One repo EACH, deliberately. The contention under test is eight processes
  # writing one registry.tsv — but eight `git worktree add` in a single repo also
  # race each other, inside git (`failed to read .git/worktrees/<n>/commondir`),
  # and a checkout git dropped is indistinguishable here from a row scruff lost.
  # That made this test flaky on Linux and, worse, able to mask the regression it
  # exists to catch. Separate repos remove git from the picture; the registry is
  # still the one shared file all eight are fighting over.
  local i repos=()
  for i in 1 2 3 4 5 6 7 8; do repos+=("$(mkrepo "par$i")"); done
  # The repos are built serially so the parallel section is nothing BUT the eight
  # concurrent registry writes.
  for i in 1 2 3 4 5 6 7 8; do
    hook_create "${repos[$((i - 1))]}" "par$i" >/dev/null 2>&1 &
  done
  wait
  [ "$(reg_rows)" -eq 8 ]
}

# ── bare PATH (the hook environment) ─────────────────────────────────────────
#
# Claude Code fires WorktreeCreate/WorktreeRemove with no PATH at all, and scruff
# shells out to git for everything. This is the case that breaks at pane-open
# time — the worst moment to find it — so it gets its own tests rather than
# riding along inside another one.

@test "bare PATH: the create hook still resolves git" {
  local main out
  main="$(mkrepo alpha)"
  out="$(printf '{"name":"barepath","cwd":"%s"}' "$main" | env -u PATH "$WT" create 2>/dev/null)"
  [ "$out" = "$CLAUDE_WT_BASE/alpha/barepath" ]
  [ -e "$out/.git" ]
  [ "$(git -C "$out" branch --show-current)" = worktree-barepath ]
}

@test "bare PATH: the rescue is APPENDED, so test shims still win" {
  # If the rescue were prepended, the real gh under /run/current-system/sw/bin
  # would beat the shim and the whole suite would quietly test the machine.
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" shimwins)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$(git -C "$dir" rev-parse HEAD)" FAKE_GH_PR=12
  export FAKE_GH_BRANCH=worktree-shimwins
  commit_in "$dir" post.txt "past the merge"
  cd "$TMP"; wt_run
  [ "$status" -eq 0 ]
  [[ "$output" == *"live+1"* ]] || fail "the shim gh lost to a real one: $output"
}

# ── heartbeat / occupancy leases ─────────────────────────────────────────────
#
# `lsof` answers "is a process cwd'd in here?", which is the right question for
# a zellij pane and the wrong one for everything else. A lease is how a client
# that knows its own sessions says so directly. The asymmetry below is the whole
# point and is worth stating twice: a lease may SAVE a checkout from the sweep,
# never condemn one, because "nobody leased it" is not evidence that nobody is
# there. SCRUFF_OCCUPANCY=lease is the one deployment entitled to say otherwise.
#
# Every test here passes `--pid $$` explicitly. The default — the CALLING
# process — is right for the embedder that exec's scruff and stays alive, and
# wrong under bats, where `run` forks a subshell that exits the moment scruff
# does. Naming the test shell keeps the lease alive across the later `reap`,
# which is the situation being tested.

@test "heartbeat: a lease keeps a landed checkout from being reaped" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" leased)"
  git -C "$main" merge -q --no-edit worktree-leased
  wt_run heartbeat "$dir" --pid $$
  [ "$status" -eq 0 ]
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"kept leased (alpha) — something is standing in the checkout"* ]]
  # A lease knows the pid but never the command, so the provider stands in.
  [[ "$output" == *"pid $$ (leases)"* ]] || fail "$output"
  [ -e "$dir/.git" ]
}

@test "heartbeat: the default holder is the calling process, which bats then reaps" {
  # The complement of the tests above: with no --pid, the lease dies with the
  # short-lived shell `run` forked for it. That is the contract working — a
  # client that exits stops vouching for its worktree the instant it does.
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" ephemeral)"
  git -C "$main" merge -q --no-edit worktree-ephemeral
  wt_run heartbeat "$dir"
  [ "$status" -eq 0 ]
  cd "$TMP"; wt_run reap
  [[ "$output" == *"reaped ephemeral (alpha)"* ]]
}

@test "heartbeat: --release drops the lease and the checkout reaps" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" transient)"
  git -C "$main" merge -q --no-edit worktree-transient
  wt_run heartbeat "$dir" --pid $$
  wt_run heartbeat "$dir" --release
  [ "$status" -eq 0 ]
  cd "$TMP"; wt_run reap
  [[ "$output" == *"reaped transient (alpha)"* ]]
  [ ! -e "$dir" ]
}

@test "heartbeat: a lease whose holder is gone protects nothing" {
  local main dir dead; main="$(mkrepo alpha)"; dir="$(mkwt "$main" ghost)"
  git -C "$main" merge -q --no-edit worktree-ghost
  # A pid we can prove is finished. The kernel is the witness here, not the
  # 90s TTL: a killed client must not hold its worktree hostage for a minute
  # and a half.
  sh -c 'exit 0' & dead=$!
  wait "$dead" 2>/dev/null || true
  wt_run heartbeat "$dir" --pid "$dead"
  [ "$status" -eq 0 ]
  cd "$TMP"; wt_run reap
  [[ "$output" == *"reaped ghost (alpha)"* ]]
}

@test "heartbeat: leases never vouch for an EMPTY checkout — no lsof still degrades" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" cautious)"
  git -C "$main" merge -q --no-edit worktree-cautious
  # A populated lease directory that says nothing about THIS worktree. The
  # temptation is to read that as "so it's free"; taking it would reap the
  # checkout of anyone who simply cd'd in without telling scruff.
  local other; other="$(mkwt "$(mkrepo beta)" elsewhere)"
  wt_run heartbeat "$other" --pid $$
  export FAKE_LSOF_BROKEN=1
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"no lsof"* ]]
  [ -e "$dir/.git" ]
}

@test "heartbeat: SCRUFF_OCCUPANCY=lease lets an embedder answer for absence" {
  # Two repos, not two worktrees of one: mkwt commits the same work.txt in each,
  # so landing both branches into a single main is an add/add conflict rather
  # than the fixture this test wants.
  local alpha beta held free
  alpha="$(mkrepo alpha)"; beta="$(mkrepo beta)"
  held="$(mkwt "$alpha" held)"; free="$(mkwt "$beta" free)"
  git -C "$alpha" merge -q --no-edit worktree-held
  git -C "$beta" merge -q --no-edit worktree-free
  wt_run heartbeat "$held" --pid $$
  # No lsof at all — the deployment this models has no processes to scan. The
  # embedder owns every session, so its leases are the whole truth.
  export FAKE_LSOF_BROKEN=1 SCRUFF_OCCUPANCY=lease
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" != *"no lsof"* ]] || fail "leases should have answered: $output"
  [[ "$output" == *"reaped free (beta)"* ]]
  [[ "$output" == *"kept held (alpha) — something is standing in the checkout"* ]]
  [ -e "$held/.git" ]
}

@test "heartbeat: a leased checkout reads as occupied in --json" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" watched)"
  wt_run heartbeat "$dir" --pid $$
  export FAKE_LSOF_BROKEN=1     # the lease is the ONLY signal left
  cd "$TMP"; wt_run list --json
  [ "$status" -eq 0 ]
  [[ "$output" == *'"occupied": true'* ]] || fail "$output"
}

@test "heartbeat: refusing a path that does not exist beats inventing a lease" {
  wt_run heartbeat "$TMP/nowhere"
  [ "$status" -eq 1 ]
  [[ "$output" == *"no such path"* ]]
}

# ── dispatch ─────────────────────────────────────────────────────────────────

@test "dispatch: --help prints the header block, including park/unpark" {
  wt_run --help
  [ "$status" -eq 0 ]
  [[ "$output" == *"scruff park [label]"* ]]
  [[ "$output" == *"scruff unpark"* ]]
  [[ "$output" != *"#!/usr/bin/env"* ]]
}

@test "dispatch: a bare unknown token is treated as a lane name" {
  wt_run gibberish
  [ "$status" -ne 0 ]
  [[ "$output" == *"no lane named 'gibberish'"* ]]
}

# Bare `scruff` IS the listing, so `scruff --json` must be the machine-readable
# listing — not "unknown flag", which is what every consumer that reached for
# the obvious spelling used to get. It has to be the SAME envelope as the
# explicit form, or the two spellings drift and consumers pick the wrong one.
@test "dispatch: bare --json is the listing, byte-identical to 'list --json'" {
  local main; main="$(mkrepo alpha)"
  mkwt "$main" sparkle >/dev/null

  wt_run --json
  [ "$status" -eq 0 ]
  [[ "$output" != *"unknown flag"* ]]
  [[ "$output" == *'"lanes"'* ]]
  [[ "$output" == *'"name": "sparkle"'* ]]

  local bare="$output"
  wt_run list --json
  [ "$status" -eq 0 ]
  [ "$output" = "$bare" ] || fail "the two spellings disagree"
}

# ── the help flag, and every other argument a verb can't explain ─────────────
#
# `scruff reap --help` SWEPT. Help was spelled only at the top level, `Reap`
# never looked at its arguments, and the flag you type to ASK A QUESTION about
# an unfamiliar verb ran the one verb that deletes. Agents hit it repeatedly,
# which is the tell: the bug is not the missing help text, it is that a verb
# swallowed an argument it could not explain and did its work anyway. Both
# halves are pinned below, and the reap half is pinned against a lane that a
# real sweep WOULD have taken — a test on an empty registry proves nothing.

@test "help: a verb's --help prints that verb and does no work" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" sweepme)"
  git -C "$main" merge -q --no-edit worktree-sweepme     # landed: reap would take it

  cd "$TMP"; wt_run reap --help
  [ "$status" -eq 0 ]
  [[ "$output" == *"sweep every LANDED lane"* ]]
  [[ "$output" != *"reaped sweepme"* ]] || fail "--help swept"
  [ -e "$dir" ] || fail "the checkout --help was asked about is gone"
  [ "$(reg_rows)" -eq 1 ]
  git -C "$main" rev-parse --verify -q worktree-sweepme >/dev/null || fail "the branch went too"
}

@test "help: the block is that verb's lines, not the whole manual" {
  wt_run park --help
  [ "$status" -eq 0 ]
  [[ "$output" == *"scruff park"* ]]
  [[ "$output" != *"scruff reap"* ]] || fail "park's help printed every other verb"
  # -h means the same thing, and an unknown verb still gets the whole thing.
  wt_run reaped -h
  [ "$status" -eq 0 ]
  [[ "$output" == *"scruff reaped"* ]]
}

@test "reap: an argument it can't explain refuses instead of sweeping" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" sweepme)"
  git -C "$main" merge -q --no-edit worktree-sweepme

  # The next typo is one nobody has thought of yet — a flag from another tool,
  # a lane name, `-n`. Every one of them has to stop the run.
  cd "$TMP"; wt_run reap --dry-run
  [ "$status" -eq 1 ] || fail "an unexplained flag must be usage, not a sweep: $status"
  [ -e "$dir" ] || fail "reap swept on an argument it did not understand"
  cd "$TMP"; wt_run reap sweepme
  [ "$status" -eq 1 ]
  [ -e "$dir" ]
  [ "$(reg_rows)" -eq 1 ]

  # And the bare verb still works — strictness must not cost the happy path.
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [ ! -e "$dir" ]
}

@test "park: --help is help, not a label it parks under" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" sparkle)"
  echo scratch >"$dir/dirty.txt"

  cd "$dir"; wt_run park --help
  [ "$status" -eq 0 ]
  [[ "$output" == *"set the working tree aside"* ]]
  [ -e "$dir/dirty.txt" ] || fail "the tree was parked by a help flag"
  [[ "$(git -C "$dir" log -1 --format=%s)" != wip:* ]] || fail "a wip: commit named --help"
}

@test "dispatch: a second bare word is a typo, not a lane resumed" {
  local main; main="$(mkrepo alpha)"; mkwt "$main" sparkle >/dev/null
  cd "$TMP"; wt_run sparkle extra
  [ "$status" -eq 1 ]
  [[ "$output" == *"second"* ]]
  cd "$TMP"; wt_run drop sparkle extra
  [ "$status" -eq 1 ] || fail "drop took a name it was not sure about: $status"
  git -C "$main" rev-parse --verify -q worktree-sparkle >/dev/null || fail "the branch was dropped anyway"
}

# ── policy seams ─────────────────────────────────────────────────────────────
#
# Every one of these asserts the same two halves of the same contract, on a
# different decision: with no hook configured, scruff behaves EXACTLY as it did
# before hooks existed (the whole rest of this suite is that half); with a hook
# configured, the hook's answer is the answer, including when it contradicts
# scruff's own. The second half is the product — a machine has to be able to be
# right about its own lanes when scruff is wrong.

mkhook() { # mkhook <name> <body> — an executable hook, echo its path
  local path="$TMP/hooks/$1"
  mkdir -p "$TMP/hooks"
  printf '#!/usr/bin/env bash\n%s\n' "$2" >"$path"
  chmod +x "$path"
  printf '%s' "$path"
}

setcfg() { # setcfg <toml body> — plant the machine config
  mkdir -p "$XDG_CONFIG_HOME/scruff"
  printf '%s\n' "$1" >"$XDG_CONFIG_HOME/scruff/config.toml"
}

@test "hooks: no config means every decision is scruff's own" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" plain)"
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"nothing to reap"* ]]      # unmerged: scruff's own rule held
  [ -e "$dir/.git" ]
}

@test "hooks: landed — a branch scruff calls unmerged is reaped when the hook says landed" {
  local main dir hook; main="$(mkrepo alpha)"; dir="$(mkwt "$main" trainlanded)"
  hook="$(mkhook landed 'echo "{\"via\": \"release-train\"}"; exit 0')"
  setcfg "[hooks]
landed = \"$hook\""
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped trainlanded (alpha)"* ]] || fail "the landed hook did not decide: $output"
  [ ! -e "$dir" ]
  run git -C "$main" show-ref -q --verify refs/heads/worktree-trainlanded
  [ "$status" -ne 0 ]
}

@test "hooks: landed — a hook that says no keeps a branch git itself calls merged" {
  local main dir hook; main="$(mkrepo alpha)"; dir="$(mkwt "$main" heldback)"
  git -C "$main" merge -q --no-edit worktree-heldback   # ancestry-merged: scruff would reap
  hook="$(mkhook landed 'exit 1')"
  setcfg "[hooks]
landed = \"$hook\""
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [ -e "$dir/.git" ]
  git -C "$main" show-ref -q --verify refs/heads/worktree-heldback
}

@test "hooks: landed — exit 3 defers, leaving scruff's own ladder in force" {
  local main dir hook; main="$(mkrepo alpha)"; dir="$(mkwt "$main" deferred)"
  git -C "$main" merge -q --no-edit worktree-deferred
  hook="$(mkhook landed 'exit 3')"
  setcfg "[hooks]
landed = \"$hook\""
  cd "$TMP"; wt_run reap
  [[ "$output" == *"reaped deferred (alpha)"* ]] || fail "defer did not fall through: $output"
}

@test "hooks: landed — a hook that cannot run warns and falls back, never fails" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" broken)"
  git -C "$main" merge -q --no-edit worktree-broken
  setcfg '[hooks]
landed = "/nonexistent/scruff-landed"'
  cd "$TMP"; wt_run reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"wouldn't run"* ]] || fail "a dead hook must say so: $output"
  [[ "$output" == *"reaped broken (alpha)"* ]] || fail "a dead hook must not cost the sweep: $output"
}

@test "hooks: preserve — it decides whether a closing pane's dirt becomes a wip commit" {
  local main dir hook; main="$(mkrepo alpha)"; dir="$(mkwt "$main" noswap)"
  echo edit >"$dir/README.md"            # a TRACKED edit: scruff would always preserve
  hook="$(mkhook preserve 'exit 1')"
  setcfg "[hooks]
preserve = \"$hook\""
  hook_remove "$dir" 2>/dev/null
  run git -C "$main" log -1 --format=%s worktree-noswap
  [[ "$output" != wip:* ]] || fail "the hook said don't preserve and scruff did anyway: $output"
}

@test "hooks: preserve — a yes wip-commits scratch scruff would have dropped" {
  local main dir tip hook; main="$(mkrepo alpha)"; dir="$(mkwt "$main" keepall)"
  tip="$(git -C "$dir" rev-parse HEAD)"
  export FAKE_GH_MERGED=1 FAKE_GH_OID="$tip"     # landed; only untracked left
  touch "$dir/scratch.o"
  hook="$(mkhook preserve 'exit 0')"
  setcfg "[hooks]
preserve = \"$hook\""
  hook_remove "$dir" 2>/dev/null
  run git -C "$main" log -1 --format=%s worktree-keepall
  [[ "$output" == wip:* ]] || fail "the hook said preserve and scruff dropped it: $output"
}

@test "hooks: a predicate is handed the situation as SCRUFF_* vars AND as JSON" {
  # Both channels carry the same table, because a seam may be a program with a
  # JSON parser or three lines of shell, and neither should have to become the
  # other. SCRUFF_BASE_BRANCH is the one that needs pinning: SCRUFF_BASE is already
  # the lane base DIRECTORY, so the default branch had to be spelled apart.
  #
  # The hook only DUMPS its two inputs; every assertion lives out here in bats.
  # The first version matched the JSON with a `case` nested inside a command
  # substitution inside the hook body, and word-split its own printf arguments
  # on both CI runners while passing on two local shells — including the exact
  # bash 3.2 macOS ships. Whatever the trigger, a hook body that only writes
  # two files cannot have it: no nested quoting, no command substitution, and
  # printf rather than echo, which is the one that mangles a JSON \n.
  local main dir hook; main="$(mkrepo alpha)"; dir="$(mkwt "$main" payload)"
  echo edit >"$dir/README.md"
  hook="$(mkhook preserve '
    read -r body
    printf "%s\n" "$body" >"'"$TMP"'/stdin"
    printf "hook=%s name=%s branch=%s repo=%s base=%s main=%s\n" \
      "$SCRUFF_HOOK" "$SCRUFF_NAME" "$SCRUFF_BRANCH" "$SCRUFF_REPO" \
      "$SCRUFF_BASE_BRANCH" "$SCRUFF_MAIN" >"'"$TMP"'/env"
    exit 3')"
  setcfg "[hooks]
preserve = \"$hook\""
  hook_remove "$dir" 2>/dev/null

  run cat "$TMP/env"
  [ "$output" = "hook=preserve name=payload branch=worktree-payload repo=acme/alpha base=main main=$main" ] \
    || fail "the SCRUFF_* vars are wrong: $output"

  run cat "$TMP/stdin"
  local json="$output" needle
  for needle in '"branch":"worktree-payload"' '"base":"main"' '"repo":"acme/alpha"'; do
    [[ "$json" == *"$needle"* ]] || fail "stdin JSON lacks $needle: $json"
  done
}

@test "hooks: resume — the hook reopens the session instead of scruff exec'ing a client" {
  local main dir hook; main="$(mkrepo alpha)"; dir="$(mkwt "$main" paned)"
  hook="$(mkhook resume '
    printf "%s %s %s\n" "$SCRUFF_NAME" "$SCRUFF_PATH" "$SCRUFF_LANE_AGENT" >"'"$TMP"'/opened"
    printf "%s\n" "$SCRUFF_COMMAND" >"'"$TMP"'/cmd"
    exit 0')"
  setcfg "[hooks]
resume = \"$hook\""
  cd "$TMP"; wt_run paned
  [ "$status" -eq 0 ]
  run cat "$TMP/opened"
  [ "$output" = "paned $dir claude" ] || fail "resume payload is wrong: $output"
  # A hook that spawns a pane runs what scruff WOULD have run, or the pane lands
  # on the picker scruff just spared the user.
  [ "$(cat "$TMP/cmd")" = "claude --continue" ] || fail "wrong command: $(cat "$TMP/cmd")"
}

@test "hooks: resume — the checkout is rebuilt BEFORE the hook is asked to open it" {
  local main dir hook; main="$(mkrepo alpha)"; dir="$(mkwt "$main" rebuilt)"
  hook_remove "$dir" >/dev/null 2>&1           # park it: branch survives, checkout gone
  [ ! -e "$dir" ]
  hook="$(mkhook resume 'test -e "$SCRUFF_PATH/.git" && echo rebuilt >"'"$TMP"'/state"; exit 0')"
  setcfg "[hooks]
resume = \"$hook\""
  cd "$TMP"; wt_run rebuilt
  [ "$status" -eq 0 ]
  [ "$(cat "$TMP/state" 2>/dev/null)" = rebuilt ] || fail "the hook was handed a checkout that isn't there"
}

@test "hooks: resume — a spawned lane tells the hook where the CHAT lives" {
  local main sub parent child hook
  main="$(mkrepo alpha)"; sub="$(mkrepo beta)"
  parent="$(mkwt "$main" workshop)"
  mkdir -p "$HOME/.claude/projects/$(printf '%s' "$parent" | tr './' '--')"
  cd "$parent"; child="$("$WT" child "$sub" workshop 2>/dev/null)"
  hook="$(mkhook resume 'printf "%s|%s\n" "$SCRUFF_PATH" "$SCRUFF_CHAT" >"'"$TMP"'/chat"; exit 0')"
  setcfg "[hooks]
resume = \"$hook\""
  cd "$TMP"; wt_run beta/workshop
  [ "$status" -eq 0 ]
  [ "$(cat "$TMP/chat")" = "$child|$parent" ] \
    || fail "a hook opening a pane in \$SCRUFF_PATH would get an empty session: $(cat "$TMP/chat")"
}

@test "hooks: resume — a hook that refuses exits 2, not 0" {
  local main dir hook; main="$(mkrepo alpha)"; dir="$(mkwt "$main" refused)"
  hook="$(mkhook resume 'exit 2')"
  setcfg "[hooks]
resume = \"$hook\""
  cd "$TMP"; wt_run refused
  [ "$status" -eq 2 ] || fail "a safety refusal must stay distinguishable from a usage error: $status"
}

@test "hooks: open — a fresh lane's session is the machine's business too" {
  local main hook; main="$(mkrepo alpha)"
  hook="$(mkhook open 'printf "%s %s\n" "$SCRUFF_NAME" "$SCRUFF_LANE_AGENT" >"'"$TMP"'/opened"; exit 0')"
  setcfg "[hooks]
open = \"$hook\""
  cd "$main"; wt_run new fresh --open
  [ "$status" -eq 0 ]
  [ "$(cat "$TMP/opened")" = "fresh claude" ] || fail "open payload is wrong: $(cat "$TMP/opened")"
  [ -e "$CLAUDE_WT_BASE/alpha/fresh/.git" ]
}

# ── --prompt: a lane that opens already knowing the task ─────────────────────

# A client that reports its own argv, one element per line. Deliberately local
# to these tests rather than a setup() shim: several tests elsewhere assert what
# happens when NO client is installed, and a global `claude` would quietly make
# those pass for the wrong reason.
mkclient() { # mkclient <id> — write a reporting shim into $BIN, echo nothing
  cat >"$BIN/$1" <<'EOF'
#!/usr/bin/env bash
for a in "$@"; do printf '<%s>\n' "$a"; done
EOF
  chmod +x "$BIN/$1"
}

@test "prompt: spawn hands the open hook the client's START invocation, not its bare open" {
  local b hook; b="$(mkrepo beta)"
  hook="$(mkhook open 'printf "%s\n" "$SCRUFF_COMMAND" >"'"$TMP"'/cmd"; exit 0')"
  setcfg "[hooks]
open = \"$hook\""
  run bash -c "'$WT' spawn '$b' notch-flicker --prompt 'fix the notch' 2>/dev/null"
  [ "$status" -eq 0 ]
  [ "$output" = "$CLAUDE_WT_BASE/beta/notch-flicker" ]   # the path is still the stdout contract
  # `--` before the prompt, always: a task beginning with a dash is a FLAG to
  # commander and clap otherwise, and dies before the pane draws anything.
  [ "$(cat "$TMP/cmd")" = "claude -- 'fix the notch'" ] || fail "wrong invocation: $(cat "$TMP/cmd")"
}

@test "prompt: a multi-line prompt with quotes and a leading dash survives SCRUFF_COMMAND" {
  # The regression this exists for: `command` used to be a bare space-join of
  # argv. Every invocation scruff had ever handed a hook was one or two bare words
  # ("claude", "codex resume --last"), so the bug was invisible — and the first
  # prompt through it would shatter into words, or unbalance the opener's shell.
  local b hook brief; b="$(mkrepo beta)"
  mkclient claude
  brief="- rewrite \"the parser\"
  Next: run \$HOME/x.sh  # don't expand me"
  hook="$(mkhook open 'bash -c "$SCRUFF_COMMAND" >"'"$TMP"'/argv" 2>&1; exit 0')"
  setcfg "[hooks]
open = \"$hook\""
  run bash -c "'$WT' spawn '$b' parser --prompt \"\$1\" 2>/dev/null" _ "$brief"
  [ "$status" -eq 0 ]
  # ONE argument after `--`, byte-identical to what went in — newline, quotes,
  # leading dash and unexpanded `$HOME` alike. (The shim reports `$@`, so the
  # client's own name is not in there.)
  [ "$(cat "$TMP/argv")" = "$(printf '<-->\n<%s>' "$brief")" ] \
    || fail "the prompt did not survive the round trip:
$(cat "$TMP/argv")"
}

@test "prompt: spawn with no open hook is degraded, not failed — the lane exists" {
  local b; b="$(mkrepo beta)"
  run bash -c "'$WT' spawn '$b' orphan --prompt 'do the thing'"
  # 3, not 1: scruff made the lane it was asked for. What was unavailable is
  # somewhere to open it, and the caller needs to tell those two apart.
  [ "$status" -eq 3 ]
  [ -e "$CLAUDE_WT_BASE/beta/orphan/.git" ]
  [[ "$output" == *"claude -- 'do the thing'"* ]] || fail "no recovery command: $output"
}

@test "prompt: --prompt-file reads the brief from a file, and - from stdin" {
  local b hook; b="$(mkrepo beta)"
  hook="$(mkhook open 'printf "%s\n" "$SCRUFF_COMMAND" >"'"$TMP"'/cmd"; exit 0')"
  setcfg "[hooks]
open = \"$hook\""
  printf 'ship the thing\n' >"$TMP/brief.md"        # trailing newline is not the task

  run bash -c "'$WT' spawn '$b' from-file --prompt-file '$TMP/brief.md' 2>/dev/null"
  [ "$status" -eq 0 ]
  [ "$(cat "$TMP/cmd")" = "claude -- 'ship the thing'" ] || fail "from file: $(cat "$TMP/cmd")"

  run bash -c "'$WT' spawn '$b' from-stdin --prompt-file - <'$TMP/brief.md' 2>/dev/null"
  [ "$status" -eq 0 ]
  [ "$(cat "$TMP/cmd")" = "claude -- 'ship the thing'" ] || fail "from stdin: $(cat "$TMP/cmd")"
}

@test "prompt: an empty --prompt-file is a usage error, not a lane opened on nothing" {
  local b; b="$(mkrepo beta)"
  : >"$TMP/empty.md"
  run "$WT" spawn "$b" nothing --prompt-file "$TMP/empty.md"
  [ "$status" -eq 1 ]
  [ ! -e "$CLAUDE_WT_BASE/beta/nothing" ] || fail "a lane was created for a prompt that isn't there"
  run "$WT" spawn "$b" nofile --prompt-file "$TMP/does-not-exist.md"
  [ "$status" -eq 1 ]
}

@test "prompt: a literal --help in the task is the task, not a request for usage" {
  # The other side of the help scan: it skips the value of a flag that takes
  # one, so a brief that happens to BEGIN with a flag still opens a lane
  # instead of printing scruff's own manual at it.
  local b hook; b="$(mkrepo beta)"
  hook="$(mkhook open 'printf "%s\n" "$SCRUFF_COMMAND" >"'"$TMP"'/cmd"; exit 0')"
  setcfg "[hooks]
open = \"$hook\""

  run bash -c "'$WT' spawn '$b' quoting --prompt '--help' 2>/dev/null"
  [ "$status" -eq 0 ] || fail "a task starting with a flag was read as help: $status"
  [ -e "$CLAUDE_WT_BASE/beta/quoting/.git" ]
  [[ "$(cat "$TMP/cmd")" == *"--help"* ]] || fail "the task did not reach the client: $(cat "$TMP/cmd")"
}

@test "prompt: spawn WITHOUT a prompt never fires the open hook" {
  # The pairing that makes the test above mean something. `scruff spawn` on its
  # own is still "make me a lane and print its path" — a caller doing its own
  # opening must not get a second window from scruff.
  local b hook; b="$(mkrepo beta)"
  hook="$(mkhook open 'echo fired >"'"$TMP"'/fired"; exit 0')"
  setcfg "[hooks]
open = \"$hook\""
  run bash -c "'$WT' spawn '$b' quiet 2>/dev/null"
  [ "$status" -eq 0 ]
  [ ! -e "$TMP/fired" ] || fail "spawn opened a window nobody asked for"
}

@test "prompt: an open hook that FAILS is degraded too, not a usage error" {
  # These used to be two answers to one situation: no hook exited 3 with a
  # recovery line, a hook that broke exited 1 with none. A palette command whose
  # window manager was down read 1 as "you asked wrong", retried, and made a
  # SECOND lane while the first sat on disk.
  local b hook; b="$(mkrepo beta)"
  hook="$(mkhook open 'exit 7')"      # 7 means nothing to scruff — a broken hook
  setcfg "[hooks]
open = \"$hook\""
  run bash -c "'$WT' spawn '$b' broken --prompt 'do the thing'"
  [ "$status" -eq 3 ] || fail "a broken opener must not read as a bad invocation: $status"
  [ -e "$CLAUDE_WT_BASE/beta/broken/.git" ]
  [[ "$output" == *"claude -- 'do the thing'"* ]] || fail "no recovery command: $output"
}

@test "prompt: an open hook that REFUSES still exits 2" {
  # A decision, not a breakage — and a caller has to tell them apart.
  local b hook; b="$(mkrepo beta)"
  hook="$(mkhook open 'exit 2')"
  setcfg "[hooks]
open = \"$hook\""
  run bash -c "'$WT' spawn '$b' declined --prompt 'do the thing'"
  [ "$status" -eq 2 ]
}

@test "prompt: an empty positional is refused instead of shifting the next one along" {
  # `scruff spawn "$repo" "" claude` used to fall through the name slot and name
  # the lane "claude". Every SDK passes the name positionally, so an unset
  # variable in a caller silently produced a misnamed lane.
  local b; b="$(mkrepo beta)"
  run "$WT" spawn "$b" "" claude
  [ "$status" -eq 1 ]
  [ ! -e "$CLAUDE_WT_BASE/beta/claude" ] || fail "the agent id became the lane name"
}

@test "prompt: an empty --prompt is refused, like an empty --prompt-file" {
  local b main; b="$(mkrepo beta)"; main="$(mkrepo alpha)"
  run "$WT" spawn "$b" blank --prompt ""
  [ "$status" -eq 1 ]
  [ ! -e "$CLAUDE_WT_BASE/beta/blank" ]
  cd "$main"; wt_run new blank --prompt "   "
  [ "$status" -eq 1 ]
  [ ! -e "$CLAUDE_WT_BASE/alpha/blank" ]
}

@test "prompt: --image with no first turn to look at it is refused" {
  # Silently dropping it opens a pane whose agent was never given the screenshot
  # the user pointed at.
  local main b; main="$(mkrepo alpha)"; b="$(mkrepo beta)"
  : >"$TMP/shot.png"
  cd "$main"; wt_run new shotless --open --image "$TMP/shot.png"
  [ "$status" -eq 1 ]
  [ ! -e "$CLAUDE_WT_BASE/alpha/shotless" ]
  run "$WT" spawn "$b" shotless --image "$TMP/shot.png"
  [ "$status" -eq 1 ]
}

@test "prompt: --image reaches a client that can attach it, and is described to one that can't" {
  local b hook; b="$(mkrepo beta)"
  : >"$TMP/shot.png"
  hook="$(mkhook open 'printf "%s\n" "$SCRUFF_COMMAND" >"'"$TMP"'/cmd"; exit 0')"
  setcfg "[hooks]
open = \"$hook\""
  run bash -c "'$WT' spawn '$b' shot --agent codex --prompt 'look' --image '$TMP/shot.png' 2>/dev/null"
  [ "$status" -eq 0 ]
  [ "$(cat "$TMP/cmd")" = "codex -i $TMP/shot.png -- look" ] || fail "codex: $(cat "$TMP/cmd")"

  # claude has no image flag, so the path is named in the prompt instead of
  # being dropped — an agent reasoning about a screenshot it was never given is
  # worse than one told where to find it.
  run bash -c "'$WT' spawn '$b' shot-claude --prompt 'look' --image '$TMP/shot.png' 2>/dev/null"
  [ "$status" -eq 0 ]
  [[ "$(cat "$TMP/cmd")" == *"A screenshot for this task is at $TMP/shot.png"* ]] \
    || fail "claude: $(cat "$TMP/cmd")"
}

@test "prompt: new --prompt implies --open, and keeps the lane's own client" {
  local main hook; main="$(mkrepo alpha)"
  hook="$(mkhook open 'printf "%s|%s\n" "$SCRUFF_LANE_AGENT" "$SCRUFF_COMMAND" >"'"$TMP"'/cmd"; exit 0')"
  setcfg "[hooks]
open = \"$hook\""
  # No --open anywhere: a prompt with no session to hand it to is a prompt
  # nobody reads, so it opens one.
  cd "$main"; wt_run new tasked --agent codex --prompt "look at the bar"
  [ "$status" -eq 0 ]
  [ "$(cat "$TMP/cmd")" = "codex|codex -- 'look at the bar'" ] || fail "wrong: $(cat "$TMP/cmd")"
  [ "$(awk -F'\t' '$1=="tasked"{print $6}' "$REG")" = codex ]
}

@test "prompt: --cmd and --prompt are refused together, before anything is created" {
  local main; main="$(mkrepo alpha)"
  cd "$main"; wt_run new clash --cmd 'echo hi' --prompt 'do it'
  [ "$status" -eq 1 ]
  [ ! -e "$CLAUDE_WT_BASE/alpha/clash" ]
}

# ── namer: a lane named after its task ───────────────────────────────────────
#
# Opt-in, cosmetic, and unable to fail a lane. Every test here is one half of
# that: what happens with no `namer` key (nothing — exactly the behaviour that
# predates the key), and what happens when the namer misbehaves (a warning and
# a random name, never a lane that didn't get made). The namer's output is a
# model's text on its way to a branch name and a path, so the last two are the
# gate that stands between them.

mknamer() { # mknamer <body> — a namer shim, and the `fake` adapter that runs it
  mkdir -p "$TMP/hooks" "$XDG_CONFIG_HOME/scruff/adapters/namer"
  printf '#!/usr/bin/env bash\n%s\n' "$1" >"$TMP/hooks/namer"
  chmod +x "$TMP/hooks/namer"
  printf 'kind = "namer"\nid = "fake"\nname = ["%s", "{{.Prompt}}"]\n' "$TMP/hooks/namer" \
    >"$XDG_CONFIG_HOME/scruff/adapters/namer/fake.toml"
}

lane_name() { # lane_name <path> — the basename scruff chose
  basename "$1"
}

# A fallback name is a random adjective-noun pair — and, when that pair is
# already taken in the repo, the numeric suffix `freeName` appends to it
# (`spry-swift-2`, new.go). Asserting the bare pair makes any test that spawns
# SEVERAL fallback lanes into one repo a coin flip: rare per spawn, certain over
# enough CI runs. It failed exactly that way on main (`'a; rm -rf ~' became
# spunky-kestrel-2`), so the suffix belongs in the pattern, not in a re-run.
is_random_name() { [[ "$1" =~ ^[a-z]+-[a-z]+(-[0-9]+)?$ ]]; }

@test "namer: with no namer configured nothing runs, and an unnamed lane is still a random pair" {
  local b; b="$(mkrepo beta)"
  mknamer 'touch "'"$TMP"'/ran"; echo mobile-nav-jitter'   # armed, but not named in the config
  run bash -c "'$WT' spawn '$b' --prompt 'the bar draws a draft PR in the merged colour' 2>/dev/null"
  [ "$status" -eq 3 ]                      # no open hook — the lane exists, nothing opened it
  [ ! -e "$TMP/ran" ] || fail "the namer ran with no namer key in the config"
  [ -d "$output" ] || fail "no lane at $output"
  is_random_name "$(lane_name "$output")" || fail "not a random pair: $(lane_name "$output")"
}

@test "namer: a configured namer names the lane, and is told the task, the repo and the neighbours" {
  local b; b="$(mkrepo beta)"
  mkwt "$b" tart-backend                   # a neighbour for the namer to avoid
  mknamer 'printf "%s" "$1" >"'"$TMP"'/req"; echo mobile-nav-jitter'
  setcfg 'namer = "fake"'
  run bash -c "'$WT' spawn '$b' --prompt 'the bar draws a draft PR in the merged colour' 2>/dev/null"
  [ "$status" -eq 3 ]
  [ "$output" = "$CLAUDE_WT_BASE/beta/mobile-nav-jitter" ] || fail "lane is at $output"
  [ -d "$CLAUDE_WT_BASE/beta/mobile-nav-jitter" ]
  git -C "$b" show-ref -q --verify refs/heads/worktree-mobile-nav-jitter

  run cat "$TMP/req"
  [[ "$output" == *"the bar draws a draft PR in the merged colour"* ]] || fail "no task: $output"
  [[ "$output" == *"acme/beta"* ]] || fail "no repo: $output"
  # The neighbours are in there because scruff's own collision handling is a
  # numeric suffix, and `fix-mobile-2` is correct and unreadable.
  [[ "$output" == *"tart-backend"* ]] || fail "no neighbours: $output"
}

@test "namer: a name the caller gave always wins, and a lane with no task never asks" {
  local b main; b="$(mkrepo beta)"; main="$(mkrepo alpha)"
  mknamer 'touch "'"$TMP"'/ran"; echo mobile-nav-jitter'
  setcfg 'namer = "fake"'

  run bash -c "'$WT' spawn '$b' notch-flicker --prompt 'fix the notch' 2>/dev/null"
  [ "$output" = "$CLAUDE_WT_BASE/beta/notch-flicker" ] || fail "the namer overrode a given name: $output"
  [ ! -e "$TMP/ran" ] || fail "the namer ran for a lane that already had a name"

  # No task, nothing to name after: `scruff new` on its own must not start a
  # process, which is the whole reason the ⌘↵ path is unaffected by this.
  cd "$main"; wt_run new
  [ "$status" -eq 0 ]
  [ ! -e "$TMP/ran" ] || fail "the namer ran for a lane with no prompt"
}

@test "namer: an answer that isn't a name warns and falls back — the lane is still made" {
  local b; b="$(mkrepo beta)"
  mknamer 'echo "I would need more detail about what you want changed."'
  setcfg 'namer = "fake"'
  run bash -c "'$WT' spawn '$b' --prompt 'fix the notch' 2>'$TMP/err'"
  [ "$status" -eq 3 ]
  [ -d "$output" ] || fail "a bad name cost the lane"
  is_random_name "$(lane_name "$output")" || fail "prose became a name: $(lane_name "$output")"
  grep -q "isn't a name" "$TMP/err" || fail "silent fallback: $(cat "$TMP/err")"
}

@test "namer: a namer that cannot run at all warns and falls back, never fails" {
  local b; b="$(mkrepo beta)"

  # Named in the config, no adapter file on disk.
  setcfg 'namer = "ollama"'
  run bash -c "'$WT' spawn '$b' --prompt 'fix the notch' 2>'$TMP/err'"
  [ "$status" -eq 3 ]
  [ -d "$output" ] || fail "a missing adapter cost the lane"
  grep -q "ollama" "$TMP/err" || fail "the missing adapter went unnamed: $(cat "$TMP/err")"

  # An adapter file whose command isn't installed.
  mkdir -p "$XDG_CONFIG_HOME/scruff/adapters/namer"
  printf 'kind = "namer"\nid = "gone"\nname = ["scruff-namer-not-installed", "{{.Prompt}}"]\n' \
    >"$XDG_CONFIG_HOME/scruff/adapters/namer/gone.toml"
  setcfg 'namer = "gone"'
  run bash -c "'$WT' spawn '$b' --prompt 'fix the notch' 2>'$TMP/err'"
  [ "$status" -eq 3 ]
  [ -d "$output" ] || fail "an uninstalled namer cost the lane"
  grep -q "PATH" "$TMP/err" || fail "the dead namer went unexplained: $(cat "$TMP/err")"
}

@test "namer: nothing a namer prints can escape the lane base or name a flag" {
  local b; b="$(mkrepo beta)"
  setcfg 'namer = "fake"'
  local answer
  for answer in '../../../etc/passwd' '-rf' '..' '/tmp/scruff-namer-escape' 'a; rm -rf ~' 'beta'; do
    mknamer "printf '%s\n' '$answer'"
    run bash -c "'$WT' spawn '$b' --prompt 'fix the notch' 2>/dev/null"
    [ "$status" -eq 3 ] || fail "spawn failed on answer '$answer': $output"
    [[ "$(dirname "$output")" = "$CLAUDE_WT_BASE/beta" ]] || fail "'$answer' escaped to $output"
    is_random_name "$(lane_name "$output")" || fail "'$answer' became $(lane_name "$output")"
  done
  [ ! -e "/tmp/scruff-namer-escape" ]
  # …and the repo naming itself is dropped rather than spending a word on what
  # every listing already shows.
  mknamer 'echo beta-nav-jitter'
  run bash -c "'$WT' spawn '$b' --prompt 'fix the notch' 2>/dev/null"
  [ "$(lane_name "$output")" = "nav-jitter" ] || fail "the repo named itself: $(lane_name "$output")"
}

@test "namer: the namer's stdin is empty, never the brief scruff just read off fd 0" {
  # `--prompt-file -` drains stdin to read the brief, and the client is handed a
  # terminal back afterwards. A namer that inherited fd 0 would either consume
  # what is left or block on a pipe nobody is writing to — a client that waits
  # on a non-tty stdin adds seconds to every single spawn.
  local b; b="$(mkrepo beta)"
  printf 'make the bar draw draft PRs in grey\n' >"$TMP/brief.md"
  mknamer 'cat >"'"$TMP"'/stdin"; echo draft-pr-grey'
  setcfg 'namer = "fake"'
  run bash -c "'$WT' spawn '$b' --prompt-file - <'$TMP/brief.md' 2>/dev/null"
  [ "$status" -eq 3 ]
  [ "$output" = "$CLAUDE_WT_BASE/beta/draft-pr-grey" ] || fail "lane is at $output"
  [ ! -s "$TMP/stdin" ] || fail "the namer was handed scruff's stdin: $(cat "$TMP/stdin")"
}

@test "hooks: a lane's own fields never shadow scruff's own environment" {
  # Every SCRUFF_* a hook is given leaks into the pane that hook spawns, and into
  # every window opened from it. So no field may be spelled as a variable scruff
  # itself reads: SCRUFF_STATE is the state DIRECTORY and SCRUFF_AGENT is the
  # one-invocation client override, which is why the lane's are SCRUFF_LANE_STATE
  # and SCRUFF_LANE_AGENT. When SCRUFF_STATE carried the lane's state, `scruff reap`
  # in an agent pane wrote the machine's reap ledger to ./live/reaped.log,
  # inside whatever git checkout the pane was sitting in.
  local main hook; main="$(mkrepo alpha)"
  hook="$(mkhook open '
    printf "state=%s agent=%s\n" "$SCRUFF_LANE_STATE" "$SCRUFF_LANE_AGENT" >"'"$TMP"'/lane"
    printf "state=%s agent=%s\n" "$SCRUFF_STATE" "$SCRUFF_AGENT" >"'"$TMP"'/shadow"
    exit 0')"
  setcfg "[hooks]
open = \"$hook\""
  cd "$main"; wt_run new noshadow --open
  [ "$status" -eq 0 ]

  run cat "$TMP/lane"
  [ "$output" = "state=live agent=claude" ] || fail "the lane's own fields are wrong: $output"

  # scruff's own two must arrive untouched by the lane — empty here, because the
  # suite sets neither. A lane value in either is the bug.
  run cat "$TMP/shadow"
  [ "$output" = "state= agent=" ] || fail "a lane field shadowed scruff's own environment: $output"
}

@test "hooks: agent — the default client can be a program, not just a constant" {
  local main hook; main="$(mkrepo alpha)"
  hook="$(mkhook agent 'echo "{\"agent\": \"codex\"}"; exit 0')"
  setcfg "agent = \"claude\"

[hooks]
agent = \"$hook\""
  cd "$main"; wt_run agent default
  [ "$status" -eq 0 ]
  [ "$output" = codex ] || fail "the agent hook lost to the static key: $output"
}

# ── watch ────────────────────────────────────────────────────────────────────
#
# `scruff watch --json` runs forever, so the suite's usual `run` — which waits
# for the process to exit before making $output/$status available — can't
# drive it. These helpers stand in: start it in the background, poll its
# output file until at least N lines have landed or a timeout passes, then
# always stop it in `teardown` — including when an assertion fails partway,
# so one red test never leaves a `scruff watch` running loose past the suite.
#
# WATCH_TIMEOUT is generous on purpose: fsnotify's underlying primitive
# (kqueue on macOS, inotify on Linux) is normally sub-second, but a loaded CI
# runner is not the machine that number was measured on, and a slow pass
# beats a flaky one. watch_wait_lines returns the moment the line count is
# met, so this only costs real time on a genuine failure.
WATCH_TIMEOUT=8

watch_start() { # watch_start [more args...] — background `scruff watch --json`; sets $WATCH_OUT
  WATCH_OUT="$TMP/watch-$BATS_TEST_NUMBER.out"
  : >"$WATCH_OUT"
  "$WT" watch --json "$@" >"$WATCH_OUT" 2>"$TMP/watch-$BATS_TEST_NUMBER.err" &
  WATCH_PID="$!"
}

watch_wait_lines() { # watch_wait_lines <n> <timeout-seconds> — block until $WATCH_OUT has n lines
  local n="$1" deadline=$((SECONDS + $2))
  while [ "$(wc -l <"$WATCH_OUT" 2>/dev/null | tr -d ' ')" -lt "$n" ]; do
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep 0.05
  done
  return 0
}

watch_line() { sed -n "${1}p" "$WATCH_OUT"; }                                    # watch_line <n>
watch_kind() { watch_line "$1" | sed -n 's/.*"kind":"\([^"]*\)".*/\1/p'; }        # watch_kind <n>

# bats allows exactly one teardown per file; every other test in this suite
# needs nothing beyond $TMP going away with it, so this only ever has watch's
# background process to reap.
teardown() {
  if [ -n "${WATCH_PID:-}" ]; then
    kill "$WATCH_PID" 2>/dev/null
    wait "$WATCH_PID" 2>/dev/null
    WATCH_PID=""
  fi
}

@test "watch: an unknown flag refuses instead of starting a stream" {
  run "$WT" watch --bogus
  [ "$status" -ne 0 ]
  [[ "$output" == *"unknown flag"* ]]
}

@test "watch: hello carries scruff+schema+capabilities, then syncs the existing lane, then ready" {
  local main; main="$(mkrepo alpha)"; mkwt "$main" sparkle >/dev/null
  cd "$TMP"; watch_start
  watch_wait_lines 3 "$WATCH_TIMEOUT" || fail "stream never reached 3 lines: $(cat "$WATCH_OUT")"

  [ "$(watch_kind 1)" = hello ] || fail "line 1 wasn't hello: $(watch_line 1)"
  [[ "$(watch_line 1)" == *'"schema":2'* ]] || fail "hello carries no schema: $(watch_line 1)"
  [[ "$(watch_line 1)" == *'"capabilities":["registry"]'* ]] \
    || fail "hello carries no capabilities: $(watch_line 1)"

  [ "$(watch_kind 2)" = sync ] || fail "line 2 wasn't sync: $(watch_line 2)"
  [[ "$(watch_line 2)" == *'"source":"registry"'* ]] || fail "sync names no source: $(watch_line 2)"
  [[ "$(watch_line 2)" == *'"name":"sparkle"'* ]] \
    || fail "sync's lane isn't the --json envelope's shape: $(watch_line 2)"

  [ "$(watch_kind 3)" = ready ] || fail "line 3 wasn't ready: $(watch_line 3)"
}

@test "watch: an empty registry goes straight from hello to ready — no phantom lane" {
  cd "$TMP"; watch_start
  watch_wait_lines 2 "$WATCH_TIMEOUT" || fail "hello/ready never landed: $(cat "$WATCH_OUT")"
  [ "$(watch_kind 1)" = hello ]
  [ "$(watch_kind 2)" = ready ]
  sleep 0.5   # give a false-positive sync every chance to show up before asserting its absence
  [ "$(wc -l <"$WATCH_OUT" | tr -d ' ')" -eq 2 ] \
    || fail "an empty registry produced a line beyond hello/ready: $(cat "$WATCH_OUT")"
}

@test "watch: a new lane appears as created" {
  local main; main="$(mkrepo alpha)"
  cd "$TMP"; watch_start
  watch_wait_lines 2 "$WATCH_TIMEOUT" || fail "hello/ready never landed"

  mkwt "$main" fresh >/dev/null

  watch_wait_lines 3 "$WATCH_TIMEOUT" || fail "the new lane never reached the stream: $(cat "$WATCH_OUT")"
  [ "$(watch_kind 3)" = created ] || fail "line 3 wasn't created: $(watch_line 3)"
  [[ "$(watch_line 3)" == *'"name":"fresh"'* ]]
  [[ "$(watch_line 3)" == *'"state":"live"'* ]]
}

@test "watch: a pane closing on an unlanded branch appears as parked" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" closing)"
  cd "$TMP"; watch_start
  watch_wait_lines 3 "$WATCH_TIMEOUT" || fail "sync of the pre-existing lane never landed"

  hook_remove "$dir" 2>/dev/null

  watch_wait_lines 4 "$WATCH_TIMEOUT" || fail "the park never reached the stream: $(cat "$WATCH_OUT")"
  [ "$(watch_kind 4)" = parked ] || fail "line 4 wasn't parked: $(watch_line 4)"
  [[ "$(watch_line 4)" == *'"state":"parked"'* ]]
}

@test "watch: scruff <name> on a parked lane appears as resumed" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" comeback)"
  hook_remove "$dir" 2>/dev/null      # park it before the stream even starts
  cd "$TMP"; watch_start
  watch_wait_lines 3 "$WATCH_TIMEOUT" || fail "sync of the already-parked lane never landed"
  [ "$(watch_kind 2)" = sync ]
  [[ "$(watch_line 2)" == *'"state":"parked"'* ]] || fail "the baseline wasn't parked: $(watch_line 2)"

  wt_run comeback
  [ "$status" -eq 0 ] || fail "resume itself failed: $output"

  watch_wait_lines 4 "$WATCH_TIMEOUT" || fail "the resume never reached the stream: $(cat "$WATCH_OUT")"
  [ "$(watch_kind 4)" = resumed ] || fail "line 4 wasn't resumed: $(watch_line 4)"
  [[ "$(watch_line 4)" == *'"state":"live"'* ]]
}

@test "watch: a landed branch swept by reap appears as reaped" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" done)"
  cd "$TMP"; watch_start
  watch_wait_lines 3 "$WATCH_TIMEOUT" || fail "sync of the pre-existing lane never landed"

  git -C "$main" merge -q --no-edit worktree-done
  wt_run reap
  [ "$status" -eq 0 ] || fail "reap itself failed: $output"

  watch_wait_lines 4 "$WATCH_TIMEOUT" || fail "the reap never reached the stream: $(cat "$WATCH_OUT")"
  [ "$(watch_kind 4)" = reaped ] || fail "line 4 wasn't reaped: $(watch_line 4)"
  [[ "$(watch_line 4)" == *'"name":"done"'* ]]
}

@test "watch: stdout is NDJSON only — every line stands alone as one JSON object" {
  local main; main="$(mkrepo alpha)"; mkwt "$main" clean >/dev/null
  cd "$TMP"; watch_start
  watch_wait_lines 3 "$WATCH_TIMEOUT" || fail "stream never settled"
  while IFS= read -r line; do
    [[ "$line" == \{*\} ]] || fail "a stdout line wasn't a bare JSON object: $line"
  done <"$WATCH_OUT"
}

# ── doctor / diagnose ────────────────────────────────────────────────────────
#
# SPEC.md §6.4's diagnose half. Three properties are what these pin down, and
# every one of them is a decision rather than an implementation detail:
#
#   * it EXITS 0 with findings — a finding is doctor working, not doctor failing,
#     and a doctor that exits non-zero on a machine merely lacking `gh` is
#     unusable under `set -e`;
#   * it FIXES NOTHING — `scruff` the listing prunes stale rows on its way past,
#     and doctor deliberately does not, because it is the output a stranger is
#     asked to paste into a bug report;
#   * "not determined" stays distinguishable from "false" in `--json`, which is
#     the frozen envelope's rule (§2.2) applied to a new set of facts.

@test "doctor: reports the machine, the repo it stands in, and the lane count" {
  local main; main="$(mkrepo alpha)"; mkwt "$main" sparkle >/dev/null
  cd "$main"; wt_run doctor
  [ "$status" -eq 0 ] || fail "a report is not a failure: $status / $output"
  [[ "$output" == *"environment"* ]] || fail "no environment section: $output"
  [[ "$output" == *"reflink"* ]] || fail "no reflink fact: $output"
  [[ "$output" == *"occupancy"* ]] || fail "no occupancy fact: $output"
  [[ "$output" == *"gh 2.63.2"* ]] || fail "the forge probe didn't run: $output"
  [[ "$output" == *"octocat"* ]] || fail "the forge probe didn't read the account: $output"
  # The repo section is the one doctor was RUN IN, and it reports the default
  # branch the sweep will actually measure against — here guessed from the name,
  # because the fixture's origin has no HEAD to assert one.
  [[ "$output" == *"acme/alpha"* ]] || fail "the repo section named no repo: $output"
  [[ "$output" == *"guessed from the name"* ]] || fail "the default-branch resolution isn't reported: $output"
  [[ "$output" == *"1 live"* ]] || fail "the lane count is wrong: $output"
}

@test "doctor: names a stray checkout and an orphan branch — and fixes neither" {
  local main dir; main="$(mkrepo alpha)"; dir="$(mkwt "$main" husk)"
  # A husk: git's admin dir gone, the tree still on disk. checkoutState reads
  # this as `stray`, and it is the shape `scruff <name>` rebuilds from.
  rm -rf "$main/.git/worktrees/husk"
  # An orphan: an agent branch with no registry row behind it.
  git -C "$main" branch worktree-lost

  cd "$TMP"; wt_run doctor
  [ "$status" -eq 0 ] || fail "findings are not failures: $status / $output"
  [[ "$output" == *"stray checkout"* ]] || fail "the husk wasn't named: $output"
  [[ "$output" == *"orphan branch"* ]] || fail "the orphan branch wasn't named: $output"
  [[ "$output" == *"lost"* ]] || fail "the orphan wasn't named: $output"
  # Read-only, all three ways it could have "helped".
  [ -d "$dir" ] || fail "doctor removed the husk"
  git -C "$main" show-ref -q --verify refs/heads/worktree-lost || fail "doctor deleted the orphan branch"
  [ "$(reg_rows)" -eq 1 ] || fail "doctor mutated the registry: $(cat "$REG")"
  # And every REMEDY is one of scruff's own verbs. Sending a user to `git
  # worktree remove` is invariant 2 defeated from the outside, so no line the
  # report offers as a fix may name it — the prose above such a line is free to
  # explain that a half-finished one is what caused the husk.
  [[ "$output" == *"scruff husk"* ]] || fail "no remedy for the husk: $output"
  run bash -c "printf '%s\n' \"\$0\" | grep '→' | grep -c 'git worktree' || true" "$output"
  [ "$output" = 0 ] || fail "a remedy line pointed at raw git"
}

@test "doctor --json: the envelope header, no 'lanes' key, and false is not null" {
  local main; main="$(mkrepo alpha)"; mkwt "$main" sparkle >/dev/null
  export FAKE_GH_UNAUTH=1
  cd "$main"; wt_run doctor --json
  [ "$status" -eq 0 ] || fail "$status / $output"
  [[ "$output" == *'"schema": 2'* ]] || fail "no schema counter: $output"
  [[ "$output" == *'"scruff":'* ]] || fail "no version key: $output"
  [[ "$output" == *'"warnings": []'* ]] || fail "no warnings channel: $output"
  # `lanes` in the frozen envelope is an ARRAY OF LANE OBJECTS. Doctor's counts
  # must not redefine that name, so they live under `summary`.
  [[ "$output" != *'"lanes": ['* ]] || fail "doctor redefined the frozen lanes key: $output"
  [[ "$output" == *'"summary":'* ]] || fail "no summary: $output"
  [[ "$output" == *'"live": 1'* ]] || fail "the lane counts are wrong: $output"
  # gh is installed and said no. That is FALSE — the case `gh auth login` fixes.
  [[ "$output" == *'"authenticated": false'* ]] || fail "an unauthenticated gh must read false: $output"
  [[ "$output" == *'"default_branch_via": "conventional"'* ]] || fail "$output"
}

@test "doctor --json: with no forge CLI at all, authenticated is null — not false" {
  # The nullable rule on a new fact. "Nobody to ask" and "asked, and it said no"
  # are different situations with different fixes, and every consumer bug in the
  # predecessor's status bar came from flattening exactly this distinction.
  local main; main="$(mkrepo alpha)"
  rm -f "$BIN/gh"
  # The rescue in path.go re-adds the profile bindir a real gh lives in, and the
  # caller's PATH has to lose gh too — same two moves as the codex test above.
  mkdir -p "$TMP/nogh"
  ln -sf "$(command -v git)" "$TMP/nogh/git"
  ln -sf "$(command -v cp)" "$TMP/nogh/cp"
  cd "$main"
  run env SCRUFF_PATH_RESCUE=0 PATH="$BIN:$TMP/nogh" "$WT" doctor --json
  [ "$status" -eq 0 ] || fail "a missing gh is a finding, not a failure: $status / $output"
  [[ "$output" == *'"authenticated": null'* ]] || fail "an absent gh must read null: $output"
  [[ "$output" == *'"available": false'* ]] || fail "$output"
}

@test "doctor --write: refuses, and names the layer it is waiting on" {
  cd "$TMP"; wt_run doctor --write
  [ "$status" -eq 1 ] || fail "want exit 1 (usage), got $status: $output"
  [[ "$output" == *".scruff.toml"* ]] || fail "the refusal didn't name what it can't write: $output"
}

# ── doctor / the base move ───────────────────────────────────────────────────
#
# 1.1.0 deleted the compat half of the rename; what remains of it here is the
# base move — the one scruff operation that relocates work on disk. The two
# tests this replaces proved the binary answered to both names; those are gone
# with internal/compat, and a stray `holt` symlink in $TMP/bin now means a
# stale build, which is exactly what the absent binary already meant.

@test "doctor: the base report names the legacy path and offers the move" {
  unset CLAUDE_WT_BASE
  mkdir -p "$HOME/.cache/claude-worktrees"
  : >"$HOME/.cache/claude-worktrees/registry.tsv"
  export REG="$HOME/.cache/claude-worktrees/registry.tsv"

  cd "$TMP"; wt_run doctor
  [ "$status" -eq 0 ]
  [[ "$output" == *"$HOME/.cache/claude-worktrees"* ]] || fail "the report didn't name the live base: $output"
  [[ "$output" == *"LEGACY"* ]] || fail "the report didn't say the path is legacy: $output"
  [[ "$output" == *"--migrate-base"* ]] || fail "the report didn't name the verb that moves it: $output"

  # An env override is a resolution worth naming too.
  export CLAUDE_WT_BASE="$TMP/wtbase"
  wt_run doctor
  [[ "$output" == *"CLAUDE_WT_BASE"* ]] || fail "the report didn't name the override: $output"
}

@test "doctor --migrate-base: refuses with exit 2 while a pane stands in the base" {
  unset CLAUDE_WT_BASE
  local main dir
  main="$(mkrepo alpha)"
  mkdir -p "$HOME/.cache/claude-worktrees"
  : >"$HOME/.cache/claude-worktrees/registry.tsv"
  dir="$(hook_create "$main" sparkle)"
  [ -d "$dir" ] || fail "create gave no checkout"
  export FAKE_LSOF_CWDS="$dir" FAKE_LSOF_CMD=node

  cd "$TMP"; wt_run doctor --migrate-base
  [ "$status" -eq 2 ] || fail "want exit 2 (refused for safety), got $status: $output"
  [[ "$output" == *"pid 4001 node"* ]] || fail "the refusal named no witness: $output"
  # Invariant 2 applied to scruff's own migration: nothing moved.
  [ -d "$HOME/.cache/claude-worktrees/alpha/sparkle" ]
  [ ! -d "$HOME/.cache/scruff" ]
}

@test "doctor --migrate-base: refuses when occupancy is unknown — the ground does not guess" {
  unset CLAUDE_WT_BASE
  mkdir -p "$HOME/.cache/claude-worktrees"
  : >"$HOME/.cache/claude-worktrees/registry.tsv"
  export FAKE_LSOF_BROKEN=1

  cd "$TMP"; wt_run doctor --migrate-base
  [ "$status" -eq 2 ] || fail "want exit 2 (uncertainty resolves to keep), got $status: $output"
  [[ "$output" == *"lsof"* ]] || fail "the refusal didn't name the missing evidence: $output"
  [ -d "$HOME/.cache/claude-worktrees" ]
  [ ! -d "$HOME/.cache/scruff" ]
}

@test "doctor --migrate-base: refuses under a base-path override — the default is the only thing it moves" {
  # The setup exports CLAUDE_WT_BASE, so this is also the every-test default.
  cd "$TMP"; wt_run doctor --migrate-base
  [ "$status" -eq 2 ] || fail "want exit 2, got $status: $output"
  [[ "$output" == *"CLAUDE_WT_BASE"* ]] || fail "the refusal didn't name the override: $output"
}

@test "doctor --migrate-base: moves the base, repairs the checkouts, leaves the old path a symlink" {
  unset CLAUDE_WT_BASE
  local main dir
  main="$(mkrepo alpha)"
  # A legacy registry must EXIST before the lane is created, or create would
  # write a fresh one under the scruff-named default and there'd be nothing to
  # move. REG follows, because the setup pointed it at the override.
  mkdir -p "$HOME/.cache/claude-worktrees"
  : >"$HOME/.cache/claude-worktrees/registry.tsv"
  export REG="$HOME/.cache/claude-worktrees/registry.tsv"
  dir="$(mkwt "$main" sparkle)"

  cd "$TMP"; wt_run doctor --migrate-base
  [ "$status" -eq 0 ] || fail "the move failed: $output"
  [[ "$output" == *"base moved"* ]] || fail "$output"

  # The registry is at the new base, with the new paths, and a .bak behind it.
  local newreg; newreg="$HOME/.cache/scruff/registry.tsv"
  [ -e "$newreg" ] || fail "no registry at the new base"
  [ -e "$newreg.bak.relocate" ] || fail "no .bak.relocate behind the rewrite"
  grep -q "$HOME/.cache/scruff/alpha/sparkle" "$newreg" || fail "registry paths were not rewritten: $(cat "$newreg")"
  ! grep -q "$HOME/.cache/claude-worktrees/" "$newreg" || fail "a stale path survived the rewrite"

  # The checkout moved with the tree, still a worktree of its repo, clean.
  local moved; moved="$HOME/.cache/scruff/alpha/sparkle"
  [ -e "$moved/.git" ] || fail "the checkout didn't move"
  [ "$(git -C "$moved" branch --show-current)" = worktree-sparkle ]
  # The link survived the move, in BOTH directions: the moved checkout still
  # resolves through a per-worktree gitdir under main's .git, and main's
  # worktree list names the moved path.
  case "$(git -C "$moved" rev-parse --git-dir)" in
    */.git/worktrees/*) ;;
    *) fail "the moved checkout doesn't resolve to a per-worktree gitdir: $(git -C "$moved" rev-parse --git-dir)" ;;
  esac
  git -C "$main" worktree list --porcelain | grep -q "^worktree $moved$" \
    || fail "main's worktree list doesn't know the moved checkout"
  [ -z "$(git -C "$moved" status --porcelain)" ] || fail "work was disturbed by the move"

  # The old path is a symlink to the new base, for one release.
  [ -L "$HOME/.cache/claude-worktrees" ] || fail "the legacy path is not a symlink"
  [ "$(readlink "$HOME/.cache/claude-worktrees")" = "$HOME/.cache/scruff" ]

  # And scruff still finds the lane without any env help at all — the
  # fallback's whole point. Then a second migrate is a no-op.
  wt_run list
  [[ "$output" == *"sparkle"* ]] || fail "scruff lost the lane after the move: $output"
  wt_run doctor --migrate-base
  [ "$status" -eq 0 ]
  [[ "$output" == *"nothing to move"* ]] || fail "$output"
}

@test "doctor --migrate-base: a lane whose link git can't repair degrades with exit 3, work intact" {
  unset CLAUDE_WT_BASE
  local main dir
  main="$(mkrepo alpha)"
  mkdir -p "$HOME/.cache/claude-worktrees"
  : >"$HOME/.cache/claude-worktrees/registry.tsv"
  export REG="$HOME/.cache/claude-worktrees/registry.tsv"
  dir="$(mkwt "$main" broken)"
  # Break the LINK, not the tree: deleting the checkout's .git pointer leaves
  # the work on disk but nothing for `git worktree repair` to re-point — the
  # shape of a link git has lost while the files survived.
  rm "$dir/.git"

  cd "$TMP"; wt_run doctor --migrate-base
  [ "$status" -eq 3 ] || fail "want exit 3 (degraded), got $status: $output"
  [ -e "$HOME/.cache/scruff/alpha/broken/work.txt" ] || fail "the work did not move with the tree"
  [[ "$output" == *"work moved with the tree"* ]] || fail "the degraded path didn't say the work is safe: $output"
  [ -L "$HOME/.cache/claude-worktrees" ] || fail "the move completed but the symlink is missing"
}

# ---------------------------------------------------------------------------
# skill — A3 of the family agent surface (the workshop's docs/agent-surface.md).
#
# The whole verb exists for a machine with scruff installed and no checkout of
# this repo, so every test here reads what the BINARY carries, never what is on
# disk beside the suite. $HOME is the fixture's, so auto-discovery finds no
# client unless a test builds one.
# ---------------------------------------------------------------------------

@test "skill: prints scruff's own SKILL.md, frontmatter first, on stdout" {
  run --separate-stderr "$WT" skill
  [ "$status" -eq 0 ] || fail "$output"
  [ "$(printf '%s\n' "$output" | head -1)" = "---" ] || fail "no frontmatter: $output"
  [[ "$output" == *"name: scruff"* ]] || fail "$output"
  # Data on stdout, per SPEC 2.3 — a caller pipes this straight into a file.
  [ -z "$stderr" ] || fail "skill wrote diagnostics to stdout's channel: $stderr"
}

@test "skill: a named skill is the sibling one, not the tool's own" {
  wt_run skill handoff
  [ "$status" -eq 0 ] || fail "$output"
  [[ "$output" == *"name: handoff"* ]] || fail "$output"
}

@test "skill: an unknown name is usage, and says what scruff does ship" {
  wt_run skill nope
  [ "$status" -eq 1 ] || fail "want exit 1, got $status: $output"
  [[ "$output" == *"handoff"* && "$output" == *"scruff"* ]] || fail "$output"
}

@test "skill install: writes EVERY skill, one directory per name" {
  wt_run skill install --dir "$TMP/skills"
  [ "$status" -eq 0 ] || fail "$output"
  # "Every" is the contract: a tool that installs only its own skill reaches no
  # standalone user with the second one.
  grep -q "name: scruff" "$TMP/skills/scruff/SKILL.md" || fail "scruff's own skill is missing"
  grep -q "name: handoff" "$TMP/skills/handoff/SKILL.md" || fail "the sibling skill is missing"
}

@test "skill install: re-running is a no-op, not a rewrite" {
  wt_run skill install --dir "$TMP/skills"
  [ "$status" -eq 0 ]
  wt_run skill install --dir "$TMP/skills"
  [ "$status" -eq 0 ] || fail "$output"
  [[ "$output" == *"0 written, 2 already current"* ]] || fail "$output"
}

@test "skill install: a file that exists and differs is refused, never overwritten" {
  wt_run skill install --dir "$TMP/skills"
  [ "$status" -eq 0 ]
  echo "someone edited this" >"$TMP/skills/handoff/SKILL.md"

  wt_run skill install --dir "$TMP/skills"
  # Exit 2 is scruff working: it declined to destroy an edit it did not make.
  [ "$status" -eq 2 ] || fail "want exit 2 (refused), got $status: $output"
  [ "$(cat "$TMP/skills/handoff/SKILL.md")" = "someone edited this" ] \
    || fail "the hand edit was clobbered"
  [[ "$output" == *"diff -"* ]] || fail "the refusal didn't say how to compare them: $output"
}

@test "skill install: a symlinked skill belongs to whatever manages it" {
  # This is the haus machine's shape: haus.ai.skill installs each skill as one
  # read-only directory symlink into the Nix store. Writing through it would
  # fail with EPERM, and an EPERM is not an explanation.
  mkdir -p "$TMP/skills" "$TMP/elsewhere"
  ln -s "$TMP/elsewhere" "$TMP/skills/scruff"

  wt_run skill install --dir "$TMP/skills"
  # Exit 0, not 2: the link is somebody else's install HOLDING, which is the
  # end state this verb wants. A refusal here would have every agent on a
  # normal haus machine — where every skill is such a link — report a broken
  # command and retry with force against a store path.
  [ "$status" -eq 0 ] || fail "want exit 0 (the end state holding), got $status: $output"
  [[ "$output" == *"symlink"* && "$output" == *"haus.ai.skill"* ]] || fail "$output"
  [ ! -e "$TMP/elsewhere/SKILL.md" ] || fail "it wrote through the symlink"
  # Per-file: the skill that wasn't linked still landed.
  grep -q "name: handoff" "$TMP/skills/handoff/SKILL.md" || fail "one skip stopped the other write"
  [[ "$output" == *"1 managed elsewhere"* ]] || fail "the summary did not count the link apart from a refusal: $output"
}

@test "skill install: a run that finds only symlinks says so, and is not a failure" {
  # The haus machine, whole: every skill is already a read-only symlink haus
  # put there.
  mkdir -p "$TMP/skills" "$TMP/store/scruff" "$TMP/store/handoff"
  ln -s "$TMP/store/scruff" "$TMP/skills/scruff"
  ln -s "$TMP/store/handoff" "$TMP/skills/handoff"
  wt_run skill install --dir "$TMP/skills"
  [ "$status" -eq 0 ] || fail "want exit 0, got $status: $output"
  [[ "$output" == *"nothing to install"* ]] || fail "$output"
  [[ "$output" == *"--dir"* ]] || fail "did not say how to place a copy elsewhere: $output"
  [[ "$output" != *"0 written"* ]] || fail "a count of zero where a sentence was owed: $output"
}

@test "skill install: no client on the machine is usage, not a silent success" {
  # $HOME is the fixture's and holds no client directory at all.
  wt_run skill install
  [ "$status" -eq 1 ] || fail "want exit 1, got $status: $output"
  [[ "$output" == *"--client"* ]] || fail "$output"
}

@test "skill install: --client resolves the client's own skills directory" {
  mkdir -p "$HOME/.codex"
  wt_run skill install --client codex
  [ "$status" -eq 0 ] || fail "$output"
  grep -q "name: scruff" "$HOME/.codex/skills/scruff/SKILL.md" || fail "not where codex reads"

  wt_run skill install --client emacs
  [ "$status" -eq 1 ] || fail "want exit 1, got $status: $output"
}

@test "skill install: a directory scruff cannot write into is refused per-file, not fatal" {
  # The same answer as a symlink — someone else owns this path — and per-file
  # for the same reason: auto-discovery walks four clients, and a read-only
  # FIRST one must not abandon the other three.
  mkdir -p "$TMP/readonly"
  chmod 555 "$TMP/readonly"
  wt_run skill install --dir "$TMP/readonly"
  chmod 755 "$TMP/readonly"   # so BATS_TEST_TMPDIR cleanup can remove it
  [ "$status" -eq 2 ] || fail "want exit 2 (refused), got $status: $output"
  [[ "$output" == *"left alone"* ]] || fail "an EPERM reached the user unexplained: $output"
}

@test "skill install: an empty --dir is an unset variable, not a request to install everywhere" {
  local unset_on_purpose=""
  wt_run skill install --dir "$unset_on_purpose"
  [ "$status" -eq 1 ] || fail "want exit 1, got $status: $output"
  [ ! -e "$HOME/.claude/skills" ] || fail "it fell through to the real client directories"
}

@test "skill install: a flag with no value, or an empty one, is usage before anything is written" {
  mkdir -p "$HOME/.claude" "$HOME/.codex"
  wt_run skill install --dir
  [ "$status" -eq 1 ] || fail "want exit 1, got $status: $output"
  [[ "$output" == *"--dir"* ]] || fail "$output"
  wt_run skill install --client
  [ "$status" -eq 1 ] || fail "want exit 1, got $status: $output"
  [[ "$output" == *"--client wants one of"* ]] || fail "$output"
  # An empty --client, let through, reaches the table as `dirs[""]` and is
  # refused as an "unknown client" — true, and not the sentence for what the
  # caller did.
  wt_run skill install --client ""
  [ "$status" -eq 1 ] || fail "want exit 1, got $status: $output"
  [[ "$output" == *"--client wants one of"* ]] || fail "$output"
  [ ! -e "$HOME/.claude/skills" ] && [ ! -e "$HOME/.codex/skills" ] \
    || fail "a valueless flag fell through to discovery"
}

@test "skill install: --dir and --client together name two destinations, so neither wins silently" {
  mkdir -p "$HOME/.codex"
  wt_run skill install --client codex --dir "$TMP/skills"
  [ "$status" -eq 1 ] || fail "want exit 1, got $status: $output"
  [ ! -e "$TMP/skills" ] || fail "it wrote to --dir and ignored --client"
  [ ! -e "$HOME/.codex/skills" ] || fail "it wrote to --client and ignored --dir"
}

@test "skill: --json says the envelope is reserved, not that the flag is wrong" {
  wt_run skill --json
  [ "$status" -eq 1 ] || fail "want exit 1, got $status: $output"
  [[ "$output" == *"14.5"* ]] || fail "a SPEC reader gets no hint it's reserved: $output"
}
