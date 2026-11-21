package world

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestSortWorldOrdersDuplicateChecksByID is the one sort whose key is not
// unique: a commit that ran the same check twice has two entries with one name.
// Preserving the order the API answered in is not enough — the next read may
// answer in a different order, and the value that feeds the digest has to be the
// same one. The unique id is the tie-break, so two answer orders must land on
// one result.
func TestSortWorldOrdersDuplicateChecksByID(t *testing.T) {
	// Enough entries that the sort is not insertion sort, and two names each
	// repeated, so an unstable sort has somewhere to reorder them.
	newWorld := func(ids []int64) World {
		var w World
		for i, id := range ids {
			w.Checks = append(w.Checks, Check{ID: id, Name: []string{"build", "vet"}[i%2]})
		}
		return w
	}
	var ascending []int64
	for i := int64(0); i < 24; i++ {
		ascending = append(ascending, i)
	}
	// A fixed permutation, not a random one, so a failure is reproducible: swap
	// each pair, then reverse the whole slice.
	var shuffled []int64
	for i := int64(0); i < 24; i += 2 {
		shuffled = append(shuffled, i+1, i)
	}
	for i := int64(0); i < 12; i++ {
		shuffled[i], shuffled[23-i] = shuffled[23-i], shuffled[i]
	}

	a := newWorld(ascending)
	b := newWorld(shuffled)
	sortWorld(&a)
	sortWorld(&b)
	if !slices.Equal(a.Checks, b.Checks) {
		t.Fatalf("same checks, two answer orders:\n ascending: %v\n shuffled:  %v", a.Checks, b.Checks)
	}

	var got []string
	for _, c := range a.Checks {
		got = append(got, fmt.Sprintf("%s %d", c.Name, c.ID))
	}
	var want []string
	for _, name := range []string{"build", "vet"} {
		for i := int64(0); i < 24; i++ {
			if []string{"build", "vet"}[i%2] == name {
				want = append(want, fmt.Sprintf("%s %d", name, i))
			}
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("Checks = %v, want %v: name first, then id", got, want)
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

// TestWorldJSONWireFormat pins the names world.json carries. The snapshot is a
// wire format read by replay and by people, so a Go field rename must not rename
// a key, and the mergeable state is written as its name rather than its ordinal.
func TestWorldJSONWireFormat(t *testing.T) {
	w := World{
		Subject: "https://github.com/marcus-beauregard/agent-runner/issues/20",
		Issue:   Issue{Number: 20, Title: "the record", State: "open", Assignees: []string{"marcus-beauregard"}, Labels: []string{"record"}},
		PR:      PR{Number: 40, HeadSHA: "aaa111", BaseSHA: "bbb222", Draft: true, Mergeable: MergeableYes, MergeableState: "clean", State: "open"},
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal World: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("decode World: %v", err)
	}
	checkJSONKeys(t, b, "subject", "issue", "pr", "reviews", "checks", "comments")
	checkJSONKeys(t, top["issue"], "number", "title", "state", "assignees", "labels")
	checkJSONKeys(t, top["pr"], "number", "head_sha", "base_sha", "draft", "mergeable", "mergeable_state", "state")
	var pr map[string]json.RawMessage
	if err := json.Unmarshal(top["pr"], &pr); err != nil {
		t.Fatalf("decode pr: %v", err)
	}
	var mergeable string
	if err := json.Unmarshal(pr["mergeable"], &mergeable); err != nil {
		t.Fatalf("mergeable is not a name: %v", err)
	}
	if mergeable != "yes" {
		t.Errorf("mergeable = %q, want yes", mergeable)
	}
}

// checkJSONKeys compares one encoded object's key set with the names the wire
// format promises. Extra and missing keys are both failures: an extra key is a
// field that joined the format without a decision.
func checkJSONKeys(t *testing.T, raw []byte, want ...string) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if len(m) != len(want) {
		t.Errorf("%s has %d keys, want %d", raw, len(m), len(want))
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("%s has no key %q", raw, k)
		}
	}
}
