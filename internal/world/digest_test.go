package world

import (
	"testing"
)

// sampleWorld is a thread with something in every field the digest covers: a
// pull request, two reviews (one of them ours), two same-named checks with
// different conclusions, one comment of ours and one from a human, and an issue
// with two labels and one assignee.
func sampleWorld() World {
	return World{
		Subject: "https://github.com/marcus-beauregard/agent-runner/pull/38",
		Issue: Issue{
			Number:    30,
			Title:     "Record: world digest, own events excluded",
			State:     "open",
			Assignees: []string{"marcus-beauregard"},
			Labels:    []string{"enhancement", "record"},
		},
		PR: PR{
			Number:         38,
			HeadSHA:        "724fd0972618a5aa17e5b72522918c726cd4754c",
			BaseSHA:        "fe9863dfada6339db67b902ad83cc8d673b13070",
			Draft:          false,
			Mergeable:      MergeableYes,
			MergeableState: "clean",
			State:          "open",
		},
		Reviews: []Review{
			{ID: 1001, State: "COMMENTED", Author: "marcus-beauregard"},
			{ID: 1002, State: "APPROVED", Author: "bfs2021"},
		},
		Checks: []Check{
			{ID: 7, Name: "build", Conclusion: "success", Status: "completed"},
			{ID: 5, Name: "build", Conclusion: "failure", Status: "completed"},
			{ID: 6, Name: "vet", Conclusion: "success", Status: "completed"},
		},
		Comments: []Comment{
			{ID: 2001, Author: "marcus-beauregard", CreatedAt: "2026-11-20T22:00:00Z"},
			{ID: 2002, Author: "bfs2021", CreatedAt: "2026-11-20T22:30:00Z"},
		},
	}
}

const ourLogin = "marcus-beauregard"

func digest(t *testing.T, w World, exclude string) string {
	t.Helper()
	got, err := w.Digest(exclude)
	if err != nil {
		t.Fatalf("Digest(%q): %v", exclude, err)
	}
	if len(got) != 64 {
		t.Fatalf("Digest(%q) = %q, want 64 hex characters", exclude, got)
	}
	return got
}

// TestDigestIsGolden pins the bytes. The digest is written to a ledger and
// compared against the next run's, so a change here is a migration, not a
// refactor: old digests would stop matching new ones and every live thread
// would read as changed once.
func TestDigestIsGolden(t *testing.T) {
	const want = "77b0e63419437e253cad1fcf68e8bceeb8186395e3194e9ec9deb37be2145402"
	if got := digest(t, sampleWorld(), ourLogin); got != want {
		t.Fatalf("Digest() = %q, want %q", got, want)
	}
}

func reversed[T any](xs []T) []T {
	out := make([]T, len(xs))
	for i, x := range xs {
		out[len(xs)-1-i] = x
	}
	return out
}

// TestDigestIgnoresOrder feeds the same thread twice with every slice reversed.
// Nothing about the thread changed, so nothing about the digest may. Assignees
// and labels are included on purpose: the reader hands them over in API order,
// so the digest is the only place that order can be flattened.
func TestDigestIgnoresOrder(t *testing.T) {
	w := sampleWorld()
	w.Reviews = reversed(w.Reviews)
	w.Checks = reversed(w.Checks)
	w.Comments = reversed(w.Comments)
	w.Issue.Assignees = reversed(w.Issue.Assignees)
	w.Issue.Labels = reversed(w.Issue.Labels)

	if got, want := digest(t, w, ourLogin), digest(t, sampleWorld(), ourLogin); got != want {
		t.Fatalf("same thread, different answer order: %q != %q", got, want)
	}
}

// TestDigestDropsOwnEvents is the guard from PRD §8: our own comments and
// reviews are not state changes, or the worker reads its own output as news and
// answers itself forever. The strongest form of that claim is that the events
// are invisible, so a World carrying them and a World with them removed must
// hash alike.
//
// The ticket's checklist said "removing our own comment changes it", which
// contradicts its own exclusion rule; the rule and the PRD win, so this test
// proves the no-op and the PR says so.
func TestDigestDropsOwnEvents(t *testing.T) {
	withOwn := sampleWorld()
	withoutOwn := sampleWorld()
	withoutOwn.Reviews = []Review{{ID: 1002, State: "APPROVED", Author: "bfs2021"}}
	withoutOwn.Comments = []Comment{{ID: 2002, Author: "bfs2021", CreatedAt: "2026-11-20T22:30:00Z"}}

	if got, want := digest(t, withOwn, ourLogin), digest(t, withoutOwn, ourLogin); got != want {
		t.Fatalf("own review and comment are invisible: %q != %q", got, want)
	}

	// And with nothing excluded, the same removal does change the digest —
	// otherwise the test above would pass for the wrong reason.
	if got, want := digest(t, withOwn, ""), digest(t, withoutOwn, ""); got == want {
		t.Fatalf("without an excludeAuthor, removing two events must change the digest, both were %q", got)
	}
}

// TestDigestCountsForeignComment is the other half: a human replying on the
// thread is news, and the digest has to move.
func TestDigestCountsForeignComment(t *testing.T) {
	w := sampleWorld()
	before := digest(t, w, ourLogin)
	w.Comments = append(w.Comments, Comment{ID: 2003, Author: "bfs2021", CreatedAt: "2026-11-20T23:00:00Z"})
	if after := digest(t, w, ourLogin); after == before {
		t.Fatalf("a new human comment left the digest at %q", before)
	}
}

// TestDigestIgnoresTimestamps covers "a changing comment body does not move
// it". A body never reaches the World — Comment has no field for one — so the
// nearest representable churn is the timestamp, and it must not move either.
// A digest that moved on every edit would keep the worker awake for nothing.
func TestDigestIgnoresTimestamps(t *testing.T) {
	w := sampleWorld()
	w.Comments[1].CreatedAt = "2026-11-21T09:15:00Z"
	if got, want := digest(t, w, ourLogin), digest(t, sampleWorld(), ourLogin); got != want {
		t.Fatalf("comment timestamps are not part of the world: %q != %q", got, want)
	}
}

// TestDigestExclusionIgnoresCase matches the way GitHub treats logins: the
// same account answered with different capitalisation is still us, and a run
// that answered itself over a capital letter would loop.
func TestDigestExclusionIgnoresCase(t *testing.T) {
	if got, want := digest(t, sampleWorld(), "Marcus-Beauregard"), digest(t, sampleWorld(), ourLogin); got != want {
		t.Fatalf("exclusion is case-insensitive: %q != %q", got, want)
	}
}

// TestDigestNilAndEmptyListsAgree guards the encoding: a nil slice marshals as
// null and an empty one as [], and a thread that has no reviews is one thread
// whichever way the reader happened to build it.
func TestDigestNilAndEmptyListsAgree(t *testing.T) {
	var empty World
	empty.Issue.Number = 7

	filled := empty
	filled.Reviews = []Review{}
	filled.Checks = []Check{}
	filled.Comments = []Comment{}
	filled.Issue.Assignees = []string{}
	filled.Issue.Labels = []string{}

	if got, want := digest(t, empty, ourLogin), digest(t, filled, ourLogin); got != want {
		t.Fatalf("nil and empty lists are one state: %q != %q", got, want)
	}
}

// TestDigestTracksSemanticFields is the table that names what the digest is
// for: every field a decision reads moves it, and only those.
func TestDigestTracksSemanticFields(t *testing.T) {
	base := digest(t, sampleWorld(), ourLogin)

	cases := []struct {
		name   string
		change func(*World)
		moves  bool
	}{
		{name: "new head sha", change: func(w *World) { w.PR.HeadSHA = "abc123" }, moves: true},
		{name: "new base sha", change: func(w *World) { w.PR.BaseSHA = "def456" }, moves: true},
		{name: "became a draft", change: func(w *World) { w.PR.Draft = true }, moves: true},
		{name: "mergeable known", change: func(w *World) { w.PR.Mergeable = MergeableUnknown }, moves: true},
		{name: "mergeable state", change: func(w *World) { w.PR.MergeableState = "blocked" }, moves: true},
		{name: "pr state", change: func(w *World) { w.PR.State = "closed" }, moves: true},
		{name: "issue state", change: func(w *World) { w.Issue.State = "closed" }, moves: true},
		{name: "issue number", change: func(w *World) { w.Issue.Number = 31 }, moves: true},
		{name: "label added", change: func(w *World) { w.Issue.Labels = append(w.Issue.Labels, "bug") }, moves: true},
		{name: "assignee added", change: func(w *World) { w.Issue.Assignees = append(w.Issue.Assignees, "bfs2021") }, moves: true},
		{name: "check conclusion", change: func(w *World) { w.Checks[0].Conclusion = "failure" }, moves: true},
		{name: "check status", change: func(w *World) { w.Checks[2].Status = "in_progress" }, moves: true},
		{name: "review state", change: func(w *World) { w.Reviews[1].State = "CHANGES_REQUESTED" }, moves: true},
		{name: "review id", change: func(w *World) { w.Reviews[1].ID = 1003 }, moves: true},
		{name: "comment author", change: func(w *World) { w.Comments[1].Author = "someone-else" }, moves: true},
		{name: "comment id", change: func(w *World) { w.Comments[1].ID = 2004 }, moves: true},
		// Not semantic, or excluded.
		{name: "subject url", change: func(w *World) { w.Subject = "https://example.test" }, moves: false},
		{name: "issue title", change: func(w *World) { w.Issue.Title = "retitled" }, moves: false},
		{name: "our own review state", change: func(w *World) { w.Reviews[0].State = "APPROVED" }, moves: false},
		{name: "our own comment id", change: func(w *World) { w.Comments[0].ID = 2010 }, moves: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := sampleWorld()
			tc.change(&w)
			got := digest(t, w, ourLogin)
			if moved := got != base; moved != tc.moves {
				t.Fatalf("digest moved = %v, want %v (%q vs %q)", moved, tc.moves, got, base)
			}
		})
	}
}
