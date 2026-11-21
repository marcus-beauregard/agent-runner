package world

import (
	"sort"
	"strconv"
	"strings"
)

// Action is one thing a thread still owes: the event that asks for it and the
// kind of write that answers it.
//
// TriggerID is one of the three forms internal/run documents —
// review:<id>:changes_requested, comment:<id>, check:<name>:failure — and it is
// the name a spend is recorded under, so the same World always names the same
// actions. It is derived from the event, never generated (PRD §8).
type Action struct {
	TriggerID string // one of the three documented forms
	Kind      string // "reply" or "fix"
}

// The two kinds of write an action stands for. Both are responses: the set has
// no member that asks for attention (PRD §8, response only).
const (
	kindReply = "reply" // answer the event, change nothing else
	kindFix   = "fix"   // change the branch, then answer
)

// Actions returns the actions the thread still owes, sorted by TriggerID.
//
// It is a pure function of the World: it reads no ledger and calls nothing. A
// spend from an earlier run is not this run's business — the ledger decides
// whether an action is still owed, and replay compares one run's snapshot with
// that run's own trigger rows (PRD §8 stage 2).
//
// self is the authenticated account. Its own reviews and comments never produce
// actions, and an empty self excludes nothing, so the function is testable
// without a login.
func (w World) Actions(self string) []Action {
	// A thread with no pull request, or one that is closed, merged or still a
	// draft, owes nothing: merged and closed both arrive as state "closed", and
	// there is no branch of ours to change or conversation to answer.
	if w.PR.Number == 0 || w.PR.Draft || !strings.EqualFold(w.PR.State, "open") {
		return nil
	}

	// Approval ends the run (PRD §8). An approved, green, mergeable pull request
	// is finished: the worker does not seek the merge, and it does not reopen a
	// conversation the maintainer has already concluded.
	if w.settled(self) {
		return nil
	}

	kinds := make(map[string]string, len(w.Reviews)+len(w.Comments)+len(w.Checks))

	// One review per author counts: the highest id, that author's latest word.
	// A CHANGES_REQUESTED the same author later approved or dismissed is
	// answered, and re-asking would be the loop talking to itself.
	for _, r := range latestReviews(w.Reviews) {
		if isOwnEvent(r.Author, self) {
			continue
		}
		if strings.EqualFold(r.State, "CHANGES_REQUESTED") {
			kinds["review:"+strconv.FormatInt(r.ID, 10)+":changes_requested"] = kindFix
		}
	}

	// A comment is owed an answer unless we have written since. Comment ids
	// increase with time — the only ordering the World carries — so "since" is
	// a higher id than the comment itself.
	var newestOwn int64
	hasOwn := false
	for _, c := range w.Comments {
		if isOwnEvent(c.Author, self) && (!hasOwn || c.ID > newestOwn) {
			newestOwn, hasOwn = c.ID, true
		}
	}
	for _, c := range w.Comments {
		if isOwnEvent(c.Author, self) {
			continue
		}
		if hasOwn && c.ID < newestOwn {
			continue
		}
		kinds["comment:"+strconv.FormatInt(c.ID, 10)] = kindReply
	}

	// A red check on the head is a fix. Same-named runs collapse to one action
	// by sharing an id: a name that ran twice is one thing to repair, not two.
	for _, c := range latestChecks(w.Checks) {
		if !strings.EqualFold(c.Status, "completed") || !strings.EqualFold(c.Conclusion, "failure") {
			continue
		}
		kinds["check:"+c.Name+":failure"] = kindFix
	}

	if len(kinds) == 0 {
		return nil
	}
	out := make([]Action, 0, len(kinds))
	for id, kind := range kinds {
		out = append(out, Action{TriggerID: id, Kind: kind})
	}
	// Sorted so two reads of one World compare byte for byte, and so replay's
	// diff is a diff rather than a reshuffle.
	sort.Slice(out, func(i, j int) bool { return out[i].TriggerID < out[j].TriggerID })
	return out
}

// settled reports whether the pull request has nothing left to do: somebody
// approved it, no author's latest review still asks for changes, every check is
// green, and GitHub says it can be merged.
//
// No checks counts as every check green: the guard is about being blocked, and
// a head with no check runs has nothing red to repair. Our own approval does not
// count — the worker does not approve, and if it ever did, that write must not
// be the one that ends its own run.
func (w World) settled(self string) bool {
	if w.PR.Mergeable != MergeableYes {
		return false
	}
	approved := false
	for _, r := range latestReviews(w.Reviews) {
		if isOwnEvent(r.Author, self) {
			continue
		}
		switch {
		case strings.EqualFold(r.State, "CHANGES_REQUESTED"):
			return false
		case strings.EqualFold(r.State, "APPROVED"):
			approved = true
		}
	}
	if !approved {
		return false
	}
	for _, c := range latestChecks(w.Checks) {
		if !strings.EqualFold(c.Status, "completed") || !strings.EqualFold(c.Conclusion, "success") {
			return false
		}
	}
	return true
}

// decisive reports whether a review carries a verdict about the pull request.
// A review that only comments says nothing about approval, and a pending one is
// a draft nobody else can see yet; neither takes back the word before it.
func decisive(state string) bool {
	return !strings.EqualFold(state, "COMMENTED") && !strings.EqualFold(state, "PENDING")
}

// latestReviews keeps each author's highest-id decisive review, ordered by id.
//
// A later review that only comments does not undo the verdict before it: GitHub
// leaves reviewDecision at APPROVED or CHANGES_REQUESTED when the same author
// comes back with remarks, and "approval ends the run" has to survive a
// thank-you note. Reviews arrive sorted, but this does not trust its input: a
// World built by hand in a test, or filled by a reader that forgets, must still
// classify the same. Logins compare case-insensitively, as GitHub does.
func latestReviews(rs []Review) []Review {
	best := make(map[string]Review, len(rs))
	for _, r := range rs {
		if !decisive(r.State) {
			continue
		}
		key := strings.ToLower(r.Author)
		if cur, ok := best[key]; !ok || r.ID > cur.ID {
			best[key] = r
		}
	}
	out := make([]Review, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// latestChecks keeps the newest run of each check name, ordered by name. A
// re-run keeps the name and takes a new id, so the highest id is what the
// commit's checks say now; an older failure that was re-run green is history,
// not work. No map order escapes: the result is sorted by name, which is unique
// after this.
func latestChecks(cs []Check) []Check {
	best := make(map[string]Check, len(cs))
	for _, c := range cs {
		if cur, ok := best[c.Name]; !ok || c.ID > cur.ID {
			best[c.Name] = c
		}
	}
	out := make([]Check, 0, len(best))
	for _, c := range best {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
