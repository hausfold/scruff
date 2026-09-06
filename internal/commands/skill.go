package commands

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	scruff "github.com/hausfold/scruff"
	"github.com/hausfold/scruff/internal/exitcode"
	"github.com/hausfold/scruff/internal/ui"
)

// `scruff skill` is A3 of the family agent surface (the workshop's
// docs/agent-surface.md): the tool hands an agent its own instructions, so a
// user on a machine with scruff installed and no checkout of this repo can say
// "teach my agent scruff" and have it work.
//
// ⚠️ This verb settles a naming collision, and the losing name is written down
// so nobody re-opens it by accident. SPEC.md §14.5 reserved the same capability
// as `scruff docs agent [--format=md|json]` with a `{version, body}` envelope —
// a different verb and a different shape for one job. `skill` won because five
// tools in this family answer to it and one spelling beats a private one; the
// envelope is orthogonal and can still arrive as `scruff skill --json` when an
// embedder needs to detect drift. §14.5 says so now rather than the reverse.
//
// The skills are EMBEDDED (../../skills.go), so what this prints is always the
// revision of the prose that shipped with this binary.

// skillDoc is one embedded skill: the name a client routes on, and where it
// lives inside the embedded tree.
type skillDoc struct {
	Name string
	Path string
}

// skillDocs walks the embedded ai/ tree rather than listing what it expects to
// find, for the reason script/check-skills.sh gives at length: a hardcoded list
// here would be a third place to forget a new skill, and the one that reaches
// standalone users.
//
// The layout is the standard's: ai/SKILL.md is the tool's own skill, named for
// the tool, and every ai/<name>/SKILL.md beside it is named for its directory.
func skillDocs() ([]skillDoc, error) {
	docs := []skillDoc{}
	if _, err := scruff.Skills.Open("ai/SKILL.md"); err == nil {
		docs = append(docs, skillDoc{Name: "scruff", Path: "ai/SKILL.md"})
	}
	entries, err := fs.ReadDir(scruff.Skills, "ai")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := "ai/" + entry.Name() + "/SKILL.md"
		if _, err := scruff.Skills.Open(path); err != nil {
			continue
		}
		docs = append(docs, skillDoc{Name: entry.Name(), Path: path})
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Name < docs[j].Name })
	return docs, nil
}

func skillNames(docs []skillDoc) string {
	names := make([]string, len(docs))
	for i, d := range docs {
		names[i] = d.Name
	}
	return strings.Join(names, ", ")
}

// skillClientDirs is where each agent client reads its skills from. The ids are
// agent.go's, deliberately: a client scruff can OPEN a lane in is a client whose
// agent should know scruff exists.
//
// The layout inside is the same for all four — <dir>/<skill name>/SKILL.md —
// which is why only the directory differs here.
func skillClientDirs(home string) map[string]string {
	return map[string]string{
		"claude":   filepath.Join(home, ".claude", "skills"),
		"codex":    filepath.Join(home, ".codex", "skills"),
		"opencode": filepath.Join(home, ".config", "opencode", "skills"),
		"pi":       filepath.Join(home, ".pi", "agent", "skills"),
	}
}

// skillClientOrder keeps the auto-discovery and every message in one stable
// order; ranging a map would shuffle them between runs.
var skillClientOrder = []string{"claude", "codex", "opencode", "pi"}

// Skill dispatches the `skill` surface.
//
//	scruff skill                 print scruff's own SKILL.md
//	scruff skill <name>          print one of the others (handoff)
//	scruff skill install         write ALL of them into every client found
//	scruff skill install --client claude|codex|opencode|pi
//	scruff skill install --dir PATH
func (e *Env) Skill(args []string) error {
	docs, err := skillDocs()
	if err != nil {
		return exitcode.Usagef("scruff ships no readable skills: %v", err)
	}

	if len(args) > 0 && args[0] == "install" {
		return e.skillInstall(args[1:], docs)
	}

	name := "scruff"
	switch {
	case len(args) == 0:
	// SPEC.md §14.5 advertises this spelling for the `{version, body}` envelope
	// an embedder diffs against its spliced-in copy. It is reserved, not built,
	// and someone who read the SPEC deserves to be told which of the two it is
	// rather than handed the generic usage line.
	case args[0] == "--json":
		return exitcode.Usagef("scruff skill --json is reserved for the {version, body} envelope (SPEC.md §14.5) and is not built yet — `scruff skill` prints the Markdown")
	case len(args) == 1 && !strings.HasPrefix(args[0], "-"):
		name = args[0]
	default:
		return exitcode.Usagef("usage: scruff skill [<name>] | scruff skill install [--client <id>] [--dir <path>]")
	}

	for _, d := range docs {
		if d.Name != name {
			continue
		}
		body, err := scruff.Skills.ReadFile(d.Path)
		if err != nil {
			return exitcode.Usagef("skill %q is embedded but unreadable: %v", name, err)
		}
		// Straight to stdout, unformatted: this is DATA under SPEC.md §2.3, and
		// a SKILL.md full of `%` in its examples is not a format string.
		os.Stdout.Write(body)
		return nil
	}
	return exitcode.Usagef("no skill named %q — scruff ships: %s", name, skillNames(docs))
}

// skillInstall writes every skill into every target.
//
// "Every skill" is the standard's word: a tool that ships a second skill and
// installs only its first reaches no standalone user with it, and scruff's
// second (handoff) is the one teaching the thing that has no verb.
//
// It never clobbers, and the two ways it declines are different answers. A
// file that is already there and DIFFERENT is somebody's edit, and a file
// scruff cannot write is a directory somebody else owns: both are the caller's
// request not honoured, reported by name, and either one makes the whole run
// exit 2 (Refused) so the caller learns it was only partly honoured. A file
// behind a SYMLINK belongs to whatever manages that link — on a haus machine,
// `haus.ai.skill` put it there and the target is read-only anyway — and that
// is the end state holding, not a refusal: it is named and left alone, and a
// run that finds only symlinks says so and exits 0. A non-zero there would
// have every agent on a normal haus machine report a broken command and retry
// with more force, against a store path. The three rules are A3 of the
// workshop's docs/agent-surface.md.
func (e *Env) skillInstall(args []string, docs []skillDoc) error {
	var dir, client string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dir":
			if i+1 >= len(args) {
				return exitcode.Usagef("--dir wants a path")
			}
			// An empty value is an unset shell variable, not a request. Falling
			// through to auto-discovery there would install into all four real
			// client directories while the caller believed it was writing to a
			// scratch path.
			if args[i+1] == "" {
				return exitcode.Usagef("--dir was given an empty path")
			}
			dir, i = args[i+1], i+1
		case "--client":
			// The same unset-variable trap as --dir. Let through, `dirs[""]`
			// answers an empty value with "unknown client" — true, and the
			// wrong sentence for what the caller did.
			if i+1 >= len(args) || args[i+1] == "" {
				return exitcode.Usagef("--client wants one of: %s", strings.Join(skillClientOrder, ", "))
			}
			client, i = args[i+1], i+1
		default:
			return exitcode.Usagef("unknown flag %q — usage: scruff skill install [--client <id>] [--dir <path>]", args[i])
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return exitcode.Usagef("cannot resolve a home directory: %v", err)
	}
	dirs := skillClientDirs(home)

	// Two answers to "where", where only one can be honoured. Picking silently
	// would write somewhere the caller named a different path for.
	if dir != "" && client != "" {
		return exitcode.Usagef("--dir and --client both name a destination — pass one")
	}

	var targets []string
	switch {
	case dir != "":
		targets = []string{dir}
	case client != "":
		d, ok := dirs[client]
		if !ok {
			return exitcode.Usagef("unknown client %q (expected %s)", client, strings.Join(skillClientOrder, ", "))
		}
		targets = []string{d}
	default:
		// Discovered, not assumed: a client's skills directory may not exist
		// yet, but its home does the moment the client has ever run. Creating
		// ~/.codex on a machine with no codex would be scruff inventing a
		// client rather than serving one.
		for _, id := range skillClientOrder {
			if _, err := os.Stat(filepath.Dir(dirs[id])); err == nil {
				targets = append(targets, dirs[id])
			}
		}
	}
	if len(targets) == 0 {
		return exitcode.Usagef("no agent client found under %s — name one with --client, or a path with --dir", home)
	}

	// `managed` is counted apart from `left` because the two are opposite
	// answers wearing the same word: a symlink is the desired end state
	// holding, a file that differs is the request not honoured, and only the
	// second may reach the exit code.
	wrote, same, managed, left := 0, 0, 0, 0
	for _, target := range targets {
		for _, d := range docs {
			body, err := scruff.Skills.ReadFile(d.Path)
			if err != nil {
				return exitcode.Usagef("skill %q is embedded but unreadable: %v", d.Name, err)
			}
			dest := filepath.Join(target, d.Name, "SKILL.md")

			// Either level being a symlink means someone else owns this name.
			// haus installs the DIRECTORY as one link into the Nix store, so
			// checking only the file would write through into the store — or,
			// more likely, fail with EPERM and make the user work out why.
			if isSymlink(filepath.Join(target, d.Name)) || isSymlink(dest) {
				ui.Say("left alone %s — a symlink, so something else manages it (on a haus machine, haus.ai.skill already did)", dest)
				managed++
				continue
			}
			if existing, err := os.ReadFile(dest); err == nil {
				if bytes.Equal(existing, body) {
					same++
					continue
				}
				ui.Warn("left alone %s — it exists and differs; compare it with: scruff skill %s | diff - %s", dest, d.Name, dest)
				left++
				continue
			}
			// A directory scruff cannot write into is the SAME answer as a
			// symlink it must not write through — someone else owns this path —
			// and it has to be per-file for the same reason. Returning here
			// would abandon the three clients after this one on a machine whose
			// FIRST client directory is read-only, and hand the user the bare
			// EPERM this verb exists to replace with a sentence.
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				ui.Warn("left alone %s — cannot create its directory: %v", dest, err)
				left++
				continue
			}
			if err := os.WriteFile(dest, body, 0o644); err != nil {
				ui.Warn("left alone %s — cannot write it: %v", dest, err)
				left++
				continue
			}
			ui.Say("wrote %s", dest)
			wrote++
		}
	}

	// The whole run was somebody else's install holding. Say that in a
	// sentence rather than as a count of zero, and exit 0: nothing was asked
	// for that does not already exist.
	if wrote == 0 && same == 0 && left == 0 && managed > 0 {
		ui.Say("nothing to install: every skill here is already a symlink something else manages (on a haus machine, haus.ai.skill)")
		ui.Say("to place a copy somewhere nothing else manages: scruff skill install --dir <path>")
		return nil
	}
	ui.Say("skills: %d written, %d already current, %d managed elsewhere, %d left alone", wrote, same, managed, left)
	if left > 0 {
		return exitcode.Refusedf("%d skill file(s) left alone — nothing was overwritten", left)
	}
	return nil
}

// isSymlink reports whether path is a symlink, without following it.
func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}
