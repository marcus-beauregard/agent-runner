//go:build linux

package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRunKillsAChildThatOutlivesTheGate: a gate that backgrounds a child has
// already broken PRD §12 by the time Run returns, and the run cannot leave that
// process behind. The child is in the gate's own group, so the kill after the
// shell exits reaches it; the test writes its pid and waits for it to go.
func TestRunKillsAChildThatOutlivesTheGate(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is not installed here")
	}
	dir := t.TempDir()
	t.Setenv("PATH", onlyPath(t, "sleep"))
	if _, err := Run(context.Background(), dir, "sleep 30 & echo $! > child.pid"); err != nil {
		t.Fatalf("run a gate that left a child behind: %v", err)
	}
	pid := readPID(t, dir, "child.pid")
	deadline := time.Now().Add(5 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			_ = exec.Command("/bin/sh", "-c", fmt.Sprintf("kill -9 %d", pid)).Run()
			t.Fatalf("process %d survived its gate", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readPID reads a pid a gate wrote to name, waiting for the write to land.
func readPID(t *testing.T, dir, name string) int {
	t.Helper()
	path := filepath.Join(dir, name)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(b)))
			if convErr != nil {
				t.Fatalf("parse %s: %v", name, convErr)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("read %s: the gate never wrote it", name)
	return 0
}

// processGone reports whether pid has exited, treating a zombie as gone: the
// process is dead and only its parent's wait is outstanding.
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
