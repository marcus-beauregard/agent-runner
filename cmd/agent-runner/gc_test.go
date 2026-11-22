package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus-beauregard/agent-runner/internal/envstore"
)

// envAt creates an environment that was built at a chosen time: the age policy
// is the behaviour under test, and a fixture built "now" could not exercise it.
func envAt(t *testing.T, state, dig string, built time.Time) string {
	t.Helper()
	e, err := envstore.Create(state, envstore.Manifest{
		Digest:    dig,
		CreatedAt: built.UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	if err := e.MarkReady(0); err != nil {
		t.Fatalf("finish environment: %v", err)
	}
	return e.Dir
}

func stateDir(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", base)
	return base
}

// TestGCCommandListsThenCollects is the ticket's done-when, first half: a dry run
// names the candidate and its reason and moves nothing, and --apply collects
// exactly that one.
func TestGCCommandListsThenCollects(t *testing.T) {
	state := stateDir(t)
	oldDig := strings.Repeat("ab", 32)
	freshDig := strings.Repeat("cd", 32)
	oldDir := envAt(t, state, oldDig, time.Now().Add(-40*24*time.Hour))
	freshDir := envAt(t, state, freshDig, time.Now().Add(-2*24*time.Hour))

	out, code := captureStdout(t, func() int { return gcCommand(nil) })
	if code != 0 {
		t.Fatalf("dry run exit code = %d, want 0", code)
	}
	for _, want := range []string{"dry run", oldDig, "unused for 40 days", freshDig, "under the 30 days age"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run printed without %q\n---\n%s", want, out)
		}
	}
	if _, err := os.Stat(oldDir); err != nil {
		t.Fatalf("the dry run deleted %s: %v", oldDig, err)
	}

	out, code = captureStdout(t, func() int { return gcCommand([]string{"--apply"}) })
	if code != 0 {
		t.Fatalf("apply exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "collected 1 of 2") {
		t.Errorf("apply printed %q, want it to name the one collection", out)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("the 40-day environment is still there: %v", err)
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("the 2-day environment was collected: %v", err)
	}
}

// TestGCCommandJSON is the number the disk question is asked in: bytes per
// digest, and whether they were freed or only would be.
func TestGCCommandJSON(t *testing.T) {
	state := stateDir(t)
	dig := strings.Repeat("ef", 32)
	dir := envAt(t, state, dig, time.Now().Add(-45*24*time.Hour))
	if err := os.WriteFile(filepath.Join(dir, "bin", "go"), make([]byte, 4096), 0o700); err != nil {
		t.Fatalf("write tool: %v", err)
	}

	out, code := captureStdout(t, func() int { return gcCommand([]string{"--json"}) })
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var report struct {
		Apply      bool  `json:"apply"`
		FreedBytes int64 `json:"freed_bytes"`
		Candidates []struct {
			Name   string `json:"name"`
			Bytes  int64  `json:"bytes"`
			Reason string `json:"reason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode --json: %v\n---\n%s", err, out)
	}
	if report.Apply {
		t.Error("a dry run reported apply=true")
	}
	if len(report.Candidates) != 1 || report.Candidates[0].Name != dig {
		t.Fatalf("candidates = %+v, want the one old digest", report.Candidates)
	}
	if report.FreedBytes < 4096 || report.Candidates[0].Bytes < 4096 {
		t.Errorf("freed_bytes = %d, candidate bytes = %d, want at least the 4096-byte tool", report.FreedBytes, report.Candidates[0].Bytes)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the dry run deleted the environment: %v", err)
	}

	out, code = captureStdout(t, func() int { return gcCommand([]string{"--json", "--apply"}) })
	if code != 0 {
		t.Fatalf("apply exit code = %d, want 0", code)
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode --json --apply: %v\n---\n%s", err, out)
	}
	if !report.Apply || report.FreedBytes < 4096 {
		t.Errorf("apply report = apply %v, freed %d; want applied and the bytes counted", report.Apply, report.FreedBytes)
	}
}

// TestGCCommandLeavesALiveEnvironment is the other done-when: a run in flight is
// untouched, and the report says why instead of silently skipping it.
func TestGCCommandLeavesALiveEnvironment(t *testing.T) {
	state := stateDir(t)
	dig := strings.Repeat("12", 32)
	dir := envAt(t, state, dig, time.Now().Add(-90*24*time.Hour))
	l, err := envstore.TryLock(state, dig)
	if err != nil {
		t.Fatalf("take the reader's lock: %v", err)
	}

	out, code := captureStdout(t, func() int { return gcCommand([]string{"--apply"}) })
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "held by a reader") {
		t.Errorf("output does not say why it kept the environment:\n%s", out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("a held environment was collected: %v", err)
	}

	if err := l.Discard(); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	if _, code := captureStdout(t, func() int { return gcCommand([]string{"--apply"}) }); code != 0 {
		t.Fatalf("apply after release exit code = %d, want 0", code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the environment survived --apply once its lock was gone: %v", err)
	}
}

// TestGCCommandRejectsNonsense is the flag surface: a negative age is a typo, not
// a policy, and a positional argument is not a selector this command has.
func TestGCCommandRejectsNonsense(t *testing.T) {
	stateDir(t)
	if _, code := captureStdout(t, func() int { return gcCommand([]string{"--envs", "-1"}) }); code != 1 {
		t.Errorf("--envs -1 = %d, want 1", code)
	}
	if _, code := captureStdout(t, func() int { return gcCommand([]string{"extra"}) }); code != 1 {
		t.Errorf("a positional argument = %d, want 1", code)
	}
}
