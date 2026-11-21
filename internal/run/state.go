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

// State is what the last finished run recorded about its subject: the digest of
// the world it answered, and when it answered it. Nothing else is kept, because
// nothing else is needed to decide whether the next run has anything to do.
type State struct {
	Digest    string    `json:"digest"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ErrNoState reports that a subject has no recorded digest: it has never been
// run, or its state file was removed. It is not a failure — the first run is
// always real work — so callers test for it with errors.Is and carry on.
var ErrNoState = errors.New("no run state")

// LoadState reads the last recorded digest for subject. State lives at
// <base>/agent-runner/state/<sha256(subject)>.json, alongside runs, so one base
// moves both; it is keyed by the subject URL so two threads never share a
// digest. A subject that changes number or repository therefore reads as new
// rather than falsely unchanged.
func LoadState(base, subject string) (State, error) {
	path, err := statePath(base, subject)
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
	return s, nil
}

// SaveState writes s as the last finished run's state for subject. The write is
// atomic — temp file in the state directory, synced, renamed over the target —
// so a crash leaves either the old digest or the new one, never half a file the
// next run reads as a digest.
func SaveState(base, subject string, s State) error {
	if s.Digest == "" {
		return fmt.Errorf("save state %s: empty digest", subject)
	}
	path, err := statePath(base, subject)
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
func Unchanged(base, subject, digest string) (bool, error) {
	s, err := LoadState(base, subject)
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
// they differ, or when there is no state at all, it creates the run directory
// and returns it; the caller records the digest with Remember once the run has
// finished.
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
	same, err := Unchanged(base, meta.Subject, digest)
	if err != nil {
		return Start{}, err
	}
	s := Start{Base: base, Subject: meta.Subject, Digest: digest, Unchanged: same}
	if same {
		return s, nil
	}
	r, err := New(base, meta)
	if err != nil {
		return Start{}, err
	}
	s.Run = r
	return s, nil
}

// Remember records the digest this start decided on, so that the next run over
// the same world finds it unchanged. It is a no-op for an unchanged start: that
// run did nothing, and the digest on file is already the right one.
func (s Start) Remember() error {
	if s.Run == nil {
		return nil
	}
	return SaveState(s.Base, s.Subject, State{Digest: s.Digest, UpdatedAt: time.Now().UTC()})
}

// statePath is where one subject's state lives: <base>/agent-runner/state/, the
// digest of the subject URL as the file name.
func statePath(base, subject string) (string, error) {
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
	sum := sha256.Sum256([]byte(subject))
	name := hex.EncodeToString(sum[:]) + ".json"
	return filepath.Join(base, "agent-runner", "state", name), nil
}
