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

// The two events a worktree's recorded life consists of. Worktree appends the
// first and CleanupWorktree the second; cleanup reads the first to tell a run
// that never got a tree — a refusal at triage, a dry run — from one whose tree
// was removed by hand and still needs its registration pruned.
const (
	kindWorktreeAdd    = "worktree:add"
	kindWorktreeRemove = "worktree:remove"
)

// WorktreeRef is what the run remembers about the tree it wrote in. It is the
// payload of worktree:add.
type WorktreeRef struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	BaseSHA string `json:"base_sha"`
}

// worktreeRemove is the payload of worktree:remove: the same path, the exit
// code the run reached before the step ran, and the error when it failed. A
// failed removal is an event, not a silence — the next reader needs to know the
// parent may still name a tree nobody owns.
type worktreeRemove struct {
	Path     string `json:"path"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

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
//
// A tree that exists without a worktree:add event is the failure this ticket
// closes: gc finds runs through their record, so a checkout nobody recorded is
// invisible. If either the record or the stream cannot be written, the tree and
// its branch come back out.
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
	oldBranch, oldBase := rec.Branch, rec.BaseSHA
	ref := WorktreeRef{Path: wt, Branch: branch, BaseSHA: base}
	rec.Branch, rec.BaseSHA = branch, base
	if err := writeRecord(runDir, *rec); err != nil {
		rollbackWorktree(repo, wt, branch)
		return "", "", fmt.Errorf("record worktree: %w", err)
	}
	// The event goes in after the record so the two agree on the tree's
	// identity. A stream that cannot take it is the same failure as a record
	// that cannot: the tree comes out and the record goes back to naming none.
	if err := appendWorktreeEvent(runDir, kindWorktreeAdd, ref); err != nil {
		rollbackWorktree(repo, wt, branch)
		rec.Branch, rec.BaseSHA = oldBranch, oldBase
		if werr := writeRecord(runDir, *rec); werr != nil {
			return "", "", fmt.Errorf("record worktree: %w (restoring the record also failed: %v)", err, werr)
		}
		return "", "", fmt.Errorf("record worktree: %w", err)
	}
	return branch, base, nil
}

// rollbackWorktree undoes an add that could not be recorded, so a failed run
// leaves the parent as it found it. Two --force flags also remove a locked
// registration, which a single one refuses.
func rollbackWorktree(repo, wt, branch string) {
	_, _ = gitRun(repo, "worktree", "remove", "--force", "--force", wt)
	_, _ = gitRun(repo, "worktree", "prune")
	_, _ = gitRun(repo, "branch", "-D", branch)
}

// RemoveWorktree deletes a run's checkout and the registration left in the
// parent, then proves the parent no longer names it (PRD §6.3). The branch
// stays: the PR is evidence, the checkout is scratch, and gc never deletes a
// branch.
//
// The proof is the point. git worktree prune exits 0 when a registration is
// locked even though it did not drop the entry, so the exit status says nothing
// about the fact this function promises. It reads `git worktree list` after the
// attempt instead, and an entry that survived is an error rather than a
// success with a stale parent.
//
// It is idempotent. A worktree that is already gone — the directory removed by
// hand, or a second cleanup after the first — still prunes, still proves, and
// succeeds, so a scheduled cleanup can run over every run without first asking
// which ones still have trees.
func RemoveWorktree(runDir string) error {
	rec, err := loadRecord(runDir)
	if err != nil {
		return err
	}
	if rec.Repo == "" {
		return fmt.Errorf("remove worktree: run record names no repo")
	}
	wt := filepath.Join(runDir, "worktree")
	_, removeErr := gitRun(rec.Repo, "worktree", "remove", "--force", "--force", wt)
	_, pruneErr := gitRun(rec.Repo, "worktree", "prune")
	listed, listErr := worktreeListed(rec.Repo, wt)
	if listErr != nil {
		return fmt.Errorf("remove worktree: %w", listErr)
	}
	if listed {
		// Naming what git said matters here: "still registered" without the
		// reason git kept it is a dead end for whoever cleans this up.
		return fmt.Errorf("remove worktree: %s is still registered in %s (remove: %v; prune: %v)",
			wt, rec.Repo, orNone(removeErr), orNone(pruneErr))
	}
	return nil
}

// orNone renders a nil error as "ok", so a failure message says which of the
// two git calls complained instead of leaving one of them blank.
func orNone(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// worktreeListed reports whether repo still registers path. Paths are compared
// cleaned, because git prints what it recorded and the caller may spell it
// differently — a trailing slash or a redundant separator is the same tree.
func worktreeListed(repo, path string) (bool, error) {
	out, err := gitRun(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("list worktrees: %w", err)
	}
	want := filepath.Clean(path)
	for _, line := range strings.Split(out, "\n") {
		if got, ok := strings.CutPrefix(line, "worktree "); ok && filepath.Clean(got) == want {
			return true, nil
		}
	}
	return false, nil
}

// CleanupWorktree removes the checkout a terminal run was given and appends
// worktree:remove with the exit code the run reached. It is the recorded step
// PRD §6.3 asks for, and it runs from Finish, which is the one place every
// terminal path passes through.
//
// A run with no terminal state never reaches Finish: its tree is still the work
// a resume continues, so it is left alone. That is also the answer to what
// --force would otherwise discard — uncommitted edits exist in a run whose
// process died, and that run is not cleaned here. A terminal run either
// committed to its branch or had its work rejected by the gate; the tree is
// scratch either way.
//
// It is a no-op for a run that never got a worktree. The ledger decides that,
// not the filesystem: a tree already removed by hand still needs its
// registration pruned and its removal recorded.
func CleanupWorktree(runDir string, code int) error {
	added, err := worktreeRecorded(runDir)
	if err != nil {
		return err
	}
	if !added {
		return nil
	}
	wt := filepath.Join(runDir, "worktree")
	data := worktreeRemove{Path: wt, ExitCode: code}
	if err := RemoveWorktree(runDir); err != nil {
		data.Error = err.Error()
		if aerr := appendWorktreeEvent(runDir, kindWorktreeRemove, data); aerr != nil {
			return fmt.Errorf("remove worktree: %w (recording it also failed: %v)", err, aerr)
		}
		return fmt.Errorf("remove worktree: %w", err)
	}
	if err := appendWorktreeEvent(runDir, kindWorktreeRemove, data); err != nil {
		return fmt.Errorf("record worktree removal: %w", err)
	}
	return nil
}

// worktreeRecorded reports whether the run's stream holds a worktree:add. A
// missing stream is a run that recorded nothing, which is the same answer as a
// run that never had a tree.
func worktreeRecorded(runDir string) (bool, error) {
	events, _, err := readLedger(filepath.Join(runDir, "run.ndjson"))
	if err != nil {
		return false, err
	}
	for _, e := range events {
		if e.Kind == kindWorktreeAdd {
			return true, nil
		}
	}
	return false, nil
}

// appendWorktreeEvent adds one worktree event to the run's stream. It opens the
// ledger for the single append rather than holding one for the run: these
// steps run at the start and the end of a run, and a long-lived handle across
// a whole run is state that a crash has to reason about.
func appendWorktreeEvent(runDir, kind string, data any) error {
	l, err := OpenLedger(filepath.Join(runDir, "run.ndjson"))
	if err != nil {
		return err
	}
	if err := l.Append(kind, "", data); err != nil {
		l.Close()
		return err
	}
	return l.Close()
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
//
// A bare or mirror parent is refused with its own sentence. It has no work tree
// to check, and letting `git status` report that fact reads as a broken parent
// rather than an unsupported one. Supporting bare parents changes what "clean
// parent" means and is its own ticket.
func requireCleanParent(repo string) error {
	inside, err := gitRun(repo, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return refuse("read the parent: %v", err)
	}
	if inside != "true" {
		return refuse("parent is bare: %s has no work tree", repo)
	}
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
