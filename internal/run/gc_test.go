package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gcRun opens a run under base and gives it a real checkout in parent: the shape
// a run that failed, was refused, or stopped for a person leaves for gc.
func gcRun(t *testing.T, base, parent, subject string) *Run {
	t.Helper()
	r, err := New(base, Meta{Kind: "build", Subject: subject, Repo: parent})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := Worktree(r.Dir, parent, Slug(subject, r.ID)); err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	return r
}

// setRunState puts a run in a terminal state the way Finish would, without the
// cleanup Finish performs: gc's candidates are exactly the runs whose cleanup
// did not happen.
func setRunState(t *testing.T, dir, state string, code int) {
	t.Helper()
	rec, err := loadRecord(dir)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	rec.State, rec.ExitCode = state, &code
	if err := writeRecord(dir, *rec); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

// ageRun moves a run's start time, which is the clock the worktree tier counts
// from, so the policy can be exercised without waiting for it.
func ageRun(t *testing.T, dir string, when time.Time) {
	t.Helper()
	rec, err := loadRecord(dir)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	rec.CreatedAt = when.UTC().Format(time.RFC3339)
	if err := writeRecord(dir, *rec); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

func entryFor(t *testing.T, entries []WorktreeEntry, id string) WorktreeEntry {
	t.Helper()
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no entry for run %s in %d entries", id, len(entries))
	return WorktreeEntry{}
}

// TestCollectWorktreesListsThenCollects is the tier's done-when, first half: a
// dry run names the old checkout, its size and its reason and moves nothing, and
// collecting it takes the tree and the cache while the record stays.
func TestCollectWorktreesListsThenCollects(t *testing.T) {
	parent := worktreeParent(t)
	base := t.TempDir()
	old := gcRun(t, base, parent, cageSubject)
	fresh := gcRun(t, base, parent, cageSubject2)
	setRunState(t, old.Dir, stateFailed, 1)
	setRunState(t, fresh.Dir, stateFailed, 1)
	ageRun(t, old.Dir, time.Now().Add(-20*24*time.Hour))
	ageRun(t, fresh.Dir, time.Now().Add(-2*24*time.Hour))
	if err := os.MkdirAll(filepath.Join(old.Dir, "cache", "go-build"), 0o700); err != nil {
		t.Fatalf("make the cache: %v", err)
	}
	writeTestFile(t, filepath.Join(old.Dir, "cache", "go-build", "obj"), strings.Repeat("c", 2048))
	// Work the run never committed: the run's own cleanup refuses this tree, and
	// §6.3 gives gc the age rule precisely so somebody can still take it.
	writeTestFile(t, filepath.Join(old.Dir, "worktree", "scratch.txt"), "uncommitted\n")

	entries, err := CollectWorktrees(base, 14*24*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("CollectWorktrees: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want one per run", len(entries))
	}
	oldEntry := entryFor(t, entries, old.ID)
	if !oldEntry.Collect {
		t.Errorf("the 20-day checkout is not a candidate: %s", oldEntry.Reason)
	}
	for _, want := range []string{"state failed", "20 days"} {
		if !strings.Contains(oldEntry.Reason, want) {
			t.Errorf("reason %q does not say %q", oldEntry.Reason, want)
		}
	}
	if oldEntry.Bytes < 2048 {
		t.Errorf("bytes = %d, want at least the 2048-byte cache", oldEntry.Bytes)
	}
	if freshEntry := entryFor(t, entries, fresh.ID); freshEntry.Collect {
		t.Errorf("the 2-day checkout qualifies: %s", freshEntry.Reason)
	} else if !strings.Contains(freshEntry.Reason, "under the 14 days age") {
		t.Errorf("reason %q does not name the age it is under", freshEntry.Reason)
	}
	if _, err := os.Stat(filepath.Join(old.Dir, "worktree")); err != nil {
		t.Fatalf("the listing deleted the checkout: %v", err)
	}

	if _, err := RemoveWorktreeEntry(oldEntry); err != nil {
		t.Fatalf("RemoveWorktreeEntry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(old.Dir, "worktree")); !os.IsNotExist(err) {
		t.Errorf("the checkout survived collection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(old.Dir, "cache")); !os.IsNotExist(err) {
		t.Errorf("the cache survived collection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(old.Dir, "run.json")); err != nil {
		t.Errorf("the record went with the checkout: %v", err)
	}
	if out := mustGit(t, parent, "worktree", "list", "--porcelain"); strings.Contains(out, old.ID) {
		t.Errorf("the parent still registers the collected tree:\n%s", out)
	}
	// The record still names a tree that is gone, so a second pass over the same
	// run must be the no-op that prunes and proves, not an error.
	if _, err := RemoveWorktreeEntry(oldEntry); err != nil {
		t.Errorf("collecting an already-collected checkout: %v", err)
	}
}

// TestCollectWorktreesKeepsAResumableRun is the one line the tier does not
// cross: a run with no terminal state is resumable, and the tree is what the
// resume edits.
func TestCollectWorktreesKeepsAResumableRun(t *testing.T) {
	parent := worktreeParent(t)
	base := t.TempDir()
	r := gcRun(t, base, parent, cageSubject)
	ageRun(t, r.Dir, time.Now().Add(-40*24*time.Hour))

	entries, err := CollectWorktrees(base, 14*24*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("CollectWorktrees: %v", err)
	}
	e := entryFor(t, entries, r.ID)
	if e.Collect {
		t.Errorf("a resumable run is a candidate: %s", e.Reason)
	}
	if !strings.Contains(e.Reason, "no terminal state") {
		t.Errorf("reason %q does not say the run is resumable", e.Reason)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "worktree")); err != nil {
		t.Errorf("the checkout was taken: %v", err)
	}
}

// TestCollectWorktreesKeepsARecordWithNoRepo is the case the tier cannot
// account for: without the parent it cannot prove a registration gone, so the
// checkout stays for a person rather than being taken halfway.
func TestCollectWorktreesKeepsARecordWithNoRepo(t *testing.T) {
	parent := worktreeParent(t)
	base := t.TempDir()
	r := gcRun(t, base, parent, cageSubject)
	rec, err := loadRecord(r.Dir)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	rec.State, rec.Repo = stateFailed, ""
	if err := writeRecord(r.Dir, *rec); err != nil {
		t.Fatalf("write record: %v", err)
	}
	ageRun(t, r.Dir, time.Now().Add(-40*24*time.Hour))

	entries, err := CollectWorktrees(base, 14*24*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("CollectWorktrees: %v", err)
	}
	e := entryFor(t, entries, r.ID)
	if e.Collect {
		t.Errorf("a record naming no repo is a candidate: %s", e.Reason)
	}
	if !strings.Contains(e.Reason, "names no repo") {
		t.Errorf("reason %q does not name the missing field", e.Reason)
	}
}

// TestPruneOrphansDropsAGoneRunsRegistration is the orphan rule: a registration
// pointing into a run directory that is gone is pruned in the parent, and the
// dry run leaves it alone.
func TestPruneOrphansDropsAGoneRunsRegistration(t *testing.T) {
	parent := worktreeParent(t)
	base := t.TempDir()
	survivor := gcRun(t, base, parent, cageSubject)
	gone := gcRun(t, base, parent, cageSubject2)
	if err := os.RemoveAll(gone.Dir); err != nil {
		t.Fatalf("remove the run directory: %v", err)
	}
	if out := mustGit(t, parent, "worktree", "list", "--porcelain"); !strings.Contains(out, gone.ID) {
		t.Fatalf("the fixture left no orphan registration to find:\n%s", out)
	}

	if n, err := PruneOrphans(base, false); err != nil {
		t.Fatalf("PruneOrphans dry run: %v", err)
	} else if n != 1 {
		t.Errorf("dry run found %d orphans, want 1", n)
	}
	if out := mustGit(t, parent, "worktree", "list", "--porcelain"); !strings.Contains(out, gone.ID) {
		t.Errorf("the dry run pruned a registration:\n%s", out)
	}

	if n, err := PruneOrphans(base, true); err != nil {
		t.Fatalf("PruneOrphans apply: %v", err)
	} else if n != 1 {
		t.Errorf("apply found %d orphans, want 1", n)
	}
	out := mustGit(t, parent, "worktree", "list", "--porcelain")
	if strings.Contains(out, gone.ID) {
		t.Errorf("the orphan registration survived the prune:\n%s", out)
	}
	// The survivor's registration is not the prune's to take.
	if !strings.Contains(out, survivor.ID) {
		t.Errorf("the prune dropped a live registration:\n%s", out)
	}
}

// TestCollectWorktreesWithoutRuns keeps the first invocation on a fresh machine
// quiet: no record directory is an empty report, not an error.
func TestCollectWorktreesWithoutRuns(t *testing.T) {
	base := t.TempDir()
	entries, err := CollectWorktrees(base, 14*24*time.Hour, time.Now())
	if err != nil || len(entries) != 0 {
		t.Fatalf("CollectWorktrees = %d entries, %v; want none", len(entries), err)
	}
	if n, err := PruneOrphans(base, true); err != nil || n != 0 {
		t.Fatalf("PruneOrphans = %d, %v; want 0", n, err)
	}
}
