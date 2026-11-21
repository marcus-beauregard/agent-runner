package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/sandbox"
)

// fieldLine returns the line run.json writes key on, or -1. The record is
// indented one field per line, and the field order is part of its shape: two
// runs that did the same thing have to produce the same bytes, so a field that
// moves is a change every reader of the file sees.
func fieldLine(b []byte, key string) int {
	quoted := "\"" + key + "\""
	for i, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), quoted+":") {
			return i
		}
	}
	return -1
}

// TestNewRecordsSandbox is #87's first half: a granted mode is in run.json
// between budget and account, it survives the Finish that rewrites the record,
// and a run that never got one has no field at all. An absent sandbox is not
// worktree, and a field spelled empty would blur the two.
func TestNewRecordsSandbox(t *testing.T) {
	base := t.TempDir()
	// Account is set so the field before and after the new one are both in the
	// file: account is omitted when empty, and a positional check against an
	// absent neighbour would pass for the wrong reason.
	r, err := New(base, Meta{Kind: "build", Subject: "https://example.test/issues/87", Account: "marcus-beauregard", Sandbox: "container"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(r.Dir, "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	if got := fieldLine(b, "sandbox"); got < 0 {
		t.Fatalf("run.json has no sandbox field:\n%s", b)
	}
	if budget, sbx, account := fieldLine(b, "budget"), fieldLine(b, "sandbox"), fieldLine(b, "account"); !(budget < sbx && sbx < account) {
		t.Errorf("sandbox is at line %d, not between budget (%d) and account (%d):\n%s", sbx, budget, account, b)
	}

	// Finish rewrites the whole record from what it loaded, so the mode has to
	// survive a run that ended, not only the New that created it.
	if err := r.Finish(0); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	b, err = os.ReadFile(filepath.Join(r.Dir, "run.json"))
	if err != nil {
		t.Fatalf("read finished run.json: %v", err)
	}
	if !strings.Contains(string(b), "\"sandbox\": \"container\"") {
		t.Errorf("finished record lost the sandbox mode:\n%s", b)
	}

	empty, err := New(base, Meta{Kind: "build", Subject: "https://example.test/issues/88"})
	if err != nil {
		t.Fatalf("New without a mode: %v", err)
	}
	b, err = os.ReadFile(filepath.Join(empty.Dir, "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	if fieldLine(b, "sandbox") >= 0 {
		t.Errorf("a run with no mode recorded one anyway:\n%s", b)
	}
}

// TestHeaderSandbox is #87's second half: the report's first screen says which
// isolation the run got, worktree carries the same sentence the warning prints,
// and a record with no mode says so rather than leaving the line out — a reader
// who sees no sandbox line assumes the strong mode.
func TestHeaderSandbox(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want []string
	}{
		{name: "container", mode: "container", want: []string{"- Sandbox: container"}},
		{
			name: "worktree names what is unenforced",
			mode: string(sandbox.Worktree),
			want: []string{"- Sandbox: worktree (", "network egress from children", "are unenforced)"},
		},
		{name: "not recorded", mode: "", want: []string{"- Sandbox: (not recorded)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			writeHeader(&b, &record{ID: "20260101-000000.000000-abcdef", State: stateRunning, Sandbox: tc.mode})
			got := b.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("header does not contain %q:\n%s", want, got)
				}
			}
			// The line has to be on the first screen, before any stage section.
			if at, firstStage := strings.Index(got, "- Sandbox:"), strings.Index(got, "## "); at < 0 || (firstStage >= 0 && at > firstStage) {
				t.Errorf("sandbox is not in the header:\n%s", got)
			}
		})
	}
}
