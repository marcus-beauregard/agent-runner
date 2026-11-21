// Package sandbox answers which of PRD §6.2's two isolation modes a run gets,
// and says out loud what the weaker one leaves unenforced.
//
// The choice is not a preference. Container mode is the one that makes the blast
// radius a namespace; asking for it on a host that cannot provide it is a
// refusal (exit 3), never a silent downgrade, because a run that quietly fell
// back would report a guarantee it never had.
package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/marcus-beauregard/agent-runner/internal/report"
)

// Mode is the isolation a run got. The values are stable strings rather than
// numbers, because a run record outlives the code that wrote it and has to stay
// readable without it. (#87 puts them in run.json and the report.)
type Mode string

const (
	// Container is a mount and network namespace: the child cannot see the host
	// filesystem and cannot reach a host the allowlist did not name.
	Container Mode = "container"
	// Worktree is the checkout alone. The diff is confined; the process is not.
	Worktree Mode = "worktree"

	// Auto is the default request: container where the host allows it, worktree
	// with a warning where it does not.
	Auto = "auto"
)

// unenforced names the things worktree mode does not stop. It is one string
// because every place that mentions the weaker mode has to mention the same
// three escapes; a second copy would be a second thing to forget to update.
const unenforced = "network egress from children, setsid escape, and writes to paths the audit does not watch"

// Unenforced returns what worktree mode leaves unenforced, as a sentence
// fragment a caller can print or put in a report. A caller that prints the mode
// without this leaves a reader to assume the strong one.
func Unenforced() string { return unenforced }

// Probe reports whether this host can build a container, and why not when it
// cannot. It is a parameter rather than a bare call so a test can answer without
// bubblewrap installed, and so detection stays one seam rather than three.
type Probe func(context.Context) (bool, string)

// probeTimeout bounds each detection command. Detection runs before any stage,
// and a probe that hangs would hold an unattended run open forever — the same
// reason every other command in this program carries a deadline.
const probeTimeout = 5 * time.Second

// Detect is the real probe.
//
// Presence of the binary is not evidence it works: bubblewrap can be installed
// and still fail to build a namespace when user namespaces are unavailable or
// the kernel is locked down, so the check runs the cheapest command that proves
// otherwise. It asks for the network namespace in the same command, because
// container mode is only worth the name when children cannot reach the internet
// (§6.2), and a probe that skipped it would answer yes for a mode this program
// could not deliver.
//
// The namespace is unshared inside bubblewrap's own user namespace, not by
// unshare in the caller's: `unshare --net true` needs CAP_SYS_ADMIN here and
// fails for an unprivileged user on hosts where bwrap works perfectly well, so
// probing it separately would refuse container mode on exactly the rootless
// hosts bubblewrap exists to serve.
func Detect(ctx context.Context) (bool, string) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return false, "bubblewrap is not installed"
	}
	if err := run(ctx, "bwrap", "--ro-bind", "/", "/", "--unshare-net", "true"); err != nil {
		return false, fmt.Sprintf("bubblewrap cannot build an isolated namespace: %v", err)
	}
	return true, ""
}

// run executes one detection command under probeTimeout. The command's own words
// are preferred over "exit status 1", because the reason a namespace was denied
// is the part an operator can act on.
func run(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("%v: %s", err, firstLine(msg))
	}
	return err
}

// firstLine keeps a detection error to the line that names the failure; the rest
// of a tool's usage text would bury it.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ErrUnavailable is the refusal: a container was asked for and this host cannot
// build one. It unwraps to report.ErrRefused, so however deep the error travelled
// the pipeline exits 3 without every caller having to remember the mapping.
type ErrUnavailable struct {
	Reason string // why the host cannot provide it, in the probe's words
}

// Error names the request and the reason it cannot be honoured.
func (e ErrUnavailable) Error() string {
	return fmt.Sprintf("%v: container sandbox unavailable: %s", report.ErrRefused, e.Reason)
}

// Unwrap reports the sentinel the pipeline maps to exit 3.
func (e ErrUnavailable) Unwrap() error { return report.ErrRefused }

// Resolve answers which mode this run gets, plus the warning to print when the
// answer is the weaker one. An empty warning means the mode needs no apology.
//
// An explicit worktree is honoured without probing: the weaker mode is a choice,
// so it needs no permission. An explicit container the host cannot provide is
// ErrUnavailable. Auto is the only request that probes, and it never fails —
// falling back is what auto means, as long as the fallback is said out loud.
func Resolve(ctx context.Context, requested string, probe Probe) (Mode, string, error) {
	switch requested = strings.TrimSpace(requested); requested {
	case "", Auto:
		ok, reason := probe(ctx)
		if ok {
			return Container, "", nil
		}
		return Worktree, warning(reason), nil
	case string(Container):
		ok, reason := probe(ctx)
		if ok {
			return Container, "", nil
		}
		return "", "", ErrUnavailable{Reason: reason}
	case string(Worktree):
		return Worktree, warning(""), nil
	default:
		return "", "", fmt.Errorf("unknown --sandbox mode %q: use %s, %s or %s", requested, Auto, Container, Worktree)
	}
}

// warning is the sentence the caller prints when a run gets worktree mode. The
// reason goes at the end, parenthesised, because why the strong mode was not
// available matters less than what is now unenforced.
func warning(reason string) string {
	s := fmt.Sprintf("sandbox: worktree mode; %s are unenforced", unenforced)
	if reason != "" {
		s += " (" + reason + ")"
	}
	return s
}
