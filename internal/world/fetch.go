package world

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// source performs one API read and decodes the response into v. It is a
// parameter rather than a method so a test can feed recorded responses instead
// of a network, and exercise every line of the conversion below.
type source func(ctx context.Context, path string, v any) error

// Fetch builds a World from an issue or pull request URL. It reaches GitHub only
// through the gh CLI, as an argv array and never a shell.
func Fetch(ctx context.Context, ref string) (World, error) {
	owner, repo, number, err := ParseRef(ref)
	if err != nil {
		return World{}, err
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return World{}, fmt.Errorf("gh CLI not available: %w", err)
	}
	return fetch(ctx, ghAPI(gh), ref, owner, repo, number)
}

// ghAPI returns the source that runs gh. Pagination is not optional here: past
// the first page a long review is dropped without a word, and because everything
// is later sorted by id the pages lost are the newest ones — exactly the ones an
// unchanged comparison exists to notice. --paginate walks the Link header;
// --slurp wraps each page in one JSON array, the only form gh prints that stays
// valid JSON whatever the page count.
//
// A non-zero exit becomes an error carrying gh's stderr: "exit status 1" alone
// never says what went wrong, and the sentence gh wrote on stderr always does.
func ghAPI(gh string) source {
	return func(ctx context.Context, path string, v any) error {
		out, err := exec.CommandContext(ctx, gh, "api", "--paginate", "--slurp", path).Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return fmt.Errorf("gh api %s: %s", path, strings.TrimSpace(string(ee.Stderr)))
			}
			return fmt.Errorf("gh api %s: %w", path, err)
		}
		var pages []json.RawMessage
		if err := json.Unmarshal(out, &pages); err != nil {
			return fmt.Errorf("parse gh api %s: %w", path, err)
		}
		if err := decodePages(pages, v); err != nil {
			return fmt.Errorf("parse gh api %s: %w", path, err)
		}
		return nil
	}
}

// decodePages turns gh's array of pages into the value the caller asked for: a
// list response becomes every page's elements joined, a single object response
// becomes the one page gh printed.
func decodePages(pages []json.RawMessage, v any) error {
	if len(pages) == 0 {
		return nil
	}
	if first := bytes.TrimSpace(pages[0]); len(first) > 0 && first[0] == '[' {
		joined, err := joinPages(pages)
		if err != nil {
			return err
		}
		return json.Unmarshal(joined, v)
	}
	if len(pages) != 1 {
		return fmt.Errorf("want one page of a single object, got %d", len(pages))
	}
	return json.Unmarshal(pages[0], v)
}

// joinPages concatenates the elements of every page into one JSON array.
func joinPages(pages []json.RawMessage) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, p := range pages {
		p = bytes.TrimSpace(p)
		if len(p) < 2 || p[0] != '[' || p[len(p)-1] != ']' {
			return nil, fmt.Errorf("page %d is not a JSON array", i+1)
		}
		inner := bytes.TrimSpace(p[1 : len(p)-1])
		if len(inner) == 0 {
			continue
		}
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.Write(inner)
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

// fetch reads the endpoints a World is made of: the issue always, and the pull
// request with its reviews and checks when the issue is one.
func fetch(ctx context.Context, src source, ref, owner, repo string, number int) (World, error) {
	w := World{Subject: ref}
	base := fmt.Sprintf("/repos/%s/%s", owner, repo)

	var iss rawIssue
	if err := src(ctx, fmt.Sprintf("%s/issues/%d", base, number), &iss); err != nil {
		return World{}, err
	}
	w.Issue = iss.issue()

	if iss.PullRequest != nil {
		var pr rawPR
		if err := src(ctx, fmt.Sprintf("%s/pulls/%d", base, number), &pr); err != nil {
			return World{}, err
		}
		w.PR = pr.pr()

		var revs []rawReview
		if err := src(ctx, fmt.Sprintf("%s/pulls/%d/reviews", base, number), &revs); err != nil {
			return World{}, err
		}
		for _, r := range revs {
			w.Reviews = append(w.Reviews, r.review())
		}

		// Checks hang off the head commit by sha, so they come after the pull.
		var cs rawChecks
		if err := src(ctx, fmt.Sprintf("%s/commits/%s/check-runs", base, w.PR.HeadSHA), &cs); err != nil {
			return World{}, err
		}
		w.Checks = cs.checks()
		// total_count is the server's count for this commit, so a read that
		// collected fewer has lost a page and must not pass for a whole one.
		if len(w.Checks) < cs.TotalCount {
			return World{}, fmt.Errorf("check-runs for %s: read %d of %d", w.PR.HeadSHA, len(w.Checks), cs.TotalCount)
		}
	}

	var cms []rawComment
	if err := src(ctx, fmt.Sprintf("%s/issues/%d/comments", base, number), &cms); err != nil {
		return World{}, err
	}
	for _, c := range cms {
		w.Comments = append(w.Comments, c.comment())
	}

	sortWorld(&w)
	return w, nil
}

// The raw types are the boundary. gh's response is decoded into these and
// dropped there: nothing outside this file ever sees an API field that world.go
// does not name, so a field GitHub adds tomorrow cannot leak into a decision.

type rawIssue struct {
	Number      int            `json:"number"`
	Title       string         `json:"title"`
	State       string         `json:"state"`
	PullRequest *struct{}      `json:"pull_request"`
	Assignees   []rawLogin     `json:"assignees"`
	Labels      []rawLabelName `json:"labels"`
}

type rawLogin struct {
	Login string `json:"login"`
}

type rawLabelName struct {
	Name string `json:"name"`
}

type rawPR struct {
	Number         int    `json:"number"`
	Draft          bool   `json:"draft"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	State          string `json:"state"`
	Head           rawSHA `json:"head"`
	Base           rawSHA `json:"base"`
}

type rawSHA struct {
	SHA string `json:"sha"`
}

type rawReview struct {
	ID    int64    `json:"id"`
	State string   `json:"state"`
	User  rawLogin `json:"user"`
}

type rawChecks struct {
	TotalCount int `json:"total_count"`
	CheckRuns  []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

type rawComment struct {
	ID        int64    `json:"id"`
	CreatedAt string   `json:"created_at"`
	User      rawLogin `json:"user"`
}

func (r rawIssue) issue() Issue {
	iss := Issue{Number: r.Number, Title: r.Title, State: r.State}
	for _, a := range r.Assignees {
		iss.Assignees = append(iss.Assignees, a.Login)
	}
	for _, l := range r.Labels {
		iss.Labels = append(iss.Labels, l.Name)
	}
	return iss
}

func (r rawPR) pr() PR {
	return PR{
		Number:         r.Number,
		HeadSHA:        r.Head.SHA,
		BaseSHA:        r.Base.SHA,
		Draft:          r.Draft,
		Mergeable:      r.mergeable(),
		MergeableState: r.MergeableState,
		State:          r.State,
	}
}

// mergeable keeps "no" apart from "not yet": GitHub sends null while it works
// out mergeability, and a plain bool reads that as false — a decision to refuse
// the merge with a reason nobody wrote.
func (r rawPR) mergeable() Mergeable {
	switch {
	case r.Mergeable == nil:
		return MergeableUnknown
	case *r.Mergeable:
		return MergeableYes
	default:
		return MergeableNo
	}
}

func (r rawReview) review() Review {
	return Review{ID: r.ID, State: r.State, Author: r.User.Login}
}

// checks keeps an empty check list empty rather than failing: a pull request
// whose head has no check runs is a normal state, not a broken read.
func (r rawChecks) checks() []Check {
	var cs []Check
	for _, c := range r.CheckRuns {
		cs = append(cs, Check{Name: c.Name, Conclusion: c.Conclusion, Status: c.Status})
	}
	return cs
}

func (r rawComment) comment() Comment {
	return Comment{ID: r.ID, Author: r.User.Login, CreatedAt: r.CreatedAt}
}
