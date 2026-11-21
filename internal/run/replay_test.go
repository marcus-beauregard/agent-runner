package run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcus-beauregard/agent-runner/internal/world"
)

// openWorld is a thread with one question on it: our own comment at id 90, and
// somebody else's at 100. The classifier owes exactly comment:100 a reply — the
// own-comment rule is what makes the account, not the world, decide that.
func openWorld() world.World {
	return world.World{
		Subject: "https://github.com/o/r/pull/8",
		Issue:   world.Issue{Number: 7, State: "OPEN"},
		PR: world.PR{
			Number: 8, HeadSHA: "head", BaseSHA: "base", State: "open",
			Mergeable: world.MergeableNo,
		},
		Comments: []world.Comment{
			{ID: 90, Author: "marcus-beauregard"},
			{ID: 100, Author: "someone"},
		},
	}
}

// openWorldWithACheck owes two actions: the comment to answer and the failing
// check to fix. Two is the smallest plan that can be partly spent, which is what
// separates a run that stopped short from a record that is wrong.
func openWorldWithACheck() world.World {
	w := openWorld()
	w.Checks = []world.Check{{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"}}
	return w
}

// newFixtureRun creates the record replay reads: a run directory, a World
// snapshot, and the account the run acted as. The record is left running, as New
// writes it; callers finish it with the outcome their case is about.
func newFixtureRun(t *testing.T, snapshot world.World) *Run {
	t.Helper()
	r, err := New(t.TempDir(), Meta{Kind: "resolve", Subject: snapshot.Subject, Account: "marcus-beauregard"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := SaveWorld(r.Dir, snapshot); err != nil {
		t.Fatalf("save world: %v", err)
	}
	return r
}

// fixtureRun is newFixtureRun finished done, the state in which a run owes its
// record the whole plan.
func fixtureRun(t *testing.T, snapshot world.World) *Run {
	t.Helper()
	r := newFixtureRun(t, snapshot)
	if err := r.Finish(0); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	return r
}

// spend records one trigger row through the ledger, the way a command does.
func spend(t *testing.T, r *Run, triggerID, action string) {
	t.Helper()
	l, err := OpenLedger(filepath.Join(r.Dir, "run.ndjson"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	if err := l.Spend(triggerID, action); err != nil {
		l.Close()
		t.Fatalf("spend %s: %v", triggerID, err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close ledger: %v", err)
	}
}

func TestReplay(t *testing.T) {
	cases := []struct {
		name     string
		snapshot world.World
		spends   [][2]string
		finish   int // exit code Finish records; 0 is done
		wantOK   bool
		wantOut  []string
	}{
		{
			name:     "a run that recorded what it owed replays",
			snapshot: openWorld(),
			spends:   [][2]string{{"comment:100", "answered #100"}},
			wantOK:   true,
			wantOut:  []string{"replay: ok", "recomputed (1):", "  reply comment:100", "missing (0):", "extra (0):"},
		},
		{
			name:     "an action the ledger never recorded is missing",
			snapshot: openWorld(),
			wantOK:   false,
			wantOut:  []string{"replay: differs", "missing (1):", "  comment:100", "recorded (0):"},
		},
		{
			name:     "a spend the world does not ask for is extra",
			snapshot: openWorld(),
			spends:   [][2]string{{"comment:100", "answered #100"}, {"comment:55", "answered #55"}},
			wantOK:   false,
			wantOut:  []string{"replay: differs", "extra (1):", "  comment:55", "missing (0):"},
		},
		{
			// The plan had two actions and the run did one of them. On a done
			// record that is a mismatch: the record claims an outcome it did not
			// reach.
			name:     "a finished run that left a planned action unspent differs",
			snapshot: openWorldWithACheck(),
			spends:   [][2]string{{"comment:100", "answered #100"}},
			wantOK:   false,
			wantOut:  []string{"replay: differs", "missing (1):", "  check:build:failure", "recorded (1):"},
		},
		{
			// The same partial spend, on a run that stopped for a person. It did
			// what it could and said so; there is nothing for replay to catch.
			name:     "a run that stopped for a person is not a mismatch",
			snapshot: openWorldWithACheck(),
			spends:   [][2]string{{"comment:100", "answered #100"}},
			finish:   4,
			wantOK:   true,
			wantOut:  []string{"replay: ok (state needs_human, 1 planned action(s) unspent)", "missing (1):"},
		},
		{
			// Stopping early excuses what was not done, never what was invented:
			// a spend the World never asked for is a broken record whatever the
			// run's state.
			name:     "a stopped run still cannot spend what its world never asked for",
			snapshot: openWorldWithACheck(),
			spends:   [][2]string{{"comment:100", "answered #100"}, {"comment:55", "answered #55"}},
			finish:   4,
			wantOK:   false,
			wantOut:  []string{"replay: differs", "extra (1):", "  comment:55"},
		},
		{
			name:     "a world that owes nothing replays to an empty set",
			snapshot: world.World{Subject: "https://github.com/o/r/issues/7", Issue: world.Issue{Number: 7, State: "OPEN"}},
			wantOK:   true,
			wantOut:  []string{"replay: ok", "recomputed (0):", "  (none)", "recorded (0):"},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := newFixtureRun(t, tt.snapshot)
			for _, s := range tt.spends {
				spend(t, r, s[0], s[1])
			}
			if err := r.Finish(tt.finish); err != nil {
				t.Fatalf("finish run: %v", err)
			}
			var buf bytes.Buffer
			ok, err := Replay(r.Dir, &buf)
			if err != nil {
				t.Fatalf("Replay: %v", err)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v; output:\n%s", ok, tt.wantOK, buf.String())
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output does not contain %q; output:\n%s", want, buf.String())
				}
			}
		})
	}
}

// TestReplayRefusesAnUnfinishedRun pins the difference between a partial ledger
// and a wrong one: a live run spent what it reached, so replay has no verdict to
// give and must not print one.
func TestReplayRefusesAnUnfinishedRun(t *testing.T) {
	r := newFixtureRun(t, openWorld())
	spend(t, r, "comment:100", "answered #100")
	var buf bytes.Buffer
	if _, err := Replay(r.Dir, &buf); !errors.Is(err, ErrUnfinished) {
		t.Fatalf("Replay = %v, want ErrUnfinished", err)
	}
	if strings.Contains(buf.String(), "replay: ok") {
		t.Errorf("replay gave a verdict on a running record:\n%s", buf.String())
	}
}

// fileState is what "writes nothing" has to preserve: the file, its size, its
// mtime and its bytes.
type fileState struct {
	size    int64
	modTime time.Time
	sum     string
}

func snapshotDir(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	out := make(map[string]fileState)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[path] = fileState{size: fi.Size(), modTime: fi.ModTime(), sum: hex.EncodeToString(sum[:])}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func TestReplayWritesNothing(t *testing.T) {
	r := fixtureRun(t, openWorld())
	spend(t, r, "comment:100", "answered #100")
	before := snapshotDir(t, r.Dir)
	if _, err := Replay(r.Dir, io.Discard); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	after := snapshotDir(t, r.Dir)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("replay changed the run directory\nbefore: %v\nafter:  %v", before, after)
	}
}

func TestReplayFailsOnATornLedger(t *testing.T) {
	r := fixtureRun(t, openWorld())
	spend(t, r, "comment:100", "answered #100")
	f, err := os.OpenFile(filepath.Join(r.Dir, "run.ndjson"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	if _, err := f.WriteString("{\"seq\":9,\"ts\":\"2026-01-01T00:00:00Z\",\"kind\":\"trigger\""); err != nil {
		t.Fatalf("write torn line: %v", err)
	}
	f.Close()

	if _, err := Replay(r.Dir, io.Discard); err == nil || !strings.Contains(err.Error(), "torn final line") {
		t.Fatalf("Replay = %v, want a torn-ledger error", err)
	}
}

func TestReplayFailsOnAnUnreadableWorld(t *testing.T) {
	r := fixtureRun(t, openWorld())
	if err := os.WriteFile(filepath.Join(r.Dir, worldName), []byte("{"), 0o600); err != nil {
		t.Fatalf("damage world.json: %v", err)
	}
	if _, err := Replay(r.Dir, io.Discard); !errors.Is(err, ErrNoWorld) {
		t.Fatalf("Replay = %v, want ErrNoWorld", err)
	}
}

func TestReplayRefusesARecordWithNoAccount(t *testing.T) {
	r, err := New(t.TempDir(), Meta{Kind: "resolve", Subject: openWorld().Subject})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := SaveWorld(r.Dir, openWorld()); err != nil {
		t.Fatalf("save world: %v", err)
	}
	if _, err := Replay(r.Dir, io.Discard); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("Replay = %v, want ErrNoAccount", err)
	}
}

func TestReplayRefusesADoubleSpend(t *testing.T) {
	r := fixtureRun(t, openWorld())
	for seq, action := range []string{"first", "second"} {
		raw, err := json.Marshal(Trigger{TriggerID: "comment:100", Action: action, At: time.Now().UTC()})
		if err != nil {
			t.Fatalf("encode trigger: %v", err)
		}
		line, err := json.Marshal(Event{Seq: int64(seq + 1), TS: time.Now().UTC(), Kind: kindTrigger, Data: raw})
		if err != nil {
			t.Fatalf("encode event: %v", err)
		}
		f, err := os.OpenFile(filepath.Join(r.Dir, "run.ndjson"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatalf("open ledger: %v", err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatalf("write event: %v", err)
		}
		f.Close()
	}
	if _, err := Replay(r.Dir, io.Discard); err == nil || !strings.Contains(err.Error(), "spent twice") {
		t.Fatalf("Replay = %v, want a double-spend error", err)
	}
}

// TestBeginRecordsTheAccount is the plumbing replay depends on: the account a
// run excluded has to survive in its record, or replay recomputes with nothing
// excluded and calls a correct record wrong.
func TestBeginRecordsTheAccount(t *testing.T) {
	w := openWorld()
	fetch := func(context.Context, string) (world.World, error) { return w, nil }
	start, err := Begin(context.Background(), t.TempDir(), Meta{Kind: "resolve", Subject: w.Subject}, fetch, "marcus-beauregard")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if start.Run == nil {
		t.Fatal("Begin created no run")
	}
	b, err := os.ReadFile(filepath.Join(start.Run.Dir, "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	if !strings.Contains(string(b), "\"account\": \"marcus-beauregard\"") {
		t.Errorf("run.json does not record the account:\n%s", b)
	}
	spend(t, start.Run, "comment:100", "answered #100")
	if err := start.Run.Finish(0); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	if ok, err := Replay(start.Run.Dir, io.Discard); err != nil || !ok {
		t.Fatalf("Replay = %v, %v; want a matching record", ok, err)
	}
}
