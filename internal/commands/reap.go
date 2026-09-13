package commands

import (
	"github.com/hausfold/scruff/internal/ui"
)

// Reap sweeps every LANDED lane now — parked ones, plus clean, landed checkouts
// that NO pane is sitting in.
//
// This is the idempotent backstop for when a pane ends WITHOUT firing the remove
// hook (a manual close, a reboot, a crash), and for `scruff child` checkouts,
// which the hook never reaps.
//
// `deadEnds` additionally retires the lanes NOTHING will ever land — PR closed
// unmerged, or repo archived — which the plain sweep only names. It is a flag
// rather than the default because of the asymmetry SPEC.md §6.4b rests on: a
// sweep that runs by itself may only ever take LANDED work, because rejected
// commits are still commits and deleting them unasked is the one thing scruff
// exists never to do. The flag is a human saying the word, exactly as typing
// `scruff drop <name>` is — it just says it once instead of once per lane, and
// every lane it takes goes through `drop`'s own refusals and leaves its undo
// line in `scruff reaped`.
func (e *Env) Reap(deadEnds bool) error {
	// A sweep that DELETES branches must ask the forge fresh. The listing's
	// 2-minute memo is right for an annotation and wrong here: a PR merged 30
	// seconds ago should reap on this run, and a PR reopened 30 seconds ago must
	// not. It is also what keeps --dead-ends honest: a closed PR REOPENED a
	// minute ago is no longer a dead end, and this is the run that would
	// otherwise delete it on a stale answer.
	cacheTTL = 0

	res := e.reapSweep(sweepAll)

	// res.Degraded needs no line of its own — Env.Warn already said it out loud
	// on the way past, and saying it twice reads as two different problems.
	for _, name := range res.Reaped {
		ui.Say("reaped %s", name)
	}
	for _, note := range res.SkippedLive {
		ui.Say("kept %s", note)
	}
	for _, note := range res.Dirty {
		ui.Say("kept %s", note)
	}
	for _, note := range res.Relanded {
		ui.Say("kept %s", note)
	}
	for _, note := range res.Diverged {
		ui.Say("kept %s", note)
	}
	for _, note := range res.Unlanded {
		ui.Say("kept %s", note)
	}
	dropped := 0
	if deadEnds {
		dropped = e.retireDeadEnds(res.DeadEnds)
	} else {
		for _, d := range res.DeadEnds {
			ui.Say("kept %s", d.Note())
		}
	}
	for _, s := range res.Strays {
		ui.Say("dangling checkout — git lost the link; `scruff <name>` moves it aside and rebuilds: %s", s)
	}
	if len(res.Reaped) == 0 && dropped == 0 {
		// Only when nothing above spoke. The old unconditional line listed the
		// three reasons in the abstract right after naming the concrete one,
		// which read as a second, contradictory verdict.
		spoke := len(res.SkippedLive) + len(res.Dirty) + len(res.Relanded) +
			len(res.Diverged) + len(res.Unlanded) + len(res.DeadEnds) + len(res.Strays)
		if spoke == 0 {
			ui.Say("nothing to reap — every lane is either unmerged, dirty, or in use.")
		} else {
			ui.Say("nothing reaped — see above for what held each lane back.")
		}
	}
	return nil
}

// retireDeadEnds drops every lane a sweep named as unlandable, and reports how
// many it took.
//
// The sweep asks its occupancy and dirt questions BEFORE the dead-end one, so a
// lane that reaches here was clean and unheld a moment ago: `retire`'s own
// refusals are the race, not the common path — a pane opened, or a file
// written, in between. They still have to be survivable, and one of them must
// not end the run: with several dead ends open, the refused lane is exactly the
// one you want reported rather than thrown. So it prints as a `kept` line like
// everything else this sweep declined, and the command still exits 0 — a lane
// kept is reap's ordinary output, never its failure.
func (e *Env) retireDeadEnds(ends []deadEndLane) int {
	n := 0
	for _, d := range ends {
		// The reason goes first, and on its own line. `retire` prints the SHA
		// and the undo, but not WHY the lane was taken — and "dropped
		// rejected (alpha)" with no cause attached is the message a person
		// reads a month later and cannot check.
		ui.Say("dead end: %s — %s", d.lane.Label(), d.why)
		if err := e.retire(d.lane); err != nil {
			ui.Say("kept %s — %s", d.lane.Label(), err)
			continue
		}
		n++
	}
	return n
}
