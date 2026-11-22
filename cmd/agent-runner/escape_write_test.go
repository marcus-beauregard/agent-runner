package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	runrec "github.com/marcus-beauregard/agent-runner/internal/run"
)

// escapeLLM makes the loop attempt one named thing and nothing else, the same
// way the daemonise fixture drives a command it could not otherwise ask for.
type escapeLLM struct{ action string }

func (e escapeLLM) Complete(ctx context.Context, prompt string) (string, error) {
	return e.action, nil
}

// TestRunRefusesAWriteOutsideItsDirectory is #58's first escape fixture, driven
// through the command the runtime exposes. The model asks write_file for
// "../escape.txt" — the one tool that can name a path outside the run — and the
// run must refuse it, name the path in the report, and leave nothing behind.
//
// The run directory is a plain directory under a temp parent, not a checkout:
// the parent is not on the audit's watchlist, so an exit code of 0 here would
// mean the escape had succeeded somewhere nothing was looking. That is the
// point of the fixture — the tool refusal is the only thing between the attempt
// and the host, and this is the test that says so.
//
// The literal "echo x > ../escape.txt" from the plan cannot be expressed: the
// command runner has no shell, so ">" is rejected in the tokenizer before any
// path exists to refuse (TestRefusesShellChaining). What an attacker can still
// write is a path argument, and that is what this drives.
func TestRunRefusesAWriteOutsideItsDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := t.TempDir()
	parent := t.TempDir()
	wd := filepath.Join(parent, "work")
	if err := os.Mkdir(wd, 0o700); err != nil {
		t.Fatalf("mkdir run directory: %v", err)
	}
	inDir(t, wd)

	old := model
	model = escapeLLM{action: "write_file {\"path\":\"../escape.txt\",\"content\":\"escape\"}"}
	t.Cleanup(func() { model = old })

	out, code := captureStdout(t, func() int {
		return run([]string{"run", "--sandbox", "worktree", "--run-dir", base, "write out"})
	})
	if code != 3 {
		t.Fatalf("exit code = %d, want 3: a refused write is not a transcript line\n%s", code, out)
	}

	// The post-condition the attack is about: the file is not there.
	escape := filepath.Join(parent, "escape.txt")
	if _, err := os.Stat(escape); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s exists after the run: the escaping write reached the host (%v)", escape, err)
	}

	rec := onlyRun(t, base)
	if rec.State != "refused" {
		t.Errorf("run state = %q, want refused", rec.State)
	}
	if rec.Refusal == nil {
		t.Fatal("run.json holds no refusal, so nothing says why the run stopped")
	}
	if rec.Refusal.Code != "outside-write-scope" {
		t.Errorf("refusal code = %q, want outside-write-scope", rec.Refusal.Code)
	}
	if rec.Refusal.Where != "tool" {
		t.Errorf("refusal stage = %q, want tool", rec.Refusal.Where)
	}
	if got := rec.Refusal.Evidence["path"]; got != "../escape.txt" {
		t.Errorf("evidence path = %q, want ../escape.txt", got)
	}

	// The report is what a person reads: the path has to be in it, not only in
	// the record behind it.
	dir := onlyRunDir(t, base)
	b, err := runrec.RenderReport(dir)
	if err != nil {
		t.Fatalf("render report: %v", err)
	}
	section := reportSection(t, string(b), "## Refused")
	for _, want := range []string{
		"- Stage: tool",
		"- Code: outside-write-scope",
		"  - path: ../escape.txt",
	} {
		if !hasLine(section, want) {
			t.Errorf("the refusal section lacks %q:\n%s", want, section)
		}
	}

	// And the stream says the same thing once, so a reader following the run
	// sees where it stopped without opening run.json.
	events := refusalEvents(t, dir)
	if len(events) != 1 {
		t.Fatalf("recorded %d refusal events, want 1", len(events))
	}
	if events[0].Data.Code != rec.Refusal.Code {
		t.Errorf("stream code = %q, record code = %q", events[0].Data.Code, rec.Refusal.Code)
	}
}
