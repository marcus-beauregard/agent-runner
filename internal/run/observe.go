package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/marcus-beauregard/agent-runner/internal/world"
)

// worldName is the snapshot's name inside a run directory. It sits beside
// run.json rather than in stages/, because it is not a stage's output: it is
// the input the whole run answered.
const worldName = "world.json"

// ErrNoWorld reports a run directory with no usable snapshot: the file is
// missing, empty, unparsable, or not a World. A run recorded before snapshots
// existed has no world.json, and that is not the same thing as a World with
// nothing in it — replay has to refuse it, not reason about an empty thread.
var ErrNoWorld = errors.New("no world snapshot")

// SaveWorld writes w as <runDir>/world.json, atomically and 0600, so a replay
// can recompute the decision without a network or a GitHub account.
//
// A World with no subject or no issue number is refused rather than written: it
// is not a thread, and a snapshot of nothing would let replay compare against
// nothing and pass.
func SaveWorld(runDir string, w world.World) error {
	if runDir == "" {
		return errors.New("save world: empty run directory")
	}
	if w.Subject == "" || w.Issue.Number <= 0 {
		return fmt.Errorf("save world: incomplete world (%q, issue %d)", w.Subject, w.Issue.Number)
	}
	b, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return fmt.Errorf("encode world.json: %w", err)
	}
	b = append(b, '\n')
	path := filepath.Join(runDir, worldName)
	f, err := os.CreateTemp(runDir, ".world.json-")
	if err != nil {
		return fmt.Errorf("create world.json: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("write world.json: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync world.json: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close world.json: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("publish world.json: %w", err)
	}
	tmp = ""
	d, err := os.Open(runDir)
	if err != nil {
		return fmt.Errorf("open run directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync run directory: %w", err)
	}
	return nil
}

// LoadWorld reads <runDir>/world.json. Everything unusable is ErrNoWorld: a
// replay that cannot read the observation must say so, and the caller can tell
// that from a failure while replaying a World it did read.
func LoadWorld(runDir string) (world.World, error) {
	b, err := os.ReadFile(filepath.Join(runDir, worldName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return world.World{}, fmt.Errorf("%w: %s", ErrNoWorld, runDir)
		}
		return world.World{}, fmt.Errorf("read world.json: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return world.World{}, fmt.Errorf("%w: %s: empty file", ErrNoWorld, runDir)
	}
	var w world.World
	if err := json.Unmarshal(b, &w); err != nil {
		return world.World{}, fmt.Errorf("%w: %s: %v", ErrNoWorld, runDir, err)
	}
	if w.Subject == "" || w.Issue.Number <= 0 {
		return world.World{}, fmt.Errorf("%w: %s: no subject or issue number", ErrNoWorld, runDir)
	}
	return w, nil
}
