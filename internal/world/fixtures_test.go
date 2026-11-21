package world

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The three Worlds in testdata/ are PRD §16's M0 proof, each of which must
// plan nothing: a pull request whose newest event is our own comment, a pull
// request we closed, and a pull request whose review we already answered.
//
// §16 asks for fixtures taken from real failures. These are authored from its
// descriptions instead — the loop they come from is not written yet, so no run
// produced them, and the only §17 failure they overlap is its first, a loop
// answering its own comment. When the loop lands, this file is its regression
// test.
//
// Each is stored in the wire format run/world.json uses and loaded offline with
// unknown fields refused: no token, no network, no GitHub.
//
// "Exit 0" is modelled by exitFor below: it restates PRD §13 rather than
// testing the loop that will own the code.
//
// Each also carries a control: undo the thing that silenced the loop and the
// same World must plan something. Without the control a fixture can pass by
// being empty, which proves nothing about the guard it names.

// fixture is one failure case: the snapshot, why it must be quiet, and the
// smallest edit that puts the failure back.
type fixture struct {
	file string
	why  string
	undo func(World) World // restores the failure; the result must plan work
}

var fixtures = []fixture{
	{
		file: "own-comment-newest.json",
		why:  "our own comment is the newest and no human has replied since",
		undo: func(w World) World { w.Comments = dropNewestOwnComment(w.Comments); return w },
	},
	{
		file: "closed-by-us.json",
		why:  "we closed the pull request, so there is no branch of ours to change",
		undo: func(w World) World { w.PR.State = "open"; return w },
	},
	{
		file: "review-already-answered.json",
		why:  "the changes-requested verdict was answered and later withdrawn by the reviewer's own approval",
		undo: func(w World) World { w.Reviews = dropReviewState(w.Reviews, "APPROVED"); return w },
	},
}

func TestFixturesPlanNothing(t *testing.T) {
	for _, f := range fixtures {
		t.Run(strings.TrimSuffix(f.file, ".json"), func(t *testing.T) {
			w := loadFixture(t, f.file)

			plan := w.Actions(ourLogin)
			if got := exitFor(plan); got != 0 {
				t.Fatalf("%s: %s, but plans %s and would exit %d; want no actions and exit 0",
					f.file, f.why, describeActions(plan), got)
			}

			// The unchanged fast path needs a digest to compare, and the run's
			// own events must not be part of it: one more comment of ours is
			// not news (PRD §17, first failure).
			before, err := w.Digest(ourLogin)
			if err != nil {
				t.Fatalf("digest %s: %v", f.file, err)
			}
			if before == "" {
				t.Fatalf("digest %s: empty", f.file)
			}
			own := w
			own.Comments = append(append([]Comment(nil), w.Comments...), Comment{
				ID:        nextCommentID(w.Comments),
				Author:    ourLogin,
				CreatedAt: "2026-11-21T06:00:00Z",
			})
			after, err := own.Digest(ourLogin)
			if err != nil {
				t.Fatalf("digest %s with an own comment: %v", f.file, err)
			}
			if after != before {
				t.Errorf("%s: digest moved when one of our own comments was added (%s -> %s); own events are not news",
					f.file, before, after)
			}
			human := w
			human.Comments = append(append([]Comment(nil), w.Comments...), Comment{
				ID:        nextCommentID(w.Comments),
				Author:    "reviewer-sal",
				CreatedAt: "2026-11-21T06:00:00Z",
			})
			after, err = human.Digest(ourLogin)
			if err != nil {
				t.Fatalf("digest %s with a human comment: %v", f.file, err)
			}
			if after == before {
				t.Errorf("%s: digest did not move when a human comment arrived; the fast path would skip real work", f.file)
			}

			if undo := f.undo(w); len(undo.Actions(ourLogin)) == 0 {
				t.Fatalf("%s: control planned nothing after undoing the failure (%s); the fixture does not exercise the guard it names",
					f.file, f.why)
			}
		})
	}
}

// loadFixture reads testdata/<file> as a World, refusing a key the wire format
// does not have. A fixture that misspells a field would otherwise load as the
// zero value and pass by being empty, which is the failure this file exists to
// catch. It also refuses the two shapes that are not a thread at all, the same
// two run.LoadWorld refuses.
func loadFixture(t *testing.T, file string) World {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var w World
	if err := dec.Decode(&w); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	if dec.More() {
		t.Fatalf("decode %s: more than one JSON value", file)
	}
	if w.Subject == "" || w.Issue.Number <= 0 {
		t.Fatalf("%s: not a thread (subject %q, issue %d)", file, w.Subject, w.Issue.Number)
	}
	return w
}

// exitFor is the exit code PRD §13 gives a plan: 0 when nothing is left
// actionable, non-zero when the run still owes the thread work. The loop that
// returns it does not exist yet; when it does, these fixtures are its test.
func exitFor(plan []Action) int {
	if len(plan) == 0 {
		return 0
	}
	return 1
}

// describeActions names a plan the way a failure message needs it: the kind and
// trigger id of each action, in the order the classifier sorted them.
func describeActions(plan []Action) string {
	if len(plan) == 0 {
		return "no actions"
	}
	ids := make([]string, 0, len(plan))
	for _, a := range plan {
		ids = append(ids, a.Kind+" "+a.TriggerID)
	}
	return strings.Join(ids, ", ")
}

// dropNewestOwnComment removes the newest comment authored by ourLogin: the
// event the loop was answering when it talked to itself.
func dropNewestOwnComment(cs []Comment) []Comment {
	var newest int64 = -1
	for _, c := range cs {
		if isOwnEvent(c.Author, ourLogin) && c.ID > newest {
			newest = c.ID
		}
	}
	out := make([]Comment, 0, len(cs))
	for _, c := range cs {
		if c.ID == newest {
			continue
		}
		out = append(out, c)
	}
	return out
}

// dropReviewState removes every review in state, the verdict whose absence puts
// the failure back.
func dropReviewState(rs []Review, state string) []Review {
	out := make([]Review, 0, len(rs))
	for _, r := range rs {
		if strings.EqualFold(r.State, state) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// nextCommentID returns an id above every comment in cs, so a test can add an
// event that reads as newer than the snapshot without editing the file.
func nextCommentID(cs []Comment) int64 {
	var max int64
	for _, c := range cs {
		if c.ID > max {
			max = c.ID
		}
	}
	return max + 1
}
