// Package audit answers one question about a run: is the host still as the run
// found it? PRD §6.2 puts the rule about the machine, not the checkout, and gives
// the audit the job of checking it before the run reports anything.
//
// It is a tripwire, not an integrity monitor: each watchlist root is walked one
// level deep, recording file sizes and, for directories, the names of their
// children. A file added or removed at that depth is a name that moved in its
// parent; a file edited in place is a changed size only when it sits directly in
// a root. A file one level down carries a name and no size, so editing
// ~/.config/app.conf in place is invisible — that is the depth this tripwire
// watches, not a claim that it can see every write. Content hashes, inodes and
// writes reverted before the second snapshot are out of scope; the container
// sandbox is the other net (§6.2).
package audit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// maxEntries bounds one snapshot. A root larger than this needs narrowing, not
// walking: the audit must cost less than the run it guards.
const maxEntries = 20000

// Entry is what the audit remembers about one path. Directories carry their
// children's names so a file created one level down is named in the drift, not
// merely noticed through its parent. Files carry size and mtime, because a
// rewrite that keeps the size still moves the clock.
type Entry struct {
	Size    int64
	ModTime time.Time
	IsDir   bool
	Names   []string
}

// Snapshot maps an absolute path to what it looked like. Plain data, so Compare
// can be a pure function of two of them.
type Snapshot map[string]Entry

// Ignore is a path prefix the audit does not look at, with the reason a write
// there is not an escape. Every default carries its reason: an unexplained
// exclusion is a hole with the same shape as no audit.
type Ignore struct {
	Prefix string
	Why    string
}

// Drift is one path that changed outside the run directory. Kind is "added",
// "removed" or "changed"; Detail says what moved, and is empty for a name that
// simply appeared or went.
type Drift struct {
	Path   string
	Kind   string
	Detail string
}

// Watchlist is the roots a build run watches: home, the system configuration,
// the global bin directory, the repository the run was given, and wherever Go
// installs binaries. Empty and duplicate entries are dropped.
func Watchlist(home, repo string) []string {
	paths := []string{home, "/etc", "/usr/local/bin", repo}
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		paths = append(paths, gobin)
	} else if gopath := os.Getenv("GOPATH"); gopath != "" {
		// GOPATH is a list; go install honours its first element.
		first := strings.Split(gopath, string(filepath.ListSeparator))[0]
		paths = append(paths, filepath.Join(first, "bin"))
	}
	return dedup(paths)
}

// Ignores is the default exclusion list for a run in runDir whose records live
// under stateBase/agent-runner, watching the repository at repo. The run
// directory comes first: it is the one exclusion that is not about the OS at
// all. The program's own bookkeeping is excluded by its exact directory rather
// than by all of stateBase: state another program owns is a write outside the
// run like any other, and the audit has to see it.
//
// <repo>/.git/worktrees is excluded for the same reason as the run directory:
// `git worktree add` registers one there on every run, so on a fresh clone the
// first run creates it whether or not anything was written outside. What the
// exclusion covers is exactly that bookkeeping — the rest of .git stays watched.
func Ignores(home, stateBase, runDir, repo string) []Ignore {
	ignores := []Ignore{
		{Prefix: runDir, Why: "the run's own directory is where it is supposed to write"},
		{Prefix: filepath.Join(home, ".cache"), Why: "caches are rewritten by every tool that runs; a cache write is not an escape"},
		{Prefix: filepath.Join(stateBase, "agent-runner"), Why: "this program's run records live here and the audit must not flag its own bookkeeping"},
		{Prefix: filepath.Join(home, ".npm"), Why: "the npm cache is scratch space the toolchain owns"},
	}
	if repo != "" {
		ignores = append(ignores, Ignore{
			Prefix: filepath.Join(repo, ".git", "worktrees"),
			Why:    "git records the run's own worktree here; that bookkeeping is the run's write, not an escape",
		})
	}
	return ignores
}

// Take records the current shape of every watchlist root. A root that does not
// exist contributes nothing rather than failing: it may legitimately be absent,
// and Compare names it if it appears by the next snapshot.
func Take(paths []string, ignores []Ignore) (Snapshot, error) {
	snap := make(Snapshot, len(paths))
	for _, root := range paths {
		if root == "" || ignored(root, ignores) {
			continue
		}
		if err := snap.take(root, ignores); err != nil {
			return nil, err
		}
	}
	return snap, nil
}

// take records one root and, one level down, its children.
func (s Snapshot) take(root string, ignores []Ignore) error {
	fi, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil
		}
		return fmt.Errorf("audit %s: %w", root, err)
	}
	if !fi.IsDir() {
		// A watchlist entry naming a file is watched as one file.
		s[root] = Entry{Size: fi.Size(), ModTime: fi.ModTime()}
		return nil
	}
	names, err := readNames(root, ignores)
	if err != nil {
		return err
	}
	s[root] = Entry{IsDir: true, Names: names}
	for _, name := range names {
		child := filepath.Join(root, name)
		if ignored(child, ignores) {
			// Do not descend into an ignored directory either: keeping a browser
			// profile out of the walk is the point of the exclusion.
			continue
		}
		ci, err := os.Lstat(child)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("audit %s: %w", child, err)
		}
		if len(s) >= maxEntries {
			return fmt.Errorf("audit: more than %d entries under %s; narrow the watchlist", maxEntries, root)
		}
		if !ci.IsDir() {
			s[child] = Entry{Size: ci.Size(), ModTime: ci.ModTime()}
			continue
		}
		inner, err := readNames(child, ignores)
		if err != nil && !errors.Is(err, fs.ErrPermission) {
			return err
		}
		// A directory it cannot read is recorded as one with no children recorded,
		// not as a failure: /etc/credstore is root-only on every Debian host, and
		// an audit that refused every run for it would report its own blind spot
		// and nothing else. What it cannot see is the container's job.
		if err != nil {
			inner = nil
		}
		s[child] = Entry{IsDir: true, Names: inner}
	}
	return nil
}

// readNames lists a directory's children by name, sorted so two snapshots of an
// unchanged directory compare equal by construction. An ignored child is dropped
// here, not in Compare, so an excluded path never enters a snapshot at all:
// filtering only the diff would still report a cache directory appearing beside
// home.
func readNames(dir string, ignores []Ignore) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if ignored(filepath.Join(dir, e.Name()), ignores) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// Compare returns every path that differs between two snapshots of the same
// watchlist, sorted by path so the refusal reads the same on every run. A
// directory whose children changed contributes one drift per child name, which
// is what lets the report name the file and not only its parent.
func Compare(before, after Snapshot) []Drift {
	var drift []Drift
	// One path can be named twice: by the entry it has in both snapshots, and by
	// its parent's list of children. Both are kept — the child list names a file
	// one level down, whose own entry exists only if it was there when the walk
	// reached its parent — so the same (path, kind) is reported once, and one
	// write is not counted as two path(s) in the refusal.
	seen := make(map[Drift]bool, len(before)+len(after))
	add := func(d Drift) {
		if seen[d] {
			return
		}
		seen[d] = true
		drift = append(drift, d)
	}
	for path, b := range before {
		a, ok := after[path]
		if !ok {
			add(Drift{Path: path, Kind: "removed"})
			continue
		}
		if b.IsDir || a.IsDir {
			if b.IsDir != a.IsDir {
				add(Drift{Path: path, Kind: "changed", Detail: "directory became a file, or the reverse"})
				continue
			}
			for _, d := range nameDrift(path, b.Names, a.Names) {
				add(d)
			}
			continue
		}
		if d := fileDrift(b, a); d != "" {
			add(Drift{Path: path, Kind: "changed", Detail: d})
		}
	}
	// Report the shallowest new path: a root that appeared with everything inside
	// it is one finding, not one finding per file.
	added := make(map[string]bool, len(after))
	for path := range after {
		if _, ok := before[path]; !ok {
			added[path] = true
		}
	}
	for path := range added {
		if added[filepath.Dir(path)] {
			continue
		}
		add(Drift{Path: path, Kind: "added"})
	}
	sort.Slice(drift, func(i, j int) bool {
		if drift[i].Path != drift[j].Path {
			return drift[i].Path < drift[j].Path
		}
		return drift[i].Kind < drift[j].Kind
	})
	return drift
}

// nameDrift reports the children one directory gained or lost.
func nameDrift(dir string, before, after []string) []Drift {
	seen := make(map[string]bool, len(before))
	for _, n := range before {
		seen[n] = true
	}
	now := make(map[string]bool, len(after))
	var drift []Drift
	for _, n := range after {
		now[n] = true
		if !seen[n] {
			drift = append(drift, Drift{Path: filepath.Join(dir, n), Kind: "added"})
		}
	}
	for _, n := range before {
		if !now[n] {
			drift = append(drift, Drift{Path: filepath.Join(dir, n), Kind: "removed"})
		}
	}
	return drift
}

// fileDrift describes what changed about one file, or "" when nothing did. Size
// and mtime are both compared: clock skew can leave the size identical, and a
// rewrite that preserves the size is still a write.
func fileDrift(b, a Entry) string {
	if b.Size == a.Size && b.ModTime.Equal(a.ModTime) {
		return ""
	}
	var what []string
	if b.Size != a.Size {
		what = append(what, fmt.Sprintf("size %d -> %d", b.Size, a.Size))
	}
	if !b.ModTime.Equal(a.ModTime) {
		what = append(what, fmt.Sprintf("mtime %s -> %s", b.ModTime.UTC().Format(time.RFC3339), a.ModTime.UTC().Format(time.RFC3339)))
	}
	return strings.Join(what, ", ")
}

// ignored reports whether path is one of the excluded prefixes or lives under
// one. The separator check keeps /home/agent-other out of an exclusion written
// for /home/agent.
func ignored(path string, ignores []Ignore) bool {
	clean := filepath.Clean(path)
	for _, ig := range ignores {
		if ig.Prefix == "" {
			continue
		}
		p := filepath.Clean(ig.Prefix)
		if clean == p || strings.HasPrefix(clean, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// dedup drops empty and repeated paths, keeping the first spelling of each.
func dedup(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		clean := filepath.Clean(p)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}
