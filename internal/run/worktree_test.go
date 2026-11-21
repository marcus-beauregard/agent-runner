package run

import (
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
