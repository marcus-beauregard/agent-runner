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
	if w.PR.Draft || !w.PR.Mergeable || w.PR.MergeableState != "clean" || w.PR.State != "open" {
		t.Errorf("PR flags = draft:%t mergeable:%t state:%q state:%q, want false true clean open",
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

	var checks []string
	for _, c := range w.Checks {
		checks = append(checks, fmt.Sprintf("%s %s %s", c.Name, c.Conclusion, c.Status))
	}
	if want := []string{"build failure completed", "vet success completed"}; !slices.Equal(checks, want) {
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

func TestChecksWithoutRunsIsEmpty(t *testing.T) {
	var cs rawChecks
	if err := json.Unmarshal([]byte(`{"total_count":0,"check_runs":[]}`), &cs); err != nil {
		t.Fatalf("parse check-runs: %v", err)
	}
	if got := cs.checks(); len(got) != 0 {
		t.Errorf("checks() = %+v, want none", got)
	}
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
