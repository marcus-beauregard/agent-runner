package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/marcus-beauregard/agent-runner/internal/report"
)

// Config names the three run-directory children a container mounts. They are
// separate fields rather than paths derived in Args, so a caller that put a run
// somewhere unexpected keeps the layout it recorded instead of being silently
// remounted from a guessed path. The zero value means the layout run.New,
// run.Worktree and tools.Env create: worktree, tmp and home under the run
// directory.
type Config struct {
	// Worktree is the checkout, mounted at /work.
	Worktree string
	// Tmp is the run's scratch space, mounted at /tmp.
	Tmp string
	// Home is the child's HOME, mounted at /home/agent.
	Home string
}

// systemReadOnly is the host runtime a child may read but never write: the
// toolchain and its shared libraries, the CA bundle, and the resolver config.
// Each entry is a source and the destination of the same name; the mount is
// read-only, so a child can execute and read, which is all a build needs.
// --ro-bind-try is used instead of --ro-bind because a host may legitimately
// lack /lib64 or /etc/resolv.conf, and a missing optional path must not turn
// every container run into a refusal.
var systemReadOnly = []string{
	"/usr", "/lib", "/lib64", "/bin", "/sbin", "/etc/ssl", "/etc/resolv.conf",
}

// Args returns the bubblewrap arguments for one container run, ready for the
// caller to append the command's own argv to.
//
// The namespace is closed: --unshare-all includes the network namespace, and
// there is deliberately no --share-net. A closed network is stronger than the
// allowlist PRD §6.2 asks for and is what this ticket can honestly deliver:
// allowlisted egress through a proxy is a proxy this program does not have yet,
// and sharing the host's network to allow github.com would hand the child the
// whole internet. Callers that need gh or git to reach the network must run
// those outside the container until that proxy exists.
//
// /etc is a fresh tmpfs remounted read-only, with only the CA bundle and the
// resolver config bound into it. Binding those two into the host's /etc would
// leave a writable directory around them, and "touch /etc/x fails" is the claim
// this function exists to make true.
//
// Args is pure: it inspects the mount sources and returns argv, and runs
// nothing. A missing or escaping mount source is a refusal wrapping
// report.ErrRefused, because a container built from the wrong path would report
// a confinement it never had.
func Args(runDir string, cfg Config) ([]string, error) {
	if strings.TrimSpace(runDir) == "" {
		return nil, refuse("sandbox: no run directory")
	}
	root, err := filepath.Abs(runDir)
	if err != nil {
		return nil, refuse("sandbox: resolve run directory: %v", err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, refuse("sandbox: resolve run directory: %v", err)
	}
	worktree, err := mount(root, cfg.Worktree, "worktree")
	if err != nil {
		return nil, err
	}
	tmp, err := mount(root, cfg.Tmp, "tmp")
	if err != nil {
		return nil, err
	}
	home, err := mount(root, cfg.Home, "home")
	if err != nil {
		return nil, err
	}

	args := []string{
		// One flag for user, pid, net, ipc, uts and cgroup namespaces: a child
		// that daemonises, reaches the network or talks to the host's IPC is
		// outside the claim otherwise.
		"--unshare-all",
		// The sandbox dies with the process that started it, rather than
		// outliving a crashed parent.
		"--die-with-parent",
		// No controlling terminal, so a child cannot inject input into the
		// terminal that started it.
		"--new-session",
		"--tmpfs", "/etc",
	}
	for _, p := range systemReadOnly {
		args = append(args, "--ro-bind-try", p, p)
	}
	args = append(args,
		// The remount comes after the /etc children: it makes the directory
		// itself read-only while leaving the read-only binds inside it as they
		// are.
		"--remount-ro", "/etc",
		"--bind", worktree, "/work",
		"--bind", tmp, "/tmp",
		"--bind", home, "/home/agent",
		"--proc", "/proc",
		// --dev creates the minimal device set bubblewrap manages (null, zero,
		// urandom and their companions); no host device is bound through.
		"--dev", "/dev",
		"--tmpfs", "/run",
		"--chdir", "/work",
	)
	return args, nil
}

// mount resolves one configured mount source and proves it is a directory
// inside the run directory. A source that is a symlink out of the run, the run
// directory itself, or a file is a refusal: each would mount something other
// than the run's own child at the destination.
func mount(root, configured, name string) (string, error) {
	dir := configured
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join(root, name)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", refuse("sandbox: resolve %s mount: %v", name, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", refuse("sandbox: %s mount %s: %v", name, dir, err)
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", refuse("sandbox: %s mount %s is outside the run directory", name, dir)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", refuse("sandbox: %s mount %s: %v", name, dir, err)
	}
	if !info.IsDir() {
		return "", refuse("sandbox: %s mount %s is not a directory", name, dir)
	}
	return real, nil
}

// refuse wraps report.ErrRefused, the sentinel report.ExitCode maps to 3.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", report.ErrRefused, fmt.Sprintf(format, args...))
}
