package run

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRun creates one run under a fresh base, so a test can hand the same
// base to Locate and to RenderReport.
func newTestRun(t *testing.T) (*Run, string) {
	t.Helper()
	base := t.TempDir()
	// A record that names a repo and a branch is a record whose tree can be
	// looked for, so the fixture's parent is a real repository rather than a
	// path that cannot be read.
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", ".")
	r, err := New(base, Meta{
		Kind:    "build",
		Subject: "https://example.test/issues/33",
		Repo:    repo,
		Branch:  "agent/report",
		BaseSHA: "abc1234",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, base
}

// appendEvent adds one event to the run's ledger, the way a pipeline would:
// open, append, close.
func appendEvent(t *testing.T, r *Run, kind, stage string, data any) {
	t.Helper()
	l, err := OpenLedger(filepath.Join(r.Dir, "run.ndjson"))
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	if err := l.Append(kind, stage, data); err != nil {
		t.Fatalf("Append %s: %v", kind, err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestRenderReport is the ticket's "done when": a run with known events prints a
// document a stranger can follow — the header, the stages in order, the gates,
// the exit code, and what is unfinished — without inventing anything the record
// does not say.
func TestRenderReport(t *testing.T) {
	r, _ := newTestRun(t)
	appendEvent(t, r, "stage", "01-triage", stageData{Event: "start"})
	appendEvent(t, r, "stage", "01-triage", stageData{Event: "finish", Hash: "sha256:aaa"})
	appendEvent(t, r, "gate", "01-triage", gateData{Command: "gofmt -l .", DurationMS: 12})
	appendEvent(t, r, "stage", "02-scope", stageData{Event: "start"})
	appendEvent(t, r, "gate", "02-scope", gateData{
		Command:    "go test ./...",
		ExitCode:   1,
		DurationMS: 4200,
		OutputTail: "FAIL\texample.test\t0.4s\n--- FAIL: TestX\n",
	})
	appendEvent(t, r, "decision", "02-scope", decisionData{
		Question: "Which of the two call sites is the cause?",
		Answer:   "The second.",
	})
	appendEvent(t, r, "decision", "02-scope", decisionData{Question: "Does the gate need network?"})
	if err := r.Finish(2); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	b, err := RenderReport(r.Dir)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	got := string(b)

	for _, want := range []string{
		"# Run " + r.ID,
		"- Kind: build",
		"- Subject: https://example.test/issues/33",
		"- Base: abc1234",
		"- State: failed",
		"- Exit code: 2",
		"## 01-triage",
		"- Output hash: sha256:aaa",
		"## 02-scope",
		"## Gates",
		"| 01-triage | gofmt -l . | 0 | 12ms |",
		"| 02-scope | go test ./... | 1 | 4.2s |",
		"FAIL example.test 0.4s --- FAIL: TestX",
		"answered: The second.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n---\n%s", want, got)
		}
	}

	if i, j := strings.Index(got, "## 01-triage"), strings.Index(got, "## 02-scope"); i < 0 || j < 0 || i > j {
		t.Errorf("stages are not in ledger order: 01-triage at %d, 02-scope at %d", i, j)
	}

	left := got[strings.Index(got, "## Left to do"):]
	for _, want := range []string{
		"Stage 02-scope started and never finished",
		"Gate go test ./... failed with exit 1.",
		"left a question open",
	} {
		if !strings.Contains(left, want) {
			t.Errorf("Left to do does not contain %q\n---\n%s", want, left)
		}
	}
	if strings.Contains(left, "01-triage") {
		t.Errorf("Left to do names a finished stage\n---\n%s", left)
	}
}

// TestRenderReportEmptyLedger covers the record just after New: run.json, no
// stream. That is a header and a resumable run, not an error and not a claim
// that nothing happened.
func TestRenderReportEmptyLedger(t *testing.T) {
	r, _ := newTestRun(t)
	b, err := RenderReport(r.Dir)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		"# Run " + r.ID,
		"- State: running",
		"- Exit code: none yet",
		"## Left to do",
		"still says running",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "## Gates") {
		t.Errorf("an empty ledger rendered a gates section\n---\n%s", got)
	}
}

// TestRenderReportIgnoredTornTail is the crash case: the last write died
// mid-line. The fragment held no event, so it is dropped and noted, and the
// reporting itself leaves the file exactly as it found it.
func TestRenderReportIgnoredTornTail(t *testing.T) {
	r, _ := newTestRun(t)
	appendEvent(t, r, "stage", "01-triage", stageData{Event: "start"})

	path := filepath.Join(r.Dir, "run.ndjson")
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	torn := []byte(`{"seq":2,"ts":"2026-11-21T00:00:00Z","kind":"stage","stage":"02-half`)
	if err := os.WriteFile(path, append(whole, torn...), 0o600); err != nil {
		t.Fatalf("write torn ledger: %v", err)
	}

	b, err := RenderReport(r.Dir)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "torn final line") {
		t.Errorf("the report did not note the torn line\n---\n%s", got)
	}
	if strings.Contains(got, "02-half") {
		t.Errorf("the report rendered a torn line\n---\n%s", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger again: %v", err)
	}
	if !bytes.Equal(after, append(whole, torn...)) {
		t.Error("RenderReport modified the ledger; it must only read")
	}
}

// TestRenderReportRejectsCorruptLine is the other half of the torn-tail rule: a
// whole line that does not parse may have held a stage.finish, so the report
// fails instead of reading past it.
func TestRenderReportRejectsCorruptLine(t *testing.T) {
	r, _ := newTestRun(t)
	path := filepath.Join(r.Dir, "run.ndjson")
	if err := os.WriteFile(path, []byte("{\"seq\":1}\nnot an event\n"), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	if _, err := RenderReport(r.Dir); err == nil {
		t.Fatal("RenderReport read past a line that is not an event")
	}
}

// TestRenderReportTerminalStateOwesWork is the failure this document exists to
// prevent: a record that ended refused, needs_human or failed owes something by
// definition, so "Left to do" must never print "Nothing" over a non-zero exit
// code just because the event stream happens to look tidy.
func TestRenderReportTerminalStateOwesWork(t *testing.T) {
	for _, tc := range []struct {
		code  int
		state string
	}{
		{1, "failed"},
		{3, "refused"},
		{4, "needs_human"},
	} {
		r, _ := newTestRun(t)
		if err := r.Finish(tc.code); err != nil {
			t.Fatalf("Finish(%d): %v", tc.code, err)
		}
		b, err := RenderReport(r.Dir)
		if err != nil {
			t.Fatalf("RenderReport: %v", err)
		}
		got := string(b)
		if !strings.Contains(got, "- State: "+tc.state) {
			t.Errorf("header does not say %q\n---\n%s", tc.state, got)
		}
		left := got[strings.Index(got, "## Left to do"):]
		if strings.Contains(left, "Nothing:") {
			t.Errorf("exit %d rendered an empty Left to do\n---\n%s", tc.code, left)
		}
		if want := fmt.Sprintf("The record ended %s with exit code %d", tc.state, tc.code); !strings.Contains(left, want) {
			t.Errorf("Left to do does not contain %q\n---\n%s", want, left)
		}
	}
}

// TestRenderReportDoneSaysNothing is the other side of the same rule: done is the
// only state that may claim nothing is left.
func TestRenderReportDoneSaysNothing(t *testing.T) {
	r, _ := newTestRun(t)
	appendEvent(t, r, "stage", "01-triage", stageData{Event: "start"})
	appendEvent(t, r, "stage", "01-triage", stageData{Event: "finish", Hash: "sha256:aaa"})
	if err := r.Finish(0); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	b, err := RenderReport(r.Dir)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	left := string(b)[strings.Index(string(b), "## Left to do"):]
	if !strings.Contains(left, "Nothing:") {
		t.Errorf("a finished run does not say nothing is left\n---\n%s", left)
	}
	if strings.Contains(left, "01-triage") {
		t.Errorf("Left to do names a finished stage\n---\n%s", left)
	}
}

// TestRenderReportStageNamedOnlyByGates keeps the report from contradicting
// itself: the stage section says no stage events were recorded, so "Left to do"
// must not claim that stage started.
func TestRenderReportStageNamedOnlyByGates(t *testing.T) {
	r, _ := newTestRun(t)
	appendEvent(t, r, "gate", "02-scope", gateData{Command: "go vet ./...", DurationMS: 30})
	b, err := RenderReport(r.Dir)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	got := string(b)
	left := got[strings.Index(got, "## Left to do"):]
	if strings.Contains(left, "started and never finished") {
		t.Errorf("Left to do says a gate-only stage started\n---\n%s", left)
	}
	if !strings.Contains(left, "recorded no stage events") {
		t.Errorf("Left to do does not note the missing stage events\n---\n%s", left)
	}
}

// TestRenderReportUnattributedEvents is the stage-less case Event.Stage allows:
// a gate that recorded no stage still ran, and dropping it would let the report
// read as a pass.
func TestRenderReportUnattributedEvents(t *testing.T) {
	r, _ := newTestRun(t)
	appendEvent(t, r, "gate", "", gateData{Command: "go test ./...", ExitCode: 1, DurationMS: 900})
	appendEvent(t, r, "decision", "", decisionData{Question: "Ship it?"})
	b, err := RenderReport(r.Dir)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		"| (unattributed) | go test ./... | 1 | 900ms |",
		"Gate go test ./... failed with exit 1.",
		"## Unattributed events",
		"A decision recorded no stage and no answer",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n---\n%s", want, got)
		}
	}
	// The bucket is not a stage, so no line may present it as one. Without this
	// the suite would enshrine the contradiction it is meant to catch.
	for _, unwanted := range []string{"Stage (unattributed)", "- No stage events recorded beyond the name"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("report names the unattributed bucket as a stage: %q\n---\n%s", unwanted, got)
		}
	}
}

// TestRenderReportUnattributedGatePasses is the other half of the vanishing-gate
// rule: a gate that passed is as much evidence as one that failed. A regression
// that kept only failing gates would otherwise leave the suite green, and the
// table would quietly lose a row.
func TestRenderReportUnattributedGatePasses(t *testing.T) {
	r, _ := newTestRun(t)
	appendEvent(t, r, "gate", "", gateData{Command: "gofmt -l .", DurationMS: 5})
	b, err := RenderReport(r.Dir)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "| (unattributed) | gofmt -l . | 0 | 5ms |") {
		t.Errorf("report dropped a stage-less gate that passed\n---\n%s", got)
	}
	if strings.Contains(got, "Stage (unattributed)") {
		t.Errorf("the unattributed bucket is named as a stage\n---\n%s", got)
	}
}

// TestRenderReportNamelessStageEvent covers the stage event the report used to
// drop: no stage name means the work cannot be attributed, but the record still
// holds it, so "Left to do" may not report nothing left.
func TestRenderReportNamelessStageEvent(t *testing.T) {
	r, _ := newTestRun(t)
	appendEvent(t, r, "stage", "", stageData{Event: "start"})
	if err := r.Finish(0); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	b, err := RenderReport(r.Dir)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	got := string(b)
	if strings.Contains(got, "Nothing: the record says done") {
		t.Errorf("report claims nothing is left over a dropped stage event\n---\n%s", got)
	}
	for _, want := range []string{
		"## Unattributed events",
		"A stage event recorded no stage name",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n---\n%s", want, got)
		}
	}
}

// TestRenderReportDoneWithNonZeroExit: state and exit code are separate fields
// in run.json, and the closing sentence must not survive a pair that disagrees.
func TestRenderReportDoneWithNonZeroExit(t *testing.T) {
	r, _ := newTestRun(t)
	path := filepath.Join(r.Dir, "run.json")
	rec := []byte("{\"id\":\"" + r.ID + "\",\"kind\":\"build\",\"state\":\"done\",\"exit_code\":2}")
	if err := os.WriteFile(path, rec, 0o600); err != nil {
		t.Fatalf("write run.json: %v", err)
	}
	b, err := RenderReport(r.Dir)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	got := string(b)
	if strings.Contains(got, "Nothing: the record says done") {
		t.Errorf("report claims nothing is left over a non-zero exit code\n---\n%s", got)
	}
	if !strings.Contains(got, "exited with code 2") {
		t.Errorf("report does not name the exit code it disagrees with\n---\n%s", got)
	}
}

// TestRenderReportStatelessRecord: loadRecord accepts a run.json with no state,
// and the report must not guess one. "The record says done" over a file that
// says nothing is the same lie in a quieter form.
func TestRenderReportStatelessRecord(t *testing.T) {
	r, _ := newTestRun(t)
	path := filepath.Join(r.Dir, "run.json")
	rec := []byte("{\"id\":\"" + r.ID + "\",\"kind\":\"build\"}")
	if err := os.WriteFile(path, rec, 0o600); err != nil {
		t.Fatalf("write run.json: %v", err)
	}
	b, err := RenderReport(r.Dir)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	got := string(b)
	if strings.Contains(got, "- State:") {
		t.Errorf("the header invented a state\n---\n%s", got)
	}
	if strings.Contains(got, "says done") {
		t.Errorf("the report claims a stateless record says done\n---\n%s", got)
	}
	if !strings.Contains(got, "does not say what state") {
		t.Errorf("the report does not flag the missing state\n---\n%s", got)
	}
}

// TestLocate keeps an id a name: the right directory is found, and anything that
// is not a run under base is ErrNoRecord rather than a path somebody chose.
func TestLocate(t *testing.T) {
	r, base := newTestRun(t)
	if dir, err := Locate(base, r.ID); err != nil || dir != r.Dir {
		t.Fatalf("Locate = %q, %v; want %q", dir, err, r.Dir)
	}
	for _, id := range []string{"", "20260101-000000.000000-nope", "../../etc", "..", "."} {
		if _, err := Locate(base, id); !errors.Is(err, ErrNoRecord) {
			t.Errorf("Locate(%q) error = %v, want ErrNoRecord", id, err)
		}
	}
}
