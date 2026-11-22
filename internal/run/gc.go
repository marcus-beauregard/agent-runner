package run

// This file is the worktree tier of PRD §6.3's cleanup: the large thing a run
// leaves behind when its ending was not one that gave the checkout back itself.
// The environment tier sits with its own store; the record tier is the run
// directory minus what this file takes, and it keeps its own clock.
//
// The tier needs the record for three facts — the parent, the tree's path and
// the state that decides resumability — and git for the registration a deleted
// directory would otherwise leave pointing at nothing. It decides nothing about
// safety beyond the policy: age is the rule, and gc is the command a person runs
// after reading the dry run. The one line it does not cross is a run with no
// terminal state, which is resumable and stays (§6.3).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// WorktreeEntry is one run's checkout as the tier sees it: what it holds, when
// the run began, and whether the policy would take it now.
type WorktreeEntry struct {
	// ID is the run directory's name, which is the run's id.
	ID string `json:"id"`
	// State is run.json's state, so the report can say what the run ended as
	// beside the reason the checkout qualifies.
	State string `json:"state,omitempty"`
	// Dir is the run directory: what a caller hands back to collect it.
	Dir string `json:"-"`
	// Bytes is the checkout and the run's cache, the two directories this tier
	// removes.
	Bytes int64 `json:"bytes"`
	// Created is the record's own start time, the clock the policy counts from.
	Created time.Time `json:"created_at"`
	// Collect is true when the policy would take the checkout now.
	Collect bool `json:"collect"`
	// Reason is one sentence: why it qualifies, or why it stays.
	Reason string `json:"reason"`
}

// RunsRoot returns <base>/agent-runner/runs, where every run record lives. An
// empty base means the user's state directory, resolved exactly as New resolves
// it, so a caller holding a relative or empty base still finds the records the
// runs were written to.
func RunsRoot(base string) (string, error) {
	if base == "" {
		d, err := defaultBase()
		if err != nil {
			return "", err
		}
		base = d
	}
	root := filepath.Join(base, "agent-runner", "runs")
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return root, nil
}

// CollectWorktrees lists every run's checkout with the reason the policy takes
// or keeps it. It changes nothing: removing is the caller's step, so a dry run
// cannot be the run that deleted something.
//
// Age is counted from run.json's created_at, which is the only time the record
// carries. A run that took a fortnight to finish therefore looks as old as its
// start, which is the safe direction only in the sense that the caller sees the
// number: the report prints the age it decided on.
func CollectWorktrees(base string, age time.Duration, now time.Time) ([]WorktreeEntry, error) {
	root, err := RunsRoot(base)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read run records: %w", err)
	}
	entries := make([]WorktreeEntry, 0, len(ents))
	for _, de := range ents {
		// A record is a directory named by the run's id. Anything else under the
		// root is not one this tier can decide about.
		if !de.IsDir() {
			continue
		}
		e, err := inspectWorktree(filepath.Join(root, de.Name()), age, now)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, nil
}

// inspectWorktree describes one run directory and decides whether the age policy
// reaches its checkout. Every case it cannot account for is kept and says why:
// gc deletes what it can name, not what it found.
func inspectWorktree(runDir string, age time.Duration, now time.Time) (WorktreeEntry, error) {
	e := WorktreeEntry{ID: filepath.Base(runDir), Dir: runDir}
	rec, err := loadRecord(runDir)
	if err != nil {
		e.Reason = "no readable run.json, so a person decides"
		return e, nil
	}
	e.State = rec.State
	if created, err := time.Parse(time.RFC3339, rec.CreatedAt); err == nil {
		e.Created = created.UTC()
	}
	// A record written before Worktree existed names no path, and its tree is at
	// the fixed location. The cache is always there, inside the run.
	wt := rec.Worktree
	if wt == "" {
		wt = filepath.Join(runDir, "worktree")
	}
	treeBytes, err := dirBytes(wt)
	if err != nil {
		return WorktreeEntry{}, fmt.Errorf("measure %s: %w", e.ID, err)
	}
	cacheBytes, err := dirBytes(filepath.Join(runDir, "cache"))
	if err != nil {
		return WorktreeEntry{}, fmt.Errorf("measure %s: %w", e.ID, err)
	}
	e.Bytes = treeBytes + cacheBytes

	tree, err := exists(wt)
	if err != nil {
		return WorktreeEntry{}, fmt.Errorf("stat %s: %w", wt, err)
	}
	switch {
	case !tree && cacheBytes == 0:
		e.Reason = "no checkout left to collect"
		return e, nil
	case rec.State == stateRunning:
		// Resumable, whatever its age: the tree is what a resume edits.
		e.Reason = "no terminal state, so a resume may still need it"
		return e, nil
	case e.Created.IsZero():
		e.Reason = "run.json names no start time, so a person decides"
		return e, nil
	case rec.Repo == "":
		// Without the parent the registration cannot be proven gone, and this
		// tier's promise is a parent that no longer names a deleted tree. It is
		// kept even when the tree is already gone: the cache is small, and a
		// record this tier cannot account for is not one it should empty.
		e.Reason = "run.json names no repo, so a person decides"
		return e, nil
	}
	unused := now.Sub(e.Created)
	if unused < age {
		e.Reason = "under the " + humanAge(age) + " age"
		return e, nil
	}
	e.Collect = true
	e.Reason = fmt.Sprintf("state %s, untouched for %s", rec.State, humanAge(unused))
	return e, nil
}

// RemoveWorktreeEntry collects one run's checkout, returning the bytes the
// listing said it held. The tree goes through RemoveWorktree, which force-removes
// and then proves the parent no longer names it: gc is the caller §6.3 allows to
// discard a tree with uncommitted work, which a run's own cleanup may not do.
//
// The record stays. This tier is the checkout and the cache, never the history:
// run.json, the stream and the stages are what the next reader needs.
func RemoveWorktreeEntry(e WorktreeEntry) (int64, error) {
	if err := RemoveWorktree(e.Dir); err != nil {
		return 0, err
	}
	if err := os.RemoveAll(filepath.Join(e.Dir, "cache")); err != nil {
		return 0, fmt.Errorf("remove cache: %w", err)
	}
	return e.Bytes, nil
}

// PruneOrphans finds registrations that point into a run directory that is gone
// and, with apply, drops them by running git worktree prune in each parent a
// surviving record names. It returns how many it found.
//
// The search starts from the parents the records name, so an orphan in a parent
// no surviving record mentions is out of reach: the record is the only place the
// parent is written down, and a run directory that is gone has no record left to
// ask. That is the price of not keeping a second index of every checkout the
// machine ever made.
//
// A parent git cannot read has nothing this command can prune, so it is skipped
// rather than failing the pass over every other run.
func PruneOrphans(base string, apply bool) (int, error) {
	root, err := RunsRoot(base)
	if err != nil {
		return 0, err
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read run records: %w", err)
	}
	present := make(map[string]bool, len(ents))
	parents := make(map[string]bool)
	for _, de := range ents {
		if !de.IsDir() {
			continue
		}
		present[de.Name()] = true
		if rec, err := loadRecord(filepath.Join(root, de.Name())); err == nil && rec.Repo != "" {
			parents[rec.Repo] = true
		}
	}
	named := make([]string, 0, len(parents))
	for p := range parents {
		named = append(named, p)
	}
	// The order is the parents', not the map's, so a dry run and the pass after
	// it print the same report.
	sort.Strings(named)

	found := 0
	for _, parent := range named {
		list, err := gitRun(parent, "worktree", "list", "--porcelain")
		if err != nil {
			continue
		}
		orphan := false
		for _, line := range strings.Split(list, "\n") {
			path, ok := strings.CutPrefix(line, "worktree ")
			if !ok {
				continue
			}
			if id, ok := runIDUnder(root, path); ok && !present[id] {
				orphan = true
				found++
			}
		}
		if orphan && apply {
			// Prune asks git to drop every stale registration it can name, which
			// is wider than ours — a tree deleted outside this program is stale
			// too, and its parent should stop naming it.
			_, _ = gitRun(parent, "worktree", "prune")
		}
	}
	return found, nil
}

// runIDUnder reports the run id a worktree registration points into, when the
// path is <runsRoot>/<id>/worktree and nothing else. Both sides are resolved so
// a run directory behind a symlink — $HOME is one on plenty of boxes — is
// recognised as the same path git printed.
func runIDUnder(root, path string) (string, bool) {
	rel, err := filepath.Rel(resolvePath(root), resolvePath(path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || parts[1] != "worktree" || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

// dirBytes is what a directory holds, regular files only: the number is for the
// disk question, so a symlink is not followed out of the run and a device file
// is not counted as the space it might occupy. A directory that is not there is
// zero bytes, not an error — a checkout already removed still has a cache to
// count, and the cache is often all that is left.
func dirBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && path == dir {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// humanAge is the coarseness a cleanup report needs: a person checks "41 days"
// against a policy, and nobody reads seconds in that sentence.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
