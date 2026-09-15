// Command scruff manages LANES — one coding agent's branch, checkout and pane —
// for any git repo.
//
// See SPEC.md for the design. The short version: scruff owns a lane's LIFECYCLE
// (create → live → parked → landed → reaped) and its safety invariants — never
// lose work, never reap something in use, keep the registry locked. What you do
// at each transition is yours.
//
// "lane" is deliberate, and the vocabulary is closed: `worktree` is git's
// checkout (a parked lane has none), `agent` is the CLIENT a lane runs, and
// `session` belongs to the multiplexer and to the clients. See SPEC.md §0.
package main

import (
	"os"

	"github.com/hausfold/scruff/internal/commands"
	"github.com/hausfold/scruff/internal/exitcode"
	"github.com/hausfold/scruff/internal/ui"
)

func main() {
	err := commands.Run(os.Args[1:])
	// An error with no message is a bare exit code (exitcode.Code): the verb
	// has said everything it had to, and the code is the answer.
	if err != nil && err.Error() != "" {
		ui.Fail(err.Error())
	}
	os.Exit(exitcode.Of(err))
}
