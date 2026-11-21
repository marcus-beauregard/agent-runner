package run

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
)

// ErrNoAccount reports a run record that does not say which account it ran as.
//
// Replay recomputes the action set with the classifier the command used, and
// that classifier has to know which reviews and comments were the run's own: a
// reply the run wrote is not a question the thread still owes. Without the
// account that answer is unknown, and assuming it empty would make every own
// comment look actionable — replay would report a mismatch in a record that is
// right. The field has no safe default, so a record without it is refused
// rather than replayed approximately.
var ErrNoAccount = errors.New("run record names no account")

// Replay recomputes the plan a recorded run decided on and compares it with the
// plan the ledger holds. It reads run.json, world.json and run.ndjson, and
// writes nothing: no run directory mutation, no network, no model call, so it is
// safe on a live run's directory as well as a finished one (PRD §8).
//
// ok is true only when the two action sets are equal. The diff is printed to w
// either way — what was recomputed, what was recorded, and which actions are
// missing from or extra to the record — because "differs" without the difference
// is not a proof of anything.
//
// A torn ledger, a World that cannot be read, or a run that does not name its
// account is an error, not a mismatch: replay can no longer see what the run
// saw, and reporting that as ok would prove the record right by ignoring the
// part of it that is damaged.
func Replay(runDir string, w io.Writer) (bool, error) {
	rec, err := loadRecord(runDir)
	if err != nil {
		return false, err
	}
	if rec.Account == "" {
		return false, fmt.Errorf("%w: %s", ErrNoAccount, runDir)
	}
	snapshot, err := LoadWorld(runDir)
	if err != nil {
		return false, err
	}
	events, torn, err := readLedger(filepath.Join(runDir, "run.ndjson"))
	if err != nil {
		return false, err
	}
	if torn != "" {
		// A report notes a torn tail; replay cannot. Those bytes were a write
		// that never completed, and they may have been the trigger row that made
		// the sets agree. Skipping them would pass a damaged record.
		return false, fmt.Errorf("replay %s: %s", runDir, torn)
	}

	recomputed := snapshot.Actions(rec.Account)
	recorded, err := recordedActions(events)
	if err != nil {
		return false, err
	}

	recordedSet := make(map[string]bool, len(recorded))
	for _, a := range recorded {
		recordedSet[a.TriggerID] = true
	}
	recomputedSet := make(map[string]bool, len(recomputed))
	for _, a := range recomputed {
		recomputedSet[a.TriggerID] = true
	}
	// Both inputs are already sorted by trigger id, so the diff is a diff and
	// not a reshuffle.
	var missing []string // recomputed, never recorded
	for _, a := range recomputed {
		if !recordedSet[a.TriggerID] {
			missing = append(missing, a.TriggerID)
		}
	}
	var extra []string // recorded, the world does not ask for it
	for _, a := range recorded {
		if !recomputedSet[a.TriggerID] {
			extra = append(extra, a.TriggerID)
		}
	}

	fmt.Fprintf(w, "run %s (%s %s)\n\n", rec.ID, rec.Kind, rec.Subject)
	lines := make([]string, 0, len(recomputed))
	for _, a := range recomputed {
		lines = append(lines, a.Kind+" "+a.TriggerID)
	}
	writeSection(w, "recomputed", lines)
	lines = lines[:0]
	for _, a := range recorded {
		lines = append(lines, a.TriggerID)
	}
	writeSection(w, "recorded", lines)
	writeSection(w, "missing", missing)
	writeSection(w, "extra", extra)
	if len(missing) == 0 && len(extra) == 0 {
		fmt.Fprintln(w, "replay: ok")
		return true, nil
	}
	fmt.Fprintln(w, "replay: differs")
	return false, nil
}

// writeSection prints one part of the diff: a heading with its count, then one
// indented line per action, or (none). Every section prints, including the empty
// ones, so the output says the set was compared rather than skipped.
func writeSection(w io.Writer, label string, ids []string) {
	fmt.Fprintf(w, "%s (%d):\n", label, len(ids))
	if len(ids) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, id := range ids {
		fmt.Fprintf(w, "  %s\n", id)
	}
}

// recordedAction is one trigger row: the event a write answered and the action
// text the run recorded beside it. The text is evidence a person reads, not a
// classification — replay compares trigger ids, because the id is what a spend
// makes single — so it is printed beside the recorded list, never compared.
type recordedAction struct {
	TriggerID string
	Action    string
}

// recordedActions returns the ledger's trigger rows, sorted by trigger id. Every
// other kind is skipped: the action set is what the run recorded spending, and a
// stage or a gate says the run did something, not what the thread owed.
//
// Two spends of one id are an error. Spend refuses the second write, so a file
// holding both was not written by this program, and collapsing them into one
// action would hide exactly the guard replay exists to check.
func recordedActions(events []Event) ([]recordedAction, error) {
	seen := make(map[string]bool, len(events))
	out := make([]recordedAction, 0, len(events))
	for _, e := range events {
		if e.Kind != kindTrigger {
			continue
		}
		var t Trigger
		if err := decodeData(e, &t); err != nil {
			return nil, err
		}
		if t.TriggerID == "" {
			return nil, fmt.Errorf("event %d (%s): the trigger names no event", e.Seq, e.Kind)
		}
		if seen[t.TriggerID] {
			return nil, fmt.Errorf("event %d: trigger %s is spent twice", e.Seq, t.TriggerID)
		}
		seen[t.TriggerID] = true
		out = append(out, recordedAction{TriggerID: t.TriggerID, Action: t.Action})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TriggerID < out[j].TriggerID })
	return out, nil
}
