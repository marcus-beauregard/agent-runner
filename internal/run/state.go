package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marcus-beauregard/agent-runner/internal/world"
)

// State is what the last finished run recorded about one subject and one kind:
// the digest of the world it answered, and when it answered it. Nothing else is
// kept, because nothing else is needed to decide whether the next run has
// anything to do.
//
// Kind and Subject are in the file as well as in the key, so a state file says
// what it is — which verb, which thread — without anyone having to guess it from
// a hash. That is what a migration or a person asking "what is this file" needs.
type State struct {
	Kind      string    `json:"kind"`
	Subject   string    `json:"subject"`
	Digest    string    `json:"digest"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ErrNoState reports that a subject has no recorded digest: it has never been
// run, or its state file was removed. It is not a failure — the first run is
// always real work — so callers test for it with errors.Is and carry on.
var ErrNoState = errors.New("no run state")

// LoadState reads the last recorded digest of one kind on subject. State lives at
// <base>/agent-runner/state/<sha256(kind, subject)>.json, alongside runs, so one
// base moves both. The key is the pair: build, resolve and amend all take an
// issue or a pull request URL and answer different questions, so one kind's
// digest must never silence another on the same thread. A subject that changes
// number therefore reads as new rather than falsely unchanged.
func LoadState(base, kind, subject string) (State, error) {
	path, err := statePath(base, kind, subject)
	if err != nil {
		return State{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, fmt.Errorf("%w: %s", ErrNoState, subject)
		}
		return State{}, fmt.Errorf("read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		// A file nobody can read is not "nothing changed": reporting it as such
		// would turn a damaged ledger into a silent no-op for every later run.
		return State{}, fmt.Errorf("parse state %s: %w", path, err)
	}
	if s.Digest == "" {
		return State{}, fmt.Errorf("parse state %s: empty digest", path)
	}
	if s.Kind != kind {
		return State{}, fmt.Errorf("parse state %s: kind %q, want %q", path, s.Kind, kind)
	}
	if s.Subject != subject {
		return State{}, fmt.Errorf("parse state %s: subject %q, want %q", path, s.Subject, subject)
	}
	return s, nil
}

// SaveState writes s as the last finished run's state for subject. The write is
// atomic — temp file in the state directory, synced, renamed over the target —
// so a crash leaves either the old digest or the new one, never half a file the
// next run reads as a digest.
func SaveState(base, kind, subject string, s State) error {
	if s.Digest == "" {
		return fmt.Errorf("save state %s: empty digest", subject)
	}
	s.Kind = kind
	s.Subject = subject
	path, err := statePath(base, kind, subject)
	if err != nil {
		return err
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".state-")
	if err != nil {
		return fmt.Errorf("create state: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("publish state: %w", err)
	}
	tmp = ""
	return nil
}

// Unchanged reports whether digest is the one the last finished run recorded for
// subject. A subject with no state is changed, because the first run is always
// real work. An unreadable state file is an error: proceeding quietly on a
// ledger nobody can parse would hide the damage until someone needed the record.
func Unchanged(base, kind, subject, digest string) (bool, error) {
	s, err := LoadState(base, kind, subject)
	if err != nil {
		if errors.Is(err, ErrNoState) {
			return false, nil
		}
		return false, err
	}
	return s.Digest == digest, nil
}

// FetchFunc reads one thread into a World. It is world.Fetch in the program and
// a fixture in a test; taking it as a parameter is what lets the fast path be
// exercised without a network or a GitHub account.
type FetchFunc func(ctx context.Context, ref string) (world.World, error)

// Start is the answer the fast path gives: either a run directory to do the work
// in, or the statement that the world has not moved since the last run.
type Start struct {
	Base      string
	Kind      string
	Subject   string
	Digest    string // the digest this decision was made on
	Unchanged bool
	Run       *Run // nil when Unchanged
}

// Begin is the top of every command's run. It fetches the subject's world,
// digests it, and compares that digest with the one the last finished run
// recorded.
//
// When the two match it creates nothing — no run directory, no state write, no
// model call — and the caller reports the unchanged status and exits 0. When
// they differ, or when there is no state at all, it creates the run directory,
// snapshots the World it decided on inside it, and returns it; the caller
// records the digest with Remember once the run has finished.
//
// The comparison is the only thing a command is allowed to decide "whether" on
// (PRD §12): no word in the thread is weighed here, only whether it changed.
func Begin(ctx context.Context, base string, meta Meta, fetch FetchFunc, excludeAuthor string) (Start, error) {
	w, err := fetch(ctx, meta.Subject)
	if err != nil {
		return Start{}, fmt.Errorf("read world: %w", err)
	}
	digest, err := w.Digest(excludeAuthor)
	if err != nil {
		return Start{}, err
	}
	same, err := Unchanged(base, meta.Kind, meta.Subject, digest)
	if err != nil {
		return Start{}, err
	}
	s := Start{Base: base, Kind: meta.Kind, Subject: meta.Subject, Digest: digest, Unchanged: same}
	if same {
		return s, nil
	}
	// The record says which account it excluded, because replay recomputes the
	// action set with the same classifier and a run's own reply reads as a
	// question the thread still owes without it (§8).
	meta.Account = excludeAuthor
	r, err := New(base, meta)
	if err != nil {
		return Start{}, err
	}
	// Snapshot before returning, so no stage can record work against a
	// decision nobody kept. If the snapshot cannot be written the run is a
	// failure and the freshly created directory goes with it: a record that
	// says running, with no World in it, is worse than no record at all.
	if err := SaveWorld(r.Dir, w); err != nil {
		os.RemoveAll(r.Dir)
		return Start{}, fmt.Errorf("snapshot world: %w", err)
	}
	s.Run = r
	return s, nil
}

// Remember records the digest this start decided on, so that the next run over
// the same world finds it unchanged. It is a no-op for an unchanged start: that
// run did nothing, and the digest on file is already the right one.
//
// It is also a no-op for a run that did not finish done. A refused run, a failed
// gate and a needs_human stop all leave work the thread is still owed; recording
// their digest would let every later run exit 0 over a question nobody answered,
// and the trigger ledger cannot catch it because such a run never reaches a
// spend. Exit 0 is the one code that means the work was done — terminalState says
// the same thing in the record.
func (s Start) Remember(code int) error {
	if s.Run == nil || code != 0 {
		return nil
	}
	return SaveState(s.Base, s.Kind, s.Subject, State{Digest: s.Digest, UpdatedAt: time.Now().UTC()})
}

// statePath is where one kind's state for one subject lives:
// <base>/agent-runner/state/, the digest of the kind and the subject URL as the
// file name. The two are hashed together with a separator so no pair of values
// can spell another pair.
func statePath(base, kind, subject string) (string, error) {
	if kind == "" {
		return "", errors.New("state kind is empty")
	}
	if subject == "" {
		return "", errors.New("state subject is empty")
	}
	if base == "" {
		d, err := defaultBase()
		if err != nil {
			return "", err
		}
		base = d
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + subject))
	name := hex.EncodeToString(sum[:]) + ".json"
	return filepath.Join(base, "agent-runner", "state", name), nil
}
