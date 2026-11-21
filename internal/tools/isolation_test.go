package tools

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bubblewrap is not installed: %v", err)
	}
	if err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-net", "true").Run(); err != nil {
		t.Skipf("this host cannot build a container namespace: %v", err)
	}
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
