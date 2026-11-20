package world

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixtureSource replays the responses recorded in testdata/thread.json for one
// scenario, so a test drives the whole conversion without a network.
func fixtureSource(t *testing.T, scenario string) source {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "thread.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var all map[string]map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	responses, ok := all[scenario]
	if !ok {
		t.Fatalf("fixture has no scenario %q", scenario)
	}
	return func(_ context.Context, path string, v any) error {
		raw, ok := responses[path]
		if !ok {
			return fmt.Errorf("fixture has no response for %s", path)
		}
		return json.Unmarshal(raw, v)
	}
}

func TestFetchPRWorld(t *testing.T) {
	const ref = "https://github.com/octo/agent-runner/pull/7"
	w, err := fetch(context.Background(), fixtureSource(t, "pull"), ref, "octo", "agent-runner", 7)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if w.Subject != ref {
		t.Errorf("Subject = %q, want %q", w.Subject, ref)
	}
	if w.Issue.Number != 7 || w.Issue.Title != "Record: thread reader builds the World" || w.Issue.State != "open" {
		t.Errorf("Issue = %d %q %q, want 7 %q %q", w.Issue.Number, w.Issue.Title, w.Issue.State,
			"Record: thread reader builds the World", "open")
	}
	if got, want := w.Issue.Assignees, []string{"octocat", "hubot"}; !slices.Equal(got, want) {
		t.Errorf("Assignees = %v, want %v", got, want)
	}
	if got, want := w.Issue.Labels, []string{"record", "needs-tests"}; !slices.Equal(got, want) {
		t.Errorf("Labels = %v, want %v", got, want)
	}

	if w.PR.Number != 7 || w.PR.HeadSHA != "aaa111" || w.PR.BaseSHA != "bbb222" {
		t.Errorf("PR numbers = %d %q %q, want 7 aaa111 bbb222", w.PR.Number, w.PR.HeadSHA, w.PR.BaseSHA)
	}
	if w.PR.Draft || w.PR.Mergeable != MergeableYes || w.PR.MergeableState != "clean" || w.PR.State != "open" {
		t.Errorf("PR flags = draft:%t mergeable:%s state:%q state:%q, want false yes clean open",
			w.PR.Draft, w.PR.Mergeable, w.PR.MergeableState, w.PR.State)
	}

	// The fixture lists the later review and the later comment first, and the
	// checks out of alphabetical order, so these also test the sorting.
	var reviews []string
	for _, r := range w.Reviews {
		reviews = append(reviews, fmt.Sprintf("%d %s %s", r.ID, r.State, r.Author))
	}
	if want := []string{"100 CHANGES_REQUESTED octocat", "900 APPROVED hubot"}; !slices.Equal(reviews, want) {
		t.Errorf("Reviews = %v, want %v", reviews, want)
	}

	// The fixture lists vet first, and each check carries the id that has to
	// survive the read: it is what makes two checks with one name order the same
	// way on the next read.
	var checks []string
	for _, c := range w.Checks {
		checks = append(checks, fmt.Sprintf("%d %s %s %s", c.ID, c.Name, c.Conclusion, c.Status))
	}
	if want := []string{"400 build failure completed", "500 vet success completed"}; !slices.Equal(checks, want) {
		t.Errorf("Checks = %v, want %v", checks, want)
	}

	var comments []string
	for _, c := range w.Comments {
		comments = append(comments, fmt.Sprintf("%d %s %s", c.ID, c.Author, c.CreatedAt))
	}
	want := []string{"10 marcus-beauregard 2026-11-19T09:00:00Z", "30 hubot 2026-11-20T09:00:00Z"}
	if !slices.Equal(comments, want) {
		t.Errorf("Comments = %v, want %v", comments, want)
	}
}

func TestFetchIssueWorldHasNoPR(t *testing.T) {
	const ref = "https://github.com/octo/agent-runner/issues/12"
	w, err := fetch(context.Background(), fixtureSource(t, "issue"), ref, "octo", "agent-runner", 12)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if w.PR != (PR{}) {
		t.Errorf("PR = %+v, want the zero value for an issue with no pull request", w.PR)
	}
	if len(w.Reviews) != 0 || len(w.Checks) != 0 || len(w.Comments) != 0 {
		t.Errorf("threads = %d reviews, %d checks, %d comments, want none",
			len(w.Reviews), len(w.Checks), len(w.Comments))
	}
	if got, want := w.Issue.Labels, []string{"record"}; !slices.Equal(got, want) {
		t.Errorf("Labels = %v, want %v", got, want)
	}
}

// TestFetchEmptyChecksIsNotAnError is the ticket's edge case, through fetch and
// not through the conversion: a head with no check runs is a normal state, and a
// read that passes through every endpoint then the count guard must return an
// empty list rather than fail.
func TestFetchEmptyChecksIsNotAnError(t *testing.T) {
	src := stubSource(t, map[string]string{
		"/repos/octo/agent-runner/issues/7":                  `{"number":7,"title":"t","state":"open","pull_request":{}}`,
		"/repos/octo/agent-runner/pulls/7":                   `{"number":7,"mergeable":true,"head":{"sha":"aaa111"},"base":{"sha":"bbb222"}}`,
		"/repos/octo/agent-runner/pulls/7/reviews":           `[]`,
		"/repos/octo/agent-runner/commits/aaa111/check-runs": `{"total_count":0,"check_runs":[]}`,
		"/repos/octo/agent-runner/issues/7/comments":         `[]`,
	})
	w, err := fetch(context.Background(), src, "https://github.com/octo/agent-runner/pull/7", "octo", "agent-runner", 7)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(w.Checks) != 0 {
		t.Errorf("Checks = %+v, want none", w.Checks)
	}
}

// stubSource answers a fetch from fixed JSON per path, for the cases a recorded
// thread does not cover.
func stubSource(t *testing.T, responses map[string]string) source {
	t.Helper()
	return func(_ context.Context, path string, v any) error {
		raw, ok := responses[path]
		if !ok {
			return fmt.Errorf("no stub for %s", path)
		}
		return json.Unmarshal([]byte(raw), v)
	}
}

// TestFetchFailsWhenCheckRunsAreShort is the guard on the guard: paging is what
// keeps a read whole, and a count the server gives is what proves it stayed
// whole. A read that collected one of three must be an error, not a World that
// looks complete.
func TestFetchFailsWhenCheckRunsAreShort(t *testing.T) {
	src := stubSource(t, map[string]string{
		"/repos/octo/agent-runner/issues/7":                  `{"number":7,"title":"t","state":"open","pull_request":{"url":"https://api.github.com/repos/octo/agent-runner/pulls/7"}}`,
		"/repos/octo/agent-runner/pulls/7":                   `{"number":7,"mergeable":true,"head":{"sha":"aaa111"},"base":{"sha":"bbb222"}}`,
		"/repos/octo/agent-runner/pulls/7/reviews":           `[]`,
		"/repos/octo/agent-runner/commits/aaa111/check-runs": `{"total_count":3,"check_runs":[{"name":"build","status":"completed","conclusion":"success"}]}`,
		"/repos/octo/agent-runner/issues/7/comments":         `[]`,
	})
	_, err := fetch(context.Background(), src, "https://github.com/octo/agent-runner/pull/7", "octo", "agent-runner", 7)
	if err == nil {
		t.Fatal("fetch succeeded, want an error naming the missing check runs")
	}
	if got := err.Error(); !strings.Contains(got, "read 1 of 3") {
		t.Errorf("error = %q, want it to say what was read of what was promised", got)
	}
}

// TestGhAPIJoinsEveryPage runs a gh that prints two pages the way
// --paginate --slurp does, and asserts both land in the slice and the flags were
// actually passed.
func TestGhAPIJoinsEveryPage(t *testing.T) {
	gh := fakeGh(t, `if [ "$2" != "--paginate" ] || [ "$3" != "--slurp" ]; then
  echo "read without pagination: $@" >&2
  exit 3
fi
cat <<'EOF'
[[{"id":3}],[{"id":1},{"id":2}]]
EOF
`)
	var got []struct {
		ID int64 `json:"id"`
	}
	if err := ghAPI(gh)(context.Background(), "/x", &got); err != nil {
		t.Fatalf("ghAPI: %v", err)
	}
	var ids []int64
	for _, g := range got {
		ids = append(ids, g.ID)
	}
	if want := []int64{3, 1, 2}; !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v; every page, in the order gh printed them", ids, want)
	}
}

// TestGhAPIMergesCheckPages covers the shape a busy commit produces: check-runs
// is an object endpoint, so --paginate --slurp gives one object per page, each
// repeating total_count. Reading only the first page would drop runs silently;
// failing on the second would fail a healthy read. The real gh does exactly this
// for any commit with more than a hundred check runs.
func TestGhAPIMergesCheckPages(t *testing.T) {
	gh := fakeGh(t, `cat <<'EOF'
[{"total_count":3,"check_runs":[{"id":1,"name":"build","status":"completed","conclusion":"failure"}]},{"total_count":3,"check_runs":[{"id":2,"name":"build","status":"completed","conclusion":"success"},{"id":3,"name":"vet","status":"completed","conclusion":"success"}]}]
EOF
`)
	var cs rawChecks
	if err := ghAPI(gh)(context.Background(), "/x", &cs); err != nil {
		t.Fatalf("ghAPI: %v", err)
	}
	if cs.TotalCount != 3 {
		t.Errorf("TotalCount = %d, want 3", cs.TotalCount)
	}
	var got []int64
	for _, c := range cs.CheckRuns {
		got = append(got, c.ID)
	}
	if want := []int64{1, 2, 3}; !slices.Equal(got, want) {
		t.Errorf("check ids = %v, want %v: both pages, in the order gh printed them", got, want)
	}
}

// TestGhAPIMismatchedCheckTotalsIsAnError guards the join: pages that
// disagree about total_count came from different reads, and merging them would
// produce a World the server never had.
func TestGhAPIMismatchedCheckTotalsIsAnError(t *testing.T) {
	gh := fakeGh(t, `cat <<'EOF'
[{"total_count":3,"check_runs":[{"id":1,"name":"build"}]},{"total_count":4,"check_runs":[{"id":2,"name":"vet"}]}]
EOF
`)
	var cs rawChecks
	err := ghAPI(gh)(context.Background(), "/x", &cs)
	if err == nil {
		t.Fatal("ghAPI succeeded, want an error naming the disagreeing counts")
	}
	if want := "page 2 says total_count 4, page 1 said 3"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

// TestGhAPIDecodesSingleObjectPage covers the other shape: an endpoint that
// returns one object, which --slurp wraps in a one-element array.
func TestGhAPIDecodesSingleObjectPage(t *testing.T) {
	gh := fakeGh(t, `cat <<'EOF'
[{"number":7,"state":"open"}]
EOF
`)
	var pr rawPR
	if err := ghAPI(gh)(context.Background(), "/x", &pr); err != nil {
		t.Fatalf("ghAPI: %v", err)
	}
	if pr.Number != 7 || pr.State != "open" {
		t.Errorf("PR = %d %q, want 7 open", pr.Number, pr.State)
	}
}

// TestMergeableKeepsNotYetApartFromNo is the tri-state itself: null means GitHub
// is still computing, which is neither yes nor no.
func TestMergeableKeepsNotYetApartFromNo(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Mergeable
	}{
		{name: "true", body: `{"mergeable":true}`, want: MergeableYes},
		{name: "false", body: `{"mergeable":false}`, want: MergeableNo},
		{name: "null while computing", body: `{"mergeable":null}`, want: MergeableUnknown},
		{name: "absent", body: `{}`, want: MergeableUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r rawPR
			if err := json.Unmarshal([]byte(tc.body), &r); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := r.pr().Mergeable; got != tc.want {
				t.Errorf("Mergeable = %s, want %s", got, tc.want)
			}
		})
	}
}

// fakeGh writes an executable gh whose body is the given shell script.
func fakeGh(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	return path
}

// TestFetchCarriesGhStderr puts a gh on PATH that fails the way the real one
// does, and asserts the sentence on stderr survives into the error.
func TestFetchCarriesGhStderr(t *testing.T) {
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	script := "#!/bin/sh\necho 'HTTP 404: Not Found (https://api.github.com/repos/octo/agent-runner/issues/9)' >&2\nexit 1\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir)

	_, err := Fetch(context.Background(), "https://github.com/octo/agent-runner/issues/9")
	if err == nil {
		t.Fatal("Fetch succeeded, want the error gh wrote on stderr")
	}
	if got := err.Error(); !strings.Contains(got, "HTTP 404: Not Found") {
		t.Errorf("error = %q, want it to carry gh's stderr", got)
	}
}
