package commands

import (
	"strings"
	"testing"
	"time"

	"github.com/hausfold/scruff/internal/exitcode"
)

// These cover RuntimeCmd's argv parsing and the pre-lookup checks in
// resolveRuntime — every case here returns before touching the registry, so a
// bare zero-value Env is enough. Lane resolution itself (matchLane, a real
// registry) is exercised end-to-end in the plan's manual verification, not
// here — this package has no existing fixture for a populated registry.
func TestRuntimeCmdUsageErrors(t *testing.T) {
	e := &Env{}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no args", nil, "scruff runtime <up|enter|down>"},
		{"unknown verb", []string{"sideways", "lane", "--backend", "tart"}, "scruff runtime <up|enter|down>"},
		{"missing lane name", []string{"up", "--backend", "tart"}, "name a lane"},
		{"unknown flag", []string{"up", "lane", "--nope"}, "unknown flag"},
		{"--backend with no value", []string{"up", "lane", "--backend"}, "needs an id"},
		{"two lane names", []string{"up", "one", "two", "--backend", "tart"}, "at most one lane name"},
		{"missing backend", []string{"up", "lane"}, "name a backend"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := e.RuntimeCmd(tc.args)
			if err == nil {
				t.Fatal("want a usage error, got nil")
			}
			if got := exitcode.Of(err); got != exitcode.Usage {
				t.Errorf("exit code = %d, want Usage (1); err = %v", got, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// The built-in tart backend's pure parts. The dance itself (clone, boot, wait
// for an address) needs a real `tart` and a real image and is verified by
// running it; what is testable here is every decision it makes BEFORE it
// shells out — which is also where its refusals live.
func TestTartNaming(t *testing.T) {
	if got := tartVM("my-lane"); got != "scruff-my-lane" {
		t.Errorf("tartVM = %q, want scruff-my-lane — the prefix is what keeps teardown off a VM scruff didn't create", got)
	}
	t.Setenv("SCRUFF_TART_USER", "")
	if got := tartUser(); got != "admin" {
		t.Errorf("tartUser = %q, want admin — every cirruslabs base image ships that account", got)
	}
	t.Setenv("SCRUFF_TART_USER", "ada")
	if got := tartUser(); got != "ada" {
		t.Errorf("tartUser = %q, want the override", got)
	}
}

func TestTartBaseRefusesRatherThanGuessing(t *testing.T) {
	t.Setenv("SCRUFF_TART_BASE", "")
	_, err := tartBase()
	if err == nil {
		t.Fatal("an unset SCRUFF_TART_BASE must refuse — the images are tens of GB and which one you want is a real choice")
	}
	if got := exitcode.Of(err); got != exitcode.Refused {
		t.Errorf("exit code = %d, want Refused (2); err = %v", got, err)
	}
	if !strings.Contains(err.Error(), "tart pull") {
		t.Errorf("the refusal must carry the command that fixes it, got %q", err.Error())
	}

	t.Setenv("SCRUFF_TART_BASE", "haus-golden")
	if got, err := tartBase(); err != nil || got != "haus-golden" {
		t.Errorf("tartBase = %q, %v; want haus-golden, nil", got, err)
	}
}

func TestRuntimeEject(t *testing.T) {
	e := &Env{}
	if err := e.RuntimeCmd([]string{"eject"}); err != nil {
		t.Errorf("bare eject should print the built-in: %v", err)
	}
	if err := e.RuntimeCmd([]string{"eject", "tart"}); err != nil {
		t.Errorf("eject tart should print the built-in: %v", err)
	}
	err := e.RuntimeCmd([]string{"eject", "apple-container"})
	if err == nil {
		t.Fatal("ejecting a backend that was never built in must be a usage error")
	}
	if got := exitcode.Of(err); got != exitcode.Usage {
		t.Errorf("exit code = %d, want Usage (1); err = %v", got, err)
	}
	if toml := tartAdapterTOML(); !strings.Contains(toml, "--no-graphics") || !strings.Contains(toml, "{{.Name}}") {
		t.Error("the ejected file must carry both the template vars and the flag that keeps the guest off the user's display")
	}
}

// The ejected skeleton is the reference a hand-written adapter copies, so the
// shape it shows is the shape the copies will have. hausfold/scruff#105 was an
// adapter that copied a bare `tart run … &`: the guest kept the caller's
// stdout for its whole life and `scruff runtime up … | tail` returned at
// teardown. Every load-bearing piece of the dance has to be ON the line.
func TestTartEjectShowsTheWholeDance(t *testing.T) {
	toml := tartAdapterTOML()
	for _, want := range []string{
		`>"$log" 2>&1 </dev/null &`,    // the redirect, on the backgrounded line itself
		"until ssh",                    // an address is not a shell
		"BatchMode=yes",                // the probe can never stop for a password
		"UserKnownHostsFile=/dev/null", // vmnet reuses addresses across clones
		"disown",                       // named, because it is the thing people reach for instead
	} {
		if !strings.Contains(toml, want) {
			t.Errorf("the ejected skeleton must show %q — a copy without it is the bug in #105 again", want)
		}
	}
}

// SCRUFF_TART_SSH_WAIT is read BEFORE the clone, so a bad value has to refuse
// as usage rather than fall back to a default silently — silent is how a
// typo'd `SCRUFF_TART_SSH_WAIT=30s` waits three minutes and nobody knows why.
func TestTartSSHWait(t *testing.T) {
	t.Setenv("SCRUFF_TART_SSH_WAIT", "")
	if got, err := tartSSHWait(); err != nil || got != tartSSHWaitDefault {
		t.Errorf("unset = %v, %v; want the default %v", got, err, tartSSHWaitDefault)
	}
	t.Setenv("SCRUFF_TART_SSH_WAIT", "45")
	if got, err := tartSSHWait(); err != nil || got != 45*time.Second {
		t.Errorf("45 = %v, %v; want 45s", got, err)
	}
	t.Setenv("SCRUFF_TART_SSH_WAIT", "0")
	if got, err := tartSSHWait(); err != nil || got != 0 {
		t.Errorf("0 = %v, %v; want 0 — one probe and then the verdict", got, err)
	}
	for _, bad := range []string{"30s", "-1", "soon"} {
		t.Setenv("SCRUFF_TART_SSH_WAIT", bad)
		_, err := tartSSHWait()
		if err == nil {
			t.Errorf("%q must refuse, not default", bad)
			continue
		}
		if got := exitcode.Of(err); got != exitcode.Usage {
			t.Errorf("%q: exit code = %d, want Usage (1); err = %v", bad, got, err)
		}
		if !strings.Contains(err.Error(), bad) {
			t.Errorf("%q: the refusal must quote the value it refused, got %q", bad, err.Error())
		}
	}
}
