package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	// Alias, for the same reason run.go uses one: worktree.go has a helper
	// named refuse.
	refusal "github.com/marcus-beauregard/agent-runner/internal/refuse"
)

// refusedRun opens a run on subject, refuses it at the plan stage, finishes it
// at the exit code a refusal earns, and returns the run it closed. Two of these
// on one base are the "two refusals in a row" the ticket describes.
func refusedRun(t *testing.T, base, subject string, code refusal.Code) *Run {
	t.Helper()
	r, err := New(base, Meta{Kind: "build", Subject: subject})
	if err != nil {
		t.Fatalf("New(%s): %v", subject, err)
	}
	if err := r.Refuse(refusal.New(code, "plan", "the plan stage refused")); err != nil {
		t.Fatalf("Refuse(%s): %v", subject, err)
	}
	if err := r.Finish(3); err != nil {
		t.Fatalf("Finish(%s): %v", subject, err)
	}
	return r
}

// TestRefusalCitesTheRefusalBeforeIt is the ticket's refuse:again claim: a
// second refusal on the same thread names the first, in run.json, in the one
// event the stream leaves, and in the report a person reads first.
func TestRefusalCitesTheRefusalBeforeIt(t *testing.T) {
	base := t.TempDir()
	subject := "https://example.test/issues/59"
	first := refusedRun(t, base, subject, refusal.IssueClosed)

	r, err := New(base, Meta{Kind: "build", Subject: subject})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	led, err := OpenLedger(filepath.Join(r.Dir, "run.ndjson"))
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	r.SetLedger(led)

	ref := refusal.New(refusal.ClaimedByOther, "plan", "someone else is on it")
	ref.Evidence = map[string]string{"assignee": "someone"}
	if err := r.Refuse(ref); err != nil {
		t.Fatalf("Refuse: %v", err)
	}
	if err := led.Close(); err != nil {
		t.Fatalf("close ledger: %v", err)
	}

	rec, err := loadRecord(r.Dir)
	if err != nil {
		t.Fatalf("loadRecord: %v", err)
	}
	got := rec.Refusal.Evidence["refuse:again"]
	for _, want := range []string{first.ID, "issue-closed", "at plan"} {
		if !strings.Contains(got, want) {
			t.Fatalf("refuse:again = %q, want it to cite %q from %s", got, want, first.ID)
		}
	}
	// The citation is added to a copy: the refusal belongs to the caller, and a
	// recorder that edited it in place would change a value it was handed.
	if _, ok := ref.Evidence["refuse:again"]; ok {
		t.Fatalf("Refuse edited the caller's evidence: %v", ref.Evidence)
	}
	if rec.Refusal.Evidence["assignee"] != "someone" {
		t.Fatalf("evidence = %v, want the caller's own entries kept", rec.Refusal.Evidence)
	}

	// The stream says what the record says, or a reader following one of them
	// would learn something the other does not.
	events, _, err := readLedger(filepath.Join(r.Dir, "run.ndjson"))
	if err != nil {
		t.Fatalf("readLedger: %v", err)
	}
	var refusals int
	for _, e := range events {
		if e.Kind != kindRefusal {
			continue
		}
		refusals++
		var d refusalData
		if err := decodeData(e, &d); err != nil {
			t.Fatalf("decode refusal event: %v", err)
		}
		if !strings.Contains(d.Evidence["refuse:again"], first.ID) {
			t.Fatalf("event refuse:again = %q, want it to cite %s", d.Evidence["refuse:again"], first.ID)
		}
	}
	if refusals != 1 {
		t.Fatalf("refusal events = %d, want exactly 1", refusals)
	}

	rendered, err := RenderReport(r.Dir)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	for _, want := range []string{"refuse:again", first.ID, "issue-closed"} {
		if !strings.Contains(string(rendered), want) {
			t.Fatalf("report has no %q:\n%s", want, rendered)
		}
	}
}

// TestRefusalCitesOnlyARepeat pins the other half of "in a row": a refusal is
// not evidence of a loop when the thread's last run finished, and it is not one
// for a thread this base has never refused.
func TestRefusalCitesOnlyARepeat(t *testing.T) {
	subject := "https://example.test/issues/59"
	cases := []struct {
		name string
		seed func(t *testing.T, base string)
	}{
		{
			name: "the previous run on the thread finished",
			seed: func(t *testing.T, base string) {
				r, err := New(base, Meta{Kind: "build", Subject: subject})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if err := r.Finish(0); err != nil {
					t.Fatalf("Finish: %v", err)
				}
			},
		},
		{
			name: "the previous refusal was another thread",
			seed: func(t *testing.T, base string) {
				refusedRun(t, base, "https://example.test/issues/60", refusal.IssueClosed)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			tc.seed(t, base)

			r, err := New(base, Meta{Kind: "build", Subject: subject})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := r.Refuse(refusal.New(refusal.ClaimedByOther, "plan", "someone else is on it")); err != nil {
				t.Fatalf("Refuse: %v", err)
			}
			if err := r.Finish(3); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			rec, err := loadRecord(r.Dir)
			if err != nil {
				t.Fatalf("loadRecord: %v", err)
			}
			if got, ok := rec.Refusal.Evidence["refuse:again"]; ok {
				t.Fatalf("refuse:again = %q, want no citation", got)
			}
		})
	}
}

// TestPreviousRunOrdersRunsWithinOneSecond pins the ordering the scan rests on.
// CreatedAt is RFC 3339 and reaches a second, so three runs opened in one second
// carry identical stamps: the id is what says which came first, and a refusal
// that started after this run must not be read as the one before it.
func TestPreviousRunOrdersRunsWithinOneSecond(t *testing.T) {
	subject := "https://example.test/issues/59"
	const at = "2026-01-01T00:00:00Z"
	ids := []string{
		"20260101-000000.000001-aaaaaa",
		"20260101-000000.000002-bbbbbb",
		"20260101-000000.000003-cccccc",
	}
	root := t.TempDir()
	open := func(id string, state string) *record {
		t.Helper()
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", id, err)
		}
		rec := record{ID: id, CreatedAt: at, Subject: subject, State: state}
		if state == stateRefused {
			rec.Refusal = &refusalRecord{Code: string(refusal.IssueClosed), Where: "plan", Reason: "the issue is closed"}
		}
		if err := writeRecord(dir, rec); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
		return &rec
	}
	first, _ := open(ids[0], stateRefused), open(ids[2], stateRefused)

	// The middle run refuses: the run before it is the first, not the third,
	// even though all three were recorded in the same second.
	mid := &record{ID: ids[1], CreatedAt: at, Subject: subject}
	got := previousRun(root, mid)
	if got == nil || got.ID != first.ID {
		t.Fatalf("previousRun = %+v, want %s", got, first.ID)
	}

	// The first run refuses: nothing started before it, so it cites nothing.
	earliest := &record{ID: ids[0], CreatedAt: at, Subject: subject}
	if got := previousRun(root, earliest); got != nil {
		t.Fatalf("previousRun = %s, want nil for the first run in the second", got.ID)
	}
}

// TestRefusalSurvivesAnUnreadableSibling is the scan's failure mode: one record
// nobody can parse must not cost the run its refusal. A refusal is what the
// caller came to record, and a broken sibling is not evidence of a loop.
func TestRefusalSurvivesAnUnreadableSibling(t *testing.T) {
	base := t.TempDir()
	subject := "https://example.test/issues/59"
	first := refusedRun(t, base, subject, refusal.IssueClosed)

	broken := filepath.Join(base, "agent-runner", "runs", "20260101-000000.000000-broken")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatalf("mkdir broken sibling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, "run.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write broken run.json: %v", err)
	}

	r, err := New(base, Meta{Kind: "build", Subject: subject})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Refuse(refusal.New(refusal.ClaimedByOther, "plan", "someone else is on it")); err != nil {
		t.Fatalf("Refuse: %v", err)
	}
	rec, err := loadRecord(r.Dir)
	if err != nil {
		t.Fatalf("loadRecord: %v", err)
	}
	if !strings.Contains(rec.Refusal.Evidence["refuse:again"], first.ID) {
		t.Fatalf("refuse:again = %q, want it to cite %s despite the broken sibling",
			rec.Refusal.Evidence["refuse:again"], first.ID)
	}
}
