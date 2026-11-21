package run

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/report"
)

const (
	cageSubject  = "https://github.com/marcus-beauregard/agent-runner/issues/49"
	cageSubject2 = "https://github.com/marcus-beauregard/agent-runner/issues/50"
)

// TestSlug pins the branch component's shape: recognisable from the subject,
// safe for git, and distinct per run.
func TestSlug(t *testing.T) {
	const id = "20260102-150405.000000-abcdef"
	cases := []struct {
		name    string
		subject string
		id      string
		want    string
	}{
		{"issue number", cageSubject, id, "49-20260102-150405-000000"},
		{"trailing slash", "https://github.com/o/r/pull/7/", id, "7-20260102-150405-000000"},
		{"title segment", "https://github.com/o/r/issues/Add-Worktrees", id, "add-worktrees-20260102-150405-000000"},
		{"capped", "https://example.test/" + strings.Repeat("a", 60), id, strings.Repeat("a", 40) + "-20260102-150405-000000"},
		{"no id", cageSubject, "", "49"},
		{"no subject", "", id, "20260102-150405-000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Slug(tc.subject, tc.id); got != tc.want {
				t.Errorf("Slug(%q, %q) = %q, want %q", tc.subject, tc.id, got, tc.want)
			}
		})
	}
}

// TestWorktreeCreatesOwnBranch is the ticket's core claim: a fresh branch cut
// from origin's default, on disk, with both facts in the record.
func TestWorktreeCreatesOwnBranch(t *testing.T) {
	parent := worktreeParent(t)
	wantBase := mustGit(t, parent, "rev-parse", "origin/main")
	r := newWorktreeRun(t, parent, cageSubject)
	slug := Slug(cageSubject, r.ID)

	branch, base, err := Worktree(r.Dir, parent, slug)
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if want := "agent/" + slug; branch != want {
		t.Errorf("branch = %q, want %q", branch, want)
	}
	if base != wantBase {
		t.Errorf("base sha = %q, want %q (origin/main)", base, wantBase)
	}
	wt := filepath.Join(r.Dir, "worktree")
	if _, err := os.Stat(filepath.Join(wt, "README.md")); err != nil {
		t.Fatalf("worktree holds no checkout: %v", err)
	}
	if got := mustGit(t, wt, "symbolic-ref", "--short", "HEAD"); got != branch {
		t.Errorf("worktree HEAD = %q, want %q", got, branch)
	}
	rec, err := loadRecord(r.Dir)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if rec.Branch != branch || rec.BaseSHA != base {
		t.Errorf("record branch/base = %q/%q, want %q/%q", rec.Branch, rec.BaseSHA, branch, base)
	}
}

// TestWorktreeBranchTracksNothing pins what cut from a remote-tracking ref used
// to cost: agent/<slug> carries origin/main as its upstream, so the build
// stage's plain `git push` lands on the default branch. The branch also has to
// start where the record says it does, not wherever origin moved to since.
func TestWorktreeBranchTracksNothing(t *testing.T) {
	parent := worktreeParent(t)
	r := newWorktreeRun(t, parent, cageSubject)

	branch, base, err := Worktree(r.Dir, parent, Slug(cageSubject, r.ID))
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for _, key := range []string{"remote", "merge"} {
		if got, err := mustGitErr(parent, "config", "--get", "branch."+branch+"."+key); err == nil {
			t.Errorf("branch.%s.%s = %q, want it unset", branch, key, got)
		}
	}
	wt := filepath.Join(r.Dir, "worktree")
	if got, err := mustGitErr(wt, "rev-parse", "--abbrev-ref", "@{u}"); err == nil {
		t.Errorf("worktree upstream = %q, want none", got)
	}
	if got := mustGit(t, wt, "rev-parse", "HEAD"); got != base {
		t.Errorf("branch is at %q, want the recorded base %q", got, base)
	}
}

// TestWorktreesAreIndependent proves two runs on one subject cannot see each
// other's writes, which is the whole point of not sharing a checkout.
func TestWorktreesAreIndependent(t *testing.T) {
	parent := worktreeParent(t)
	a, b := newWorktreeRun(t, parent, cageSubject), newWorktreeRun(t, parent, cageSubject)
	slugA, slugB := Slug(cageSubject, a.ID), Slug(cageSubject, b.ID)
	if slugA == slugB {
		t.Fatalf("two runs on %s share slug %q", cageSubject, slugA)
	}
	for _, r := range []*Run{a, b} {
		if _, _, err := Worktree(r.Dir, parent, Slug(cageSubject, r.ID)); err != nil {
			t.Fatalf("Worktree(%s): %v", r.ID, err)
		}
	}
	writeTestFile(t, filepath.Join(a.Dir, "worktree", "only-in-a.txt"), "a\n")
	if _, err := os.Stat(filepath.Join(b.Dir, "worktree", "only-in-a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a's write is visible in b's worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.Dir, "worktree", "only-in-a.txt")); err != nil {
		t.Fatalf("a's own write is missing: %v", err)
	}
}

// TestRemoveWorktreeLeavesParentClean checks cleanup as a step: the tree and
// its registration go, the branch stays, and a second call is a no-op.
func TestRemoveWorktreeLeavesParentClean(t *testing.T) {
	parent := worktreeParent(t)
	r := newWorktreeRun(t, parent, cageSubject)
	slug := Slug(cageSubject, r.ID)
	branch, _, err := Worktree(r.Dir, parent, slug)
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := RemoveWorktree(r.Dir); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	wt := filepath.Join(r.Dir, "worktree")
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree still on disk after cleanup: %v", err)
	}
	if list := mustGit(t, parent, "worktree", "list", "--porcelain"); strings.Contains(list, wt) {
		t.Errorf("parent still registers %s:\n%s", wt, list)
	}
	if _, err := mustGitErr(parent, "rev-parse", "--verify", "refs/heads/"+branch); err != nil {
		t.Errorf("cleanup deleted the branch: %v", err)
	}
	if err := RemoveWorktree(r.Dir); err != nil {
		t.Errorf("second RemoveWorktree = %v, want nil for an already clean run", err)
	}
}

// TestWorktreeRefuses covers the four states the ticket names plus a default
// branch git cannot name. Every one is a refusal (exit 3), and none leaves a
// worktree directory behind.
func TestWorktreeRefuses(t *testing.T) {
	clean := worktreeParent(t)
	dirty := worktreeParent(t)
	writeTestFile(t, filepath.Join(dirty, "scratch.txt"), "x\n")
	noOrigin := t.TempDir()
	mustGit(t, noOrigin, "init", "-q", "--initial-branch=main", ".")
	unsetHead := worktreeParent(t)
	mustGit(t, unsetHead, "remote", "set-head", "origin", "--delete")
	plain := t.TempDir()

	busy := newWorktreeRun(t, clean, cageSubject)
	if err := os.Mkdir(filepath.Join(busy.Dir, "worktree"), 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		repo string
		run  *Run
		want string
		skip bool // the state predates a worktree, so there is nothing to list or stat
	}{
		{"not a checkout", plain, newWorktreeRun(t, plain, cageSubject), "not a git checkout", true},
		{"no origin", noOrigin, newWorktreeRun(t, noOrigin, cageSubject), "no origin remote", false},
		{"dirty parent", dirty, newWorktreeRun(t, dirty, cageSubject), "parent checkout is dirty", false},
		{"origin/HEAD unset", unsetHead, newWorktreeRun(t, unsetHead, cageSubject), "origin/HEAD is not set", false},
		{"worktree exists", clean, busy, "worktree already exists", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Worktree(tc.run.Dir, tc.repo, Slug(cageSubject, tc.run.ID))
			if !errors.Is(err, report.ErrRefused) {
				t.Fatalf("Worktree = %v, want a refusal wrapping report.ErrRefused", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
			if tc.skip {
				return
			}
			if _, err := os.Stat(filepath.Join(tc.run.Dir, "worktree")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("refusal left a worktree behind: %v", err)
			}
			if list := mustGit(t, tc.repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
				t.Errorf("refusal registered something with the parent:\n%s", list)
			}
		})
	}
}

// newTestRun opens a real run record for repo, so the worktree code reads and
// rewrites the same run.json a build run would have.
func newWorktreeRun(t *testing.T, repo, subject string) *Run {
	t.Helper()
	r, err := New(t.TempDir(), Meta{Kind: "build", Subject: subject, Repo: repo})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// worktreeParent returns a clone whose origin is a bare repository: the shape
// a run finds on a machine, with a default branch named by origin/HEAD.
func worktreeParent(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	mustGit(t, root, "init", "-q", "--bare", "--initial-branch=main", origin)
	seed := filepath.Join(root, "seed")
	mustGit(t, root, "init", "-q", "--initial-branch=main", seed)
	writeTestFile(t, filepath.Join(seed, "README.md"), "seed\n")
	mustGit(t, seed, "add", ".")
	mustGit(t, seed, "commit", "-q", "-m", "seed")
	mustGit(t, seed, "remote", "add", "origin", origin)
	mustGit(t, seed, "push", "-q", "origin", "main")
	parent := filepath.Join(root, "parent")
	mustGit(t, root, "clone", "-q", origin, parent)
	return parent
}

// mustGit runs git for test setup only. The host's own git config is kept out
// of it so a global hook or default branch cannot decide the result.
func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := mustGitErr(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return out
}

func mustGitErr(dir string, args ...string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", err
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWorktreeRecordsAdd pins the first half of the recorded lifecycle: the
// event is there, and it names the same tree the record and the branch do.
func TestWorktreeRecordsAdd(t *testing.T) {
	parent := worktreeParent(t)
	r := newWorktreeRun(t, parent, cageSubject)
	branch, base, err := Worktree(r.Dir, parent, Slug(cageSubject, r.ID))
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	events := readEvents(t, r.Dir)
	if len(events) != 1 || events[0].Kind != kindWorktreeAdd {
		t.Fatalf("events = %+v, want exactly one %s", events, kindWorktreeAdd)
	}
	var ref WorktreeRef
	if err := json.Unmarshal(events[0].Data, &ref); err != nil {
		t.Fatalf("decode %s: %v", kindWorktreeAdd, err)
	}
	want := WorktreeRef{Path: filepath.Join(r.Dir, "worktree"), Branch: branch, BaseSHA: base}
	if ref != want {
		t.Errorf("ref = %+v, want %+v", ref, want)
	}
}

// TestFinishRemovesRecordedWorktree is the ticket's done-when: a run that fails
// at the gate leaves no registration behind and both steps in the stream. The
// tree is dirty on purpose — cleanup is what discards it, and a single --force
// would refuse a tree with uncommitted work.
func TestFinishRemovesRecordedWorktree(t *testing.T) {
	parent := worktreeParent(t)
	r := newWorktreeRun(t, parent, cageSubject)
	branch, _, err := Worktree(r.Dir, parent, Slug(cageSubject, r.ID))
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	wt := filepath.Join(r.Dir, "worktree")
	writeTestFile(t, filepath.Join(wt, "half-done.txt"), "wip\n")

	if err := r.Finish(2); err != nil {
		t.Fatalf("Finish(2): %v", err)
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree still on disk after Finish: %v", err)
	}
	if list := mustGit(t, parent, "worktree", "list", "--porcelain"); strings.Contains(list, wt) {
		t.Errorf("parent still registers %s:\n%s", wt, list)
	}
	if _, err := mustGitErr(parent, "rev-parse", "--verify", "refs/heads/"+branch); err != nil {
		t.Errorf("cleanup deleted the branch: %v", err)
	}

	events := readEvents(t, r.Dir)
	var add, remove *Event
	for i := range events {
		switch events[i].Kind {
		case kindWorktreeAdd:
			add = &events[i]
		case kindWorktreeRemove:
			remove = &events[i]
		}
	}
	if add == nil || remove == nil {
		t.Fatalf("stream = %+v, want both %s and %s", events, kindWorktreeAdd, kindWorktreeRemove)
	}
	var rem worktreeRemove
	if err := json.Unmarshal(remove.Data, &rem); err != nil {
		t.Fatalf("decode %s: %v", kindWorktreeRemove, err)
	}
	if rem.Path != wt || rem.ExitCode != 2 || rem.Error != "" {
		t.Errorf("remove = %+v, want path %s, exit 2, no error", rem, wt)
	}

	rec, err := loadRecord(r.Dir)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if rec.State != stateFailed || rec.ExitCode == nil || *rec.ExitCode != 2 {
		t.Errorf("record state/exit = %q/%v, want failed/2", rec.State, rec.ExitCode)
	}
}

// TestFinishKeepsWorktreeTheWorkIsNotSettled pins the other half of the cleanup
// rule: a run that stopped with edits in hand keeps them. needs_human is the
// case that matters — the tree is what the person is being asked to look at —
// and a failure or a refusal can stop holding the same uncommitted work.
func TestFinishKeepsWorktreeTheWorkIsNotSettled(t *testing.T) {
	cases := []struct {
		code  int
		state string
	}{
		{1, stateFailed},
		{3, stateRefused},
		{4, stateNeedsHuman},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			parent := worktreeParent(t)
			r := newWorktreeRun(t, parent, cageSubject)
			if _, _, err := Worktree(r.Dir, parent, Slug(cageSubject, r.ID)); err != nil {
				t.Fatalf("Worktree: %v", err)
			}
			wt := filepath.Join(r.Dir, "worktree")
			halfDone := filepath.Join(wt, "half-done.txt")
			writeTestFile(t, halfDone, "wip\n")

			if err := r.Finish(tc.code); err != nil {
				t.Fatalf("Finish(%d): %v", tc.code, err)
			}
			if _, err := os.Stat(halfDone); err != nil {
				t.Errorf("cleanup discarded work a person was handed: %v", err)
			}
			if list := mustGit(t, parent, "worktree", "list", "--porcelain"); !strings.Contains(list, wt) {
				t.Errorf("parent stopped registering %s, which still holds edits: %s", wt, list)
			}
			if events := readEvents(t, r.Dir); len(events) != 1 || events[0].Kind != kindWorktreeAdd {
				t.Errorf("stream = %+v, want only %s", events, kindWorktreeAdd)
			}
			rec, err := loadRecord(r.Dir)
			if err != nil {
				t.Fatalf("load record: %v", err)
			}
			if rec.State != tc.state || rec.ExitCode == nil || *rec.ExitCode != tc.code {
				t.Errorf("record state/exit = %q/%v, want %q/%d", rec.State, rec.ExitCode, tc.state, tc.code)
			}
		})
	}
}

// TestFinishRemovesWorktreeOnDone covers the other settled code: the work is on
// the branch, so the checkout is scratch and goes.
func TestFinishRemovesWorktreeOnDone(t *testing.T) {
	parent := worktreeParent(t)
	r := newWorktreeRun(t, parent, cageSubject)
	if _, _, err := Worktree(r.Dir, parent, Slug(cageSubject, r.ID)); err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	wt := filepath.Join(r.Dir, "worktree")
	if err := r.Finish(0); err != nil {
		t.Fatalf("Finish(0): %v", err)
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree still on disk after a finished run: %v", err)
	}
	if list := mustGit(t, parent, "worktree", "list", "--porcelain"); strings.Contains(list, wt) {
		t.Errorf("parent still registers %s: %s", wt, list)
	}
}

// TestFinishWithoutWorktreeRecordsNoStep keeps a dry run's stream empty. The
// cleanup step is about a tree that exists; a run that was refused at triage
// never had one.
func TestFinishWithoutWorktreeRecordsNoStep(t *testing.T) {
	r := newWorktreeRun(t, worktreeParent(t), cageSubject)
	if err := r.Finish(0); err != nil {
		t.Fatalf("Finish(0): %v", err)
	}
	if events := readEvents(t, r.Dir); len(events) != 0 {
		t.Errorf("stream = %+v, want no events for a run that had no worktree", events)
	}
}

// TestRemoveWorktreePrunesAStaleRegistration covers the tree deleted by hand:
// the directory is gone, the parent still names it, and cleanup has to drop the
// registration anyway.
func TestRemoveWorktreePrunesAStaleRegistration(t *testing.T) {
	parent := worktreeParent(t)
	r := newWorktreeRun(t, parent, cageSubject)
	if _, _, err := Worktree(r.Dir, parent, Slug(cageSubject, r.ID)); err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	wt := filepath.Join(r.Dir, "worktree")
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if err := RemoveWorktree(r.Dir); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if list := mustGit(t, parent, "worktree", "list", "--porcelain"); strings.Contains(list, wt) {
		t.Errorf("parent still registers a tree that is gone:\n%s", list)
	}
}

// TestRemoveWorktreeDropsALockedRegistration is the bug the handoff named:
// prune exits 0 without dropping a locked entry, so the function must remove
// the lock with the tree and then prove the list is clean.
func TestRemoveWorktreeDropsALockedRegistration(t *testing.T) {
	parent := worktreeParent(t)
	r := newWorktreeRun(t, parent, cageSubject)
	if _, _, err := Worktree(r.Dir, parent, Slug(cageSubject, r.ID)); err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	wt := filepath.Join(r.Dir, "worktree")
	mustGit(t, parent, "worktree", "lock", wt)
	if err := RemoveWorktree(r.Dir); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if list := mustGit(t, parent, "worktree", "list", "--porcelain"); strings.Contains(list, wt) {
		t.Errorf("parent still registers a locked tree:\n%s", list)
	}
}

// TestRequireCleanParentNamesABareParent keeps the refusal readable. A bare
// parent has no work tree, and the old path reported that as git status
// failing rather than as a shape the runner does not support.
func TestRequireCleanParentNamesABareParent(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "bare.git")
	mustGit(t, root, "init", "-q", "--bare", "--initial-branch=main", bare)
	err := requireCleanParent(bare)
	if !errors.Is(err, report.ErrRefused) {
		t.Fatalf("requireCleanParent = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "parent is bare") {
		t.Errorf("error = %q, want it to say the parent is bare", err)
	}
}

// readEvents returns the run's stream, failing on a torn or unparseable line:
// a test that quietly read past damage would prove less than it claims.
func readEvents(t *testing.T, runDir string) []Event {
	t.Helper()
	events, torn, err := readLedger(filepath.Join(runDir, "run.ndjson"))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if torn != "" {
		t.Fatalf("ledger is torn: %s", torn)
	}
	return events
}
