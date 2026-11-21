//go:build linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcus-beauregard/agent-runner/internal/policy"
)

// allowShell permits sh -c for one test. sh is deliberately absent from the
// allowlist; it is here because the ticket names that exact command, and the
// entry is removed when the test ends.
func allowShell(t *testing.T) {
	t.Helper()
	old, had := policy.Commands["sh"]
	policy.Commands["sh"] = policy.Command{Flags: []string{"-c"}}
	t.Cleanup(func() {
		if had {
			policy.Commands["sh"] = old
			return
		}
		delete(policy.Commands, "sh")
	})
}

// readPID waits for a command to write its pid to name, relative to root, and
// returns it.
func readPID(t *testing.T, root, name string) int {
	t.Helper()
	path := filepath.Join(root, name)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(b)))
			if convErr != nil {
				t.Fatalf("parse %s: %v", path, convErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file %s was never written", path)
	return 0
}

// processGone reports a pid absent from the process table, or present only as a
// zombie waiting to be reaped. Either way nothing is running.
func processGone(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 || i+2 >= len(b) {
		return false
	}
	return b[i+2] == 'Z'
}

// waitGone fails unless pid leaves the process table within the deadline.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d survived its command's process group", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The ticket's command: the shell backgrounds a child and exits 0. The child
// inherits the group, so the kill after the command reaches it. The command
// itself was not stopped, so exit code 0 is true and says nothing about the
// kill; the record has to name it on its own.
func TestCommandLeavesNoBackgroundChild(t *testing.T) {
	allowShell(t)
	r := newRegistry(t)
	led := &fakeLedger{}
	r.SetLedger(led)
	// The child's output is redirected so it cannot hold the capture pipe:
	// this is the clean exit 0 the field exists for, not the wait-delay
	// refusal that a child sharing stdout would produce.
	_, err := run(t, r, `sh -c "sleep 300 >/dev/null 2>&1 & echo $! > child.pid"`)
	if err != nil {
		t.Fatalf("command returned %v, want a clean exit", err)
	}
	pid := readPID(t, r.root, "child.pid")
	waitGone(t, pid)
	if len(led.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(led.events))
	}
	exit, ok := led.events[0].data.(commandExit)
	if !ok {
		t.Fatalf("event payload is %T, want commandExit", led.events[0].data)
	}
	if exit.ExitCode != 0 || exit.Killed != "" {
		t.Fatalf("recorded code %d, killed %q; want 0 and no kill", exit.ExitCode, exit.Killed)
	}
	if exit.Refused != "" {
		t.Fatalf("recorded refused %q, want a clean outcome", exit.Refused)
	}
	if !exit.Orphans {
		t.Fatal("record does not name the backgrounded child the group kill reached")
	}
}

func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	allowShell(t)
	r := newRegistry(t)
	led := &fakeLedger{}
	r.SetLedger(led)
	r.SetTimeout(toolRunCommand, 250*time.Millisecond)
	_, err := run(t, r, `sh -c "sleep 300 & echo $! > child.pid; echo $$ > self.pid; wait"`)
	wantErr(t, err, "command timed out")
	// The leader alone proves nothing: exec.CommandContext's default kill
	// satisfies that. The child is what makes this a test of the group.
	dead := readPID(t, r.root, "child.pid")
	pid := readPID(t, r.root, "self.pid")
	waitGone(t, pid)
	waitGone(t, dead)
	if len(led.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(led.events))
	}
	exit, ok := led.events[0].data.(commandExit)
	if !ok {
		t.Fatalf("event payload is %T, want commandExit", led.events[0].data)
	}
	if exit.Killed != "timeout" || exit.ExitCode != -1 {
		t.Fatalf("recorded killed %q, code %d; want timeout and -1", exit.Killed, exit.ExitCode)
	}
}

// A command whose child keeps the output pipe open fails with ErrWaitDelay.
// The process itself exited 0, so the record has to name the refusal rather
// than let exit_code 0 read as success.
func TestWaitDelayIsRecorded(t *testing.T) {
	allowShell(t)
	r := newRegistry(t)
	led := &fakeLedger{}
	r.SetLedger(led)
	_, err := run(t, r, `sh -c "sleep 300 & echo $! > child.pid; echo done"`)
	wantErr(t, err, "left a child holding its output")
	waitGone(t, readPID(t, r.root, "child.pid"))
	if len(led.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(led.events))
	}
	exit, ok := led.events[0].data.(commandExit)
	if !ok {
		t.Fatalf("event payload is %T, want commandExit", led.events[0].data)
	}
	if exit.Refused != "wait-delay" {
		t.Fatalf("recorded refused %q, want %q", exit.Refused, "wait-delay")
	}
}

func TestCancelKillsTheProcessGroup(t *testing.T) {
	allowShell(t)
	r := newRegistry(t)
	led := &fakeLedger{}
	r.SetLedger(led)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(250*time.Millisecond, cancel)
	defer cancel()
	_, err := r.Run(ctx, toolRunCommand, map[string]any{
		"command": `sh -c "sleep 300 & echo $! > child.pid; echo $$ > self.pid; wait"`,
	})
	if err == nil {
		t.Fatal("canceled command returned a nil error")
	}
	pid := readPID(t, r.root, "self.pid")
	dead := readPID(t, r.root, "child.pid")
	waitGone(t, pid)
	waitGone(t, dead)
	if len(led.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(led.events))
	}
	exit, ok := led.events[0].data.(commandExit)
	if !ok {
		t.Fatalf("event payload is %T, want commandExit", led.events[0].data)
	}
	if exit.Killed != "canceled" || exit.ExitCode != -1 {
		t.Fatalf("recorded killed %q, code %d; want canceled and -1", exit.Killed, exit.ExitCode)
	}
}

// A command the deadline stopped whose group kill then fails must record the
// failed kill too. Killed already says the group was signalled on purpose, so
// an empty Refused would let the record read as a containment that did not
// happen — on the one path where the child may still be alive.
func TestFailedGroupKillOnTimeoutIsRecorded(t *testing.T) {
	allowShell(t)
	r := newRegistry(t)
	led := &fakeLedger{}
	r.SetLedger(led)
	r.SetTimeout(toolRunCommand, 250*time.Millisecond)
	old := killGroup
	killGroup = func(*exec.Cmd) (bool, error) { return false, errors.New("operation not permitted") }
	t.Cleanup(func() { killGroup = old })
	_, err := run(t, r, `sh -c "sleep 300"`)
	wantErr(t, err, "command timed out")
	wantErr(t, err, "kill process group")
	if len(led.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(led.events))
	}
	exit, ok := led.events[0].data.(commandExit)
	if !ok {
		t.Fatalf("event payload is %T, want commandExit", led.events[0].data)
	}
	if exit.Killed != "timeout" {
		t.Fatalf("recorded killed %q, want %q", exit.Killed, "timeout")
	}
	if exit.Refused != "kill-failed" {
		t.Fatalf("recorded refused %q, want %q", exit.Refused, "kill-failed")
	}
	if exit.ExitCode != -1 {
		t.Fatalf("recorded exit code %d, want -1 for a stopped command", exit.ExitCode)
	}
	if exit.Orphans {
		t.Fatal("record claims orphans for a command the run killed")
	}
}
