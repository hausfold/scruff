package commands

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hausfold/scruff/internal/exitcode"
	"github.com/hausfold/scruff/internal/ui"
)

// The built-in `tart` backend — the one runtime adapter scruff ships rather than
// reads from a file.
//
// Every other backend is a `~/.config/scruff/adapters/runtime/<id>.toml` the
// caller writes, and that stays the mechanism: this file is one adapter's worth
// of default, not a new seam. It exists because of what §5.5's three-argv
// contract costs the person who has nothing yet. A container backend is one
// `container run -d` per verb and fits the TOML exactly; tart does not — the
// setup step is clone, boot headless in the background, wait for an IP, then
// wait for a shell on it, which is four commands and two loops. So the first
// standalone user's route to "give this lane its own macOS" was: read SPEC.md
// §5.5, discover the argv slots can't hold a multi-step dance, write a shell
// script, write a TOML pointing at it, THEN run the verb. Every one of them
// would write the same script. scruff already knows how it goes.
//
// So `--backend tart` works with no config on any machine that has `tart`
// installed, and a TOML with that id still wins if one exists — which is how a
// packager (haus writes exactly this dance as a script + TOML) or anyone who
// wants a different image, user or share keeps overriding it. `scruff runtime
// eject tart` prints the equivalent TOML to start from.
//
// Deliberately still explicit: nothing here runs from create or reap. A lane
// gets a VM when somebody asks for one, the same as before.

// tartVM is the guest name for a lane. `scruff-` prefixed so `tart list` says
// which VMs are lanes and which are the user's own, and so teardown can never
// delete something scruff didn't create.
func tartVM(name string) string { return "scruff-" + name }

// tartUser is the guest account `enter` sshes in as. cirruslabs' macOS base
// images all ship `admin`; anything else is a custom image, hence the override.
func tartUser() string {
	if u := os.Getenv("SCRUFF_TART_USER"); u != "" {
		return u
	}
	return "admin"
}

// tartBase is the image a lane's VM is cloned from, and the one thing scruff
// refuses to guess. The images are tens of GB and which one you want is a real
// choice — a bare macOS base boots and tests nothing, an image with your own
// stack baked in is the point — so an unset variable gets the two commands
// that fix it rather than a surprise download.
func tartBase() (string, error) {
	if b := os.Getenv("SCRUFF_TART_BASE"); b != "" {
		return b, nil
	}
	return "", exitcode.Refusedf(
		"set SCRUFF_TART_BASE to the image lanes clone from — either one you have already (`tart list`), or:\n" +
			"  tart pull ghcr.io/cirruslabs/macos-tahoe-base:latest\n" +
			"  export SCRUFF_TART_BASE=ghcr.io/cirruslabs/macos-tahoe-base:latest\n" +
			"A bare base boots, but it has none of your stack in it — bake an image once and point this at that instead")
}

// tartSSHWaitDefault is how long setup gives a guest that has an address to
// start answering on it. A cirruslabs base reaches sshd about a minute after
// its DHCP lease on an M-series host; a bigger image on a loaded machine takes
// longer, and a wait that runs out is a message, not a hang, so it errs long.
const tartSSHWaitDefault = 180 * time.Second

// tartSSHWait is that bound, in seconds, as `SCRUFF_TART_SSH_WAIT`. Read
// before the clone: a value that isn't a number is refused up front rather
// than after tens of GB have been copied.
func tartSSHWait() (time.Duration, error) {
	v := os.Getenv("SCRUFF_TART_SSH_WAIT")
	if v == "" {
		return tartSSHWaitDefault, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, exitcode.Usagef("SCRUFF_TART_SSH_WAIT is seconds, and %q is not a number", v)
	}
	return time.Duration(n) * time.Second, nil
}

// tartAvailable is the same degrade every file-backed adapter gets from
// runtimeCommandError when its binary is missing (SPEC.md §2.4's exit 3):
// scruff did its half, the tool to do the rest isn't here, install it and the
// same command works.
func tartAvailable() error {
	if _, err := exec.LookPath("tart"); err != nil {
		return exitcode.Degradedf("tart is unavailable — `brew install cirruslabs/cli/tart` (or `nix profile install nixpkgs#tart`), then try again")
	}
	return nil
}

// tartSSHOpts go on every ssh into a guest — `enter` and setup's readiness
// probe alike — and both options are consequences of the clone being
// disposable, not of anything about security. vmnet hands the same
// 192.168.64.x addresses out again from one lane to the next, so an address is
// a DIFFERENT host, with a different key, every time a clone is made; an entry
// for it in the user's known_hosts turns the next lane's first ssh into a
// REMOTE HOST IDENTIFICATION HAS CHANGED refusal, and that wedge outlives the
// VM that caused it. So these hosts are never written to known_hosts at all.
// The far end is a VM this machine cloned itself, minutes ago, on a private
// bridge it also owns; there is nothing else that could be answering.
func tartSSHOpts() []string {
	return []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR"}
}

// tartSetup clones the base image and boots the clone headless with the lane's
// worktree shared in, then blocks until a shell answers on the guest's address.
//
// `--no-graphics` is not a preference. Without it `tart run` opens the guest's
// window on whatever display the caller is sitting at, full size — which for
// the agent lanes this backend exists for is precisely the interruption the VM
// was supposed to prevent. Headless, the guest still runs a full WindowServer
// and renders the real UI; it just renders it to nothing, and `screencapture`
// over ssh returns real pixels from it.
//
// `--dir` shares the worktree over virtiofs rather than copying it, matching
// scruff's reflink-not-copy bias (SPEC.md §6.3) for the same reason: a lane's
// tree is the thing being worked on, and a copy is a second truth.
func (e *Env) tartSetup(name, path string) error {
	if err := tartAvailable(); err != nil {
		return err
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		return exitcode.Degradedf("ssh is unavailable — setup needs it to tell a booted guest from an addressed one; install it, then try again")
	}
	base, err := tartBase()
	if err != nil {
		return err
	}
	wait, err := tartSSHWait()
	if err != nil {
		return err
	}
	// A parked lane matches by name but has no checkout: without this the
	// clone happens, `--dir` points at nothing, and the caller finds out sixty
	// seconds later as "never got an address".
	if path == "" {
		return exitcode.Usagef("%s has no checkout to share in — `scruff %s` to rebuild it first", name, name)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return exitcode.Usagef("%s is parked — its checkout %s isn't there. `scruff %s` rebuilds it, then try again", name, path, name)
	}

	vm := tartVM(name)
	if present, _ := tartExists(vm); present {
		return exitcode.Usagef("tart VM %s already exists — `scruff runtime down %s --backend tart` to reset it", vm, name)
	}

	ui.Say("cloning %s → %s …", base, vm)
	if err := runInherited([]string{"tart", "clone", base, vm}); err != nil {
		return runtimeCommandError("tart", "setup", name, "tart", err)
	}

	// Backgrounded and detached, not inherited: `tart run` blocks until the
	// guest STOPS, and setup is meant to return once the guest is reachable.
	//
	// ⚠️ The redirect is the load-bearing half of that, not the detach. This
	// child lives as long as the VM, and it keeps every fd it was handed for
	// that whole life. Give it scruff's own stdout and stderr and a caller that
	// reads setup through a pipe or a `$( )` — which is what an agent running
	// `scruff runtime up` always is — waits on a pipe the guest holds open for
	// hours, with the VM up and reachable the entire time and nothing anywhere
	// saying so; scruff itself exited long ago. (hausfold/scruff#105 measured
	// exactly that against an adapter that copied this dance with a bare `&`.)
	// So the guest's console goes to a log, and to a FILE rather than
	// /dev/null, because a guest that never takes an address explains itself
	// nowhere else.
	log, err := tartLog(vm)
	if err != nil {
		return tartOrphan(name, err)
	}
	run := exec.Command("tart", "run", vm, "--no-graphics", "--dir=work:"+path)
	run.Stdout, run.Stderr = log, log
	// Setsid so the guest outlives this process and does not take a ^C aimed
	// at the shell that started it.
	run.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := run.Start(); err != nil {
		log.Close()
		return tartOrphan(name, runtimeCommandError("tart", "setup", name, "tart", err))
	}
	_ = run.Process.Release()
	log.Close()

	ui.Say("booting %s, waiting for an address …", vm)
	ip, err := tartIP(vm, "60")
	if err != nil {
		return tartOrphan(name, exitcode.Usagef("%s booted but never got an address — %s says what happened", vm, tartLogPath(vm)))
	}
	// ⚠️ An address is not a shell. `tart ip` answers the moment the guest
	// takes a DHCP lease, which is most of a minute before sshd accepts
	// anything, so a setup that returned here would hand its caller an address
	// that refuses the first command sent to it — and the caller's loop reads
	// as a network problem rather than a guest that is still booting. Wait for
	// the thing the caller actually needs, and report the address only once
	// something on the far end has answered on it.
	if err := tartWaitSSH(name, vm, ip, wait); err != nil {
		return err
	}
	ui.Say("%s is up at %s — the lane is at /Volumes/My Shared Files/work inside it", vm, ip)
	ui.Out("scruff runtime enter %s --backend tart\n", name)
	return nil
}

// tartProbeWall bounds ONE ssh attempt, because `ConnectTimeout` bounds the
// TCP connect and nothing after it — and that gap is exactly where the
// readiness probe lives. macOS's sshd is launchd socket-activated, so early in
// a boot the connect succeeds instantly against a socket launchd is holding
// while the daemon behind it is not serving yet; a probe that stalls in the
// banner or auth exchange there has no client-side timeout to end it, and
// would sit past the deadline for as long as the guest felt like — a silent
// hang inside the wait that exists to prevent one.
const tartProbeWall = 15 * time.Second

// tartProbeGap is the pause between two probes that failed. Short, because
// the guest answers exactly once and every second after that is the caller's.
const tartProbeGap = time.Second

// tartProbe is one bounded, non-interactive ssh into the guest: true when a
// shell ran `true` and came back. Its stderr is kept, because `BatchMode`
// turns "this image wants a password" into a probe that can never succeed,
// and without the text nothing anywhere would say so.
func tartProbe(user, ip string) (ok bool, said string) {
	ctx, cancel := context.WithTimeout(context.Background(), tartProbeWall)
	defer cancel()
	argv := append(tartSSHOpts(), "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", user+"@"+ip, "true")
	cmd := exec.CommandContext(ctx, "ssh", argv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	said = strings.TrimSpace(stderr.String())
	if err != nil && ctx.Err() != nil && said == "" {
		said = fmt.Sprintf("the connect succeeded but nothing answered inside %s", tartProbeWall)
	}
	return err == nil, said
}

// tartWaitSSH probes until the guest at ip runs a command, or the wait runs
// out. Said out loud first, because a minute of `tart ip` and up to three of
// this with nothing on either stream is the same dead terminal the stdout
// leak produced — the symptom, arrived at honestly.
//
// On timeout the guest is deliberately left RUNNING so it can be looked at:
// it has an address, so the boot log and `tart ip` both still work. That is
// why the recovery named is `scruff runtime down`, whose stop is the force
// `tart delete` lacks — `tart delete` on its own refuses a running VM.
func tartWaitSSH(name, vm, ip string, wait time.Duration) error {
	ui.Say("waiting up to %ds for sshd on %s …", int(wait.Seconds()), ip)
	deadline := time.Now().Add(wait)
	for {
		ok, said := tartProbe(tartUser(), ip)
		if ok {
			return nil
		}
		if !time.Now().Before(deadline) {
			msg := fmt.Sprintf("%s is at %s but sshd did not answer inside %ds — %s says what happened", vm, ip, int(wait.Seconds()), tartLogPath(vm))
			if said != "" {
				msg += "\nthe last attempt said: " + said
			}
			return exitcode.Usagef("%s\nit is still running so it can be looked at — `scruff runtime down %s --backend tart` stops it and removes the clone", msg, name)
		}
		time.Sleep(tartProbeGap)
	}
}

// tartEnter sshes into an already-running guest, exec-replacing scruff the same
// way a file-backed adapter's `enter` argv does: an interactive session should
// own the terminal, and scruff has nothing left to do afterwards.
//
// No `BatchMode` and no `ConnectTimeout` here, on purpose: this is a person's
// shell. Batch mode could not ask for a password, and a five-second connect
// timeout is not a person's patience.
func (e *Env) tartEnter(name string) error {
	if err := tartAvailable(); err != nil {
		return err
	}
	vm := tartVM(name)
	ip, err := tartIP(vm, "10")
	if err != nil {
		return exitcode.Usagef("%s has no address — is it running? `scruff runtime up %s --backend tart` first", vm, name)
	}
	argv := append([]string{"ssh"}, tartSSHOpts()...)
	return execClient(append(argv, tartUser()+"@"+ip))
}

// tartTeardown stops and deletes the clone. Both steps tolerate a guest that
// is already gone: `down` is the command someone runs to make sure a VM isn't
// there, and failing because it already isn't would be answering the wrong
// question.
func (e *Env) tartTeardown(name string) error {
	if err := tartAvailable(); err != nil {
		return err
	}
	vm := tartVM(name)
	// Only "definitely not there" is a no-op. A `tart list` that failed means
	// scruff could not tell, and the honest move is to try the delete and let
	// tart answer, not to report success over a clone still on disk.
	if present, known := tartExists(vm); known && !present {
		ui.Say("no tart VM %s — nothing to tear down", vm)
		return nil
	}
	// `tart delete` takes no --force: it deletes a stopped VM and refuses a
	// running one, so stopping first IS the force. The stop is best-effort
	// because a VM that is already stopped makes it fail, which is not a
	// reason to leave the clone on disk.
	_ = exec.Command("tart", "stop", vm).Run()
	if err := runInherited([]string{"tart", "delete", vm}); err != nil {
		return runtimeCommandError("tart", "teardown", name, "tart", err)
	}
	return nil
}

// tartExists asks `tart list` rather than parsing `tart get`, and matches the
// name exactly — a substring match would see `scruff-api` in `scruff-api-two`.
//
// It returns whether it could TELL, separately from the answer, because the
// two callers want opposite things from a `tart list` that failed. Collapsing
// them into a bare false is how `down` ends up printing "nothing to tear down"
// and exiting 0 over a running guest and a tens-of-GB clone.
func tartExists(vm string) (present, known bool) {
	out, err := exec.Command("tart", "list", "--quiet").Output()
	if err != nil {
		return false, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == vm {
			return true, true
		}
	}
	return false, true
}

// tartIP blocks until the guest publishes an address or the wait runs out.
func tartIP(vm, wait string) (string, error) {
	out, err := exec.Command("tart", "ip", vm, "--wait", wait).Output()
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("tart ip %s returned nothing", vm)
	}
	return ip, nil
}

// tartLogPath is where a guest's console output lands: scruff's STATE dir, not
// $TMPDIR (because "why did my VM never come up" is asked hours later) and not
// the config dir (because config is a packager's to own — haus ships
// ~/.config/scruff as read-only symlinks into the nix store, and a log that
// wants to be written there fails, after the clone has already happened).
// Same resolver as every other piece of machine-global state, so SCRUFF_STATE
// moves this with the rest.
func tartLogPath(vm string) string {
	dir, _ := resolveStateDir()
	if dir == "" {
		return filepath.Join(os.TempDir(), vm+".log")
	}
	return filepath.Join(dir, "runtime", vm+".log")
}

// tartOrphan wraps a failure that happened AFTER the clone exists, so the
// message names the one command that cleans it up. A tens-of-GB clone nobody
// mentioned is how a disk fills up quietly.
func tartOrphan(name string, err error) error {
	return exitcode.Usagef("%v\nthe clone is still on disk — `scruff runtime down %s --backend tart` removes it", err, name)
}

func tartLog(vm string) (*os.File, error) {
	path := tartLogPath(vm)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, exitcode.Usagef("making %s: %v", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, exitcode.Usagef("opening %s: %v", path, err)
	}
	return f, nil
}

// tartAdapterTOML is what `scruff runtime eject tart` prints: the built-in,
// written out as the file that would replace it. Not the same shape as the
// code above — it CAN'T be, the argv slots hold one command each — so it hands
// over a script skeleton to fill in rather than pretending three lines cover
// it. That honesty is the point: someone ejecting has decided the default is
// wrong for them, and the fastest way to be wrong again is a TOML that looks
// complete and silently drops a step — or a skeleton that shows the dance
// with the redirect left off, which is how hausfold/scruff#105 happened: the
// adapter that copied it backgrounded `tart run` on the caller's stdout.
func tartAdapterTOML() string {
	return `# Save as ~/.config/scruff/adapters/runtime/tart.toml — a file with this id
# takes precedence over scruff's built-in tart backend.
#
# The setup step is a multi-command dance (clone, boot headless with the lane
# shared in, wait for an address, wait for a shell on it) and an argv slot
# holds ONE command, so point it at a script of your own. scruff's built-in is
# the reference for what that script has to do:
#
#   tart clone "$SCRUFF_TART_BASE" "scruff-$1"
#   tart run "scruff-$1" --no-graphics --dir="work:$2" >"$log" 2>&1 </dev/null &
#   ip=$(tart ip "scruff-$1" --wait 60)
#   until ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=no \
#             -o UserKnownHostsFile=/dev/null "admin@$ip" true; do sleep 2; done
#
# Four things on those lines are load-bearing, and each has been a bug:
#
#   --no-graphics    without it the guest's window opens on the display you
#                    are sitting at.
#   >"$log" 2>&1     ` + "`tart run`" + ` lives as long as the guest, and keeps every fd it
#                    was handed for that whole life. Backgrounded on YOUR
#                    stdout, a caller reading setup through a pipe or a $( ) —
#                    every agent — waits until the VM is torn down, hours
#                    later. ` + "`disown`" + ` does not help: it takes the job out of the
#                    shell's table, not the fds out of the child. A file, not
#                    /dev/null — a guest that never takes an address explains
#                    itself nowhere else.
#   the ssh loop     ` + "`tart ip`" + ` answers on a DHCP lease, most of a minute before
#                    sshd does. Return then and the caller's first ssh fails.
#   UserKnownHostsFile=/dev/null
#                    vmnet reuses 192.168.64.x across clones, so an entry there
#                    turns the next lane's ssh into REMOTE HOST IDENTIFICATION
#                    HAS CHANGED. Carry it on enter's ssh too.

kind     = "runtime"
id       = "tart"
setup    = ["/path/to/your/tart-adapter.sh", "setup", "{{.Name}}", "{{.Path}}"]
enter    = ["/path/to/your/tart-adapter.sh", "enter", "{{.Name}}"]
teardown = ["/path/to/your/tart-adapter.sh", "teardown", "{{.Name}}"]
`
}
