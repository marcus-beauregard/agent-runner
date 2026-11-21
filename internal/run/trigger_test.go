package run

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSpendIsSingleUse is the promise of the whole ticket: an event is answered
// once. The second spend of an id fails without writing a line, and Spent agrees
// with what the file says.
func TestSpendIsSingleUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.ndjson")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	const id = "comment:4711"
	if err := l.Spend(id, "reply"); err != nil {
		t.Fatalf("first Spend: %v", err)
	}
	if err := l.Spend(id, "reply"); !errors.Is(err, ErrAlreadySpent) {
		t.Fatalf("second Spend = %v, want ErrAlreadySpent", err)
	}
	if err := l.Spend("comment:4712", "fix"); err != nil {
		t.Fatalf("Spend of a fresh id: %v", err)
	}
	spent, err := l.Spent(id)
	if err != nil {
		t.Fatalf("Spent: %v", err)
	}
	if !spent {
		t.Error("Spent = false after a successful Spend")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// One line per successful spend, and the refused one left nothing.
	lines, torn := readIntact(t, path)
	if torn != "" {
		t.Fatalf("torn tail %q", torn)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), lines)
	}
	var e Event
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("first line is not an event: %v", err)
	}
	if e.Kind != "trigger" {
		t.Errorf("kind = %q, want trigger", e.Kind)
	}
	var trig Trigger
	if err := json.Unmarshal(e.Data, &trig); err != nil {
		t.Fatalf("trigger payload: %v", err)
	}
	if trig.TriggerID != id || trig.Action != "reply" {
		t.Errorf("trigger = %+v, want id %q and action reply", trig, id)
	}
	if trig.At.Location() != time.UTC {
		t.Errorf("at location = %v, want UTC", trig.At.Location())
	}
}

// TestSpendHoldsAcrossRestart proves the spend is in the file rather than in the
// process: a ledger closed and reopened still knows the id was spent, so the
// resumed process does not answer the same event again.
func TestSpendHoldsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.ndjson")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	if err := l.Spend("review:88:changes_requested", "fix"); err != nil {
		t.Fatalf("Spend: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	l2, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	spent, err := l2.Spent("review:88:changes_requested")
	if err != nil {
		t.Fatalf("Spent after reopen: %v", err)
	}
	if !spent {
		t.Fatal("Spent = false after a reopen; the spend lives in the file")
	}
	if err := l2.Spend("review:88:changes_requested", "fix"); !errors.Is(err, ErrAlreadySpent) {
		t.Fatalf("Spend after reopen = %v, want ErrAlreadySpent", err)
	}
	if err := l2.Spend("review:89:changes_requested", "fix"); err != nil {
		t.Errorf("Spend of a different id = %v, want nil", err)
	}
}

// TestSpentReadsTheFile covers the states a ledger can be in when the question is
// asked, including the one a crash leaves: a torn final line held no complete
// spend, so it counts for nothing.
func TestSpentReadsTheFile(t *testing.T) {
	spendLine := func(id string) string {
		return `{"seq":1,"ts":"2026-11-21T00:00:00Z","kind":"trigger","data":{"trigger_id":"` + id + `","action":"reply","at":"2026-11-21T00:00:00Z"}}` + "\n"
	}
	stageLine := `{"seq":1,"ts":"2026-11-21T00:00:00Z","kind":"stage","stage":"triage","data":null}` + "\n"

	tests := []struct {
		name    string
		content string
		create  bool
		id      string
		want    bool
		wantErr bool
	}{
		{name: "no ledger at all", id: "comment:1"},
		{name: "empty ledger", create: true, id: "comment:1"},
		{name: "spent", create: true, content: spendLine("comment:1"), id: "comment:1", want: true},
		{name: "other id spent", create: true, content: spendLine("comment:2"), id: "comment:1"},
		{name: "other kinds ignored", create: true, content: stageLine, id: "comment:1"},
		{name: "torn line skipped", create: true, content: spendLine("comment:2") + spendLine("comment:1")[:40], id: "comment:1"},
		{name: "spend before a torn tail", create: true, content: spendLine("comment:1") + stageLine[:20], id: "comment:1", want: true},
		{name: "corrupt complete line", create: true, content: "not an event\n", id: "comment:1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "run.ndjson")
			if tt.create {
				if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
					t.Fatalf("prepare ledger: %v", err)
				}
			}
			// The Ledger is only how the path is named here: Spent reads the
			// file, so the ledger's own state must not matter.
			l := &Ledger{path: path}
			got, err := l.Spent(tt.id)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Spent accepted a corrupt line")
				}
				return
			}
			if err != nil {
				t.Fatalf("Spent: %v", err)
			}
			if got != tt.want {
				t.Errorf("Spent(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

// TestSpendRacesOnlyOnce is the reason the check and the append share a mutex:
// eight goroutines spend the same id at once, exactly one succeeds, and the file
// holds exactly one line. A check outside the lock would let several through and
// the run would answer the same event twice.
func TestSpendRacesOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.ndjson")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- l.Spend("check:ci:build:failure", "push")
		}()
	}
	wg.Wait()
	close(errs)

	var won, lost int
	for err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrAlreadySpent):
			lost++
		default:
			t.Fatalf("Spend returned %v, want nil or ErrAlreadySpent", err)
		}
	}
	if won != 1 {
		t.Errorf("%d goroutines spent one trigger, want exactly 1", won)
	}
	if lost != racers-1 {
		t.Errorf("%d goroutines were refused, want %d", lost, racers-1)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	lines, torn := readIntact(t, path)
	if torn != "" {
		t.Fatalf("torn tail %q", torn)
	}
	if len(lines) != 1 {
		t.Fatalf("ledger holds %d lines after one winning spend, want 1", len(lines))
	}
}

// TestSpendRefusesBadCalls covers the calls that cannot mean anything: a spend
// has to name an event and say what it did, and after Close nothing is written
// at all.
func TestSpendRefusesBadCalls(t *testing.T) {
	l, err := OpenLedger(filepath.Join(t.TempDir(), "run.ndjson"))
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	if err := l.Spend("", "reply"); err == nil {
		t.Error("Spend accepted an empty id")
	}
	if err := l.Spend("comment:1", ""); err == nil {
		t.Error("Spend accepted an empty action")
	}
	if _, err := l.Spent(""); err == nil {
		t.Error("Spent accepted an empty id")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Spend("comment:1", "reply"); !errors.Is(err, ErrLedgerClosed) {
		t.Errorf("Spend after Close = %v, want ErrLedgerClosed", err)
	}
}
