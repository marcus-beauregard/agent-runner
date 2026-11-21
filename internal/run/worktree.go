package run

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/marcus-beauregard/agent-runner/internal/report"
)

// slugMax is how much of the subject's last path segment survives in a branch
// name. An issue number needs two characters and a title segment needs enough
// to be recognisable, not enough to test git's own limits.
const slugMax = 40

// Slug names the branch a run works on: the subject's last path segment
// reduced to [a-z0-9-] and capped at slugMax characters, then the run id's
// time component.
//
// The time component is what keeps two runs on the same issue apart. Without
// it the second run would either adopt the first run's branch or fail, and
// neither is a worktree of its own.
func Slug(subject, id string) string {
	name, stamp := subjectName(subject), idStamp(id)
	switch {
	case name == "":
		return stamp
	case stamp == "":
		return name
	}
	return name + "-" + stamp
}

// subjectName reduces a subject URL to a branch-safe word: the last path
// segment, lowercased, with every run of characters outside [a-z0-9] turned
// into one dash. A subject that is not a URL is treated as a path, which is
// what lets the same function name a run from a bare reference.
func subjectName(subject string) string {
	s := subject
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > slugMax {
		out = strings.TrimRight(out[:slugMax], "-")
	}
	return out
}

// idStamp is the time part of a run id — 20060102-150405.000000 — with the dot
// turned into a dash and the random suffix dropped. An id of another shape
// contributes nothing rather than a fragment that could collide with another
// run's.
func idStamp(id string) string {
	date, rest, ok := strings.Cut(id, "-")
	if !ok {
		return ""
	}
	if i := strings.LastIndex(rest, "-"); i >= 0 {
		rest = rest[:i]
	}
	stamp := date + "-" + strings.Replace(rest, ".", "-", 1)
	for _, r := range stamp {
		if (r < '0' || r > '9') && r != '-' {
			return ""
		}
	}
	return stamp
}

// Worktree gives a run its own checkout: <runDir>/worktree, on a new branch
// agent/<slug> cut from the commit origin's default branch points at. The
// parent stays the object store — git records the worktree in it and that
// registration is the only thing that changes there — and the branch and the
// commit it was cut from are recorded in run.json, which already carries both
// fields.
//
// The branch tracks nothing. A branch cut from origin/<default> inherits it as
// its upstream, and the build stage's plain `git push` then goes to the default
// branch.
//
// Every precondition failure is a refusal wrapping report.ErrRefused, so the
// run exits 3 with a reason instead of leaving half a tree for the next run to
// trip over: the parent is not a git checkout, it has no origin, its default
// branch cannot be named, it is dirty, or the worktree directory already
// exists.
func Worktree(runDir, repo, slug string) (branch, baseSHA string, err error) {
	if err := checkSlug(slug); err != nil {
		return "", "", err
	}
	rec, err := loadRecord(runDir)
	if err != nil {
		return "", "", err
	}
	wt := filepath.Join(runDir, "worktree")
	if _, err := os.Lstat(wt); err == nil {
		return "", "", refuse("worktree already exists: %s", wt)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("stat worktree: %w", err)
	}
	base, _, err := originBase(repo)
	if err != nil {
		return "", "", err
	}
	if err := requireCleanParent(repo); err != nil {
		return "", "", err
	}

	branch = "agent/" + slug
	// --no-track keeps the branch's upstream unset, and the resolved sha is the
	// start point so the recorded BaseSHA is the commit the branch holds even
	// if origin moves between the two git calls.
	if _, err := gitRun(repo, "worktree", "add", "--no-track", wt, "-b", branch, base); err != nil {
		return "", "", fmt.Errorf("add worktree: %w", err)
	}
	rec.Branch, rec.BaseSHA = branch, base
	if err := writeRecord(runDir, *rec); err != nil {
		// A checkout no record names is worse than none: gc finds runs through
		// the record, so this one would be invisible (PRD §6.3).
		_, removeErr := gitRun(repo, "worktree", "remove", "--force", wt)
		_, _ = gitRun(repo, "worktree", "prune")
		_, _ = gitRun(repo, "branch", "-D", branch)
		if removeErr != nil {
			return "", "", fmt.Errorf("record worktree: %w (removing it also failed: %v)", err, removeErr)
		}
		return "", "", fmt.Errorf("record worktree: %w", err)
	}
	return branch, base, nil
}

// RemoveWorktree deletes a run's checkout and the registration left in the
// parent, then prunes the parent so it does not accumulate entries pointing at
// nothing (PRD §6.3). The branch stays: the PR is evidence, the checkout is
// scratch, and gc never deletes a branch.
//
// It is idempotent. A worktree that is already gone — the directory removed by
// hand, or a second cleanup after the first — still prunes and succeeds, so a
// scheduled cleanup can run over every run without first asking which ones
// still have trees.
func RemoveWorktree(runDir string) error {
	rec, err := loadRecord(runDir)
	if err != nil {
		return err
	}
	if rec.Repo == "" {
		return fmt.Errorf("remove worktree: run record names no repo")
	}
	wt := filepath.Join(runDir, "worktree")
	_, removeErr := gitRun(rec.Repo, "worktree", "remove", "--force", wt)
	_, pruneErr := gitRun(rec.Repo, "worktree", "prune")
	if removeErr != nil {
		if _, statErr := os.Lstat(wt); statErr == nil {
			return fmt.Errorf("remove worktree: %w", removeErr)
		}
	}
	if pruneErr != nil {
		return fmt.Errorf("prune worktrees: %w", pruneErr)
	}
	return nil
}

// originBase resolves what the worktree starts from: the commit origin's
// default branch points at. Guessing "main" would cut a branch from a branch
// the repository does not use, so an origin/HEAD that is not set is a refusal
// with the command that fixes it.
func originBase(repo string) (baseSHA, defaultBranch string, err error) {
	if repo == "" {
		return "", "", refuse("no repository path")
	}
	if _, err := gitRun(repo, "rev-parse", "--git-dir"); err != nil {
		return "", "", refuse("not a git checkout: %s: %v", repo, err)
	}
	if _, err := gitRun(repo, "remote", "get-url", "origin"); err != nil {
		return "", "", refuse("no origin remote in %s", repo)
	}
	head, err := gitRun(repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", "", refuse("origin/HEAD is not set in %s: run git -C %s remote set-head origin --auto", repo, repo)
	}
	defaultBranch = strings.TrimPrefix(head, "origin/")
	if defaultBranch == "" || defaultBranch == "origin/HEAD" {
		return "", "", refuse("origin/HEAD is not set in %s: run git -C %s remote set-head origin --auto", repo, repo)
	}
	baseSHA, err = gitRun(repo, "rev-parse", "--verify", "origin/"+defaultBranch+"^{commit}")
	if err != nil {
		return "", "", refuse("origin/%s is not fetched in %s: %v", defaultBranch, repo, err)
	}
	return baseSHA, defaultBranch, nil
}

// requireCleanParent refuses a parent checkout with uncommitted work. The
// worktree itself would be clean either way, but a run that starts beside
// someone's half-finished branch makes "what did the run change" unanswerable
// the moment they go back to editing.
func requireCleanParent(repo string) error {
	out, err := gitRun(repo, "status", "--porcelain")
	if err != nil {
		return refuse("read the parent status: %v", err)
	}
	if out != "" {
		return refuse("parent checkout is dirty: %s", repo)
	}
	return nil
}

// checkSlug guards the branch name Worktree builds. Slug already produces
// [a-z0-9-]; this catches a caller that passed its own.
func checkSlug(slug string) error {
	if slug == "" {
		return errors.New("worktree: empty slug")
	}
	if len(slug) > 80 {
		return fmt.Errorf("worktree: slug %q is longer than 80 characters", slug)
	}
	for _, r := range slug {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return fmt.Errorf("worktree: slug %q is not [a-z0-9-]", slug)
		}
	}
	return nil
}

// refuse wraps report.ErrRefused, the sentinel report.ExitCode maps to 3.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", report.ErrRefused, fmt.Sprintf(format, args...))
}

// gitRun runs one git command in dir and returns its trimmed stdout. A failure
// carries git's stderr: "exit status 128" alone never says which precondition
// failed, and the sentence git wrote always does.
func gitRun(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(string(out)), nil
}
