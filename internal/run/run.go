// Package run creates the durable record of a single run: the directory tree
// every later stage writes into, and the run.json that describes it.
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

// Meta is what the caller knows about a run before it starts. Every field is
// recorded verbatim; none of it is interpreted here.
type Meta struct {
	Kind    string // "build", "resolve" or "amend"
	Subject string // the issue or pull request URL
	Repo    string // local checkout path
	Branch  string
	BaseSHA string
	Budget  string // e.g. "--budget 45m"
}

// Run is an open run directory.
type Run struct {
	ID  string
	Dir string
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
	Budget    string `json:"budget"`
	State     string `json:"state"`
	ExitCode  *int   `json:"exit_code"`
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
		State:     stateRunning,
	}
	if err := writeRecord(dir, rec); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &Run{ID: id, Dir: dir}, nil
}

// Finish writes the run's terminal outcome: the state its exit code stands for,
// and the code itself. It runs once the result is known and before the process
// exits, on every path that created a record — including a refusal, which is a
// finished run and not an unfinished one. Without it a record says running for a
// process that has already exited, which is state that lies.
//
// A run killed before Finish keeps the running state it was created with: that
// is correct, because it was never finished and remains resumable.
func (r *Run) Finish(code int) error {
	rec, err := loadRecord(r.Dir)
	if err != nil {
		return err
	}
	rec.State = terminalState(code)
	rec.ExitCode = &code
	return writeRecord(r.Dir, *rec)
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
