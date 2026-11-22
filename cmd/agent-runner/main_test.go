package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
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
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(ghScript(out, code)), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// ghScript is the body of a gh that prints out and exits with code. Naming it
// separately lets a test put one answer on a PATH that holds nothing else, which
// is how the plan stage is made to refuse for the host's reasons rather than for
// whatever this machine happens to have installed.
func ghScript(out string, code int) string {
	return "#!/bin/sh\nprintf '%s\\n' '" + out + "'\nexit " + strconv.Itoa(code) + "\n"
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
	Sandbox  string `json:"sandbox"`
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code"`
	Refusal  *struct {
		Code     string            `json:"code"`
		Where    string            `json:"where"`
		Reason   string            `json:"reason"`
		Evidence map[string]string `json:"evidence"`
	} `json:"refusal"`
}

// refusalEvent is the part of one refusal event these tests read back.
type refusalEvent struct {
	Kind  string `json:"kind"`
	Stage string `json:"stage"`
	Data  struct {
		Code     string            `json:"code"`
		Reason   string            `json:"reason"`
		Evidence map[string]string `json:"evidence"`
	} `json:"data"`
}

// refusalEvents reads the run's stream and returns every refusal event in it.
// The stream is the file a reader follows in order, so a test that only reads
// run.json cannot tell whether the run said anything as it stopped.
func refusalEvents(t *testing.T, dir string) []refusalEvent {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "run.ndjson"))
	if err != nil {
		t.Fatalf("read run.ndjson: %v", err)
	}
	var out []refusalEvent
	for i, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e refusalEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse run.ndjson line %d: %v", i+1, err)
		}
		if e.Kind == "refusal" {
			out = append(out, e)
		}
	}
	return out
}

// onlyRunDir digs the single run directory out of base.
func onlyRunDir(t *testing.T, base string) string {
	t.Helper()
	runs := filepath.Join(base, "agent-runner", "runs")
	entries, err := os.ReadDir(runs)
	if err != nil {
		t.Fatalf("read runs directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("runs directory holds %d entries, want 1", len(entries))
	}
	return filepath.Join(runs, entries[0].Name())
}

// onlyRun digs the single run directory out of base and returns its record.
func onlyRun(t *testing.T, base string) record {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(onlyRunDir(t, base), "run.json"))
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

// TestRunAuditedUnderASymlinkedHome is the follow-up blocker: $HOME is often
// a symlink, and a run in a directory under it must not have its own writes
// read as drift, any more than a run under a symlinked checkout may.
func TestRunAuditedUnderASymlinkedHome(t *testing.T) {
	realHome := t.TempDir()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(realHome, home); err != nil {
		t.Skipf("symlink: %v", err)
	}
	t.Setenv("HOME", home)
	runDir := filepath.Join(home, "project")
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	registry, err := tools.New(runDir)
	if err != nil {
		t.Fatalf("tools.New: %v", err)
	}

	if _, err := runAudited(context.Background(), runDir, "tidy up",
		writingLLM{path: filepath.Join(runDir, "note.txt")}, registry); err != nil {
		t.Fatalf("a write in the run directory was refused: %v", err)
	}
}

// TestSnapshotRefusesAHomeItDidNotWatch pins the guard: Take skips a root that
// does not exist without a word, so a home that was never sampled used to read
// as a clean run over a machine the audit had half seen.
func TestSnapshotRefusesAHomeItDidNotWatch(t *testing.T) {
	home := filepath.Join(t.TempDir(), "gone")
	t.Setenv("HOME", home)
	if _, err := snapshotHost(t.TempDir()); err == nil {
		t.Fatalf("a home that does not exist (%s) was reported as watched", home)
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
	t.Run("a repository git refused to describe", func(t *testing.T) {
		// A broken submodule gitdir says "not a git repository: <path>" with
		// exit 128 too. It is a failure to answer, not the absence of a
		// repository, and reading it as absence would drop the watch.
		stubPATH(t, map[string]string{"git": "#!/bin/sh\nprintf '%s\\n' 'fatal: not a git repository: /repo/.git/modules/sub' >&2\nexit 128\n"})
		if _, err := repoRoot(t.TempDir()); err == nil {
			t.Fatal("a repository git refused to describe read as \"no repository\"")
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

// TestWorkSandboxFlag is the CLI half of the package's rules: both modes
// complete a pipeline with the mode in run.json, an explicit container on a host
// that cannot build one exits 3 and leaves a record saying refused, and an
// explicit worktree runs and finishes done. bwrap is the host probe wearing a
// disguise — a stub that fails stands for a host without a namespace, one that
// succeeds for a host with it — so the table covers the mode a run got, not only
// the mode it asked for. The warning the weaker mode prints goes to stderr,
// which this test does not capture; the package test is where that sentence is
// pinned.
func TestWorkSandboxFlag(t *testing.T) {
	// The two hosts in the table: one refuses the namespace, one builds it.
	const (
		bwrapUnavailable = "#!/bin/sh\nexit 1\n"
		bwrapAvailable   = "#!/bin/sh\nexit 0\n"
	)
	tests := []struct {
		name        string
		mode        string
		bwrap       string // the host probe this row runs against; empty means unavailable
		wantCode    int
		wantSt      string
		wantSandbox string // the mode the record claims, empty when none was granted
		wantRefusal string // the refusal code, empty when the run did not refuse
	}{
		// Both modes completing is #87's done-when: the field is only worth
		// writing if a real pipeline puts it there.
		{name: "container accepted", mode: "container", bwrap: bwrapAvailable, wantCode: 0, wantSt: "done", wantSandbox: "container"},
		{name: "auto takes container when the host allows it", mode: "auto", bwrap: bwrapAvailable, wantCode: 0, wantSt: "done", wantSandbox: "container"},
		{name: "container refused without a namespace", mode: "container", wantCode: 3, wantSt: "refused", wantRefusal: "sandbox-unavailable"},
		{name: "worktree accepted", mode: "worktree", wantCode: 0, wantSt: "done", wantSandbox: "worktree"},
		// Auto is the case the record exists for: it asked for the strong mode
		// and got the weak one, so the record must say what was granted rather
		// than echo the request.
		{name: "auto falls back to worktree", mode: "auto", wantCode: 0, wantSt: "done", wantSandbox: "worktree"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			bwrap := tc.bwrap
			if bwrap == "" {
				bwrap = bwrapUnavailable
			}
			stubPATH(t, map[string]string{"gh": ghStub, "bwrap": bwrap})
			ref := "https://example.test/issues/56"
			args := []string{ref, "--run-dir", base, "--sandbox", tc.mode, "--dry-run"}
			if code := work(context.Background(), args); code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tc.wantCode)
			}
			rec := onlyRun(t, base)
			if rec.State != tc.wantSt {
				t.Errorf("state = %q, want %q", rec.State, tc.wantSt)
			}
			if rec.Sandbox != tc.wantSandbox {
				t.Errorf("sandbox = %q, want %q", rec.Sandbox, tc.wantSandbox)
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
			// The stream says the same thing, and says nothing when the run did
			// not refuse: an event on a completed run would read as a stop.
			events := refusalEvents(t, onlyRunDir(t, base))
			switch {
			case tc.wantRefusal == "" && len(events) != 0:
				t.Errorf("refusal events = %d, want none", len(events))
			case tc.wantRefusal != "" && len(events) != 1:
				t.Errorf("refusal events = %d, want exactly one", len(events))
			case tc.wantRefusal != "" && events[0].Data.Code != tc.wantRefusal:
				t.Errorf("refusal event code = %q, want %q", events[0].Data.Code, tc.wantRefusal)
			}
		})
	}
}

// ghClosedIssue, ghClaimedIssue and ghFixedIssue are the states the plan stage
// refuses on: the issue is over, another person already has it, or a pull
// request already closed it. The fixed fixture answers two questions, because
// the plan asks gh twice there — the issue names the link, the pull request
// says how it ended — and a stub with one answer would feed the issue's JSON
// to the second call.
const (
	ghClosedIssue  = `{"number":27,"title":"Record","state":"CLOSED","assignees":[]}`
	ghClaimedIssue = `{"number":27,"title":"Record","state":"OPEN","assignees":[{"login":"someone-else"}]}`
	ghFixedIssue   = `#!/bin/sh
case "$1 $2" in
"issue view") printf '%s\n' '{"number":27,"title":"Record","state":"OPEN","assignees":[],"closedByPullRequestsReferences":[{"number":41,"url":"https://example.test/pull/41"}]}' ;;
"pr view") printf '%s\n' '{"number":41,"state":"MERGED","url":"https://example.test/pull/41"}' ;;
*) printf 'unexpected gh call: %s %s\n' "$1" "$2" >&2; exit 1 ;;
esac
exit 0
`
	// ghOpenLinkedPR names the same link but the pull request is still open:
	// work in flight, which the plan stage must not read as already fixed.
	ghOpenLinkedPR = `#!/bin/sh
case "$1 $2" in
"issue view") printf '%s\n' '{"number":27,"title":"Record","state":"OPEN","assignees":[],"closedByPullRequestsReferences":[{"number":41,"url":"https://example.test/pull/41"}]}' ;;
"pr view") printf '%s\n' '{"number":41,"state":"OPEN","url":"https://example.test/pull/41"}' ;;
*) printf 'unexpected gh call: %s %s\n' "$1" "$2" >&2; exit 1 ;;
esac
exit 0
`
)

// refusalCase is one cause the plan stage can raise, fed through the command:
// what the host looks like, and what the run has to say afterwards.
type refusalCase struct {
	name     string
	path     map[string]string // exactly the programs on PATH; nothing means none
	gate     string            // the gate the plan is told to run; "" means none
	code     string            // the code in the record, the report and the stream
	reason   string            // the sentence every reader has to say, exactly
	evidence map[string]string // key to the value it must hold; "" wants a non-empty one
}

// TestWorkRecordsWhyItRefused is the ticket's claim at the binary, one row per
// cause the plan stage can raise: a run that refuses names its cause in run.json
// — code, stage, reason, evidence — the report repeats it in the same words, and
// the stream holds exactly one refusal event saying the same thing. Only causes
// the binary has a stage for get a row; a cause with nothing to raise it would
// prove the table compiles and nothing else.
func TestWorkRecordsWhyItRefused(t *testing.T) {
	tests := []refusalCase{
		{
			name:     "issue closed",
			path:     map[string]string{"gh": ghScript(ghClosedIssue, 0)},
			code:     "issue-closed",
			reason:   "issue is closed",
			evidence: map[string]string{"state": "CLOSED"},
		},
		{
			name:     "claimed by another",
			path:     map[string]string{"gh": ghScript(ghClaimedIssue, 0)},
			code:     "claimed-by-other",
			reason:   "issue is claimed by someone-else",
			evidence: map[string]string{"assignees": "someone-else"},
		},
		{
			name:     "already fixed",
			path:     map[string]string{"gh": ghFixedIssue},
			code:     "already-fixed",
			reason:   "issue was already fixed by merged PR #41",
			evidence: map[string]string{"pull_request": "https://example.test/pull/41", "state": "MERGED"},
		},
		{
			name:     "tool unavailable",
			path:     map[string]string{},
			code:     "tool-unavailable",
			reason:   "gh CLI is not available",
			evidence: map[string]string{"error": ""},
		},
		{
			name:     "one gate program missing",
			path:     map[string]string{"gh": ghStub},
			gate:     "go vet ./...",
			code:     "gate-unavailable",
			reason:   "gate cannot run: go is not on PATH",
			evidence: map[string]string{"gate": "go vet ./...", "missing": "go"},
		},
		{
			name:     "two gate programs missing",
			path:     map[string]string{"gh": ghStub},
			gate:     "go vet ./... && cloc .",
			code:     "gate-unavailable",
			reason:   "gate cannot run: go, cloc are not on PATH",
			evidence: map[string]string{"gate": "go vet ./... && cloc .", "missing": "go, cloc"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			stubPATH(t, tc.path)
			ref := "https://example.test/issues/27"
			args := []string{ref, "--run-dir", base, "--dry-run"}
			if tc.gate != "" {
				args = append(args, "--gate", tc.gate)
			}
			if code := work(context.Background(), args); code != 3 {
				t.Fatalf("exit code = %d, want 3", code)
			}
			rec := onlyRun(t, base)
			if rec.State != "refused" {
				t.Errorf("state = %q, want refused", rec.State)
			}
			dir, err := runrec.Locate(base, rec.ID)
			if err != nil {
				t.Fatalf("locate run: %v", err)
			}
			checkRefusal(t, dir, rec, tc)
		})
	}
}

// TestWorkProceedsPastAnUnmergedLinkedPR: a pull request that names the issue
// but has not merged is work in flight, not a fix. The plan stage asks the pull
// request its state and goes on; refusing there would make every issue with a
// PR in review unattendable, the opposite of what the cause is for.
func TestWorkProceedsPastAnUnmergedLinkedPR(t *testing.T) {
	base := t.TempDir()
	stubPATH(t, map[string]string{"gh": ghOpenLinkedPR})
	ref := "https://example.test/issues/27"
	if code := work(context.Background(), []string{ref, "--run-dir", base, "--dry-run"}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	rec := onlyRun(t, base)
	if rec.Refusal != nil {
		t.Errorf("refusal = %+v, want none", rec.Refusal)
	}
	if rec.State != "done" {
		t.Errorf("state = %q, want done", rec.State)
	}
}

// TestWorkProceedsWhenTheGateCanRun is the other half of the gate-unavailable
// row in TestWorkRecordsWhyItRefused: the check is about the
// host, not the command. A gate whose programs are on PATH is planned normally,
// whether or not those programs would pass — nothing here runs the gate, and a
// check that did would be the gate stage wearing a plan's clothes.
func TestWorkProceedsWhenTheGateCanRun(t *testing.T) {
	base := t.TempDir()
	stubPATH(t, map[string]string{"gh": ghStub, "go": "#!/bin/sh\nexit 0\n"})
	ref := "https://example.test/issues/27"
	if code := work(context.Background(), []string{ref, "--run-dir", base, "--dry-run", "--gate", "go vet ./..."}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	rec := onlyRun(t, base)
	if rec.Refusal != nil {
		t.Errorf("refusal = %+v, want none", rec.Refusal)
	}
	if rec.State != "done" {
		t.Errorf("state = %q, want done", rec.State)
	}
}

// checkRefusal asserts one refusal everywhere a reader meets it: the record, the
// report, and the stream. All three have to agree on the code, the stage and the
// words, or the record is a summary of a refusal rather than the refusal.
func checkRefusal(t *testing.T, dir string, rec record, tc refusalCase) {
	t.Helper()
	if rec.Refusal == nil {
		t.Fatal("no refusal recorded")
	}
	// The ticket's headline is "refusals exit 3", and run.json is where a reader
	// goes to learn what a run did: a process code with no record behind it is a
	// run whose directory lies about how it ended.
	switch {
	case rec.ExitCode == nil:
		t.Error("record has no exit code, want 3")
	case *rec.ExitCode != 3:
		t.Errorf("record exit code = %d, want 3", *rec.ExitCode)
	}
	if rec.Refusal.Code != tc.code {
		t.Errorf("refusal code = %q, want %q", rec.Refusal.Code, tc.code)
	}
	if rec.Refusal.Where != "plan" {
		t.Errorf("refusal stage = %q, want plan", rec.Refusal.Where)
	}
	// Exact, not a phrase: "the issue is not closed" carries "issue is closed"
	// and says the opposite, so a substring is not a check on the words.
	if rec.Refusal.Reason != tc.reason {
		t.Errorf("refusal reason = %q, want %q", rec.Refusal.Reason, tc.reason)
	}
	checkEvidence(t, "record", rec.Refusal.Evidence, tc.evidence)

	out, err := runrec.RenderReport(dir)
	if err != nil {
		t.Fatalf("render report: %v", err)
	}
	// The section, not the document: a line that matches somewhere else in the
	// report says nothing about what the refusal section holds. Every line below
	// has to be a whole line of that section.
	section := reportSection(t, string(out), "## Refused")
	want := []string{
		"- Stage: plan",
		"- Code: " + tc.code,
		"- Reason: " + rec.Refusal.Reason,
		"- Evidence:",
	}
	for key := range tc.evidence {
		// A case that cannot write the value down (an error from the host) still
		// has one in the record, and the report has to print that one.
		value := tc.evidence[key]
		if value == "" {
			value = rec.Refusal.Evidence[key]
		}
		if value == "" {
			t.Errorf("evidence %q is empty in the record; the report cannot carry it", key)
			continue
		}
		want = append(want, "  - "+key+": "+value)
	}
	for _, line := range want {
		if !hasLine(section, line) {
			t.Errorf("Refused section has no line %q:\n%s", line, section)
		}
	}

	events := refusalEvents(t, dir)
	if len(events) != 1 {
		t.Fatalf("refusal events = %d, want exactly one", len(events))
	}
	if events[0].Stage != "plan" {
		t.Errorf("refusal event stage = %q, want plan", events[0].Stage)
	}
	if events[0].Data.Code != tc.code {
		t.Errorf("refusal event code = %q, want %q", events[0].Data.Code, tc.code)
	}
	if events[0].Data.Reason != rec.Refusal.Reason {
		t.Errorf("refusal event reason = %q, want %q", events[0].Data.Reason, rec.Refusal.Reason)
	}
	checkEvidence(t, "refusal event", events[0].Data.Evidence, tc.evidence)
	// The stream and the record are two copies of one refusal, so the maps are
	// equal — not equal on the keys a case happened to name. A value the case
	// cannot write down (the host's error) has to be the same value in both, or
	// following the stream reaches a different story than reading run.json.
	if !maps.Equal(events[0].Data.Evidence, rec.Refusal.Evidence) {
		t.Errorf("refusal event evidence = %v, record holds %v", events[0].Data.Evidence, rec.Refusal.Evidence)
	}
}

// reportSection returns the body of the report's section with the given heading,
// up to the next heading.
func reportSection(t *testing.T, doc, heading string) string {
	t.Helper()
	_, body, ok := strings.Cut(doc, heading+"\n")
	if !ok {
		t.Fatalf("report has no %q section:\n%s", heading, doc)
	}
	if next := strings.Index(body, "\n## "); next >= 0 {
		body = body[:next]
	}
	return body
}

// hasLine reports whether section holds want as one whole line.
func hasLine(section, want string) bool {
	for _, line := range strings.Split(section, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

// checkEvidence is the evidence half of a refusal assertion: every key the case
// names is there with the value it names — or, when it names none, with any
// value at all, which is how a case pins an error it cannot write down.
func checkEvidence(t *testing.T, where string, got, want map[string]string) {
	t.Helper()
	for key, value := range want {
		have, ok := got[key]
		switch {
		case !ok:
			t.Errorf("%s evidence has no %q: %v", where, key, got)
		case have == "":
			t.Errorf("%s evidence %s is empty, want the reason that was read", where, key)
		case value != "" && have != value:
			t.Errorf("%s evidence %s = %q, want %q", where, key, have, value)
		}
	}
}

// gitAbsent is a git whose only answer is "there is no repository here": what a
// run in a temp directory sees, and what snapshotHost reads as nothing to
// watch rather than as a failed watch.
const gitAbsent = "#!/bin/sh\necho 'fatal: not a git repository (or any of the parent directories): .git' >&2\nexit 128\n"

// TestRunSandboxFlag is #95 at the binary: the mode the run got is the one the
// record names, a host that cannot build the namespace refuses at exit 3 with
// the reason in the record, and worktree mode still runs on the host.
func TestRunSandboxFlag(t *testing.T) {
	const (
		bwrapUnavailable = "#!/bin/sh\nexit 1\n"
		bwrapAvailable   = "#!/bin/sh\nexit 0\n"
		lsStub           = "#!/bin/sh\necho inside\n"
	)
	tests := []struct {
		name        string
		mode        string
		bwrap       string
		wantCode    int
		wantState   string
		wantSandbox string
		wantRefusal string
	}{
		{name: "container accepted", mode: "container", bwrap: bwrapAvailable, wantCode: 0, wantState: "done", wantSandbox: "container"},
		{name: "auto takes container", mode: "auto", bwrap: bwrapAvailable, wantCode: 0, wantState: "done", wantSandbox: "container"},
		{name: "container refused without a namespace", mode: "container", wantCode: 3, wantState: "refused", wantRefusal: "sandbox-unavailable"},
		{name: "worktree accepted", mode: "worktree", wantCode: 0, wantState: "done", wantSandbox: "worktree"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			inDir(t, t.TempDir())
			bwrap := tc.bwrap
			if bwrap == "" {
				bwrap = bwrapUnavailable
			}
			stubPATH(t, map[string]string{"git": gitAbsent, "bwrap": bwrap, "ls": lsStub})
			base := t.TempDir()
			if code := run([]string{"run", "--sandbox", tc.mode, "--run-dir", base, "list the files"}); code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tc.wantCode)
			}
			rec := onlyRun(t, base)
			if rec.Kind != "run" {
				t.Errorf("kind = %q, want run", rec.Kind)
			}
			if rec.State != tc.wantState {
				t.Errorf("state = %q, want %q", rec.State, tc.wantState)
			}
			if rec.Sandbox != tc.wantSandbox {
				t.Errorf("sandbox = %q, want %q", rec.Sandbox, tc.wantSandbox)
			}
			if rec.ExitCode == nil || *rec.ExitCode != tc.wantCode {
				t.Errorf("exit_code = %v, want %d", rec.ExitCode, tc.wantCode)
			}
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

// TestRunTaskIsNotReinterpreted is the other half of the usage line's claim:
// the flags come before the task, and the task is every word from the first
// non-flag on, unchanged. A sentence that contains "--sandbox" stays a
// sentence — lifting the word out would switch the mode and drop words from the
// recorded subject, which is the silent misread this ticket exists to remove.
func TestRunTaskIsNotReinterpreted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	inDir(t, t.TempDir())
	stubPATH(t, map[string]string{
		"git":   gitAbsent,
		"bwrap": "#!/bin/sh\nexit 0\n",
		"ls":    "#!/bin/sh\necho inside\n",
	})
	base := t.TempDir()
	// worktree is the word in the sentence and auto answers container on this
	// stub host, so the mode in the record shows whether the word was taken.
	const task = "explain why --sandbox worktree is not a mode"
	if code := run([]string{"run", "--run-dir", base, "explain", "why", "--sandbox", "worktree", "is", "not", "a", "mode"}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	rec := onlyRun(t, base)
	if rec.Subject != task {
		t.Errorf("subject = %q, want %q", rec.Subject, task)
	}
	if rec.Sandbox != "container" {
		t.Errorf("sandbox = %q, want container from auto: the task word must not switch the mode", rec.Sandbox)
	}
}

// TestRunDoubleDashKeepsADashTask pins the escape the usage line advertises:
// "--" ends the flags, so a task that begins with a dash word is text and not a
// mode switch.
func TestRunDoubleDashKeepsADashTask(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	inDir(t, t.TempDir())
	stubPATH(t, map[string]string{
		"git":   gitAbsent,
		"bwrap": "#!/bin/sh\nexit 0\n",
		"ls":    "#!/bin/sh\necho inside\n",
	})
	base := t.TempDir()
	const task = "--sandbox is a word here"
	if code := run([]string{"run", "--run-dir", base, "--", "--sandbox", "is", "a", "word", "here"}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	rec := onlyRun(t, base)
	if rec.Subject != task {
		t.Errorf("subject = %q, want %q", rec.Subject, task)
	}
	if rec.Sandbox != "container" {
		t.Errorf("sandbox = %q, want container from auto: the word after -- is text", rec.Sandbox)
	}
}

// TestRefusalRecordedKeepsARefusedRunGoing pins the caller's rule, which is the
// half of #59 a test of the record alone cannot see: a refusal the record holds
// goes on to Finish even when its event was lost, and a refusal the record
// cannot hold stops the caller instead of writing an outcome with no reason.
func TestRefusalRecordedKeepsARefusedRunGoing(t *testing.T) {
	r, err := runrec.New(filepath.Join(t.TempDir(), "run"), runrec.Meta{Kind: "run", Subject: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	led, err := runrec.OpenLedger(filepath.Join(r.Dir, "run.ndjson"))
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	// A closed stream stands in for a write that fails: the refusal is in
	// run.json by then, and only its event has nowhere to land.
	if err := led.Close(); err != nil {
		t.Fatalf("close ledger: %v", err)
	}
	r.SetLedger(led)

	if !refusalRecorded(r, refuse.New(refuse.SandboxUnavailable, "sandbox", "no namespace on this host")) {
		t.Fatal("a lost event must not stop the run before Finish")
	}
	if refusalRecorded(r, refuse.New(refuse.Code("not-in-the-vocabulary"), "sandbox", "x")) {
		t.Fatal("a refusal the record cannot hold must stop the caller")
	}
	if !refusalRecorded(nil, nil) {
		t.Fatal("an unrecorded run has nothing to stop for")
	}
}

// TestRunRecordsARefusedNamespace is the record side of the hole Detect cannot
// see: bwrap answers its own probe and then refuses this run's mounts. The run
// is refused at exit 3 with a code in run.json, not recorded as a container run
// whose only command happened to fail.
func TestRunRecordsARefusedNamespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	inDir(t, t.TempDir())
	stubPATH(t, map[string]string{
		"git":   gitAbsent,
		"bwrap": "#!/bin/sh\ncase \"$*\" in\n*--unshare-all*)\techo 'bwrap: setting up uid map: Permission denied' >&2; exit 1;;\nesac\nexit 0\n",
		"ls":    "#!/bin/sh\necho inside\n",
	})
	base := t.TempDir()
	if code := run([]string{"run", "--sandbox", "container", "--run-dir", base, "list the files"}); code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	rec := onlyRun(t, base)
	if rec.State != "refused" {
		t.Errorf("state = %q, want refused", rec.State)
	}
	if rec.Refusal == nil {
		t.Fatal("no refusal recorded: the record says container and nothing else")
	}
	if rec.Refusal.Code != "sandbox-unavailable" {
		t.Errorf("refusal code = %q, want sandbox-unavailable", rec.Refusal.Code)
	}
	if !strings.Contains(rec.Refusal.Reason, "Permission denied") {
		t.Errorf("refusal reason = %q, want bwrap's own words", rec.Refusal.Reason)
	}
	// The refusal is decided before the run has a command to make, and it is
	// still an event: the stream exists from the record, not from the first
	// command, so a reader following it sees the stop.
	events := refusalEvents(t, onlyRunDir(t, base))
	if len(events) != 1 {
		t.Fatalf("refusal events = %d, want exactly one", len(events))
	}
	if events[0].Stage != "sandbox" {
		t.Errorf("refusal event stage = %q, want sandbox", events[0].Stage)
	}
	if events[0].Data.Code != "sandbox-unavailable" {
		t.Errorf("refusal event code = %q, want sandbox-unavailable", events[0].Data.Code)
	}
}
