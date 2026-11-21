package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcus-beauregard/agent-runner/internal/world"
)

const testSubject = "https://github.com/marcus-beauregard/agent-runner/issues/20"

// testWorld is a thread with something in every field the digest covers, so a
// change to any one of them is visible as a different digest.
func testWorld(subject string) world.World {
	return world.World{
		Subject: subject,
		Issue: world.Issue{
			Number:    20,
			Title:     "the record",
			State:     "open",
			Assignees: []string{"marcus-beauregard"},
			Labels:    []string{"record"},
		},
		PR: world.PR{
			Number:    40,
			HeadSHA:   "aaa111",
			BaseSHA:   "bbb222",
			Mergeable: world.MergeableYes,
			State:     "open",
		},
		Reviews:  []world.Review{{ID: 1, State: "APPROVED", Author: "marcus-beauregard"}},
		Checks:   []world.Check{{ID: 7, Name: "test", Conclusion: "success", Status: "completed"}},
		Comments: []world.Comment{{ID: 100, Author: "reviewer", CreatedAt: "2026-01-01T00:00:00Z"}},
	}
}

// TestLoadStateMissing proves an unrun subject is ErrNoState rather than a plain
// error: the caller has to tell "never run" from "state is broken".
func TestLoadStateMissing(t *testing.T) {
	if _, err := LoadState(t.TempDir(), testSubject); !errors.Is(err, ErrNoState) {
		t.Fatalf("LoadState with no file = %v, want an error wrapping ErrNoState", err)
	}
}

// TestSaveLoadState checks the round trip and the file's privacy: the state
// records when this machine last looked at a thread, which is nobody else's.
func TestSaveLoadState(t *testing.T) {
	base := t.TempDir()
	want := State{Digest: "abc123", UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	if err := SaveState(base, testSubject, want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(base, testSubject)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Digest != want.Digest {
		t.Errorf("digest = %q, want %q", got.Digest, want.Digest)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("updated_at = %s, want %s", got.UpdatedAt, want.UpdatedAt)
	}
	path, err := statePath(base, testSubject)
	if err != nil {
		t.Fatalf("statePath: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file is %04o, want 0600", perm)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat state directory: %v", err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("state directory is %04o, want 0700", perm)
	}
}

// TestStateKeyedBySubject proves two threads never share a digest: the file name
// is the hash of the subject, so one thread's state cannot answer for another.
func TestStateKeyedBySubject(t *testing.T) {
	base := t.TempDir()
	other := "https://github.com/marcus-beauregard/agent-runner/issues/21"
	if err := SaveState(base, testSubject, State{Digest: "one"}); err != nil {
		t.Fatalf("SaveState first: %v", err)
	}
	if err := SaveState(base, other, State{Digest: "two"}); err != nil {
		t.Fatalf("SaveState second: %v", err)
	}
	s, err := LoadState(base, testSubject)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if s.Digest != "one" {
		t.Errorf("first subject digest = %q, want one", s.Digest)
	}
	if _, err := LoadState(base, other); err != nil {
		t.Fatalf("LoadState other: %v", err)
	}
}

// TestLoadStateCorrupt proves a damaged file is an error and not a quiet
// "changed" or "unchanged": either answer would be a guess about a ledger
// somebody has to fix.
func TestLoadStateCorrupt(t *testing.T) {
	base := t.TempDir()
	path, err := statePath(base, testSubject)
	if err != nil {
		t.Fatalf("statePath: %v", err)
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	if _, err := LoadState(base, testSubject); err == nil || errors.Is(err, ErrNoState) {
		t.Fatalf("LoadState on corrupt state = %v, want a parse error that is not ErrNoState", err)
	}
	if _, err := Unchanged(base, testSubject, "abc"); err == nil {
		t.Fatal("Unchanged on corrupt state = nil error, want the parse error")
	}
}

// TestSaveStateRefusesEmptyDigest keeps a half-built State from writing a file
// that every later run reads as "state exists, digest empty".
func TestSaveStateRefusesEmptyDigest(t *testing.T) {
	if err := SaveState(t.TempDir(), testSubject, State{}); err == nil {
		t.Fatal("SaveState with an empty digest = nil, want an error")
	}
}

// TestBegin is the fast path end to end: the first run proceeds and records its
// digest; the second run over the same world creates no run directory and says
// unchanged; a change to the world makes the next run proceed again.
func TestBegin(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	w := testWorld(testSubject)
	fetch := func(context.Context, string) (world.World, error) { return w, nil }
	meta := Meta{Kind: "resolve", Subject: testSubject}
	runs := filepath.Join(base, "agent-runner", "runs")

	first, err := Begin(ctx, base, meta, fetch, "")
	if err != nil {
		t.Fatalf("first Begin: %v", err)
	}
	if first.Unchanged {
		t.Fatal("first Begin reported unchanged with no state on disk")
	}
	if first.Run == nil {
		t.Fatal("first Begin created no run directory")
	}
	if first.Digest == "" {
		t.Error("first Begin returned an empty digest")
	}
	if _, err := os.Stat(first.Run.Dir); err != nil {
		t.Fatalf("first run directory: %v", err)
	}
	// The digest is recorded when the run finishes, not when it starts.
	if _, err := LoadState(base, testSubject); !errors.Is(err, ErrNoState) {
		t.Fatalf("state exists before the run finished: %v", err)
	}
	if err := first.Remember(); err != nil {
		t.Fatalf("first Remember: %v", err)
	}
	statePathBefore, err := statePath(base, testSubject)
	if err != nil {
		t.Fatalf("statePath: %v", err)
	}
	before, err := os.ReadFile(statePathBefore)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}

	second, err := Begin(ctx, base, meta, fetch, "")
	if err != nil {
		t.Fatalf("second Begin: %v", err)
	}
	if !second.Unchanged {
		t.Fatal("second Begin over an unchanged world did not report unchanged")
	}
	if second.Run != nil {
		t.Errorf("second Begin created %s, want no run directory", second.Run.Dir)
	}
	if err := second.Remember(); err != nil {
		t.Fatalf("Remember on an unchanged start: %v", err)
	}
	entries, err := os.ReadDir(runs)
	if err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("runs holds %d entries after the unchanged run, want 1", len(entries))
	}
	after, err := os.ReadFile(statePathBefore)
	if err != nil {
		t.Fatalf("read state after the unchanged run: %v", err)
	}
	if string(after) != string(before) {
		t.Error("the unchanged run rewrote the state file")
	}

	w.Comments = append(w.Comments, world.Comment{ID: 101, Author: "reviewer", CreatedAt: "2026-01-02T00:00:00Z"})
	third, err := Begin(ctx, base, meta, fetch, "")
	if err != nil {
		t.Fatalf("third Begin: %v", err)
	}
	if third.Unchanged {
		t.Fatal("Begin reported unchanged after the world gained a comment")
	}
	if third.Run == nil {
		t.Fatal("Begin after a change created no run directory")
	}
	if third.Digest == second.Digest {
		t.Error("the digest did not move with the world")
	}
}

// TestBeginOtherSubjectIsNewWork proves the state key is the subject: the same
// world read through a different URL is a thread this machine has not answered,
// so it must not be reported unchanged.
func TestBeginOtherSubjectIsNewWork(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	const other = "https://github.com/marcus-beauregard/agent-runner/issues/21"
	fetch := func(context.Context, string) (world.World, error) { return testWorld(other), nil }

	first, err := Begin(ctx, base, Meta{Kind: "resolve", Subject: testSubject}, fetch, "")
	if err != nil {
		t.Fatalf("first Begin: %v", err)
	}
	if err := first.Remember(); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	second, err := Begin(ctx, base, Meta{Kind: "resolve", Subject: other}, fetch, "")
	if err != nil {
		t.Fatalf("second Begin: %v", err)
	}
	if second.Unchanged {
		t.Fatal("a different subject was reported unchanged from the first subject's state")
	}
}
