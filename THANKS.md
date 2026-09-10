# Thanks

## Founding testers

The people who ran scruff before it was public, on their own machines, with no
help from me while they did it. Each one chose how they appear here, and the
order is the order they reported in. It stays that way.

*The early alpha hasn't started yet. This is where the list goes.*

What a founding tester gets, and the limits on it, are written down once:
[FOUNDING.md](https://github.com/hausfold/workshop/blob/main/FOUNDING.md).

## Standing on

### git, first

scruff does not implement worktrees. `git worktree` has been in git since 2015
and does the hard part; scruff owns the life around it, which is the part
nobody had claimed. Every lane is a real branch and a real checkout, readable
by any git that has ever existed.

### The Go side

| | |
|---|---|
| [Go](https://go.dev) | One static binary, no runtime to install, which is why `go install` is a door |
| [fsnotify](https://github.com/fsnotify/fsnotify) | How the registry notices a checkout moving underneath it |
| [go-runewidth](https://github.com/mattn/go-runewidth) and [displaywidth](https://github.com/clipperhouse/displaywidth) | Column widths that survive emoji, CJK and combining marks, which is most of what makes a table line up |
| [uax29](https://github.com/clipperhouse/uax29) | Unicode segmentation underneath that |
| [x/ansi](https://github.com/charmbracelet/x) | Escape-sequence handling, by way of [snug](https://github.com/hausfold/snug) |
| [go-colorful](https://github.com/lucasb-eyer/go-colorful) | Colour maths for the palette |
| [golang.org/x/term and x/sys](https://pkg.go.dev/golang.org/x) | Terminal detection and the syscalls under it |

None of these projects know we exist. If you find scruff useful, some of that
belongs upstream.
