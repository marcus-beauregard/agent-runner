package run

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ErrAlreadySpent reports a trigger id that already has a spend in the ledger.
// The event has been answered once; a retry, a replay or a later scheduled run
// that finds this error must do nothing and say so (PRD §8).
var ErrAlreadySpent = errors.New("trigger already spent")

// kindTrigger is the event kind a spend is written under.
const kindTrigger = "trigger"

var (
	// errNoTriggerID reports a spend or a lookup that names no event. An empty
	// id would match the first spend ever written, so it is refused rather than
	// treated as a name.
	errNoTriggerID = errors.New("trigger: empty id")
	// errNoAction reports a spend that does not say what it did.
	errNoAction = errors.New("trigger: empty action")
)

// Trigger is the payload of a trigger event: which event the action answers,
// what the action was, and when it was taken. The id is derived from the World
// event, never generated, so two processes reading the same event compute the
// same id.
type Trigger struct {
	TriggerID string    `json:"trigger_id"`
	Action    string    `json:"action"`
	At        time.Time `json:"at"`
}

// Spend records one action against the event it answers. The first spend of a
// trigger id succeeds; every later one returns ErrAlreadySpent without writing.
// The lookup and the append happen under the ledger mutex, so two racing
// callers cannot both believe they are first.
func (l *Ledger) Spend(triggerID, action string) error {
	if triggerID == "" {
		return errNoTriggerID
	}
	if action == "" {
		return errNoAction
	}
	raw, err := json.Marshal(Trigger{TriggerID: triggerID, Action: action, At: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("encode trigger: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		return ErrLedgerClosed
	}
	spent, err := spentIn(l.path, triggerID)
	if err != nil {
		return err
	}
	if spent {
		return ErrAlreadySpent
	}
	return l.appendLocked(kindTrigger, "", raw)
}

// Spent reports whether triggerID has been spent. It reads the ledger file on
// every call, so the answer holds across a restart with nothing kept in memory.
// No ledger is no history: a missing file is false and no error.
func (l *Ledger) Spent(triggerID string) (bool, error) {
	if triggerID == "" {
		return false, errNoTriggerID
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return spentIn(l.path, triggerID)
}

// spentIn scans path for a spend of triggerID. It is the shared body of Spent
// and of the check inside Spend, which holds the mutex over both.
//
// The scan is linear and every spend reads the whole file. A run's ledger is
// small — one line per step — and being provably right across a restart is
// worth more here than an index that could disagree with the file.
//
// A line that is not a trigger event is skipped, so other kinds can share the
// stream. A complete line that does not parse is an error rather than a skip:
// an unreadable line may have held the spend, and calling the id unspent would
// answer the same event twice. Only the unterminated fragment after the last
// newline is ignored, exactly as the ledger's own reader ignores it — that
// write never completed, so nothing was spent.
func spentIn(path, triggerID string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read ledger %s: %w", path, err)
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for n := 1; ; n++ {
		line, readErr := r.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return false, fmt.Errorf("read ledger %s: %w", path, readErr)
		}
		if line == "" {
			return false, nil
		}
		if !strings.HasSuffix(line, "\n") {
			return false, nil
		}
		var e Event
		if err := json.Unmarshal([]byte(strings.TrimSuffix(line, "\n")), &e); err != nil {
			return false, fmt.Errorf("read ledger %s: line %d: %w", path, n, err)
		}
		if e.Kind != kindTrigger {
			continue
		}
		var t Trigger
		if err := json.Unmarshal(e.Data, &t); err != nil {
			return false, fmt.Errorf("read ledger %s: line %d: %w", path, n, err)
		}
		if t.TriggerID == triggerID {
			return true, nil
		}
	}
}
