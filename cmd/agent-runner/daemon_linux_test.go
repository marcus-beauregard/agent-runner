//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcus-beauregard/agent-runner/internal/agent"
	"github.com/marcus-beauregard/agent-runner/internal/policy"
	runrec "github.com/marcus-beauregard/agent-runner/internal/run"
)

// daemonLLM is #58's second attacker: it asks for the exact command the ticket
// names — a shell that backgrounds a child and exits 0 — and then answers.
type daemonLLM struct{ asked bool }

var _ agent.LLM = (*daemonLLM)(nil)

func (d *daemonLLM) Complete(ctx context.Context, prompt string) (string, error) {
	if d.asked {
		return "DONE", nil
	}
	d.asked = true
	return `run_command {"command": "sh -c \"sleep 300 >/dev/null 2>&1 & echo $! > child.pid\""}`, nil
}

// allowShell permits sh -c for the fixture and removes the entry afterwards.
// sh is deliberately absent from the allowlist; the ticket names that exact
// command, and a model that could reach a shell in production is the hole the
// allowlist exists to close.
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

// readPID waits for a command to write its pid to path and returns it.
func readPID(t *testing.T, path string) int {
	t.Helper()
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
		if !os.IsNotExist(err) {
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
	if os.IsNotExist(err) {
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

// readEvents parses every event in a run's stream.
func readEvents(t *testing.T, path string) []runrec.Event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the run stream: %v", err)
	}
	var events []runrec.Event
	for n, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e runrec.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse event %d of %s: %v", n+1, path, err)
		}
		events = append(events, e)
	}
	return events
}

// TestRunRecordsTheGroupKillOfADaemonisedChild is #58's second escape fixture,
// driven through the command the runtime exposes. The shell backgrounds a child
// and exits 0, so the run finished and exit code 0 is true — the attack is what
// the child would have done next, and that it was stopped has to be in the
// record rather than inferred from a clean exit.
//
// Three assertions, one per claim: the run exits 0, the child leaves the
// process table, and run.ndjson carries one cmd:exit with orphans set. The
// last is why the run opens the stream at all.
//
// The mode is worktree, and the fixture says so: on the host the process group
// is the only thing between a backgrounded child and the machine, which is the
// containment this attack tests. In container mode the child lives in a pid
// namespace that ends with the command, so the pid it writes is not one the
// host can look up — the namespace is a different (and already proven) claim.
func TestRunRecordsTheGroupKillOfADaemonisedChild(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wd := t.TempDir()
	inDir(t, wd)
	base := t.TempDir()
	allowShell(t)

	old := model
	model = &daemonLLM{}
	t.Cleanup(func() { model = old })

	out, code := captureStdout(t, func() int {
		return run([]string{"run", "--sandbox", "worktree", "--run-dir", base, "start a daemon"})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0: the command exited on its own\n%s", code, out)
	}
	if !strings.Contains(out, "\"done\": true") {
		t.Fatalf("the run did not finish:\n%s", out)
	}
	// The post-condition the attack is about: nothing is left running.
	waitGone(t, readPID(t, filepath.Join(wd, "child.pid")))

	rec := onlyRun(t, base)
	if rec.Sandbox != "worktree" {
		t.Fatalf("run recorded sandbox %q, want worktree: the fixture attacks the group kill", rec.Sandbox)
	}
	if rec.State != "done" {
		t.Fatalf("run state = %q, want done", rec.State)
	}

	events := readEvents(t, filepath.Join(onlyRunDir(t, base), "run.ndjson"))
	var exits int
	for _, e := range events {
		if e.Kind != "cmd:exit" {
			continue
		}
		exits++
		var exit struct {
			ExitCode int    `json:"exit_code"`
			Killed   string `json:"killed"`
			Orphans  bool   `json:"orphans"`
		}
		if err := json.Unmarshal(e.Data, &exit); err != nil {
			t.Fatalf("parse cmd:exit data: %v", err)
		}
		if exit.ExitCode != 0 {
			t.Errorf("recorded exit code %d, want 0 for a command that finished", exit.ExitCode)
		}
		if exit.Killed != "" {
			t.Errorf("recorded killed %q; the command was not stopped, its child was", exit.Killed)
		}
		if !exit.Orphans {
			t.Error("the record does not name the backgrounded child the group kill reached")
		}
	}
	if exits != 1 {
		t.Fatalf("recorded %d cmd:exit events, want 1", exits)
	}
}
