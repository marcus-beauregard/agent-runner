package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLedgerConcurrentAppends is the promise of the mutex: four goroutines
// appending at once produce 1000 whole lines whose seq values increase in file
// order, not merely in the order the calls started.
func TestLedgerConcurrentAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.ndjson")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}

	const writers, perWriter = 4, 250
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := l.Append("tool", "", map[string]any{"writer": w, "call": i}); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines, torn := readIntact(t, path)
	if torn != "" {
		t.Fatalf("file ends in a torn line %q, want a newline after every event", torn)
	}
	if len(lines) != writers*perWriter {
		t.Fatalf("got %d lines, want %d", len(lines), writers*perWriter)
	}
	var last int64
	for i, line := range lines {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not JSON: %v: %q", i, err, line)
		}
		if e.Seq <= last {
			t.Fatalf("line %d has seq %d after %d; seq must increase", i, e.Seq, last)
		}
		last = e.Seq
	}
	if last != writers*perWriter {
		t.Errorf("last seq = %d, want %d", last, writers*perWriter)
	}
}

// TestOpenLedgerRecovers covers the states a run.ndjson can be in when a
// process opens it: absent, empty, whole, cut mid-line, or not a stream this
// package wrote. The first four continue numbering from the last whole line —
// a resumed run must not reuse numbers already on disk — and the last is an
// error rather than a guess. Every existing file is left at 0600.
func TestOpenLedgerRecovers(t *testing.T) {
	line := func(seq int) string {
		return fmt.Sprintf(`{"seq":%d,"ts":"2026-11-21T00:00:00Z","kind":"stage","stage":"triage","data":null}`, seq)
	}
	tests := []struct {
		name    string
		content string
		create  bool
		want    int // complete lines after the recovery append
		wantErr bool
	}{
		{name: "absent", want: 1},
		{name: "empty file", create: true, want: 1},
		{name: "whole lines", create: true, content: line(1) + "\n" + line(2) + "\n", want: 3},
		{name: "torn tail", create: true, content: line(1) + "\n" + line(2) + "\n" + line(3)[:20], want: 3},
		{name: "corrupt last line", create: true, content: line(1) + "\nnot an event\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "run.ndjson")
			if tt.create {
				if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
					t.Fatalf("prepare ledger: %v", err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatalf("loosen mode: %v", err)
				}
			}

			l, err := OpenLedger(path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("OpenLedger accepted a last line that is not an event")
				}
				return
			}
			if err != nil {
				t.Fatalf("OpenLedger: %v", err)
			}
			if err := l.Append("stage", "implement", nil); err != nil {
				t.Fatalf("Append: %v", err)
			}
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			lines, torn := readIntact(t, path)
			if torn != "" {
				t.Fatalf("torn tail %q survived the open", torn)
			}
			if len(lines) != tt.want {
				t.Fatalf("got %d lines, want %d: %q", len(lines), tt.want, lines)
			}
			for i, raw := range lines {
				var e Event
				if err := json.Unmarshal([]byte(raw), &e); err != nil {
					t.Fatalf("line %d is not JSON: %v: %q", i, err, raw)
				}
				if e.Seq != int64(i+1) {
					t.Errorf("line %d has seq %d, want %d", i, e.Seq, i+1)
				}
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat ledger: %v", err)
			}
			if perm := fi.Mode().Perm(); perm != 0o600 {
				t.Errorf("ledger is %04o, want 0600", perm)
			}
		})
	}
}

// TestLedgerAppendAfterClose checks the end of the stream is an error, not a
// panic and not a silent write to a dead descriptor. Close is safe twice.
func TestLedgerAppendAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.ndjson")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	if err := l.Append("state", "", "running"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	if err := l.Append("state", "", "done"); !errors.Is(err, ErrLedgerClosed) {
		t.Fatalf("Append after Close = %v, want ErrLedgerClosed", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("Append after Close changed the file:\nbefore %q\nafter  %q", before, after)
	}
}

// TestLedgerFailedAppendWritesNothing covers the two failures before a line
// exists, and the one after: unmarshalable data and a closed descriptor leave no
// sequence number and no bytes behind, and a ledger whose write failed refuses
// to append onto the fragment that write may have left.
func TestLedgerFailedAppendWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.ndjson")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	if err := l.Append("tool", "", make(chan int)); err == nil {
		t.Fatal("Append accepted a channel as data")
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("stat ledger: %v", err)
	} else if fi.Size() != 0 {
		t.Fatalf("failed encode wrote %d bytes", fi.Size())
	}
	if err := l.Append("state", "", "running"); err != nil {
		t.Fatalf("Append after a failed encode: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	// Closing the descriptor behind the ledger is the shape of a write the
	// kernel refuses: the line may or may not have landed.
	if err := l.w.Close(); err != nil {
		t.Fatalf("close the descriptor behind the ledger: %v", err)
	}
	if err := l.Append("state", "", "done"); err == nil {
		t.Fatal("Append succeeded on a closed descriptor")
	}
	if err := l.Append("state", "", "failed"); err == nil || !strings.Contains(err.Error(), "failed write") {
		t.Fatalf("second Append = %v, want the ledger to report its failed write", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("a failed write left debris: %q", after)
	}
}

// TestLedgerLineShape pins the format the file's readers depend on: one object
// per line, fixed field order, stage only when there is one, a UTC timestamp,
// and 0600 on a ledger created on someone's machine.
func TestLedgerLineShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.ndjson")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	if err := l.Append("stage", "triage", map[string]string{"issue": "#28"}); err != nil {
		t.Fatalf("Append stage: %v", err)
	}
	if err := l.Append("decision", "", map[string]string{"why": "still open"}); err != nil {
		t.Fatalf("Append decision: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat ledger: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("ledger is %04o, want 0600", perm)
	}

	lines, torn := readIntact(t, path)
	if torn != "" {
		t.Fatalf("torn tail %q", torn)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}

	ordered := []string{"seq", "ts", "kind", "stage", "data"}
	for i := 1; i < len(ordered); i++ {
		prev := strings.Index(lines[0], `"`+ordered[i-1]+`"`)
		next := strings.Index(lines[0], `"`+ordered[i]+`"`)
		if prev < 0 || next < 0 || prev > next {
			t.Fatalf("field %q must follow %q in the first line: %s", ordered[i], ordered[i-1], lines[0])
		}
	}
	if strings.Contains(lines[1], `"stage"`) {
		t.Errorf("empty stage should be omitted, got %s", lines[1])
	}

	var e Event
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("first line is not an event: %v", err)
	}
	if e.Seq != 1 || e.Kind != "stage" || e.Stage != "triage" {
		t.Errorf("event = %+v, want seq 1, kind stage, stage triage", e)
	}
	if lt := e.TS.Location(); lt != time.UTC {
		t.Errorf("ts location = %v, want UTC", lt)
	}
	if string(e.Data) != `{"issue":"#28"}` {
		t.Errorf("data = %s, want the payload unchanged", e.Data)
	}
}

// readIntact returns the newline-terminated lines of the ledger and the
// unterminated fragment after the last newline, if any. It is the test's model
// of a reader: a partial final line is reported and skipped, never parsed.
func readIntact(t *testing.T, path string) (lines []string, torn string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	text := string(b)
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			return lines, text
		}
		lines = append(lines, text[:i])
		text = text[i+1:]
	}
}
