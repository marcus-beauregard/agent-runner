// Package run creates the durable record of a single run: the directory tree
// every later stage writes into, the run.json that describes it, and the
// append-only event stream it appends to as it goes.
//
// The record also says what the run answered. Every outbound action spends a
// trigger, named after the event it answers — review:<id>:changes_requested,
// comment:<id>, or check:<name>:failure — and a trigger id is spent at most
// once, so a retry or a later scheduled run finds it spent and does nothing.
package run

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Alias, because worktree.go already has a helper named refuse: this
	// package needs the vocabulary, not another way to spell an error.
	refusal "github.com/marcus-beauregard/agent-runner/internal/refuse"
)

// ErrNoRecord reports a directory that holds no usable run.json: the file is
// missing, empty, or not a record. A crash between the mkdir and the atomic
// rename leaves exactly that, and it is not state.
var ErrNoRecord = errors.New("no run record")

// stateRunning is what New writes. A record that still says running may belong
// to a live process or to one that died between the mkdir and Finish, and the
// difference is not knowable from the file: both are resumable (PRD §5), and gc
// collects neither (§6.3).
const stateRunning = "running"

// The terminal states Finish writes. A crash and a considered stop must never
// look the same in the record (PRD §10), so every exit the binary can return has
// a name here rather than an anonymous failure.
const (
	stateDone       = "done"        // exit 0
	stateRefused    = "refused"     // exit 3
	stateNeedsHuman = "needs_human" // exit 4
	stateFailed     = "failed"      // gate failed (2) or anything unexpected (1)
)

// Meta is what the caller knows about a run before it starts, recorded as
// given. Worktree later replaces Repo, Branch and BaseSHA with the values it
// resolved — the parent it actually ran git in, and the branch it created.
type Meta struct {
	Kind    string // "build", "resolve" or "amend"
	Subject string // the issue or pull request URL
	Repo    string // local checkout path
	Branch  string
	BaseSHA string
	Budget  string // e.g. "--budget 45m"
	// Sandbox is the isolation the run was granted: "container" or "worktree".
	// It is what the run was, not what it asked for, and is empty only when the
	// run never got a mode — a refusal decided before any stage ran.
	Sandbox string
	// Account is the authenticated account the run acted as. Replay recomputes
	// the plan with the same classifier the command used, and that classifier
	// cannot tell the run's own reply from a question without it (PRD §8).
	Account string
}

// Run is an open run directory.
type Run struct {
	ID  string
	Dir string
	// stream is the run's event stream when a caller attached one. Refuse
	// appends to it, so a refusal sits in the same file as the events that led
	// to it rather than in run.json alone.
	stream *Ledger
}

// record is run.json. The field order is the file's field order, fixed so that
// two runs which did the same thing produce the same bytes.
type record struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Kind      string `json:"kind"`
	Subject   string `json:"subject"`
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	BaseSHA   string `json:"base_sha"`
	// Worktree is the checkout this run was given, omitted when it never had
	// one. The record carries it because the record is written before the
	// stream: a run killed between the two still names its tree here, so
	// cleanup finds an orphan the event alone would have hidden.
	Worktree string `json:"worktree,omitempty"`
	Budget   string `json:"budget"`
	// Sandbox is the isolation the run got, omitted when it never got one. An
	// absent field stays distinguishable from worktree: a record written before
	// this field existed must not read as a run that chose the weaker mode, and
	// a refused run that never reached a mode has nothing to name here.
	Sandbox string `json:"sandbox,omitempty"`
	// Account is omitted when empty, which is what a run recorded before this
	// field existed looks like. Replay refuses that rather than guessing it.
	Account  string `json:"account,omitempty"`
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code"`
	// Refusal is why the run stopped, omitted when it did not refuse. The state
	// says refused; this says for what, and the report prints it. A record
	// written before this section existed still reads, one without it.
	Refusal *refusalRecord `json:"refusal,omitempty"`
}

// refusalRecord is the refusal section of run.json: the code from the closed
// vocabulary, the stage that raised it, the one sentence a person reads, and
// the evidence the sentence can be checked against.
type refusalRecord struct {
	Code     string            `json:"code"`
	Where    string            `json:"where,omitempty"`
	Reason   string            `json:"reason"`
	Evidence map[string]string `json:"evidence,omitempty"`
}

// New creates <base>/agent-runner/runs/<id>/ with its stages/ subdirectory and
// writes run.json inside it. An empty base means the user's state directory:
// $XDG_STATE_HOME, else ~/.local/state. The run directory is 0700, and an id
// that already exists fails rather than being reused.
func New(base string, meta Meta) (*Run, error) {
	if base == "" {
		d, err := defaultBase()
		if err != nil {
			return nil, err
		}
		base = d
	}
	root := filepath.Join(base, "agent-runner", "runs")
	if err := ensureDir(root); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	id, err := generateID(now)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, id)
	// The directory is a location, and every path derived from it is resolved
	// by a different process with a different working directory: git runs in
	// the parent, the guard in the caller's. An absolute path is the only
	// spelling they all agree on, so the run owns one from the start.
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("run %s already exists: %w", id, err)
		}
		return nil, fmt.Errorf("create run directory: %w", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "stages"), 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("create stages directory: %w", err)
	}
	rec := record{
		ID:        id,
		CreatedAt: now.Format(time.RFC3339),
		Kind:      meta.Kind,
		Subject:   meta.Subject,
		Repo:      meta.Repo,
		Branch:    meta.Branch,
		BaseSHA:   meta.BaseSHA,
		Budget:    meta.Budget,
		Sandbox:   meta.Sandbox,
		Account:   meta.Account,
		State:     stateRunning,
	}
	if err := writeRecord(dir, rec); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &Run{ID: id, Dir: dir}, nil
}

// ErrEventLost reports a refusal the record holds whose event could not be
// appended. The run is refused all the same: only the line a reader following
// the stream sees is missing, and a caller that stopped here would leave the
// record saying running for a process that has already exited — the state
// Finish exists to prevent.
var ErrEventLost = errors.New("refusal recorded, event lost")

// Refuse records why the run stopped, before Finish writes its outcome. The
// code is checked against the vocabulary: a record is the evidence a person or
// a later run reads, and an unknown code in it would be a reason nobody can
// name. Calling it after Finish is a mistake — Finish rewrites the whole record
// and a refusal written after it would be a record of a run that already ended.
//
// The record is written before the event, so a failure after that point still
// leaves a refusal someone can read. Such a failure is reported as ErrEventLost
// rather than as a plain error: the refusal is recorded, and the caller's next
// step is still Finish.
func (r *Run) Refuse(ref *refusal.Refusal) error {
	if ref == nil {
		return errors.New("refuse: nil refusal")
	}
	if !refusal.Valid(ref.Code) {
		return fmt.Errorf("refuse: unknown code %q", ref.Code)
	}
	rec, err := loadRecord(r.Dir)
	if err != nil {
		return err
	}
	rec.Refusal = &refusalRecord{
		Code:     string(ref.Code),
		Where:    ref.Where,
		Reason:   ref.Reason,
		Evidence: ref.Evidence,
	}
	if err := writeRecord(r.Dir, *rec); err != nil {
		return err
	}
	// The stream carries the same refusal, so a reader following the run's
	// events sees where it stopped without opening run.json. It is appended
	// after the record: an event about a refusal the record does not hold would
	// be the only account of it, and the record is the one that counts.
	if r.stream != nil {
		if err := r.stream.Refusal(ref); err != nil {
			return fmt.Errorf("%w: %v", ErrEventLost, err)
		}
	}
	return nil
}

// SetLedger attaches the run's event stream, which Refuse appends its event to.
// A run with no stream records a refusal in run.json alone, which is all a
// caller that never opened one can offer.
func (r *Run) SetLedger(l *Ledger) { r.stream = l }

// Finish writes the run's terminal outcome: the state its exit code stands for,
// and the code itself. It runs once the result is known and before the process
// exits, on every path that created a record — including a refusal, which is a
// finished run and not an unfinished one. Without it a record says running for a
// process that has already exited, which is state that lies.
//
// A run killed before Finish keeps the running state it was created with: that
// is correct, because it was never finished and remains resumable, and its
// worktree is left exactly as the resume needs it.
//
// Finish then removes the run's worktree when one was recorded and the work in
// it is settled (settledWork). A cleanup that fails rewrites the record as
// failed: the record must not say done over a parent that still names a tree
// nobody owns.
func (r *Run) Finish(code int) error {
	rec, err := loadRecord(r.Dir)
	if err != nil {
		return err
	}
	rec.State = terminalState(code)
	rec.ExitCode = &code
	if err := writeRecord(r.Dir, *rec); err != nil {
		return err
	}
	// Cleanup is a step, not a background hope (PRD §6.3), but only the codes
	// whose work is settled take the tree with them. A run with no terminal
	// state never reaches Finish, so a resume still finds its work in progress;
	// a run that stopped with edits in hand keeps them for the person or the gc.
	if settledWork(code) {
		if err := CleanupWorktree(r.Dir, code); err != nil {
			// The run is not the state it just claimed: a parent still naming a
			// tree nobody owns is a failure whatever the work itself achieved.
			// Record that so run.json and the exit code agree, then say why.
			failed := 1
			rec.State, rec.ExitCode = stateFailed, &failed
			if werr := writeRecord(r.Dir, *rec); werr != nil {
				return fmt.Errorf("cleanup worktree: %w (recording the failure also failed: %v)", err, werr)
			}
			return fmt.Errorf("cleanup worktree: %w", err)
		}
	}
	return nil
}

// settledWork reports whether a terminal exit code leaves a checkout that is
// only scratch. 0 finished with its work on the branch, so the checkout is
// regenerable from it and cleanup can delete it.
//
// 1, 2, 3 and 4 are not settled. The gate runs before the commit stage, so the
// work a run had refused (2) never reached the branch: that tree holds the only
// copy of the attempted change and of why it was refused, exactly as it does
// for needs_human (4), which exists to hand a tree with uncommitted work to a
// person. A failure (1) or a refusal (3) can stop holding the same edits. A
// --force removal there deletes the only copy. Those trees are collected by
// age in gc; PRD §6.3 gives only a settled run the right to remove its own.
func settledWork(code int) bool {
	switch code {
	case 0:
		return true
	default:
		return false
	}
}

// terminalState names the outcome an exit code stands for. Budget exhaustion is
// not here yet: report.ExitCode does not return 5 until the budget work lands,
// and inventing the state before the code that produces it would be the same
// mistake in reverse.
func terminalState(code int) string {
	switch code {
	case 0:
		return stateDone
	case 3:
		return stateRefused
	case 4:
		return stateNeedsHuman
	default:
		return stateFailed
	}
}

// Load reads the run recorded in dir. A missing, empty or unparseable run.json
// is ErrNoRecord: the directory exists but carries no state to resume from.
func Load(dir string) (*Run, error) {
	rec, err := loadRecord(dir)
	if err != nil {
		return nil, err
	}
	return &Run{ID: rec.ID, Dir: dir}, nil
}

// loadRecord reads and validates run.json, keeping every field: Finish rewrites
// the whole record, so it must not be built from the two fields a caller sees.
func loadRecord(dir string) (*record, error) {
	b, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoRecord, dir)
		}
		return nil, fmt.Errorf("read run.json: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoRecord, dir)
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrNoRecord, dir, err)
	}
	if rec.ID == "" {
		return nil, fmt.Errorf("%w: %s", ErrNoRecord, dir)
	}
	return &rec, nil
}

// defaultBase resolves the state directory the record lives under.
func defaultBase() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("XDG_STATE_HOME is not absolute: %s", d)
		}
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate state directory: %w", err)
	}
	return filepath.Join(home, ".local", "state"), nil
}

// StateBase returns the directory run records live under, resolved exactly as
// New resolves a base it was not given. A caller that has to exclude this
// program's own bookkeeping — the host audit does, or it flags every run it
// writes — asks here rather than repeating the XDG_STATE_HOME rule: two copies
// of that rule drift, and the exclusion then quietly covers nothing.
func StateBase() (string, error) { return defaultBase() }

// generateID is newID behind a variable so a test can force the collision New
// refuses; nothing in the program reassigns it.
var generateID = newID

// newID returns an id that sorts newest-last: UTC to the microsecond, then six
// random hex characters so two runs in the same instant still differ. Second
// precision is not enough — two runs started in the same second would sort by
// their random suffix rather than by when they started.
func newID(now time.Time) (string, error) {
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return now.UTC().Format("20060102-150405.000000") + "-" + hex.EncodeToString(suffix[:]), nil
}

// ensureDir creates path with 0700 and tightens an existing directory whose
// permissions are looser. A directory it cannot make private is an error, not
// a warning: the record holds paths and reasoning.
func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("tighten %s: %w", path, err)
	}
	fi, err = os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is %04o, want 0700", path, perm)
	}
	return nil
}

// writeRecord writes run.json atomically: a temp file in the same directory,
// synced, renamed over the target, then the directory synced. A crash or a
// power loss leaves either no run.json or a complete one, never half a file a
// later run mistakes for a record.
func writeRecord(dir string, rec record) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run.json: %w", err)
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(dir, ".run.json-")
	if err != nil {
		return fmt.Errorf("create run.json: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("write run.json: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync run.json: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close run.json: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "run.json")); err != nil {
		return fmt.Errorf("publish run.json: %w", err)
	}
	tmp = ""
	// Sync the directory too: the rename is durable only once the entry itself
	// is on disk, not just the bytes the entry points at.
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open run directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync run directory: %w", err)
	}
	return nil
}
