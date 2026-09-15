// Package ui is scruff's only writer of human-facing text.
//
// It is a thin adapter over [snug], which owns the palette, the glyphs and the
// folding. scruff names ROLES, never colours: the three xterm-256 indices this
// file used to carry were copied out of the bash `wt` during the cutover, and
// measured against nebelung they sat ΔE 21.8 / 22.3 / 27.4 from the tokens they
// were meant to be — with `say` resolving to blue, the one hue nebelung exists
// to strip out. snug resolves each role against the real palette and degrades it
// by terminal capability, so there is nothing here left to get wrong.
//
// The one hard rule survives the move unchanged, and it is a contract rather
// than a style choice (SPEC.md §2.3): stdout carries DATA only — the new
// checkout path from `create`/`child`, the JSON from `--json`. Every diagnostic,
// prompt and progress line goes to stderr, because callers do
// `cd "$(scruff child …)"` and Claude Code's WorktreeCreate hook reads the path off
// stdout. snug's Printer holds the same contract from its side: Say/Warn/Fail
// write to Err, and Data is the only thing that reaches Out.
package ui

import (
	"os"

	"github.com/hausfold/snug"
)

// printer is scruff's voice, taken once at startup as snug intends — the terminal
// is measured there, not per line.
var printer = snug.NewPrinter()

// Say prints an informational line to stderr.
func Say(format string, a ...any) { printer.Say(format, a...) }

// Warn prints a caution line to stderr.
func Warn(format string, a ...any) { printer.Warn(format, a...) }

// Fail prints an error line to stderr. It does not exit — main owns that, so
// that every path returns an error carrying its exit code.
//
// It takes a message rather than a format, because every caller already has one
// built; the "%s" is what keeps a `%` inside a branch name from being read as a
// verb.
func Fail(msg string) { printer.Fail("%s", msg) }

// Out prints to stdout. Reserve it for data.
func Out(format string, a ...any) { printer.Data(format, a...) }

// IsTTY reports whether f is a terminal. Callers use it to decide between
// exec-ing an interactive client and printing the command to run instead.
func IsTTY(f *os.File) bool { return snug.DetectTerm(f).IsTTY }

// A table is the one shape scruff draws that a format string cannot: `%-38s` is
// a width the terminal never agreed to. snug budgets the columns against the
// real window instead, sheds the slack by weight, and drops to stacked
// key/value when even the minimums do not fit — so the types below are ALIASES
// rather than wrappers. A caller names the same semantics snug documents
// (Min, Weight, Role, Cut) without importing it, and this file stays scruff's
// only mention of the library.
type (
	// Col describes one column's appetite: its label, the width below which
	// it stops being worth showing, and its share of whatever is left over.
	Col = snug.Col
	// Side is which end of a value a column gives up when it has to.
	Side = snug.Side
	// Role is what a column IS, not what colour it wears.
	Role = snug.Role
)

const (
	CutRight = snug.CutRight // a name: keep the front — `bump-flake-and-…`
	CutLeft  = snug.CutLeft  // a path: keep the tail — `…/internal/ui`
	CutNever = snug.CutNever // a count or a marker: it is right or it is absent
)

const (
	Body    = snug.Body    // ordinary text
	Muted   = snug.Muted   // secondary detail — clients, timestamps
	Subject = snug.Subject // the thing under discussion — a lane, a repo
)

// gutter is the family's table indent. It is wider than a single space on
// purpose: two cells read as a gap between columns, three as a margin.
const gutter = 3

// Table prints a report to STDOUT, budgeted to that stream.
//
// stdout because a listing is what the user ran the command for, not the tool
// talking about it (SPEC.md §2.3) — `scruff | less` carries the table whole
// while the narration stays on fd 2. It also means the width comes from the
// stream the table lands on: a pipe is never truncated, so a captured listing
// keeps every name at full length for whatever reads it next.
func Table(cols []Col, rows [][]string) {
	printer.PrintData(snug.Table{Cols: cols, Rows: rows, Indent: gutter, Header: true})
}

// The roles a report paints per CELL rather than per column, and the tag that
// carries one. `overlap` is the first table whose mark and filename change
// colour per row — amber where two lanes' edits share a hunk, muted where they
// only share a file — and a row built with the escapes already in it is what
// snug's Cell exists to replace: the padding would count them.
const (
	Caution = snug.Warn // stale, wants attention — the role a Warn line wears
	Err     = snug.Err  // failed, refused, conflicting
	Path    = snug.Path // a filesystem path
)

// Cell tags one cell with a role of its own, overriding its column's.
func Cell(r Role, s string) string { return snug.Cell(r, s) }

// Hint prints a next-step line to stderr, muted: what to do about what the
// report above just said.
func Hint(format string, a ...any) { printer.Hint(format, a...) }
