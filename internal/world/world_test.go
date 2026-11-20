package world

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestSortWorldKeepsDuplicateCheckOrder holds the one sort that cannot rely on
// its key being unique: a commit that ran the same check twice has two entries
// with one name, and they have to keep the order the API answered in.
func TestSortWorldKeepsDuplicateCheckOrder(t *testing.T) {
	// Enough entries that the sort is not insertion sort, and two names each
	// repeated, so an unstable sort has somewhere to reorder them.
	var w World
	var want []string
	for i := 0; i < 24; i++ {
		name := []string{"build", "vet"}[i%2]
		w.Checks = append(w.Checks, Check{Name: name, Conclusion: fmt.Sprintf("run-%02d", i)})
	}
	for _, name := range []string{"build", "vet"} {
		for i := 0; i < 24; i++ {
			if []string{"build", "vet"}[i%2] == name {
				want = append(want, name+" run-"+fmt.Sprintf("%02d", i))
			}
		}
	}
	sortWorld(&w)
	var got []string
	for _, c := range w.Checks {
		got = append(got, c.Name+" "+c.Conclusion)
	}
	if !slices.Equal(got, want) {
		t.Errorf("Checks = %v, want %v: duplicate names must keep the API's order", got, want)
	}
}

func TestMergeableString(t *testing.T) {
	cases := []struct {
		m    Mergeable
		want string
	}{
		{m: MergeableYes, want: "yes"},
		{m: MergeableNo, want: "no"},
		{m: MergeableUnknown, want: "unknown"},
	}
	for _, tc := range cases {
		if got := tc.m.String(); got != tc.want {
			t.Errorf("Mergeable(%d).String() = %q, want %q", int(tc.m), got, tc.want)
		}
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		name    string
		ref     string
		owner   string
		repo    string
		number  int
		wantErr string // a substring of the error; empty means the ref must parse
	}{
		{name: "pull request", ref: "https://github.com/octo/agent-runner/pull/7", owner: "octo", repo: "agent-runner", number: 7},
		{name: "issue", ref: "https://github.com/octo/agent-runner/issues/29", owner: "octo", repo: "agent-runner", number: 29},
		{name: "trailing slash", ref: "https://github.com/octo/agent-runner/pull/7/", owner: "octo", repo: "agent-runner", number: 7},
		{name: "query and fragment", ref: "https://github.com/octo/agent-runner/issues/29?notification_referrer_id=1#issuecomment-2", owner: "octo", repo: "agent-runner", number: 29},
		{name: "www host", ref: "https://www.github.com/octo/agent-runner/issues/1", owner: "octo", repo: "agent-runner", number: 1},
		{name: "not a URL", ref: "nonsense", wantErr: "not an https URL"},
		{name: "plain http", ref: "http://github.com/octo/agent-runner/issues/1", wantErr: "not an https URL"},
		{name: "other host", ref: "https://gitlab.com/octo/agent-runner/issues/1", wantErr: "not github.com"},
		{name: "not a number", ref: "https://github.com/octo/agent-runner/issues/twelve", wantErr: "is not a number"},
		{name: "zero", ref: "https://github.com/octo/agent-runner/pull/0", wantErr: "is not a number"},
		{name: "too short", ref: "https://github.com/octo/agent-runner", wantErr: "want /owner/repo"},
		{name: "neither issues nor pull", ref: "https://github.com/octo/agent-runner/wiki/7", wantErr: "neither issues nor pull"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, number, err := ParseRef(tc.ref)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseRef(%q) = %s/%s#%d, want an error containing %q", tc.ref, owner, repo, number, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseRef(%q) error = %q, want it to contain %q", tc.ref, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", tc.ref, err)
			}
			if owner != tc.owner || repo != tc.repo || number != tc.number {
				t.Fatalf("ParseRef(%q) = %s/%s#%d, want %s/%s#%d", tc.ref, owner, repo, number, tc.owner, tc.repo, tc.number)
			}
		})
	}
}
