// Package world reads one thread into a single typed value, so that every step
// after the fetch — digest, classify, unchanged — works from a value rather
// than from a pile of gh output.
package world

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// World is one thread as the worker sees it: the issue, the pull request that
// answers it when there is one, and everything the pair carries that a decision
// depends on.
type World struct {
	Subject  string    // the ref it was read from, verbatim
	Issue    Issue     // never the zero value: every thread has an issue
	PR       PR        // the zero value when the ref is an issue with no pull request
	Reviews  []Review  // sorted by ID
	Checks   []Check   // sorted by Name
	Comments []Comment // sorted by ID
}

// Issue is the slice of an issue a decision needs.
type Issue struct {
	Number    int
	Title     string
	State     string
	Assignees []string
	Labels    []string
}

// PR is the slice of a pull request a decision needs.
type PR struct {
	Number         int
	HeadSHA        string
	BaseSHA        string
	Draft          bool
	Mergeable      bool
	MergeableState string
	State          string
}

// Review is one review on the pull request.
type Review struct {
	ID     int64
	State  string
	Author string
}

// Check is one check run on the pull request's head commit.
type Check struct {
	Name       string
	Conclusion string
	Status     string
}

// Comment is one comment on the issue, which is also where a pull request's
// conversation lives: both are issues to the API.
type Comment struct {
	ID        int64
	Author    string
	CreatedAt string
}

// ParseRef turns a github.com issue or pull request URL into its owner, repo and
// number. It accepts .../issues/<n> and .../pull/<n>, with or without a trailing
// slash, query or fragment, and rejects a URL that is not https, not
// github.com, or does not end in a positive integer.
func ParseRef(ref string) (owner, repo string, number int, err error) {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", "", 0, fmt.Errorf("parse ref %q: %w", ref, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", "", 0, fmt.Errorf("parse ref %q: not an https URL", ref)
	}
	if host := strings.ToLower(u.Host); host != "github.com" && host != "www.github.com" {
		return "", "", 0, fmt.Errorf("parse ref %q: not github.com", ref)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" {
		return "", "", 0, fmt.Errorf("parse ref %q: want /owner/repo/{issues,pull}/number", ref)
	}
	if parts[2] != "issues" && parts[2] != "pull" {
		return "", "", 0, fmt.Errorf("parse ref %q: %q is neither issues nor pull", ref, parts[2])
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return "", "", 0, fmt.Errorf("parse ref %q: %q is not a number", ref, parts[3])
	}
	return parts[0], parts[1], n, nil
}

// sortWorld puts every slice in its documented order, so that two runs which
// read the same thread produce the same value — and later the same digest —
// whatever order the API happened to answer in.
func sortWorld(w *World) {
	sort.Slice(w.Reviews, func(i, j int) bool { return w.Reviews[i].ID < w.Reviews[j].ID })
	sort.Slice(w.Checks, func(i, j int) bool { return w.Checks[i].Name < w.Checks[j].Name })
	sort.Slice(w.Comments, func(i, j int) bool { return w.Comments[i].ID < w.Comments[j].ID })
}
