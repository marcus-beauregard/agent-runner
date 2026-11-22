package envstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// now is the clock every case is judged against: the policy is about age, and a
// moving clock would make a failure unreadable.
var now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// putEnv writes an environment by hand. Create stamps the manifest "now", and
// the thing under test is what the store does with one that was built weeks ago,
// so the manifest is written directly rather than aged after the fact.
func putEnv(t *testing.T, base, name, dig string, built time.Time, state string, size int) string {
	t.Helper()
	dir := filepath.Join(base, "agent-runner", "envs", name)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
		t.Fatalf("make %s: %v", name, err)
	}
	b, err := json.Marshal(Manifest{Digest: dig, State: state, CreatedAt: built.UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env.json"), b, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if size > 0 {
		if err := os.WriteFile(filepath.Join(dir, "bin", "tool"), make([]byte, size), 0o700); err != nil {
			t.Fatalf("write tool: %v", err)
		}
	}
	return dir
}

func entryFor(t *testing.T, entries []Entry, name string) Entry {
	t.Helper()
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry named %q in %d entries", name, len(entries))
	return Entry{}
}

// TestCollectByAge is the ticket's first sentence: a directory is collected when
// the policy's age has passed, never before, and the reason says which.
func TestCollectByAge(t *testing.T) {
	base := t.TempDir()
	stopped := "abababab.tmp-123456"
	putEnv(t, base, digest, digest, now.Add(-40*24*time.Hour), StateReady, 100)
	putEnv(t, base, otherDigest(), otherDigest(), now.Add(-2*24*time.Hour), StateReady, 200)
	if err := os.Rename(
		putEnv(t, base, "tmpold", "tmpold", now.Add(-40*24*time.Hour), StateBuilding, 50),
		filepath.Join(base, "agent-runner", "envs", stopped),
	); err != nil {
		t.Fatalf("rename tmp sibling: %v", err)
	}
	putEnv(t, base, "cccccccc.tmp-999", "cccccccc", now.Add(-time.Hour), StateBuilding, 60)
	// A lock file is not an environment, and a young build is not old enough.
	if err := os.WriteFile(filepath.Join(base, "agent-runner", "envs", digest+".lock"), nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	entries, err := Collect(base, Policy{Age: 30 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("%d entries, want 4 (a lock is not an environment): %+v", len(entries), entries)
	}

	old := entryFor(t, entries, digest)
	if !old.Collect || old.Reason != "unused for 40 days" {
		t.Errorf("old environment = collect %v, %q; want collected and \"unused for 40 days\"", old.Collect, old.Reason)
	}
	// The tool is 100 bytes and env.json is counted with it: the number is what
	// the directory holds, not what the build installed.
	if old.Bytes < 100 {
		t.Errorf("old environment bytes = %d, want at least the 100-byte tool", old.Bytes)
	}
	if old.Digest != digest {
		t.Errorf("digest = %q, want %q", old.Digest, digest)
	}

	young := entryFor(t, entries, otherDigest())
	if young.Collect || !strings.Contains(young.Reason, "under the 30 days age") {
		t.Errorf("young environment = collect %v, %q; want kept for being under the age", young.Collect, young.Reason)
	}
	if young.Digest != otherDigest() {
		t.Errorf("young digest = %q, want %q", young.Digest, otherDigest())
	}

	// A build that stopped is the store's own garbage: a sibling under the
	// digest, never an environment (envstore.go's Create doc says gc collects it).
	dead := entryFor(t, entries, stopped)
	if !dead.Collect || !strings.Contains(dead.Reason, "a build that stopped 40 days ago") {
		t.Errorf("stopped build = collect %v, %q; want collected as a stopped build", dead.Collect, dead.Reason)
	}
	if dead.Digest != "abababab" {
		t.Errorf("stopped build digest = %q, want abababab", dead.Digest)
	}
	if fresh := entryFor(t, entries, "cccccccc.tmp-999"); fresh.Collect {
		t.Errorf("a one-hour-old build was collected: %q", fresh.Reason)
	}
}

// TestCollectKeepsWhatItCannotDescribe is the refusal half: a directory the
// store cannot account for is evidence, and gc does not delete evidence.
func TestCollectKeepsWhatItCannotDescribe(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "agent-runner", "envs")
	if err := os.MkdirAll(filepath.Join(root, otherDigest()), 0o700); err != nil {
		t.Fatalf("make directory: %v", err)
	}
	putEnv(t, base, strings.Repeat("cd", 32), strings.Repeat("cd", 32), now.Add(-90*24*time.Hour), StateBuilding, 10)
	putEnv(t, base, strings.Repeat("ef", 32), digest, now.Add(-90*24*time.Hour), StateReady, 10)

	entries, err := Collect(base, Policy{Age: 30 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	noManifest := entryFor(t, entries, otherDigest())
	if noManifest.Collect || !strings.Contains(noManifest.Reason, "no readable env.json") {
		t.Errorf("directory without a manifest = collect %v, %q", noManifest.Collect, noManifest.Reason)
	}
	unfinished := entryFor(t, entries, strings.Repeat("cd", 32))
	if unfinished.Collect || !strings.Contains(unfinished.Reason, "building") {
		t.Errorf("published directory that never finished = collect %v, %q", unfinished.Collect, unfinished.Reason)
	}
	moved := entryFor(t, entries, strings.Repeat("ef", 32))
	if moved.Collect || !strings.Contains(moved.Reason, "another digest") {
		t.Errorf("manifest naming another digest = collect %v, %q", moved.Collect, moved.Reason)
	}
}

// TestCollectKeepsTheNewest is the tier's other rule: keeping a few digests is
// cheaper than provisioning them again, so --keep spares the newest whatever
// their age says.
func TestCollectKeepsTheNewest(t *testing.T) {
	base := t.TempDir()
	names := []string{strings.Repeat("11", 32), strings.Repeat("22", 32), strings.Repeat("33", 32)}
	for i, n := range names {
		putEnv(t, base, n, n, now.Add(-time.Duration(60+i)*24*time.Hour), StateReady, 10)
	}
	entries, err := Collect(base, Policy{Age: 30 * 24 * time.Hour, Keep: 2}, now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// The newest two are the *least* old: 60 and 61 days.
	if e := entryFor(t, entries, names[0]); e.Collect || e.Reason != "one of the newest 2" {
		t.Errorf("newest = collect %v, %q; want kept", e.Collect, e.Reason)
	}
	if e := entryFor(t, entries, names[1]); e.Collect {
		t.Errorf("second newest was collected: %q", e.Reason)
	}
	if e := entryFor(t, entries, names[2]); !e.Collect {
		t.Errorf("oldest was kept: %q", e.Reason)
	}
}

// TestCollectReportsAnEnvironmentHeldByAReader is the ticket's "never a live
// one": the lock is what says live, and a listing that could not take it says so
// instead of calling the environment collectable.
func TestCollectReportsAnEnvironmentHeldByAReader(t *testing.T) {
	base := t.TempDir()
	putEnv(t, base, digest, digest, now.Add(-40*24*time.Hour), StateReady, 10)
	l, err := TryLock(base, digest)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	defer l.Discard()

	entries, err := Collect(base, Policy{Age: 30 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if e := entryFor(t, entries, digest); e.Collect || e.Reason != "held by a reader or a build" {
		t.Errorf("held environment = collect %v, %q; want kept as held", e.Collect, e.Reason)
	}
	if _, err := Remove(base, entryFor(t, entries, digest)); !errors.Is(err, ErrLocked) {
		t.Errorf("Remove on a held environment = %v, want ErrLocked", err)
	}
	if _, err := os.Stat(filepath.Join(base, "agent-runner", "envs", digest)); err != nil {
		t.Errorf("a held environment was deleted: %v", err)
	}
}

// TestCollectKeepsABuildInProgress is the same rule for the tree that is not an
// environment yet. Create's lock is the only evidence a build is live, and the
// policy says nothing about it: even an age of zero must not reach it.
func TestCollectKeepsABuildInProgress(t *testing.T) {
	base := t.TempDir()
	e, err := Create(base, Manifest{Digest: digest, CreatedAt: now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if e.Dir == filepath.Join(base, "agent-runner", "envs", digest) {
		t.Fatalf("Create published the environment at its digest: %q", e.Dir)
	}

	entries, err := Collect(base, Policy{Age: 0}, now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	build := entryFor(t, entries, filepath.Base(e.Dir))
	if build.Collect || !strings.Contains(build.Reason, "held by a reader or a build") {
		t.Errorf("a build in progress = collect %v, %q; want kept as held", build.Collect, build.Reason)
	}
	if _, err := Remove(base, build); !errors.Is(err, ErrLocked) {
		t.Errorf("Remove on a build in progress = %v, want ErrLocked", err)
	}
	if _, err := os.Stat(e.Dir); err != nil {
		t.Errorf("a build in progress was deleted: %v", err)
	}

	// Once it publishes, the sibling is gone and only the environment is left
	// for the age policy to consider.
	if err := e.MarkReady(0); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	entries, err = Collect(base, Policy{Age: 30 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("Collect after MarkReady: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries after publish, want 1: %+v", len(entries), entries)
	}
	if !entries[0].Collect {
		t.Errorf("the published environment was kept: %q", entries[0].Reason)
	}
}

// TestRemoveCollectsOneEnvironment is the apply half: the directory and its lock
// file go, the bytes come back, and nothing else in the store moves.
func TestRemoveCollectsOneEnvironment(t *testing.T) {
	base := t.TempDir()
	dir := putEnv(t, base, digest, digest, now.Add(-40*24*time.Hour), StateReady, 128)
	putEnv(t, base, otherDigest(), otherDigest(), now.Add(-40*24*time.Hour), StateReady, 16)
	held, err := TryLock(base, digest)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	// Unlock leaves the file behind, as a listing does; the collector it guards
	// is what removes it.
	if err := held.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	entries, err := Collect(base, Policy{Age: 30 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	n, err := Remove(base, entryFor(t, entries, digest))
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if n < 128 {
		t.Errorf("freed %d bytes, want at least the 128-byte tool", n)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("environment directory still there: %v", err)
	}
	if _, err := os.Stat(dir + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "agent-runner", "envs", otherDigest())); err != nil {
		t.Errorf("a sibling was deleted too: %v", err)
	}
}

// TestCollectOnAnEmptyStore is the first run: no store is nothing to collect, not
// a failure.
func TestCollectOnAnEmptyStore(t *testing.T) {
	entries, err := Collect(t.TempDir(), Policy{Age: 30 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("Collect on an empty store: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d entries, want none", len(entries))
	}
}
