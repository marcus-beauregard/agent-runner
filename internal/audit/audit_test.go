package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write puts content at path, creating parents.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// testRepo is the repository a run under home would be given: inside home, so
// one snapshot of home covers it too, which is what the real watchlist does.
func testRepo(home string) string {
	return filepath.Join(home, "src", "agent-runner")
}

// runIgnores is the exclusion list a run under home would use, with its state
// base resolved the way internal/run resolves it.
func runIgnores(home string) []Ignore {
	stateBase := filepath.Join(home, ".local", "state")
	return Ignores(home, stateBase, filepath.Join(stateBase, "agent-runner", "runs", "one"), testRepo(home))
}

func take(t *testing.T, paths []string, ignores []Ignore) Snapshot {
	t.Helper()
	snap, err := Take(paths, ignores)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	return snap
}

func findDrift(drift []Drift, want string) (Drift, bool) {
	for _, d := range drift {
		if d.Path == want {
			return d, true
		}
	}
	return Drift{}, false
}

// TestEscapeFileIsNamed is the ticket's done-when: a run that writes
// $HOME/.config/agent-runner-escape is refused, and the refusal names the path.
// The file sits one level below the watched home, so only the config directory's
// children can name it.
func TestEscapeFileIsNamed(t *testing.T) {
	home := t.TempDir()
	ignores := runIgnores(home)
	write(t, filepath.Join(home, ".config", "app.conf"), "x")
	before := take(t, []string{home}, ignores)

	escape := filepath.Join(home, ".config", "agent-runner-escape")
	write(t, escape, "touched")
	after := take(t, []string{home}, ignores)

	d, ok := findDrift(Compare(before, after), escape)
	if !ok || d.Kind != "added" {
		t.Fatalf("drift %+v does not name %s as added", d, escape)
	}
}

// TestCleanRunHasNoDrift is the other half of the done-when: two snapshots of an
// untouched host compare empty, so a clean run is not refused for existing.
func TestCleanRunHasNoDrift(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "notes.txt"), "hello")
	write(t, filepath.Join(home, ".config", "app.conf"), "x")
	ignores := runIgnores(home)
	before := take(t, []string{home}, ignores)
	after := take(t, []string{home}, ignores)
	if drift := Compare(before, after); len(drift) != 0 {
		t.Fatalf("drift on an unchanged host: %v", drift)
	}
}

// TestDirectChildDriftIsNamedOnce covers the path that is both a parent's new
// child and an entry of its own: one write must read as one path in the refusal,
// not twice. The depth-two case above cannot see this, because a file under a
// directory is only ever named by that directory's children.
func TestDirectChildDriftIsNamedOnce(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".config", "app.conf"), "x")
	file := filepath.Join(home, "x")
	before := take(t, []string{home}, nil)

	write(t, file, "one write")
	after := take(t, []string{home}, nil)
	drift := Compare(before, after)
	if len(drift) != 1 || drift[0].Path != file || drift[0].Kind != "added" {
		t.Fatalf("adding %s produced %v, want one added entry", file, drift)
	}

	present := after
	if err := os.Remove(file); err != nil {
		t.Fatalf("remove: %v", err)
	}
	gone := take(t, []string{home}, nil)
	drift = Compare(present, gone)
	if len(drift) != 1 || drift[0].Path != file || drift[0].Kind != "removed" {
		t.Fatalf("removing %s produced %v, want one removed entry", file, drift)
	}
}

// TestChangedFileReportsSizeAndTime covers an edit in place: the name in the
// parent does not move, so the size and clock are what carry it.
func TestChangedFileReportsSizeAndTime(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, "notes.txt")
	write(t, file, "a")
	before := take(t, []string{home}, nil)
	write(t, file, "longer content")
	after := take(t, []string{home}, nil)

	d, ok := findDrift(Compare(before, after), file)
	if !ok || d.Kind != "changed" || !strings.Contains(d.Detail, "size 1 -> 14") {
		t.Fatalf("drift = %+v (found %v), want a changed entry naming the new size", d, ok)
	}
}

// TestSameSizeRewriteReportsMtime covers the edit the size cannot see. A rewrite
// that keeps the length still moves the clock, and the mtime half of fileDrift is
// the only thing that reports it: drop that half and this test fails while the
// size test above still passes.
func TestSameSizeRewriteReportsMtime(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, "notes.txt")
	write(t, file, "abcdefghij")
	before := take(t, []string{home}, nil)

	write(t, file, "klmnopqrst") // same length, new contents
	later := before[file].ModTime.Add(2 * time.Second)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	after := take(t, []string{home}, nil)

	d, ok := findDrift(Compare(before, after), file)
	if !ok || d.Kind != "changed" {
		t.Fatalf("drift = %+v (found %v), want %s changed", d, ok, file)
	}
	if !strings.Contains(d.Detail, "mtime") || strings.Contains(d.Detail, "size") {
		t.Fatalf("detail = %q, want the mtime alone: the size did not change", d.Detail)
	}
}

// TestNestedFileEditIsInvisible pins the depth this audit watches, so the wiring
// in #85 is written against the real boundary and not against the old comment.
// A file one level below a root carries a name and no size, so editing it in
// place moves neither: it is a blind spot by design, and a change that shrinks it
// has to change this test on purpose.
func TestNestedFileEditIsInvisible(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, ".config", "authorized_keys")
	write(t, file, "ssh-ed25519 AAAA one")
	before := take(t, []string{home}, nil)

	write(t, file, "ssh-ed25519 AAAA two")
	after := take(t, []string{home}, nil)
	if drift := Compare(before, after); len(drift) != 0 {
		t.Fatalf("editing %s in place produced %v; the audit watches one level deep", file, drift)
	}
}

// TestRunDirectoryIsExcluded proves the one write the run is allowed to make is
// invisible to the audit, and that the walk never enters it.
func TestRunDirectoryIsExcluded(t *testing.T) {
	home := t.TempDir()
	stateBase := filepath.Join(home, ".local", "state")
	runDir := filepath.Join(stateBase, "agent-runner", "runs", "one")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("mkdir run: %v", err)
	}
	ignores := Ignores(home, stateBase, runDir, testRepo(home))
	before := take(t, []string{home}, ignores)
	write(t, filepath.Join(runDir, "REPORT.md"), "the run wrote this")
	after := take(t, []string{home}, ignores)

	if drift := Compare(before, after); len(drift) != 0 {
		t.Fatalf("the run's own directory produced drift: %v", drift)
	}
	for path := range after {
		if strings.HasPrefix(path, runDir) {
			t.Fatalf("snapshot walked into the run directory: %s", path)
		}
	}
}

// TestNamedExclusions covers the OS-touched prefixes: a browser rewriting its
// cache is not an escape, and saying so in code is what keeps the audit usable.
func TestNamedExclusions(t *testing.T) {
	home := t.TempDir()
	ignores := runIgnores(home)
	before := take(t, []string{home}, ignores)
	write(t, filepath.Join(home, ".cache", "chrome", "Cache", "data_0"), "cache")
	after := take(t, []string{home}, ignores)

	if drift := Compare(before, after); len(drift) != 0 {
		t.Fatalf("a cache write produced drift: %v", drift)
	}
	// The separator guard is what keeps a neighbour of an excluded directory
	// visible. The paths are built from the temp home so they share a string
	// prefix with a real exclusion: without the guard, ".cache2" matches the
	// exclusion written for ".cache".
	if ignored(filepath.Join(home, ".cache2", "notes.txt"), ignores) {
		t.Fatal("an exclusion leaked into the neighbouring directory " + filepath.Join(home, ".cache2"))
	}
	if !ignored(filepath.Join(home, ".local", "state", "agent-runner", "runs", "one", "REPORT.md"), ignores) {
		t.Fatal("a path inside the run directory was not excluded")
	}
	// Only this program's own directory is excluded. State another program
	// owns sits outside the run like any other write, and must stay visible:
	// excluding all of the state base is how a hole the size of a state tree
	// gets blessed with a reason.
	if ignored(filepath.Join(home, ".local", "state", "other", "note.json"), ignores) {
		t.Fatal("an unrelated program's state was excluded from the audit")
	}
}

// TestWorktreeRegistrationIsExcluded is the fresh-clone case: `git worktree add`
// creates <repo>/.git/worktrees on the first run, and an audit that reported it
// would refuse a run that changed nothing. The rest of .git stays watched, so the
// exclusion is as narrow as its reason.
func TestWorktreeRegistrationIsExcluded(t *testing.T) {
	home := t.TempDir()
	repo := testRepo(home)
	write(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	ignores := runIgnores(home)
	before := take(t, Watchlist(home, repo), ignores)

	if err := os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "run-one"), 0o700); err != nil {
		t.Fatalf("mkdir worktree bookkeeping: %v", err)
	}
	after := take(t, Watchlist(home, repo), ignores)
	if drift := Compare(before, after); len(drift) != 0 {
		t.Fatalf("registering a worktree produced drift: %v", drift)
	}

	// A write to the rest of .git is still a write outside the run.
	packed := filepath.Join(repo, ".git", "packed-refs")
	write(t, packed, "refs/heads/main\n")
	written := take(t, Watchlist(home, repo), ignores)
	if d, ok := findDrift(Compare(after, written), packed); !ok || d.Kind != "added" {
		t.Fatalf("drift = %+v (found %v), want %s added", d, ok, packed)
	}
}

// TestMissingRootIsNotAnError covers a watchlist entry that does not exist yet:
// nothing is recorded for it, and a later appearance is drift naming the root.
func TestMissingRootIsNotAnError(t *testing.T) {
	home := t.TempDir()
	gobin := filepath.Join(home, "go", "bin")
	before := take(t, []string{gobin}, nil)
	if len(before) != 0 {
		t.Fatalf("snapshot of a missing root = %v, want empty", before)
	}

	write(t, filepath.Join(gobin, "tool"), "a binary")
	after := take(t, []string{gobin}, nil)
	drift := Compare(before, after)
	if len(drift) != 1 || drift[0].Path != gobin {
		t.Fatalf("drift = %v, want the single new root %s", drift, gobin)
	}
}

// TestRemovedName covers the write in the other direction: deleting a file
// outside the run directory is drift too, because the next run would assume a
// host that still has it.
func TestRemovedName(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, "precious")
	write(t, file, "x")
	before := take(t, []string{home}, nil)
	if err := os.Remove(file); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after := take(t, []string{home}, nil)

	d, ok := findDrift(Compare(before, after), file)
	if !ok || d.Kind != "removed" {
		t.Fatalf("drift = %+v (found %v), want %s removed", d, ok, file)
	}
}
