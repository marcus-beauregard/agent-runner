package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/refuse"
	runrec "github.com/marcus-beauregard/agent-runner/internal/run"
)

// A run's ledger is what the registry is handed in production; keep the
// interface honest.
var _ Ledger = (*runrec.Ledger)(nil)

// newRegistry builds a Registry rooted in a fresh temp directory with the
// fixtures the ported tests expect.
func newRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"package.json": "{\n  \"name\": \"agent-runner\"\n}\n",
		"README.md":    "agent-runner\n",
		"inside.txt":   "inside",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func run(t *testing.T, r *Registry, cmd string) (string, error) {
	t.Helper()
	return r.Run(context.Background(), "run_command", map[string]any{"command": cmd})
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected error containing %q, got %q", substr, err.Error())
	}
}

func TestRefusesShellChaining(t *testing.T) {
	r := newRegistry(t)
	for _, c := range []string{
		"ls; cat /etc/passwd",
		"ls && cat /etc/passwd",
		"cat package.json | wc -l",
		"ls $(id)",
		"ls `id`",
		"cat package.json > out.txt",
	} {
		_, err := run(t, r, c)
		wantErr(t, err, "shell syntax is not supported")
	}
}

func TestRefusesInterpreters(t *testing.T) {
	r := newRegistry(t)
	_, err := run(t, r, `node -e "require('fs')"`)
	wantErr(t, err, "command not allowed: node")
	_, err = run(t, r, `python3 -c "print(1)"`)
	wantErr(t, err, "command not allowed: python3")
}

func TestRefusesLauncherFlags(t *testing.T) {
	r := newRegistry(t)
	for _, c := range []struct{ cmd, msg string }{
		{"git -c core.pager=cat log", "git: subcommand not allowed"},
		{"git log --exec-path=/tmp", "argument not allowed"},
		{"cat -n package.json", "cat: argument not allowed: -n"},
		{"ls --color=always", "ls: argument not allowed"},
	} {
		_, err := run(t, r, c.cmd)
		wantErr(t, err, c.msg)
	}
}

func TestRefusesPathsOutside(t *testing.T) {
	r := newRegistry(t)
	_, err := run(t, r, "cat /etc/passwd")
	wantErr(t, err, "path escapes the working directory")
	_, err = run(t, r, "cat ../../etc/passwd")
	wantErr(t, err, "path escapes the working directory")
}

func TestRefusesControlCharacters(t *testing.T) {
	r := newRegistry(t)
	_, err := run(t, r, "ls\ncat package.json")
	wantErr(t, err, "invalid character in command")
	_, err = run(t, r, "cat \"line\nfeed\"")
	wantErr(t, err, "invalid character in command")
}

func TestRefusesSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "inside.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(work, "link.txt")); err != nil {
		t.Fatal(err)
	}
	r, err := New(work)
	if err != nil {
		t.Fatal(err)
	}
	_, err = run(t, r, "cat link.txt")
	wantErr(t, err, "path escapes the working directory")
	out, err := run(t, r, "cat inside.txt")
	if err != nil {
		t.Fatal(err)
	}
	if out != "inside" {
		t.Fatalf("got %q, want %q", out, "inside")
	}
}

// gitRun runs git as an argv array for test setup only — the allowlist is what
// is under test, not the fixture.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// newGitRegistry returns a Registry rooted in a committed git repository.
func newGitRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("agent-runner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{\n  \"name\": \"agent-runner\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	r, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunsAllowlisted(t *testing.T) {
	r := newGitRegistry(t)

	out, err := run(t, r, "ls -1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "package.json") {
		t.Fatalf("ls -1 output %q lacks package.json", out)
	}

	out, err = run(t, r, "wc -l package.json")
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.TrimLeft(out, " \t\r\n"); s == "" || s[0] < '0' || s[0] > '9' {
		t.Fatalf("wc -l output %q does not start with a count", out)
	}

	if _, err = run(t, r, "git status --short"); err != nil {
		t.Fatal(err)
	}

	out, err = run(t, r, `cat "package.json"`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "agent-runner") {
		t.Fatalf("cat output %q lacks agent-runner", out)
	}
}

func TestRefusesGitOperandsOutside(t *testing.T) {
	r := newGitRegistry(t)
	_, err := run(t, r, "git status /etc/passwd")
	wantErr(t, err, "path escapes the working directory")
	_, err = run(t, r, "git diff ../../outside")
	wantErr(t, err, "path escapes the working directory")
	out, err := run(t, r, "git show HEAD:README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "agent-runner") {
		t.Fatalf("git show output %q lacks agent-runner", out)
	}
}

// read_file and write_file must be confined by the same function as
// run_command. These tests cover the gap that existed in the JavaScript port.
func TestReadWriteFileConfinement(t *testing.T) {
	r := newRegistry(t)

	_, err := r.Run(context.Background(), "read_file", map[string]any{"path": "/etc/passwd"})
	wantErr(t, err, "path escapes the working directory")
	_, err = r.Run(context.Background(), "write_file", map[string]any{"path": "../out.txt", "content": "x"})
	// A write that leaves the root is a refusal, not an error string: the run
	// stops on it, and the report has to be able to name what was attempted.
	var ref *refuse.Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("write_file outside the root = %v, want a *refuse.Refusal", err)
	}
	if ref.Code != refuse.OutsideWriteScope {
		t.Errorf("refusal code = %q, want %q", ref.Code, refuse.OutsideWriteScope)
	}
	if got := ref.Evidence["path"]; got != "../out.txt" {
		t.Errorf("evidence path = %q, want ../out.txt", got)
	}
	// The check that refused it is in the evidence, so a reader can see which
	// rule the path broke without re-running the tool.
	if got := ref.Evidence["check"]; !strings.Contains(got, "path escapes the working directory") {
		t.Errorf("evidence check = %q, want the path rule that refused it", got)
	}

	out, err := r.Run(context.Background(), "write_file", map[string]any{"path": "new.txt", "content": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "wrote new.txt" {
		t.Fatalf("write_file returned %q", out)
	}
	out, err = r.Run(context.Background(), "read_file", map[string]any{"path": "new.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello" {
		t.Fatalf("read_file returned %q, want %q", out, "hello")
	}
}

func TestReadFileRefusesSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(work, "link.txt")); err != nil {
		t.Fatal(err)
	}
	r, err := New(work)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), "read_file", map[string]any{"path": "link.txt"})
	wantErr(t, err, "path escapes the working directory")
}

// testEvent is one event a Registry appended during a test.
type testEvent struct {
	kind  string
	stage string
	data  any
}

// fakeLedger stands in for a run ledger.
type fakeLedger struct {
	events []testEvent
	err    error
}

func (f *fakeLedger) Append(kind, stage string, data any) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, testEvent{kind: kind, stage: stage, data: data})
	return nil
}

func TestRecordsCommandExit(t *testing.T) {
	r := newRegistry(t)
	led := &fakeLedger{}
	r.SetLedger(led)
	if _, err := run(t, r, "ls -1"); err != nil {
		t.Fatal(err)
	}
	if len(led.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(led.events))
	}
	e := led.events[0]
	if e.kind != kindCommandExit || e.stage != "" {
		t.Fatalf("event = %q in stage %q, want %q in no stage", e.kind, e.stage, kindCommandExit)
	}
	exit, ok := e.data.(commandExit)
	if !ok {
		t.Fatalf("event payload is %T, want commandExit", e.data)
	}
	if exit.Name != "ls" {
		t.Fatalf("name %q, want %q", exit.Name, "ls")
	}
	if got := strings.Join(exit.Argv, " "); got != "ls -1" {
		t.Fatalf("argv %q, want %q", got, "ls -1")
	}
	if exit.ExitCode != 0 || exit.Killed != "" {
		t.Fatalf("exit = code %d, killed %q; want 0 and no kill", exit.ExitCode, exit.Killed)
	}
	if exit.Orphans {
		t.Fatal("record names a killed group for a command that left none behind")
	}
	if exit.DurationMS < 0 {
		t.Fatalf("duration %d ms, want non-negative", exit.DurationMS)
	}
}

// A refused command must not be recorded as a clean exit: the process can
// exit 0 while its output is thrown away, which is exactly what the ledger has
// to say out loud.
func TestRefusedOutputIsRecorded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), make([]byte, 2*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	led := &fakeLedger{}
	r.SetLedger(led)
	if _, err := r.Run(context.Background(), toolRunCommand, map[string]any{"command": "cat big.txt"}); err == nil {
		t.Fatal("command over the output cap returned a nil error")
	}
	if len(led.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(led.events))
	}
	exit, ok := led.events[0].data.(commandExit)
	if !ok {
		t.Fatalf("event payload is %T, want commandExit", led.events[0].data)
	}
	if exit.Refused != "output-limit" {
		t.Fatalf("recorded refused %q, want %q", exit.Refused, "output-limit")
	}
	if exit.ExitCode != 0 {
		t.Fatalf("recorded exit code %d, want the process's own 0", exit.ExitCode)
	}
}

// A kill that fails must not leave a record claiming nothing was left behind:
// the group may still be running, so the call fails and the record names it.
func TestFailedGroupKillFailsTheCall(t *testing.T) {
	r := newRegistry(t)
	led := &fakeLedger{}
	r.SetLedger(led)
	old := killGroup
	killGroup = func(*exec.Cmd) (bool, error) { return false, errors.New("operation not permitted") }
	t.Cleanup(func() { killGroup = old })
	_, err := run(t, r, "ls")
	wantErr(t, err, "kill process group")
	if len(led.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(led.events))
	}
	exit, ok := led.events[0].data.(commandExit)
	if !ok {
		t.Fatalf("event payload is %T, want commandExit", led.events[0].data)
	}
	if exit.Refused != "kill-failed" {
		t.Fatalf("recorded refused %q, want %q", exit.Refused, "kill-failed")
	}
	if exit.Orphans {
		t.Fatal("record claims the kill reached a group that it did not")
	}
}

// A ledger that fails while the command also fails must report both, not drop
// the record error on the floor.
func TestRecordFailureSurvivesCommandFailure(t *testing.T) {
	r := newRegistry(t)
	r.SetLedger(&fakeLedger{err: errors.New("disk full")})
	_, err := run(t, r, "cat missing-file")
	wantErr(t, err, "record cmd:exit: disk full")
	wantErr(t, err, "command failed")
}

func TestRecordFailureFailsTheCommand(t *testing.T) {
	r := newRegistry(t)
	r.SetLedger(&fakeLedger{err: errors.New("disk full")})
	_, err := run(t, r, "ls -1")
	wantErr(t, err, "record cmd:exit: disk full")
}

func TestReadFileRefusesLarge(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), make([]byte, 2*1024*1024+1), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), "read_file", map[string]any{"path": "big.txt"})
	wantErr(t, err, "too large")
}
