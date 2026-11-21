//go:build linux

package tools

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus-beauregard/agent-runner/internal/policy"
	"github.com/marcus-beauregard/agent-runner/internal/sandbox"
)

// probeName is the copied test binary the fixture runs as its attacker. It is
// spelled relative on purpose: the registry starts every command in its root
// and the container starts it in /work, so "./probe" names the same file in
// both, and exec resolves it after the child's chdir.
const probeName = "./probe"

// probeEnv carries the listener address to the copied binary.
const probeEnv = "CAGE_PROBE_ADDR"

// TestContainerChildHasNoEgress is #58's third attack in the half this ticket
// can assert: a child inside the cage cannot open a connection to a listener
// outside it, while the same child on the host can.
//
// The pair is what makes it a test. "Unreachable inside a container" alone
// passes on a host with no network at all, so the control run proves the
// connection lands before the isolation is applied and fails after it.
//
// The listener is on loopback and the probe is the test binary itself, so the
// fixture needs no internet, no DNS and no tool the host may not have. The
// container's loopback is its own: a listener on the host's 127.0.0.1 is not
// reachable from inside the new namespace even though the address looks the
// same, which is exactly the distinction under test.
func TestContainerChildHasNoEgress(t *testing.T) {
	containerOrSkip(t)
	root, runDir := containerRoots(t)
	copyProbe(t, root)
	allowProbe(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Setenv(probeEnv, ln.Addr().String())

	// The control: worktree mode is the host, and the attack has to land
	// there, or the container run afterwards proves nothing.
	host, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, host, probeName+" -test.run=TestCageEgressProbe")
	if err != nil {
		t.Fatalf("control probe on the host: %v", err)
	}
	if !strings.HasPrefix(out, "connected") {
		t.Fatalf("control probe did not reach the listener: %q", out)
	}

	caged, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := caged.SetIsolation(Isolation{Mode: sandbox.Container, RunDir: runDir}); err != nil {
		t.Fatalf("SetIsolation: %v", err)
	}
	out, err = run(t, caged, probeName+" -test.run=TestCageEgressProbe")
	if err != nil {
		t.Fatalf("probe inside the container: %v", err)
	}
	if !strings.HasPrefix(out, "unreachable:") {
		t.Fatalf("the container child reached a host listener: %q", out)
	}
}

// TestCageEgressProbe is the child half of the egress fixture. It dials the
// address in CAGE_PROBE_ADDR and prints its verdict, exiting 0 either way: the
// caller reads the word, because a refused connection and a broken probe would
// otherwise both arrive as a failed command with its output thrown away.
//
// Run as part of the ordinary suite it is not the probe and skips.
func TestCageEgressProbe(t *testing.T) {
	addr := os.Getenv(probeEnv)
	if addr == "" {
		t.Skip("not the probe")
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		fmt.Printf("unreachable: %v\n", err)
		return
	}
	conn.Close()
	fmt.Println("connected")
}

// copyProbe puts the running test binary into the registry's root, where the
// /work mount carries it into the container.
func copyProbe(t *testing.T, dir string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("locate the test binary: %v", err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read the test binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, strings.TrimPrefix(probeName, "./")), b, 0o755); err != nil {
		t.Fatalf("write the probe: %v", err)
	}
}

// allowProbe lets the registry launch the probe. A test owns the allowlist the
// same way allowShell does: this fixture is the one caller that needs a binary
// the product does not ship.
func allowProbe(t *testing.T) {
	t.Helper()
	old, had := policy.Commands[probeName]
	policy.Commands[probeName] = policy.Command{Flags: []string{"-test.run"}}
	t.Cleanup(func() {
		if had {
			policy.Commands[probeName] = old
			return
		}
		delete(policy.Commands, probeName)
	})
}
