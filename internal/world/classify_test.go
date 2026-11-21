package world

import (
	"slices"
	"testing"
)

// openPR is a world with an open pull request and nothing on it: the base each
// case below varies one field of. Mergeable is "no" so that a review alone never
// settles the thread, and a case that wants an approval to end the run says so.
func openPR() World {
	return World{
		Subject: "https://github.com/marcus-beauregard/agent-runner/pull/46",
		Issue:   Issue{Number: 44, State: "open"},
		PR: PR{
			Number:    46,
			HeadSHA:   "abc123",
			BaseSHA:   "def456",
			Mergeable: MergeableNo,
			State:     "open",
		},
	}
}

func TestActions(t *testing.T) {
	tests := []struct {
		name  string
		self  string
		world func() World
		want  []Action
	}{
		{
			name:  "no pull request owes nothing",
			self:  ourLogin,
			world: func() World { return World{Issue: Issue{Number: 44, State: "open"}} },
		},
		{
			name:  "a closed pull request owes nothing",
			self:  ourLogin,
			world: func() World { w := openPR(); w.PR.State = "closed"; return w },
		},
		{
			name:  "a draft pull request owes nothing",
			self:  ourLogin,
			world: func() World { w := openPR(); w.PR.Draft = true; return w },
		},
		{
			name:  "an unanswered comment is a reply",
			self:  ourLogin,
			world: func() World { w := openPR(); w.Comments = []Comment{{ID: 2002, Author: "bfs2021"}}; return w },
			want:  []Action{{TriggerID: "comment:2002", Kind: kindReply}},
		},
		{
			name: "our own newest comment owes nothing",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.Comments = []Comment{{ID: 2001, Author: "bfs2021"}, {ID: 2002, Author: ourLogin}}
				return w
			},
		},
		{
			name:  "only our own comment owes nothing",
			self:  ourLogin,
			world: func() World { w := openPR(); w.Comments = []Comment{{ID: 2001, Author: ourLogin}}; return w },
		},
		{
			name: "an empty self excludes nothing",
			self: "",
			world: func() World {
				w := openPR()
				w.Comments = []Comment{{ID: 2001, Author: ourLogin}, {ID: 2002, Author: "bfs2021"}}
				return w
			},
			want: []Action{
				{TriggerID: "comment:2001", Kind: kindReply},
				{TriggerID: "comment:2002", Kind: kindReply},
			},
		},
		{
			name: "a login compares case-insensitively",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.Comments = []Comment{{ID: 2001, Author: "Marcus-Beauregard"}}
				w.Reviews = []Review{{ID: 1001, State: "CHANGES_REQUESTED", Author: "MARCUS-BEAUREGARD"}}
				return w
			},
		},
		{
			name: "changes requested is a fix",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.Reviews = []Review{{ID: 1002, State: "CHANGES_REQUESTED", Author: "bfs2021"}}
				return w
			},
			want: []Action{{TriggerID: "review:1002:changes_requested", Kind: kindFix}},
		},
		{
			name: "changes requested then approved by the same author owes nothing",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.Reviews = []Review{
					{ID: 1002, State: "CHANGES_REQUESTED", Author: "bfs2021"},
					{ID: 1005, State: "APPROVED", Author: "bfs2021"},
				}
				return w
			},
		},
		{
			name: "our own changes requested owes nothing",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.Reviews = []Review{{ID: 1002, State: "CHANGES_REQUESTED", Author: ourLogin}}
				return w
			},
		},
		{
			name: "a failed check is a fix",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.Checks = []Check{{ID: 5, Name: "build", Status: "completed", Conclusion: "failure"}}
				return w
			},
			want: []Action{{TriggerID: "check:build:failure", Kind: kindFix}},
		},
		{
			name: "a failure re-run green owes nothing",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.Checks = []Check{
					{ID: 5, Name: "build", Status: "completed", Conclusion: "failure"},
					{ID: 9, Name: "build", Status: "completed", Conclusion: "success"},
				}
				return w
			},
		},
		{
			name: "same-named failures collapse to one action",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.Checks = []Check{
					{ID: 5, Name: "build", Status: "completed", Conclusion: "failure"},
					{ID: 7, Name: "build", Status: "completed", Conclusion: "failure"},
				}
				return w
			},
			want: []Action{{TriggerID: "check:build:failure", Kind: kindFix}},
		},
		{
			name: "a check still running is not a failure",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.Checks = []Check{{ID: 5, Name: "build", Status: "in_progress"}}
				return w
			},
		},
		{
			name: "an approved, green, mergeable pull request owes nothing",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.PR.Mergeable = MergeableYes
				w.Reviews = []Review{{ID: 1002, State: "APPROVED", Author: "bfs2021"}}
				w.Checks = []Check{
					{ID: 5, Name: "build", Status: "completed", Conclusion: "success"},
					{ID: 6, Name: "vet", Status: "completed", Conclusion: "success"},
				}
				return w
			},
		},
		{
			name: "approval ends the run even with a later comment",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.PR.Mergeable = MergeableYes
				w.Reviews = []Review{{ID: 1002, State: "APPROVED", Author: "bfs2021"}}
				w.Checks = []Check{{ID: 5, Name: "build", Status: "completed", Conclusion: "success"}}
				w.Comments = []Comment{{ID: 2003, Author: "bfs2021"}}
				return w
			},
		},
		{
			name: "our own approval does not end the run",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.PR.Mergeable = MergeableYes
				w.Reviews = []Review{{ID: 1002, State: "APPROVED", Author: ourLogin}}
				w.Checks = []Check{{ID: 5, Name: "build", Status: "completed", Conclusion: "success"}}
				return w
			},
		},
		{
			name: "an approval with a red check is still a fix",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.PR.Mergeable = MergeableYes
				w.Reviews = []Review{{ID: 1002, State: "APPROVED", Author: "bfs2021"}}
				w.Checks = []Check{{ID: 5, Name: "build", Status: "completed", Conclusion: "failure"}}
				return w
			},
			want: []Action{{TriggerID: "check:build:failure", Kind: kindFix}},
		},
		{
			name: "several kinds at once come back sorted",
			self: ourLogin,
			world: func() World {
				w := openPR()
				w.Reviews = []Review{{ID: 1002, State: "CHANGES_REQUESTED", Author: "bfs2021"}}
				w.Comments = []Comment{{ID: 2002, Author: "bfs2021"}}
				w.Checks = []Check{
					{ID: 6, Name: "vet", Status: "completed", Conclusion: "failure"},
					{ID: 5, Name: "build", Status: "completed", Conclusion: "failure"},
				}
				return w
			},
			want: []Action{
				{TriggerID: "check:build:failure", Kind: kindFix},
				{TriggerID: "check:vet:failure", Kind: kindFix},
				{TriggerID: "comment:2002", Kind: kindReply},
				{TriggerID: "review:1002:changes_requested", Kind: kindFix},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.world().Actions(tt.self)
			if len(got) != len(tt.want) {
				t.Fatalf("Actions = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Actions[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
			// Strictly increasing ids are the whole contract of the order: sorted
			// for comparison, and no id twice for the ledger to spend twice.
			for i := 1; i < len(got); i++ {
				if got[i-1].TriggerID >= got[i].TriggerID {
					t.Errorf("Actions not strictly ordered: %q then %q", got[i-1].TriggerID, got[i].TriggerID)
				}
			}
		})
	}
}

// TestActionsTreatNilAndEmptySlicesAlike pins that an absent list and an empty
// one classify the same: a World read from JSON may carry either, and the action
// set must not depend on which.
func TestActionsTreatNilAndEmptySlicesAlike(t *testing.T) {
	nilWorld := openPR()
	emptyWorld := openPR()
	emptyWorld.Reviews = []Review{}
	emptyWorld.Checks = []Check{}
	emptyWorld.Comments = []Comment{}

	if got := nilWorld.Actions(ourLogin); len(got) != 0 {
		t.Fatalf("Actions on an empty world = %v, want none", got)
	}
	if !slices.Equal(nilWorld.Actions(ourLogin), emptyWorld.Actions(ourLogin)) {
		t.Errorf("nil and empty lists classified differently: %v vs %v",
			nilWorld.Actions(ourLogin), emptyWorld.Actions(ourLogin))
	}
}
