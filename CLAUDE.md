# CLAUDE.md

@AGENTS.md

Claude-only wiring, mapped in [`.agents/README.md`](./.agents/README.md):
`SessionStart` → `.agents/setup.sh`, and the worktree hooks in
`~/.claude/settings.json` — haus-declared and re-asserted every rebuild, so a
hand-edit there is reverted; fix the hook in this repo.
