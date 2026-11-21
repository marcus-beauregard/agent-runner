package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcus-beauregard/agent-runner/internal/report"
)

// containerLayout creates the three children a container run mounts and returns
// the run directory. Args refuses a missing mount source, so every test that
// wants argv rather than a refusal starts here.
func containerLayout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"worktree", "tmp", "home"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	return dir
}

// TestArgsBuildsTheContainerCommand pins the whole argv, not a sample of it:
// every flag here is part of the confinement claim, and a missing mount is as
// visible in a diff as a wrong one.
func TestArgsBuildsTheContainerCommand(t *testing.T) {
	dir := containerLayout(t)
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve run directory: %v", err)
	}
	got, err := Args(dir, Config{})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	want := []string{
		"--unshare-all",
		"--die-with-parent",
		"--new-session",
		"--tmpfs", "/etc",
		"--ro-bind-try", "/usr", "/usr",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/sbin", "/sbin",
		"--ro-bind-try", "/etc/ssl", "/etc/ssl",
		"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf",
		"--remount-ro", "/etc",
		"--bind", filepath.Join(real, "worktree"), "/work",
		"--bind", filepath.Join(real, "tmp"), "/tmp",
		"--bind", filepath.Join(real, "home"), "/home/agent",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/run",
		"--chdir", "/work",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

// TestArgsHonoursConfiguredMounts covers the layout a caller recorded rather
// than the one Args would guess. The paths still have to be inside the run.
func TestArgsHonoursConfiguredMounts(t *testing.T) {
	dir := containerLayout(t)
	for _, name := range []string{"tree", "scratch", "userhome"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve run directory: %v", err)
	}
	got, err := Args(dir, Config{
		Worktree: filepath.Join(dir, "tree"),
		Tmp:      filepath.Join(dir, "scratch"),
		Home:     filepath.Join(dir, "userhome"),
	})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	for _, want := range []string{
		filepath.Join(real, "tree") + " /work",
		filepath.Join(real, "scratch") + " /tmp",
		filepath.Join(real, "userhome") + " /home/agent",
	} {
		if !strings.Contains(strings.Join(got, " "), want) {
			t.Errorf("argv %q does not bind %q", got, want)
		}
	}
}

// TestArgsRefusesAMountItCannotTrust covers every way a source is not the run's
// own child. Each is a refusal (exit 3), because a container built from the
// wrong path would report a confinement it never had.
func TestArgsRefusesAMountItCannotTrust(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) (runDir string, cfg Config)
	}{
		{
			name: "no run directory",
			setup: func(t *testing.T) (string, Config) {
				return "", Config{}
			},
		},
		{
			name: "run directory does not exist",
			setup: func(t *testing.T) (string, Config) {
				return filepath.Join(t.TempDir(), "gone"), Config{}
			},
		},
		{
			name: "worktree missing",
			setup: func(t *testing.T) (string, Config) {
				dir := containerLayout(t)
				if err := os.Remove(filepath.Join(dir, "worktree")); err != nil {
					t.Fatalf("remove worktree: %v", err)
				}
				return dir, Config{}
			},
		},
		{
			name: "source is a file",
			setup: func(t *testing.T) (string, Config) {
				dir := containerLayout(t)
				path := filepath.Join(dir, "worktree")
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove worktree: %v", err)
				}
				if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("write worktree: %v", err)
				}
				return dir, Config{}
			},
		},
		{
			name: "source outside the run",
			setup: func(t *testing.T) (string, Config) {
				dir := containerLayout(t)
				return dir, Config{Worktree: t.TempDir()}
			},
		},
		{
			name: "symlink out of the run",
			setup: func(t *testing.T) (string, Config) {
				dir := containerLayout(t)
				outside := t.TempDir()
				path := filepath.Join(dir, "worktree")
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove worktree: %v", err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatalf("symlink worktree: %v", err)
				}
				return dir, Config{}
			},
		},
		{
			name: "the run directory itself",
			setup: func(t *testing.T) (string, Config) {
				dir := containerLayout(t)
				return dir, Config{Worktree: dir}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, cfg := tc.setup(t)
			_, err := Args(dir, cfg)
			if err == nil {
				t.Fatal("Args accepted a mount it cannot trust")
			}
			if !errors.Is(err, report.ErrRefused) {
				t.Errorf("error %v does not wrap report.ErrRefused", err)
			}
			if got := report.ExitCode(err); got != 3 {
				t.Errorf("exit code = %d, want 3", got)
			}
		})
	}
}

// TestArgsLeavesNoWayBackToTheHost is the other half of the argv test: the flags
// that would undo the namespace are absent, so a later edit has to delete an
// assertion to add one.
func TestArgsLeavesNoWayBackToTheHost(t *testing.T) {
	got, err := Args(containerLayout(t), Config{})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	forbidden := []string{"--share-net", "--unshare-net", "--cap-add", "--dev-bind", "--bind-try"}
	for _, arg := range got {
		for _, bad := range forbidden {
			if arg == bad {
				t.Errorf("argv contains %q, which reopens the host", bad)
			}
		}
	}
	joined := strings.Join(got, " ")
	if strings.Contains(joined, " / / ") {
		t.Errorf("argv binds the host root: %q", got)
	}
}

// requireContainer skips loudly when this host cannot build the namespace the
// test would attack. A skip names the capability, so a run that proves nothing
// does not read as a pass.
func requireContainer(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bubblewrap is not installed: %v", err)
	}
	args, err := Args(dir, Config{})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	if out, err := exec.Command("bwrap", append(args, "/bin/true")...).CombinedOutput(); err != nil {
		t.Skipf("this host cannot build the container: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

// runContainer runs one shell script under the argv Args returned, with no
// extra flags: the test exercises the same command a caller would.
func runContainer(t *testing.T, dir, script string) (string, error) {
	t.Helper()
	args, err := Args(dir, Config{})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	cmd := exec.Command("bwrap", append(args, "/bin/sh", "-c", script)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestContainerRefusesTheThreeEscapes is the ticket's done-when, run for real:
// a write outside the mounts fails, a write inside the worktree succeeds, the
// network namespace has no interface but loopback, and a backgrounded child
// does not outlive the sandbox.
func TestContainerRefusesTheThreeEscapes(t *testing.T) {
	dir := containerLayout(t)
	requireContainer(t, dir)

	if out, err := runContainer(t, dir, "touch /etc/x"); err == nil {
		t.Errorf("touch /etc/x succeeded; output %q", out)
	}
	if _, err := os.Stat("/etc/x"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("/etc/x exists on the host after the sandbox tried to create it")
	}

	if out, err := runContainer(t, dir, "touch /work/x"); err != nil {
		t.Errorf("touch /work/x failed: %v (%s)", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "worktree", "x")); err != nil {
		t.Errorf("write inside /work did not reach the worktree: %v", err)
	}

	// No interface but loopback is a stronger statement than one refused
	// request: there is no route for any off-allowlist host to answer on, and
	// proving it needs no network, no curl, and no timeout.
	script := "tail -n +3 /proc/net/dev | grep -v '^ *lo:' | grep -q . && exit 1 || exit 0"
	if out, err := runContainer(t, dir, script); err != nil {
		t.Errorf("sandbox has an interface besides loopback: %v (%s)", err, out)
	}

	marker := filepath.Join(dir, "worktree", "alive")
	if out, err := runContainer(t, dir, "sh -c 'sleep 1; touch /work/alive' & exit 0"); err != nil {
		t.Fatalf("start a background child: %v (%s)", err, out)
	}
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a backgrounded child outlived the sandbox and wrote its marker")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat marker: %v", err)
	}
}
