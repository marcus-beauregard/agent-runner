// Package world reads one thread into a single typed value, so that every step
// after the fetch — digest, classify, unchanged — works from a value rather
// than from a pile of gh output.
package world

import (
	"encoding/json"
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
	Subject  string    `json:"subject"`  // the ref it was read from, verbatim
	Issue    Issue     `json:"issue"`    // never the zero value: every thread has an issue
	PR       PR        `json:"pr"`       // the zero value when the ref is an issue with no pull request
	Reviews  []Review  `json:"reviews"`  // sorted by ID
	Checks   []Check   `json:"checks"`   // sorted by Name, ties by ID
	Comments []Comment `json:"comments"` // sorted by ID
}

// Issue is the slice of an issue a decision needs.
type Issue struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Assignees []string `json:"assignees"`
	Labels    []string `json:"labels"`
}

// PR is the slice of a pull request a decision needs.
type PR struct {
	Number         int       `json:"number"`
	HeadSHA        string    `json:"head_sha"`
	BaseSHA        string    `json:"base_sha"`
	Draft          bool      `json:"draft"`
	Mergeable      Mergeable `json:"mergeable"`
	MergeableState string    `json:"mergeable_state"`
	State          string    `json:"state"`
}

// Mergeable is GitHub's answer to whether the pull request can be merged, which
// has three states and not two: yes, no, and not computed yet. GitHub reports
// the third as JSON null, and a bool turns that into "no" — a refusal with a
// reason nobody gave.
type Mergeable int

const (
	// MergeableUnknown means GitHub has not computed mergeability yet.
	MergeableUnknown Mergeable = iota
	// MergeableNo means GitHub says the pull request cannot be merged.
	MergeableNo
	// MergeableYes means GitHub says the pull request can be merged.
	MergeableYes
)

// String names the state, so a log line or a report does not print a bare int.
func (m Mergeable) String() string {
	switch m {
	case MergeableYes:
		return "yes"
	case MergeableNo:
		return "no"
	default:
		return "unknown"
	}
}

// MarshalJSON writes the state as its name. world.json is a record a person
// reads as well as a replay input, and `"mergeable": 2` says nothing without
// this file open beside it.
func (m Mergeable) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

// UnmarshalJSON accepts exactly the names MarshalJSON writes, so a snapshot
// carrying anything else is an error rather than MergeableUnknown. The three
// states are not interchangeable: unknown means GitHub has not decided, and a
// damaged record must not be read as one.
func (m *Mergeable) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("mergeable: %w", err)
	}
	switch s {
	case "unknown":
		*m = MergeableUnknown
	case "no":
		*m = MergeableNo
	case "yes":
		*m = MergeableYes
	default:
		return fmt.Errorf("mergeable: %q is not yes, no or unknown", s)
	}
	return nil
}

// Review is one review on the pull request.
type Review struct {
	ID     int64  `json:"id"`
	State  string `json:"state"`
	Author string `json:"author"`
}

// Check is one check run on the pull request's head commit.
type Check struct {
	ID         int64  `json:"id"` // unique, and the tie-break when a name repeats
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
}

// Comment is one comment on the issue, which is also where a pull request's
// conversation lives: both are issues to the API.
type Comment struct {
	ID        int64  `json:"id"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
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
	// Every one of these keys is a total order, which is the point: the value has
	// to be the same on two reads of an unchanged thread, and a stable sort only
	// promises to preserve the order it was handed. Check names repeat — a commit
	// that ran "build" twice has two of them — and GitHub does not promise the
	// same order for them on the next read, so the name alone would leave the
	// value depending on the answer. The id is unique and stable; sort on that
	// once the names are equal.
	sort.Slice(w.Reviews, func(i, j int) bool { return w.Reviews[i].ID < w.Reviews[j].ID })
	sort.Slice(w.Checks, func(i, j int) bool {
		if w.Checks[i].Name != w.Checks[j].Name {
			return w.Checks[i].Name < w.Checks[j].Name
		}
		return w.Checks[i].ID < w.Checks[j].ID
	})
	sort.Slice(w.Comments, func(i, j int) bool { return w.Comments[i].ID < w.Comments[j].ID })
}
