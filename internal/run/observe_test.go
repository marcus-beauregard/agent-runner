package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/world"
)

// TestSaveLoadWorldRoundTrip is the whole contract of a snapshot: what was
// written is what comes back, the same digest included, and the file is
// nobody else's to read.
func TestSaveLoadWorldRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := testWorld(testSubject)
	if err := SaveWorld(dir, want); err != nil {
		t.Fatalf("SaveWorld: %v", err)
	}
	got, err := LoadWorld(dir)
	if err != nil {
		t.Fatalf("LoadWorld: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the World:\n got %+v\nwant %+v", got, want)
	}
	wantDigest, err := want.Digest("")
	if err != nil {
		t.Fatalf("digest saved world: %v", err)
	}
	gotDigest, err := got.Digest("")
	if err != nil {
		t.Fatalf("digest loaded world: %v", err)
	}
	if gotDigest != wantDigest {
		t.Errorf("loaded digest = %s, want %s", gotDigest, wantDigest)
	}
	fi, err := os.Stat(filepath.Join(dir, worldName))
	if err != nil {
		t.Fatalf("stat world.json: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("world.json is %04o, want 0600", perm)
	}
	// The atomic write publishes by rename; nothing may be left beside it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read run directory: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("run directory holds %d entries after SaveWorld, want 1", len(entries))
	}
}

// TestLoadWorldRefusesUnusableSnapshot proves a torn or absent observation is an
// error and a zero World: replay has to say it cannot replay the run rather than
// compare against a thread with nothing in it.
func TestLoadWorldRefusesUnusableSnapshot(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		write bool
	}{
		{name: "missing"},
		{name: "empty", write: true},
		{name: "whitespace", body: " \t", write: true},
		{name: "truncated", body: "{\"subject\":\"https://github.com/o/r/issues/1\",\"issue\":{", write: true},
		{name: "empty object", body: "{}", write: true},
		{name: "no issue number", body: "{\"subject\":\"https://github.com/o/r/issues/1\"}", write: true},
		{name: "mergeable not a state", body: "{\"subject\":\"https://github.com/o/r/issues/1\",\"issue\":{\"number\":1},\"pr\":{\"mergeable\":\"maybe\"}}", write: true},
		{name: "mergeable as ordinal", body: "{\"subject\":\"https://github.com/o/r/issues/1\",\"issue\":{\"number\":1},\"pr\":{\"mergeable\":2}}", write: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.write {
				if err := os.WriteFile(filepath.Join(dir, worldName), []byte(tc.body), 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}
			got, err := LoadWorld(dir)
			if !errors.Is(err, ErrNoWorld) {
				t.Fatalf("LoadWorld = %v, want an error wrapping ErrNoWorld", err)
			}
			if !reflect.DeepEqual(got, world.World{}) {
				t.Errorf("LoadWorld returned %+v beside the error, want the zero World", got)
			}
		})
	}
}

// TestSaveWorldRefusesEmptyWorld keeps a run from recording a decision it cannot
// name: a World with no subject or no issue is not a thread.
func TestSaveWorldRefusesEmptyWorld(t *testing.T) {
	cases := []struct {
		name string
		w    world.World
	}{
		{name: "zero", w: world.World{}},
		{name: "no subject", w: world.World{Issue: world.Issue{Number: 20}}},
		{name: "no issue", w: world.World{Subject: testSubject}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := SaveWorld(dir, tc.w); err == nil {
				t.Fatal("SaveWorld accepted a World that is not a thread")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read run directory: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("SaveWorld left %d entries behind after refusing", len(entries))
			}
		})
	}
}

// TestBeginSnapshotsWorld proves Begin keeps the World it decided on, not just
// its digest: the snapshot round-trips to the same World and the same digest.
func TestBeginSnapshotsWorld(t *testing.T) {
	base := t.TempDir()
	want := testWorld(testSubject)
	fetch := func(context.Context, string) (world.World, error) { return want, nil }
	s, err := Begin(context.Background(), base, Meta{Kind: testKind, Subject: testSubject}, fetch, "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if s.Run == nil {
		t.Fatal("Begin created no run directory")
	}
	got, err := LoadWorld(s.Run.Dir)
	if err != nil {
		t.Fatalf("LoadWorld: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot changed the World:\n got %+v\nwant %+v", got, want)
	}
	digest, err := got.Digest("")
	if err != nil {
		t.Fatalf("digest snapshot: %v", err)
	}
	if digest != s.Digest {
		t.Errorf("snapshot digest = %s, want the started digest %s", digest, s.Digest)
	}
}

// TestBeginUnchangedKeepsSnapshot proves an unchanged start writes nothing: the
// run it would have created does not exist, so no second world.json appears and
// the first one is byte for byte where it was.
func TestBeginUnchangedKeepsSnapshot(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	fetch := func(context.Context, string) (world.World, error) { return testWorld(testSubject), nil }
	meta := Meta{Kind: testKind, Subject: testSubject}
	runs := filepath.Join(base, "agent-runner", "runs")

	first, err := Begin(ctx, base, meta, fetch, "")
	if err != nil {
		t.Fatalf("first Begin: %v", err)
	}
	if err := first.Remember(0); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	snapshot := filepath.Join(first.Run.Dir, worldName)
	before, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	second, err := Begin(ctx, base, meta, fetch, "")
	if err != nil {
		t.Fatalf("second Begin: %v", err)
	}
	if !second.Unchanged || second.Run != nil {
		t.Fatalf("second Begin = unchanged %v with run %v, want unchanged and no run", second.Unchanged, second.Run)
	}
	after, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("read snapshot after the unchanged start: %v", err)
	}
	if string(after) != string(before) {
		t.Error("the unchanged start rewrote world.json")
	}
	entries, err := os.ReadDir(runs)
	if err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("runs holds %d entries after the unchanged start, want 1", len(entries))
	}
}

// TestBeginFailsWithoutSnapshot proves a run whose World cannot be kept is not
// started: the error comes back and the directory it just made is gone, so no
// record claims a decision nobody can inspect.
func TestBeginFailsWithoutSnapshot(t *testing.T) {
	base := t.TempDir()
	fetch := func(context.Context, string) (world.World, error) { return world.World{}, nil }
	s, err := Begin(context.Background(), base, Meta{Kind: testKind, Subject: testSubject}, fetch, "")
	if err == nil {
		t.Fatalf("Begin accepted a World it could not snapshot; got start %+v", s)
	}
	if s.Run != nil {
		t.Errorf("failed Begin returned run %s", s.Run.Dir)
	}
	entries, err := os.ReadDir(filepath.Join(base, "agent-runner", "runs"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Fatalf("read runs: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("runs holds %d entries after a failed start, want 0", len(entries))
	}
}
