# thanks

## founding testers

The people who run scruff before it is public, on their own machines, with no
help from me while they do it. Each one chooses how they appear here, and the
order is the order they report in. It stays that way.

*The early alpha hasn't started yet.*

What a founding tester gets, and the limits on it, are written down once:
[FOUNDING.md](https://github.com/hausfold/workshop/blob/main/FOUNDING.md).

## standing on

### git, first

scruff does not implement worktrees. `git worktree` landed in Git 2.5 in 2015
and does the hard part; scruff owns the life around it, which is the part
nobody had claimed. Every lane is a real branch and a real checkout, readable
by plain `git worktree list` on a box with no scruff on it. `gh` is optional
and load-bearing when present: whether a branch is safe to reap comes down to
what it answers.

### what scruff imports

| | |
|---|---|
| [Go](https://go.dev) | One binary, no runtime to install, which is why `go install` is a door |
| [fsnotify](https://github.com/fsnotify/fsnotify) | What makes `scruff watch --json` a stream rather than a poll. It sees the registry file change; the disk state it cannot see is what the periodic rescan is for |
| [snug](https://github.com/hausfold/snug) | Every row, column and colour scruff puts on a terminal |

### and reached through snug

| | |
|---|---|
| [x/ansi](https://pkg.go.dev/github.com/charmbracelet/x/ansi) | Escape sequences, and the width measurement underneath every table |
| [displaywidth](https://github.com/clipperhouse/displaywidth) | Column widths that survive emoji, CJK and combining marks, which is most of what makes a table line up |
| [uax29](https://github.com/clipperhouse/uax29) | The Unicode segmentation underneath that |
| [go-runewidth](https://github.com/mattn/go-runewidth) | In the graph, on the older width path |
| [go-colorful](https://github.com/lucasb-eyer/go-colorful) | In the graph, inside x/ansi |
| [x/term](https://pkg.go.dev/golang.org/x/term) and [x/sys](https://pkg.go.dev/golang.org/x/sys) | Terminal detection, and the syscalls under it |

None of these projects asked to be part of this. If you find scruff useful,
some of that belongs upstream.
