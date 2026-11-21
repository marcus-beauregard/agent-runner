package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	runrec "github.com/marcus-beauregard/agent-runner/internal/run"
	"github.com/marcus-beauregard/agent-runner/internal/world"
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

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what it
// wrote, so a command whose whole job is printing is tested on its output, not
// only on its exit code.
func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = wr
	code := fn()
	wr.Close()
	os.Stdout = old
	out, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(out), code
}

// TestReportCommand is the CLI half of the report: a recorded run prints the
// document, an id that does not exist exits 1, and asking for nothing exits 1.
func TestReportCommand(t *testing.T) {
	base := t.TempDir()
	r, err := runrec.New(base, runrec.Meta{Kind: "build", Subject: "https://example.test/issues/33"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Finish(0); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	out, code := captureStdout(t, func() int { return reportRun([]string{r.ID, "--run-dir", base}) })
	if code != 0 {
		t.Errorf("report on a recorded run = %d, want 0", code)
	}
	for _, want := range []string{"# Run " + r.ID, "- State: done", "## Left to do"} {
		if !strings.Contains(out, want) {
			t.Errorf("report printed without %q\n---\n%s", want, out)
		}
	}
	if code := reportRun([]string{"20260101-000000.000000-nope", "--run-dir", base}); code != 1 {
		t.Errorf("report on an unknown id = %d, want 1", code)
	}
	if code := reportRun(nil); code != 1 {
		t.Errorf("report with no id = %d, want 1", code)
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

// TestReplayCommand is the exit-code side of the ticket: a record that replays
// cleanly exits 0, one whose recorded set disagrees with its own world exits 1,
// and a directory that does not name its account is refused rather than guessed.
func TestReplayCommand(t *testing.T) {
	snapshot := world.World{
		Subject:  "https://example.test/pull/8",
		Issue:    world.Issue{Number: 7, State: "OPEN"},
		PR:       world.PR{Number: 8, State: "open", Mergeable: world.MergeableNo},
		Comments: []world.Comment{{ID: 100, Author: "someone"}},
	}
	tests := []struct {
		name       string
		account    string
		spend      bool
		unfinished bool
		wantCode   int
	}{
		{name: "matching record", account: "marcus-beauregard", spend: true, wantCode: 0},
		{name: "record disagrees", account: "marcus-beauregard", wantCode: 1},
		{name: "no account recorded", wantCode: 1},
		// A live run has spent only part of its plan. Replay cannot call that a
		// mismatch, and it cannot pass it either: no verdict, exit 1.
		{name: "unfinished record", account: "marcus-beauregard", spend: true, unfinished: true, wantCode: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := runrec.New(t.TempDir(), runrec.Meta{Kind: "resolve", Subject: snapshot.Subject, Account: tc.account})
			if err != nil {
				t.Fatalf("create run: %v", err)
			}
			if err := runrec.SaveWorld(r.Dir, snapshot); err != nil {
				t.Fatalf("save world: %v", err)
			}
			if tc.spend {
				l, err := runrec.OpenLedger(filepath.Join(r.Dir, "run.ndjson"))
				if err != nil {
					t.Fatalf("open ledger: %v", err)
				}
				if err := l.Spend("comment:100", "answered #100"); err != nil {
					t.Fatalf("spend: %v", err)
				}
				if err := l.Close(); err != nil {
					t.Fatalf("close ledger: %v", err)
				}
			}
			if !tc.unfinished {
				if err := r.Finish(0); err != nil {
					t.Fatalf("finish run: %v", err)
				}
			}
			if code := run([]string{"replay", r.Dir}); code != tc.wantCode {
				t.Errorf("replay exit = %d, want %d", code, tc.wantCode)
			}
		})
	}
}
