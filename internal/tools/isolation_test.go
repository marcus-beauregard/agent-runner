package tools

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/refuse"
	"github.com/marcus-beauregard/agent-runner/internal/report"
	"github.com/marcus-beauregard/agent-runner/internal/sandbox"
)

// containerRoots builds the two directories a container run needs: the
// registry root to bind at /work, and a run directory with the tmp and home
// children the mounts expect.
func containerRoots(t *testing.T) (root, runDir string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runDir = t.TempDir()
	for _, name := range []string{"tmp", "home"} {
		if err := os.MkdirAll(filepath.Join(runDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root, runDir
}

// TestContainerRunsInsideTheNamespace is the ticket's done-when for the
// runner: the command reads the worktree through the /work mount, so a missing
// mount or an unwrapped command fails here. Only a host that cannot build a
// namespace may skip, and the skip names the capability it lacked.
func TestContainerRunsInsideTheNamespace(t *testing.T) {
	containerOrSkip(t)
	root, runDir := containerRoots(t)
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetIsolation(Isolation{Mode: sandbox.Container, RunDir: runDir}); err != nil {
		t.Fatalf("SetIsolation: %v", err)
	}
	out, err := run(t, r, "ls -1")
	if err != nil {
		t.Fatalf("run inside the container: %v", err)
	}
	if !strings.Contains(out, "inside.txt") {
		t.Errorf("ls output %q lacks the worktree file", out)
	}
}

// TestContainerWithoutBwrapIsARefusal is decided before the first command: no
// bwrap is exit 3 with the sentinel, not a run that discovers it per command.
func TestContainerWithoutBwrapIsARefusal(t *testing.T) {
	root, runDir := containerRoots(t)
	t.Setenv("PATH", t.TempDir())
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	err = r.SetIsolation(Isolation{Mode: sandbox.Container, RunDir: runDir})
	if err == nil {
		t.Fatal("SetIsolation accepted a container with no bwrap")
	}
	if !errors.Is(err, report.ErrRefused) || report.ExitCode(err) != 3 {
		t.Fatalf("error = %v, want a refusal at exit 3", err)
	}
}

// TestContainerPrefixCarriesTheMounts checks the argv the child is started
// with, not just where it ended up: a stub bwrap records what it was asked to
// run, so a missing prefix, a missing mount, or a ledger that repeats the
// sandbox flags each fail here. The effect-only test above cannot tell a host
// run from a container run, because the command's cwd holds the same files on
// both sides of the /work mount.
func TestContainerPrefixCarriesTheMounts(t *testing.T) {
	root, runDir := containerRoots(t)
	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "argv")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$BWRAP_STUB_ARGS\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bwrap"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BWRAP_STUB_ARGS", argsFile)

	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	led := &fakeLedger{}
	r.SetLedger(led)
	if err := r.SetIsolation(Isolation{Mode: sandbox.Container, RunDir: runDir}); err != nil {
		t.Fatalf("SetIsolation: %v", err)
	}
	if _, err := run(t, r, "ls -1"); err != nil {
		t.Fatalf("run through the stub: %v", err)
	}

	body, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the stub bwrap was never invoked: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(got) < 4 {
		t.Fatalf("bwrap got %d args, want the mounts and the command: %q", len(got), got)
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	realRun, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		t.Fatal(err)
	}
	bindPair := func(source, dest string) bool {
		for i := 0; i+2 < len(got); i++ {
			if got[i] == "--bind" && got[i+1] == source && got[i+2] == dest {
				return true
			}
		}
		return false
	}
	for _, want := range [][2]string{
		{realRoot, "/work"},
		{filepath.Join(realRun, "tmp"), "/tmp"},
		{filepath.Join(realRun, "home"), "/home/agent"},
	} {
		if !bindPair(want[0], want[1]) {
			t.Errorf("argv lacks --bind %s %s\ngot: %s", want[0], want[1], strings.Join(got, " "))
		}
	}
	if want := []string{"--chdir", "/work", "ls", "-1"}; !slices.Equal(got[len(got)-4:], want) {
		t.Errorf("argv ends %q, want %q", got[len(got)-4:], want)
	}

	if len(led.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(led.events))
	}
	exit, ok := led.events[0].data.(commandExit)
	if !ok {
		t.Fatalf("event payload is %T, want commandExit", led.events[0].data)
	}
	if !slices.Equal(exit.Argv, []string{"ls", "-1"}) {
		t.Errorf("recorded argv %q, want the raw command", exit.Argv)
	}
}

// TestContainerNamespaceThatWillNotBuildIsARefusal is the hole Detect cannot
// see: bwrap is installed and its own probe succeeds, but this run's mounts are
// the ones that fail. The refusal is decided before the first command and it
// carries a code, so the record says sandbox-unavailable instead of showing a
// command that failed and a run that still calls itself container.
func TestContainerNamespaceThatWillNotBuildIsARefusal(t *testing.T) {
	root, runDir := containerRoots(t)
	binDir := t.TempDir()
	// Detect's probe is the only bwrap call that does not carry --unshare-all;
	// the full argv does, and this stub refuses that one.
	stub := "#!/bin/sh\ncase \"$*\" in\n*--unshare-all*)\techo 'bwrap: setting up uid map: Permission denied' >&2; exit 1;;\nesac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bwrap"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	err = r.SetIsolation(Isolation{Mode: sandbox.Container, RunDir: runDir})
	if err == nil {
		t.Fatal("SetIsolation accepted a namespace that bwrap refuses to build")
	}
	if !errors.Is(err, report.ErrRefused) || report.ExitCode(err) != 3 {
		t.Fatalf("error = %v, want a refusal at exit 3", err)
	}
	var ref *refuse.Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("error = %T, want a typed refusal the record can hold", err)
	}
	if ref.Code != refuse.SandboxUnavailable {
		t.Errorf("code = %q, want %q", ref.Code, refuse.SandboxUnavailable)
	}
	if !strings.Contains(ref.Reason, "Permission denied") {
		t.Errorf("reason = %q, want bwrap's own words", ref.Reason)
	}
}
