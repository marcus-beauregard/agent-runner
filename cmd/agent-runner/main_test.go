package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/agent"
	"github.com/marcus-beauregard/agent-runner/internal/audit"
	"github.com/marcus-beauregard/agent-runner/internal/refuse"
	"github.com/marcus-beauregard/agent-runner/internal/report"
	runrec "github.com/marcus-beauregard/agent-runner/internal/run"
	"github.com/marcus-beauregard/agent-runner/internal/tools"
	"github.com/marcus-beauregard/agent-runner/internal/world"
)

// stubGH puts a gh on PATH that prints out and exits with code, so the tests
// exercise the real work path without a network or a GitHub account.
func stubGH(t *testing.T, out string, code int) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' '" + out + "'\nexit " + strconv.Itoa(code) + "\n"
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stubPATH puts exactly these scripts on PATH, alone: detection and the plan
// must both answer from the host state the test built, not from whatever this
// machine happens to have installed.
func stubPATH(t *testing.T, scripts map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	t.Setenv("PATH", dir)
}

// ghStub is a gh that prints one issue, so the plan path runs without a network.
const ghStub = `#!/bin/sh
printf '%s\n' '{"number":56,"title":"Sandbox","state":"OPEN","assignees":[]}'
exit 0
`

// record is the part of run.json these tests read back.
type record struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Subject  string `json:"subject"`
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code"`
	Refusal  *struct {
		Code     string            `json:"code"`
		Where    string            `json:"where"`
		Reason   string            `json:"reason"`
		Evidence map[string]string `json:"evidence"`
	} `json:"refusal"`
}

// onlyRun digs the single run directory out of base and returns its record.
func onlyRun(t *testing.T, base string) record {
	t.Helper()
	runs := filepath.Join(base, "agent-runner", "runs")
	entries, err := os.ReadDir(runs)
	if err != nil {
		t.Fatalf("read runs directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("runs directory holds %d entries, want 1", len(entries))
	}
	b, err := os.ReadFile(filepath.Join(runs, entries[0].Name(), "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("parse run.json: %v", err)
	}
	return rec
}

// TestWorkRecordsBothOutcomes is the ticket's claim: with --run-dir, the run is
// recorded before planning and finished on the path it actually took, so the
// record of a refusal does not read as a run still in flight.
func TestWorkRecordsBothOutcomes(t *testing.T) {
	open := `{"number":27,"title":"Record","state":"OPEN","assignees":[]}`
	tests := []struct {
		name     string
		ghOut    string
		ghCode   int
		wantCode int
		wantSt   string
	}{
		{name: "success", ghOut: open, ghCode: 0, wantCode: 0, wantSt: "done"},
		{name: "refusal", ghOut: "", ghCode: 1, wantCode: 3, wantSt: "refused"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			stubGH(t, tc.ghOut, tc.ghCode)
			ref := "https://example.test/issues/27"
			code := work(context.Background(), []string{ref, "--run-dir", base, "--dry-run"})
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tc.wantCode)
			}
			rec := onlyRun(t, base)
			if rec.State != tc.wantSt {
				t.Errorf("state = %q, want %q", rec.State, tc.wantSt)
			}
			if rec.ExitCode == nil || *rec.ExitCode != tc.wantCode {
				t.Errorf("exit_code = %v, want %d", rec.ExitCode, tc.wantCode)
			}
			if rec.Subject != ref {
				t.Errorf("subject = %q, want %q", rec.Subject, ref)
			}
			if rec.Kind != "build" {
				t.Errorf("kind = %q, want build", rec.Kind)
			}
			if rec.ID == "" {
				t.Error("id is empty")
			}
		})
	}
}

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what it
// wrote, so a command whose whole job is printing is tested on its output, not
// only on its exit code.
func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = wr
	code := fn()
	wr.Close()
	os.Stdout = old
	out, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(out), code
}

// captureStderr is captureStdout's twin, for the lines a refusal prints where
// a person will read them. Both replace the file the process writes to, so the
// command under test stays unaware of the capture.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = wr
	fn()
	wr.Close()
	os.Stderr = old
	out, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(out)
}

// inDir runs the rest of the test from dir, then puts the working directory
// back. run() takes no directory argument — the process cwd is what a test has
// to move — so the restore is a cleanup rather than a defer, to survive a
// failing assertion.
func inDir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatalf("chdir back to %s: %v", old, err)
		}
	})
}

// writingLLM is a model that writes a file before it answers. It hands the loop
// the one thing the toolset refuses — a write outside the working directory —
// so the audit is what has to catch it.
type writingLLM struct {
	path string
}

var _ agent.LLM = writingLLM{}

func (w writingLLM) Complete(ctx context.Context, prompt string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(w.path, []byte("escape\n"), 0o600); err != nil {
		return "", err
	}
	return "DONE", nil
}

// TestRunRefusesAHostWrite is #85 at the loop: a write outside the run directory
// is refused, named on stderr, and exits 3 — not printed as a successful run
// with an escape file left behind it.
func TestRunRefusesAHostWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wd := t.TempDir()
	registry, err := tools.New(wd)
	if err != nil {
		t.Fatalf("tools.New: %v", err)
	}
	// A real home already has ~/.config; without it the audit would name the
	// directory that appeared rather than the file written inside it, which is
	// a different (and shallower) finding than the escape being tested.
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o700); err != nil {
		t.Fatalf("prepare home: %v", err)
	}
	escape := filepath.Join(home, ".config", "agent-runner-escape")

	var got error
	stderr := captureStderr(t, func() {
		_, got = runAudited(context.Background(), wd, "tidy up", writingLLM{path: escape}, registry)
	})
	if got == nil {
		t.Fatal("the run reported success after a write outside its directory")
	}
	var refusal *refuse.Refusal
	if !errors.As(got, &refusal) {
		t.Fatalf("error = %v, want a *refuse.Refusal", got)
	}
	if refusal.Code != refuse.OutsideRunDir {
		t.Errorf("refusal code = %q, want %q", refusal.Code, refuse.OutsideRunDir)
	}
	if refusal.Where != "audit" {
		t.Errorf("refusal stage = %q, want audit", refusal.Where)
	}
	if code := report.ExitCode(got); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if !strings.Contains(stderr, escape) {
		t.Errorf("stderr does not name %s:\n%s", escape, stderr)
	}
	if _, ok := refusal.Evidence[escape]; !ok {
		t.Errorf("evidence does not carry %s: %v", escape, refusal.Evidence)
	}
}

// TestCleanRunHasNoDrift is the other half: the same audit around a loop that
// writes only where it should exits 0 and prints its result. The snapshot is
// asserted non-empty too — an audit that watched nothing would pass the drift
// check while proving nothing at all.
func TestCleanRunHasNoDrift(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wd := t.TempDir()
	inDir(t, wd)

	snap, err := snapshotHost(wd)
	if err != nil {
		t.Fatalf("snapshotHost: %v", err)
	}
	if len(snap) == 0 {
		t.Fatal("the audit watched nothing: every root was excluded")
	}

	out, code := captureStdout(t, func() int { return run([]string{"run", "list the files"}) })
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "\"done\": true") {
		t.Errorf("clean run did not report a finished result:\n%s", out)
	}
}

// gitRepo makes a directory a real repository, so the tests ask repoRoot the
// question a run asks instead of assuming the answer.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "--quiet", dir).CombinedOutput(); err != nil {
		t.Skipf("git init: %v: %s", err, out)
	}
	return dir
}

// TestSnapshotWatchesTheRepositoryAroundTheRun is the first half of the blocker
// on #90: with the run directory inside the checkout, the checkout is in the
// snapshot, so a write beside the run directory is drift and a write inside it
// is not.
func TestSnapshotWatchesTheRepositoryAroundTheRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)
	runDir := filepath.Join(repo, "work")
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}

	before, err := snapshotHost(runDir)
	if err != nil {
		t.Fatalf("snapshotHost: %v", err)
	}
	if _, ok := before[repo]; !ok {
		t.Fatalf("the checkout %s is not watched (%d roots in the snapshot)", repo, len(before))
	}

	if err := os.WriteFile(filepath.Join(runDir, "scratch.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatalf("write in the run directory: %v", err)
	}
	stray := filepath.Join(repo, "stray.txt")
	if err := os.WriteFile(stray, []byte("not mine\n"), 0o600); err != nil {
		t.Fatalf("write beside the run directory: %v", err)
	}
	after, err := snapshotHost(runDir)
	if err != nil {
		t.Fatalf("snapshotHost: %v", err)
	}
	seen := make(map[string]bool, len(audit.Compare(before, after)))
	for _, d := range audit.Compare(before, after) {
		seen[d.Path] = true
	}
	if !seen[stray] {
		t.Errorf("drift did not name %s: %v", stray, seen)
	}
	for path := range seen {
		if path == runDir || strings.HasPrefix(path, runDir+string(filepath.Separator)) {
			t.Errorf("the run's own write under %s was reported as drift: %v", runDir, seen)
		}
	}
}

// TestSnapshotAtTheRepositoryRootClaimsNoRepositoryWatch is the second half: at
// the checkout root the whole tree is the run's to write, so no repository root
// is claimed — and a write in it is not drift. The alternative reading, that the
// checkout is watched and every write in it is an escape, would refuse the
// ordinary way to run the command.
func TestSnapshotAtTheRepositoryRootClaimsNoRepositoryWatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)

	before, err := snapshotHost(repo)
	if err != nil {
		t.Fatalf("snapshotHost: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("the audit watched nothing at all")
	}
	for path := range before {
		if path == repo || strings.HasPrefix(path, repo+string(filepath.Separator)) {
			t.Errorf("the checkout is in the snapshot as %s, but it is the run directory", path)
		}
	}

	if err := os.WriteFile(filepath.Join(repo, "written-by-the-run.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write in the run directory: %v", err)
	}
	after, err := snapshotHost(repo)
	if err != nil {
		t.Fatalf("snapshotHost: %v", err)
	}
	if drift := audit.Compare(before, after); len(drift) != 0 {
		t.Errorf("a write in the run directory was drift: %v", drift)
	}
}

// TestSnapshotFollowsTheRunDirectoryThroughASymlink is the review's first
// blocker: os.Getwd keeps the symlink the caller walked through while git
// prints the physical path, so the ignore and the repository walk named one
// directory two ways and the run's own writes came back as drift.
func TestSnapshotFollowsTheRunDirectoryThroughASymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)
	work := filepath.Join(repo, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(work, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	before, err := snapshotHost(link)
	if err != nil {
		t.Fatalf("snapshotHost: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "scratch.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatalf("write in the run directory: %v", err)
	}
	stray := filepath.Join(repo, "stray.txt")
	if err := os.WriteFile(stray, []byte("not mine\n"), 0o600); err != nil {
		t.Fatalf("write beside the run directory: %v", err)
	}
	after, err := snapshotHost(link)
	if err != nil {
		t.Fatalf("snapshotHost: %v", err)
	}
	seen := make(map[string]bool)
	for _, d := range audit.Compare(before, after) {
		seen[d.Path] = true
	}
	if !seen[stray] {
		t.Errorf("drift did not name %s: %v", stray, seen)
	}
	for path := range seen {
		if path == work || strings.HasPrefix(path, work+string(filepath.Separator)) {
			t.Errorf("the run's own write under %s was reported as drift: %v", work, seen)
		}
	}
}

// TestRepoRootIgnoresTheCallersGitEnvironment is the second blocker: an
// exported GIT_DIR or GIT_CEILING_DIRECTORIES used to make a checkout inside
// the repository answer "not a git repository", so the run reported clean over
// a watch that was never established.
func TestRepoRootIgnoresTheCallersGitEnvironment(t *testing.T) {
	repo := gitRepo(t)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", repo)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere", ".git"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())

	root, err := repoRoot(sub)
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	if !sameDir(root, repo) {
		t.Errorf("repoRoot = %q, want %q", root, repo)
	}
}

// TestRunAuditedInsideACheckout is the composition neither half-test covered:
// the snapshots and the loop together in a real repository, where the run's own
// write in its directory is not drift and a write beside it is a refusal.
func TestRunAuditedInsideACheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)
	runDir := filepath.Join(repo, "work")
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	registry, err := tools.New(runDir)
	if err != nil {
		t.Fatalf("tools.New: %v", err)
	}

	t.Run("own write", func(t *testing.T) {
		_, err := runAudited(context.Background(), runDir, "tidy up",
			writingLLM{path: filepath.Join(runDir, "note.txt")}, registry)
		if err != nil {
			t.Fatalf("a write in the run directory was refused: %v", err)
		}
	})
	t.Run("write beside it", func(t *testing.T) {
		escape := filepath.Join(repo, "stray.txt")
		var got error
		captureStderr(t, func() {
			_, got = runAudited(context.Background(), runDir, "tidy up", writingLLM{path: escape}, registry)
		})
		var refusal *refuse.Refusal
		if !errors.As(got, &refusal) {
			t.Fatalf("error = %v, want a *refuse.Refusal", got)
		}
		if refusal.Code != refuse.OutsideRunDir {
			t.Errorf("refusal code = %q, want %q", refusal.Code, refuse.OutsideRunDir)
		}
		if _, ok := refusal.Evidence[escape]; !ok {
			t.Errorf("evidence does not carry %s: %v", escape, refusal.Evidence)
		}
	})
}

// TestRepoRootTellsAbsenceFromFailure pins the distinction the review asked for:
// git answering "not a repository" leaves nothing to watch, while git failing to
// answer is a watch that was never established and has to stop the run rather
// than report it clean.
func TestRepoRootTellsAbsenceFromFailure(t *testing.T) {
	t.Run("not a repository", func(t *testing.T) {
		stubPATH(t, map[string]string{"git": "#!/bin/sh\nprintf '%s\\n' 'fatal: not a git repository (or any of the parent directories): .git' >&2\nexit 128\n"})
		root, err := repoRoot(t.TempDir())
		if err != nil {
			t.Fatalf("repoRoot: %v", err)
		}
		if root != "" {
			t.Errorf("repoRoot = %q, want empty", root)
		}
	})
	t.Run("git failed", func(t *testing.T) {
		stubPATH(t, map[string]string{"git": "#!/bin/sh\nprintf '%s\\n' 'fatal: cannot read config' >&2\nexit 1\n"})
		if _, err := repoRoot(t.TempDir()); err == nil {
			t.Fatal("a failed git lookup read as \"no repository\"")
		}
	})
	t.Run("git is missing", func(t *testing.T) {
		stubPATH(t, map[string]string{})
		if _, err := repoRoot(t.TempDir()); err == nil {
			t.Fatal("a missing git read as \"no repository\"")
		}
	})
}

// TestReportCommand is the CLI half of the report: a recorded run prints the
// document, an id that does not exist exits 1, and asking for nothing exits 1.
func TestReportCommand(t *testing.T) {
	base := t.TempDir()
	r, err := runrec.New(base, runrec.Meta{Kind: "build", Subject: "https://example.test/issues/33"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Finish(0); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	out, code := captureStdout(t, func() int { return reportRun([]string{r.ID, "--run-dir", base}) })
	if code != 0 {
		t.Errorf("report on a recorded run = %d, want 0", code)
	}
	for _, want := range []string{"# Run " + r.ID, "- State: done", "## Left to do"} {
		if !strings.Contains(out, want) {
			t.Errorf("report printed without %q\n---\n%s", want, out)
		}
	}
	if code := reportRun([]string{"20260101-000000.000000-nope", "--run-dir", base}); code != 1 {
		t.Errorf("report on an unknown id = %d, want 1", code)
	}
	if code := reportRun(nil); code != 1 {
		t.Errorf("report with no id = %d, want 1", code)
	}
}

// TestWorkWithoutRunDirWritesNothing keeps the old contract: a dry run with no
// --run-dir records nothing anywhere.
func TestWorkWithoutRunDirWritesNothing(t *testing.T) {
	base := t.TempDir()
	stubGH(t, `{"number":27,"title":"Record","state":"OPEN","assignees":[]}`, 0)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))

	if code := work(context.Background(), []string{"https://example.test/issues/27", "--dry-run"}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, dir := range []string{base, filepath.Join(base, "state")} {
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read %s: %v", dir, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s holds %d entries, want none", dir, len(entries))
		}
	}
}

// TestReplayCommand is the exit-code side of the ticket: a record that replays
// cleanly exits 0, one whose recorded set disagrees with its own world exits 1,
// and a directory that does not name its account is refused rather than guessed.
func TestReplayCommand(t *testing.T) {
	snapshot := world.World{
		Subject:  "https://example.test/pull/8",
		Issue:    world.Issue{Number: 7, State: "OPEN"},
		PR:       world.PR{Number: 8, State: "open", Mergeable: world.MergeableNo},
		Comments: []world.Comment{{ID: 100, Author: "someone"}},
	}
	tests := []struct {
		name       string
		account    string
		spend      bool
		unfinished bool
		wantCode   int
	}{
		{name: "matching record", account: "marcus-beauregard", spend: true, wantCode: 0},
		{name: "record disagrees", account: "marcus-beauregard", wantCode: 1},
		{name: "no account recorded", wantCode: 1},
		// A live run has spent only part of its plan. Replay cannot call that a
		// mismatch, and it cannot pass it either: no verdict, exit 1.
		{name: "unfinished record", account: "marcus-beauregard", spend: true, unfinished: true, wantCode: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := runrec.New(t.TempDir(), runrec.Meta{Kind: "resolve", Subject: snapshot.Subject, Account: tc.account})
			if err != nil {
				t.Fatalf("create run: %v", err)
			}
			if err := runrec.SaveWorld(r.Dir, snapshot); err != nil {
				t.Fatalf("save world: %v", err)
			}
			if tc.spend {
				l, err := runrec.OpenLedger(filepath.Join(r.Dir, "run.ndjson"))
				if err != nil {
					t.Fatalf("open ledger: %v", err)
				}
				if err := l.Spend("comment:100", "answered #100"); err != nil {
					t.Fatalf("spend: %v", err)
				}
				if err := l.Close(); err != nil {
					t.Fatalf("close ledger: %v", err)
				}
			}
			if !tc.unfinished {
				if err := r.Finish(0); err != nil {
					t.Fatalf("finish run: %v", err)
				}
			}
			if code := run([]string{"replay", r.Dir}); code != tc.wantCode {
				t.Errorf("replay exit = %d, want %d", code, tc.wantCode)
			}
		})
	}
}

// TestWorkSandboxFlag is the CLI half of the package's rules: an explicit
// container on a host that cannot build one exits 3 and leaves a record saying
// refused, while an explicit worktree runs and finishes done. The warning the
// weaker mode prints goes to stderr, which this test does not capture — the
// package test is where that sentence is pinned.
func TestWorkSandboxFlag(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		wantCode    int
		wantSt      string
		wantRefusal string // the refusal code, empty when the run did not refuse
	}{
		{name: "container refused without a namespace", mode: "container", wantCode: 3, wantSt: "refused", wantRefusal: "sandbox-unavailable"},
		{name: "worktree accepted", mode: "worktree", wantCode: 0, wantSt: "done"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			stubPATH(t, map[string]string{"gh": ghStub, "bwrap": "#!/bin/sh\nexit 1\n"})
			ref := "https://example.test/issues/56"
			args := []string{ref, "--run-dir", base, "--sandbox", tc.mode, "--dry-run"}
			if code := work(context.Background(), args); code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tc.wantCode)
			}
			rec := onlyRun(t, base)
			if rec.State != tc.wantSt {
				t.Errorf("state = %q, want %q", rec.State, tc.wantSt)
			}
			if rec.ExitCode == nil || *rec.ExitCode != tc.wantCode {
				t.Errorf("exit_code = %v, want %d", rec.ExitCode, tc.wantCode)
			}
			// The state says the run stopped; the refusal says why, and a run
			// that did not stop must not carry one.
			switch {
			case tc.wantRefusal == "" && rec.Refusal != nil:
				t.Errorf("refusal = %+v, want none", rec.Refusal)
			case tc.wantRefusal != "" && rec.Refusal == nil:
				t.Error("no refusal recorded, want one")
			case tc.wantRefusal != "" && rec.Refusal.Code != tc.wantRefusal:
				t.Errorf("refusal code = %q, want %q", rec.Refusal.Code, tc.wantRefusal)
			}
		})
	}
}

// TestWorkRecordsWhyItRefused is the ticket's claim at the binary: a run that
// refuses names its cause in run.json — code, stage, reason, evidence — and the
// report repeats it, instead of the record saying only that it stopped.
func TestWorkRecordsWhyItRefused(t *testing.T) {
	closed := `{"number":27,"title":"Record","state":"CLOSED","assignees":[]}`
	base := t.TempDir()
	stubGH(t, closed, 0)
	ref := "https://example.test/issues/27"
	if code := work(context.Background(), []string{ref, "--run-dir", base, "--dry-run"}); code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	rec := onlyRun(t, base)
	if rec.Refusal == nil {
		t.Fatal("no refusal recorded")
	}
	if rec.Refusal.Code != "issue-closed" {
		t.Errorf("refusal code = %q, want issue-closed", rec.Refusal.Code)
	}
	if rec.Refusal.Where != "plan" {
		t.Errorf("refusal stage = %q, want plan", rec.Refusal.Where)
	}
	if !strings.Contains(rec.Refusal.Reason, "closed") {
		t.Errorf("refusal reason = %q, want it to say the issue is closed", rec.Refusal.Reason)
	}
	if got := rec.Refusal.Evidence["state"]; got != "CLOSED" {
		t.Errorf("evidence state = %q, want CLOSED", got)
	}
	dir, err := runrec.Locate(base, rec.ID)
	if err != nil {
		t.Fatalf("locate run: %v", err)
	}
	out, err := runrec.RenderReport(dir)
	if err != nil {
		t.Fatalf("render report: %v", err)
	}
	if !strings.Contains(string(out), "- Code: issue-closed") {
		t.Errorf("report does not name the refusal:\n%s", out)
	}
}
