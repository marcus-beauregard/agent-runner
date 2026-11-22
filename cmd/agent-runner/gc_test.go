package main

import (
	"encoding/json"
	"fmt"
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
	if !strings.Contains(out, "held by a reader or a build") {
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
// a policy, a positional argument is not a selector this command has, and an age
// too large to multiply is refused instead of wrapping into "collect everything".
func TestGCCommandRejectsNonsense(t *testing.T) {
	stateDir(t)
	if _, code := captureStdout(t, func() int { return gcCommand([]string{"--envs", "-1"}) }); code != 1 {
		t.Errorf("--envs -1 = %d, want 1", code)
	}
	if _, code := captureStdout(t, func() int { return gcCommand([]string{"extra"}) }); code != 1 {
		t.Errorf("a positional argument = %d, want 1", code)
	}

	// One day past the bound: the product would wrap negative, and a negative
	// age is under every age, so every environment would qualify.
	var code int
	msg := captureStderr(t, func() { code = gcCommand([]string{"--envs", "106752"}) })
	if code != 1 {
		t.Errorf("--envs 106752 = %d, want 1", code)
	}
	if !strings.Contains(msg, "more than the") {
		t.Errorf("the refusal does not say what the bound is: %q", msg)
	}
}

// TestGCCommandListsRunCheckouts is the worktree tier through the command: both
// records appear, only the old one is a candidate, and the dry run takes
// nothing. The records are written by hand because the tier reads the record,
// not the run that wrote it.
func TestGCCommandListsRunCheckouts(t *testing.T) {
	state := stateDir(t)
	runs := filepath.Join(state, "agent-runner", "runs")
	const oldID, runningID = "20260101-000000.000001-aaaaaa", "20260101-000000.000002-bbbbbb"
	for _, r := range []struct{ id, state string }{
		{oldID, "done"},
		{runningID, "running"},
	} {
		dir := filepath.Join(runs, r.id)
		if err := os.MkdirAll(filepath.Join(dir, "worktree"), 0o700); err != nil {
			t.Fatalf("make the run directory: %v", err)
		}
		body := fmt.Sprintf(`{"id":%q,"created_at":"2026-01-01T00:00:00Z","kind":"build","repo":%q,"state":%q}`, r.id, state, r.state)
		if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte(body), 0o600); err != nil {
			t.Fatalf("write run.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "worktree", "blob"), make([]byte, 2048), 0o600); err != nil {
			t.Fatalf("write the checkout's file: %v", err)
		}
	}

	out, code := captureStdout(t, func() int { return gcCommand([]string{"--json"}) })
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var report struct {
		Apply     bool `json:"apply"`
		Worktrees []struct {
			ID      string `json:"id"`
			Bytes   int64  `json:"bytes"`
			Collect bool   `json:"collect"`
			Reason  string `json:"reason"`
		} `json:"worktrees"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode --json: %v\n---\n%s", err, out)
	}
	if report.Apply || len(report.Worktrees) != 2 {
		t.Fatalf("report = apply %v, %d checkouts; want a dry run over two", report.Apply, len(report.Worktrees))
	}
	seen := map[string]bool{}
	for _, e := range report.Worktrees {
		seen[e.ID] = true
		switch e.ID {
		case oldID:
			if !e.Collect || e.Bytes < 2048 {
				t.Errorf("the old run = collect %v, %d bytes; want a candidate of at least 2048", e.Collect, e.Bytes)
			}
		case runningID:
			if e.Collect {
				t.Errorf("a resumable run is a candidate: %s", e.Reason)
			}
			if !strings.Contains(e.Reason, "no terminal state") {
				t.Errorf("reason %q does not say the run is resumable", e.Reason)
			}
		default:
			t.Errorf("unexpected run %q in the report", e.ID)
		}
	}
	if !seen[oldID] || !seen[runningID] {
		t.Errorf("report named %v, want both runs", seen)
	}
	if _, err := os.Stat(filepath.Join(runs, oldID, "worktree")); err != nil {
		t.Errorf("the dry run deleted the checkout: %v", err)
	}
}

// TestGCCommandRejectsANegativeWorktreeAge is the second tier's flag surface:
// a negative age is a typo, and the flag says so rather than collecting
// everything.
func TestGCCommandRejectsANegativeWorktreeAge(t *testing.T) {
	stateDir(t)
	var code int
	msg := captureStderr(t, func() { code = gcCommand([]string{"--worktrees", "-1"}) })
	if code != 1 {
		t.Errorf("--worktrees -1 = %d, want 1", code)
	}
	if !strings.Contains(msg, "cannot be negative") {
		t.Errorf("the refusal does not name the flag: %q", msg)
	}
}
