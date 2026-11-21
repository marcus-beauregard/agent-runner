package sandbox

import (
	"errors"
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
	)
	gitDir, err := gitMount(worktree)
	if err != nil {
		return nil, err
	}
	if gitDir != "" {
		// The gitdir comes after the run's own mounts: a run directory under
		// /tmp or /home would otherwise cover it again with the bind that
		// carries it, and git would resolve a path the container lost.
		args = append(args, "--ro-bind", gitDir, gitDir)
	}
	args = append(args,
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

// gitMount returns the common git directory a linked worktree's .git file
// names, or the empty string when the worktree needs no extra mount.
//
// A run's checkout is a linked worktree, so <worktree>/.git is a file holding
// "gitdir: <repo>/.git/worktrees/<name>" — an absolute host path that appears
// in no mount. The container sees /work and nothing outside it, so every git
// command answers "fatal: not a git repository" for the directory the file
// names, and git is the registry's only substantive tool. Binding the common
// directory read-only is enough: the allowlist has no writing subcommand, and
// read-only objects and refs are the one thing a child must not change.
//
// The empty string also covers the worktree whose .git is a directory, because
// that one is already inside the /work mount. Everything else that cannot be
// resolved is a refusal: a container built around a dangling gitdir would
// report a repository it does not have.
//
// The shape is checked rather than trusted, because the .git file is inside
// the worktree a previous run wrote to. Only <common>/worktrees/<name>, with a
// HEAD and an objects directory above it, is mounted — so a crafted file naming
// /root/.ssh is refused instead of read into the cage.
func gitMount(worktree string) (string, error) {
	path := filepath.Join(worktree, ".git")
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("sandbox: stat worktree .git: %w", err)
	case info.IsDir():
		return "", nil
	case !info.Mode().IsRegular():
		return "", refuse("sandbox: worktree .git is neither a file nor a directory")
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("sandbox: read worktree .git: %w", err)
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(body)), "gitdir: ")
	if !ok {
		return "", refuse("sandbox: worktree .git does not name a gitdir: %q", strings.TrimSpace(string(body)))
	}
	dir := strings.TrimSpace(target)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(worktree, dir)
	}
	gitDir, err := resolveDir(dir)
	if err != nil {
		return "", refuse("sandbox: worktree gitdir %s: %v", dir, err)
	}

	// A linked worktree usually keeps its own directory under the common one
	// and records that in commondir; without the entry the gitdir is the common
	// directory itself.
	common := gitDir
	if body, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		rel := strings.TrimSpace(string(body))
		if rel != "" {
			if !filepath.IsAbs(rel) {
				rel = filepath.Join(gitDir, rel)
			}
			if common, err = resolveDir(rel); err != nil {
				return "", refuse("sandbox: worktree commondir %s: %v", rel, err)
			}
		}
	}

	rel, err := filepath.Rel(common, gitDir)
	if err != nil {
		return "", refuse("sandbox: worktree gitdir %s is outside %s", gitDir, common)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if common != gitDir && (len(parts) != 2 || parts[0] != "worktrees") {
		return "", refuse("sandbox: worktree gitdir %s is not under %s/worktrees", gitDir, common)
	}
	if info, err := os.Stat(filepath.Join(common, "HEAD")); err != nil || info.IsDir() {
		return "", refuse("sandbox: gitdir %s has no HEAD file", common)
	}
	if info, err := os.Stat(filepath.Join(common, "objects")); err != nil || !info.IsDir() {
		return "", refuse("sandbox: gitdir %s has no objects directory", common)
	}
	return common, nil
}

// resolveDir proves a path names an existing directory, following symlinks so
// the returned path is the one the container will bind and git will read.
func resolveDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return real, nil
}

// refuse wraps report.ErrRefused, the sentinel report.ExitCode maps to 3.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", report.ErrRefused, fmt.Sprintf(format, args...))
}
