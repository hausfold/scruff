package commands

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/hausfold/scruff/internal/gitx"
	"github.com/hausfold/scruff/internal/occupancy"
)

// The --json envelope is a frozen public contract (SPEC.md §2.2): `bench`, the
// haus statusline and pounce's Spawn Agent command all pin it within a day
// of cutover. Field ADDITIONS are non-breaking and consumers must ignore unknown
// keys; removals and meaning changes are not.
//
// The array is `lanes`, not `worktrees`, because a parked entry has no checkout
// on disk at all — the branch is the durable artifact. `agent` inside each lane
// keeps its own meaning: the CLIENT (claude | codex | opencode | pi), never the lane.
//
// The nullable fields are the part that matters. `occupied`, `dirty` and `pr`
// are pointers so that "not determined" (no lsof, no forge, cache miss) is
// distinguishable from "false". Every consumer bug in the shell version's
// statusline came from conflating those two.

type jsonEnvelope struct {
	Scruff   string     `json:"scruff"`
	Schema   int        `json:"schema"`
	Lanes    []jsonLane `json:"lanes"`
	Warnings []string   `json:"warnings"`
}

type jsonLane struct {
	Name           string        `json:"name"`
	Repo           string        `json:"repo"`
	Main           string        `json:"main"`
	Branch         string        `json:"branch"`
	Path           string        `json:"path"`
	Parent         string        `json:"parent"`
	Chat           string        `json:"chat"`
	Agent          string        `json:"agent"`
	State          string        `json:"state"`
	Occupied       *bool         `json:"occupied"`
	OccupiedBy     []jsonHolder  `json:"occupied_by,omitempty"`
	Dirty          *bool         `json:"dirty"`
	Landed         jsonLanded    `json:"landed"`
	PostMergeAhead jsonPostMerge `json:"post_merge_ahead"`
	Last           string        `json:"last_commit"`
}

// jsonHolder is the evidence behind `occupied: true` — an ADDITION to the
// frozen envelope, and omitted entirely when nothing is standing there, so a
// consumer that never learns the key sees byte-identical output to before.
//
// It exists because `occupied` alone cannot be checked. A statusline showing a
// lane as busy for five days is either a long-running agent or a stray daemon
// nobody can see, and the two want opposite responses. `pid` is the whole
// point; `command` and `path` are what make it recognisable without a ps.
//
// `via` names the provider that saw it (`lsof` | `leases`), because an
// embedder's lease and a machine-wide cwd scan are different kinds of evidence
// and a consumer weighing them should not have to guess which it got.
type jsonHolder struct {
	PID     int    `json:"pid"`
	Command string `json:"command,omitempty"`
	Path    string `json:"path"`
	Via     string `json:"via"`
}

type jsonLanded struct {
	Verdict    string `json:"verdict"` // yes | no | fresh | contained
	Via        string `json:"via"`
	Confidence string `json:"confidence"`
}

type jsonPostMerge struct {
	Commits  int  `json:"commits"`
	PR       int  `json:"pr"`
	Diverged bool `json:"diverged"` // true: the tip isn't built on the merged PR — stale/sideways, not new work
}

func (e *Env) listJSON(rows []listRow) error {
	occ := e.Occupancy()

	out := jsonEnvelope{
		Scruff:   Version,
		Schema:   2,
		Lanes:    []jsonLane{},
		Warnings: []string{},
	}
	for _, r := range rows {
		out.Lanes = append(out.Lanes, e.toJSONLane(r, occ))
	}
	out.Warnings = append(out.Warnings, e.Warnings...)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// toJSONLane builds one lane's --json payload.
//
// `scruff watch` (SPEC.md §14.3 step 2) calls this too, and that reuse is load-
// bearing, not incidental: the constraint on that command is that event
// payloads carry this exact shape, not a parallel one an SDK would have to
// learn twice. occ is threaded in rather than recomputed here because a sweep
// scans it once and shares it across every lane — watch does the same on
// every rescan.
func (e *Env) toJSONLane(r listRow, occ occupancy.Report) jsonLane {
	entry := r.Entry
	slug, err := gitx.RemoteSlug(entry.Main)
	if err != nil {
		slug = "local/" + filepath.Base(entry.Main)
	}
	w := jsonLane{
		Name:           r.Name,
		Repo:           slug,
		Main:           entry.Main,
		Branch:         entry.Branch,
		Path:           entry.Path,
		Agent:          r.Agent,
		State:          string(entry.State),
		Last:           r.Last,
		PostMergeAhead: jsonPostMerge{Commits: r.Ahead, PR: r.AheadPR, Diverged: r.Diverged},
	}
	if row, ok := e.Reg.Find(entry.Path); ok {
		w.Parent = row.Parent
	}
	w.Chat = e.jsonChat(r.Agent, entry.Path)
	// true / false / null, and the three are genuinely different answers.
	// A lease asserts presence even when nothing on this machine can vouch
	// for absence, so "held" outranks "unknowable" — but the reverse never
	// happens, and an unvouched miss stays null rather than becoming false.
	switch holders := occ.Holders(entry.Path); {
	case len(holders) > 0:
		yes := true
		w.Occupied = &yes
		for _, h := range holders {
			w.OccupiedBy = append(w.OccupiedBy, jsonHolder{
				PID: h.PID, Command: h.Cmd, Path: h.Path, Via: h.Via,
			})
		}
	case occ.Known():
		no := false
		w.Occupied = &no
	}
	if entry.State == Live {
		dirty := gitx.Dirty(entry.Path)
		w.Dirty = &dirty
	}
	v := e.Landed(entry.Main, entry.Branch)
	w.Landed = jsonLanded{Verdict: "no", Via: v.Via, Confidence: v.Confidence}
	switch {
	case v.Landed && v.Via == "never-diverged":
		// Reapable like any other ancestor of the default branch — but nothing
		// landed, because nothing ever happened here. Its own word, so a
		// consumer can render a fresh lane as fresh instead of as `merged`,
		// which is what every one of them did while this shared "yes".
		//
		// Gated on Landed as well as on the via, because a `landed` HOOK names
		// its own via (hookVerdict) — a hook answering NO under this name must
		// not paint `fresh` on a lane that will never be reaped.
		w.Landed.Verdict = "fresh"
	case v.Landed:
		w.Landed.Verdict = "yes"
	case v.Via == "merge-tree-empty":
		// Advisory only: this cannot tell a squash merge from a branch that
		// never did anything, so `reap` never acts on it — a person who knows
		// the work is upstream takes the lane with `scruff drop`.
		w.Landed.Verdict = "contained"
	}
	return w
}
