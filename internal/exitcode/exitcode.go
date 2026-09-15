// Package exitcode defines scruff's public exit-code contract (SPEC.md §2.4).
//
// The distinction that matters to every wrapper script is Usage (1) vs Refused
// (2): "you asked wrong" and "I declined to destroy something" are different
// answers, and the bash `wt` conflated them because it only had `die`.
package exitcode

import (
	"errors"
	"fmt"
)

const (
	OK       = 0 // success — including "nothing to do"
	Usage    = 1 // bad arguments, missing precondition, not a git repo
	Refused  = 2 // declined for safety: occupied, dirty, or not provably landed
	Degraded = 3 // completed, but a signal was unavailable (forge down, no lsof)
	Conflict = 4 // a finding, not an error: `scruff overlap` / `scruff batch`
	Locked   = 5 // another scruff holds the registry lock
)

// Error is an error carrying the exit code scruff should terminate with.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func newf(code int, format string, a ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

func Usagef(format string, a ...any) error    { return newf(Usage, format, a...) }
func Refusedf(format string, a ...any) error  { return newf(Refused, format, a...) }
func Degradedf(format string, a ...any) error { return newf(Degraded, format, a...) }
func Lockedf(format string, a ...any) error   { return newf(Locked, format, a...) }

// Code is an exit code with nothing left to say. The verb has already printed
// its report and the code IS the finding — `scruff overlap`'s 3 and 4 — so
// main prints no line for it: a ✗ under a report that is not a failure would
// be the tool contradicting itself.
func Code(code int) error { return &Error{Code: code} }

// Of reports the exit code an error should terminate with. Anything that isn't
// an *Error is a bug or an unexpected failure, which is Usage's bucket — never
// 0, and never one of the codes a wrapper acts on.
func Of(err error) int {
	if err == nil {
		return OK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Usage
}
