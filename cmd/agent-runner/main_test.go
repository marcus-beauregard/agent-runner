package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// stubGH puts a gh on PATH that prints out and exits with code, so the tests
// exercise the real work path without a network or a GitHub account.
func stubGH(t *testing.T, out string, code int) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' '" + out + "'\nexit " + strconv.Itoa(code) + "\n"
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// record is the part of run.json these tests read back.
type record struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Subject  string `json:"subject"`
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code"`
}

// onlyRun digs the single run directory out of base and returns its record.
func onlyRun(t *testing.T, base string) record {
	t.Helper()
	runs := filepath.Join(base, "agent-runner", "runs")
	entries, err := os.ReadDir(runs)
	if err != nil {
		t.Fatalf("read runs directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("runs directory holds %d entries, want 1", len(entries))
	}
	b, err := os.ReadFile(filepath.Join(runs, entries[0].Name(), "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("parse run.json: %v", err)
	}
	return rec
}

// TestWorkRecordsBothOutcomes is the ticket's claim: with --run-dir, the run is
// recorded before planning and finished on the path it actually took, so the
// record of a refusal does not read as a run still in flight.
func TestWorkRecordsBothOutcomes(t *testing.T) {
	open := `{"number":27,"title":"Record","state":"OPEN","assignees":[]}`
	tests := []struct {
		name     string
		ghOut    string
		ghCode   int
		wantCode int
		wantSt   string
	}{
		{name: "success", ghOut: open, ghCode: 0, wantCode: 0, wantSt: "done"},
		{name: "refusal", ghOut: "", ghCode: 1, wantCode: 3, wantSt: "refused"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			stubGH(t, tc.ghOut, tc.ghCode)
			ref := "https://example.test/issues/27"
			code := work(context.Background(), []string{ref, "--run-dir", base, "--dry-run"})
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tc.wantCode)
			}
			rec := onlyRun(t, base)
			if rec.State != tc.wantSt {
				t.Errorf("state = %q, want %q", rec.State, tc.wantSt)
			}
			if rec.ExitCode == nil || *rec.ExitCode != tc.wantCode {
				t.Errorf("exit_code = %v, want %d", rec.ExitCode, tc.wantCode)
			}
			if rec.Subject != ref {
				t.Errorf("subject = %q, want %q", rec.Subject, ref)
			}
			if rec.Kind != "build" {
				t.Errorf("kind = %q, want build", rec.Kind)
			}
			if rec.ID == "" {
				t.Error("id is empty")
			}
		})
	}
}

// TestWorkWithoutRunDirWritesNothing keeps the old contract: a dry run with no
// --run-dir records nothing anywhere.
func TestWorkWithoutRunDirWritesNothing(t *testing.T) {
	base := t.TempDir()
	stubGH(t, `{"number":27,"title":"Record","state":"OPEN","assignees":[]}`, 0)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))

	if code := work(context.Background(), []string{"https://example.test/issues/27", "--dry-run"}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, dir := range []string{base, filepath.Join(base, "state")} {
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read %s: %v", dir, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s holds %d entries, want none", dir, len(entries))
		}
	}
}
