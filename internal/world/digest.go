package world

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Digest returns a hex SHA-256 over the World's semantic fields, with any event
// authored by excludeAuthor dropped.
//
// The digest is what turns "nothing changed" into a comparison rather than a
// judgement. It covers what a decision can act on — the pull request's head and
// base, its draft and mergeable flags, every review and check, every comment's
// id and author, and the issue's state, assignees and labels — and nothing
// else. Timestamps are left out on purpose: a thread nobody touched has the
// same digest the next morning, and one new comment does not.
//
// Two reads that agree semantically have to agree here, so the hashed value is
// built rather than borrowed. A struct names the fields in a fixed order, every
// slice is sorted first, and no map appears anywhere: Go randomises map
// iteration, and a digest that changes from run to run is worse than none.
//
// excludeAuthor is the authenticated account, because its own comments and
// reviews are not news to it. The comparison ignores case — GitHub logins do —
// and an empty excludeAuthor drops nothing.
func (w World) Digest(excludeAuthor string) (string, error) {
	b, err := json.Marshal(w.canonical(excludeAuthor))
	if err != nil {
		return "", fmt.Errorf("marshal digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// canonical is the exact shape the digest hashes. It is a set of types of its
// own rather than the World itself, so that a field added to World cannot
// silently change every digest already written to a ledger: a new field joins
// the digest by being added here, on purpose, with a test that pins the result.
// The tag order is the order hashed.
type canonical struct {
	PR       canonicalPR        `json:"pr"`
	Reviews  []canonicalReview  `json:"reviews"`
	Checks   []canonicalCheck   `json:"checks"`
	Comments []canonicalComment `json:"comments"`
	Issue    canonicalIssue     `json:"issue"`
}

type canonicalPR struct {
	HeadSHA        string `json:"head_sha"`
	BaseSHA        string `json:"base_sha"`
	Draft          bool   `json:"draft"`
	Mergeable      string `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	State          string `json:"state"`
}

type canonicalReview struct {
	ID     int64  `json:"id"`
	State  string `json:"state"`
	Author string `json:"author"`
}

// canonicalCheck carries no id. A check that is re-run keeps its name and
// conclusion and takes a new id, which is transport noise rather than a change
// in the world; hashing the id would make every re-run read as news.
type canonicalCheck struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
}

// canonicalComment carries no body, and Comment has no field for one: an edited
// comment body churns without changing what the thread asks for, and a digest
// that moved on every typo fix would keep the worker working for nothing.
type canonicalComment struct {
	ID     int64  `json:"id"`
	Author string `json:"author"`
}

type canonicalIssue struct {
	Number    int      `json:"number"`
	State     string   `json:"state"`
	Assignees []string `json:"assignees"`
	Labels    []string `json:"labels"`
}

// canonical builds the digestable value: own events dropped, every slice
// sorted, and empty slices non-nil so that an absent list and an empty one
// encode alike — a nil slice marshals as null, and `null` next to `[]` is two
// digests for one thread with nothing to tell them apart.
func (w World) canonical(excludeAuthor string) canonical {
	c := canonical{
		PR: canonicalPR{
			HeadSHA:        w.PR.HeadSHA,
			BaseSHA:        w.PR.BaseSHA,
			Draft:          w.PR.Draft,
			Mergeable:      w.PR.Mergeable.String(),
			MergeableState: w.PR.MergeableState,
			State:          w.PR.State,
		},
		Reviews:  make([]canonicalReview, 0, len(w.Reviews)),
		Checks:   make([]canonicalCheck, 0, len(w.Checks)),
		Comments: make([]canonicalComment, 0, len(w.Comments)),
		Issue: canonicalIssue{
			Number:    w.Issue.Number,
			State:     w.Issue.State,
			Assignees: sortedCopy(w.Issue.Assignees),
			Labels:    sortedCopy(w.Issue.Labels),
		},
	}

	for _, r := range w.Reviews {
		if isOwnEvent(r.Author, excludeAuthor) {
			continue
		}
		c.Reviews = append(c.Reviews, canonicalReview{ID: r.ID, State: r.State, Author: r.Author})
	}
	for _, ck := range w.Checks {
		c.Checks = append(c.Checks, canonicalCheck{Name: ck.Name, Conclusion: ck.Conclusion, Status: ck.Status})
	}
	for _, cm := range w.Comments {
		if isOwnEvent(cm.Author, excludeAuthor) {
			continue
		}
		c.Comments = append(c.Comments, canonicalComment{ID: cm.ID, Author: cm.Author})
	}

	// SortWorld already orders the World, but canonical does not trust its
	// input: a World built by hand in a test, or filled by a future reader that
	// forgets, must still hash the same. Checks sort on what they carry, since
	// the id is gone and same-named entries would otherwise keep API order.
	sort.Slice(c.Reviews, func(i, j int) bool { return c.Reviews[i].ID < c.Reviews[j].ID })
	sort.Slice(c.Comments, func(i, j int) bool { return c.Comments[i].ID < c.Comments[j].ID })
	sort.Slice(c.Checks, func(i, j int) bool {
		a, b := c.Checks[i], c.Checks[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Conclusion != b.Conclusion {
			return a.Conclusion < b.Conclusion
		}
		return a.Status < b.Status
	})
	return c
}

// isOwnEvent reports whether author is the account whose own writes the digest
// must ignore. An empty excludeAuthor matches nothing: a run that does not know
// its own login should digest too much, never nothing.
func isOwnEvent(author, excludeAuthor string) bool {
	return excludeAuthor != "" && strings.EqualFold(author, excludeAuthor)
}

// sortedCopy returns ss in sorted order, as a non-nil slice so that it encodes
// as [] rather than null. Assignees and labels arrive in API order and sortWorld
// leaves them alone, so a copy is where that order stops mattering.
func sortedCopy(ss []string) []string {
	out := make([]string, 0, len(ss))
	out = append(out, ss...)
	sort.Strings(out)
	return out
}
