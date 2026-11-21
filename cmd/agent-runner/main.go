// Command agent-runner is a step-capped autonomous agent runtime and the
// worker built on it: hand it an issue, get back a reviewable plan.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/marcus-beauregard/agent-runner/internal/agent"
	"github.com/marcus-beauregard/agent-runner/internal/audit"
	"github.com/marcus-beauregard/agent-runner/internal/refuse"
	"github.com/marcus-beauregard/agent-runner/internal/report"
	runrec "github.com/marcus-beauregard/agent-runner/internal/run"
	"github.com/marcus-beauregard/agent-runner/internal/sandbox"
	"github.com/marcus-beauregard/agent-runner/internal/tools"
)

// version is the default; ldflags may override it at build time.
var version = "2.0.0"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(args) == 0 {
		usage(os.Stderr)
		return 1
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	case "version", "--version", "-v":
		fmt.Printf("agent-runner %s\n", version)
		return 0
	case "run":
		return runAgent(ctx, args[1:])
	case "work":
		return work(ctx, args[1:])
	case "report":
		return reportRun(args[1:])
	case "replay":
		return replayRun(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "agent-runner: unknown command: %s\n", args[0])
		return 1
	}
}

// runAgent runs the offline mock loop in the current directory and prints the
// same JSON the JavaScript entry point printed.
func runAgent(ctx context.Context, args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		usage(os.Stdout)
		return 0
	}
	task := "List the files in this project"
	if len(args) > 0 {
		task = strings.Join(args, " ")
	}
	wd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	registry, err := tools.New(wd)
	if err != nil {
		return fail(err)
	}
	result, err := runAudited(ctx, wd, task, agent.MockLLM{}, registry)
	if err != nil {
		return fail(err)
	}
	out := struct {
		Done       bool     `json:"done"`
		Steps      int      `json:"steps"`
		Transcript []string `json:"transcript"`
	}{result.Done, result.Steps, result.Summary()}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fail(err)
	}
	if !result.Done {
		return fail(fmt.Errorf("%w: agent stopped at the step cap", report.ErrNeedsHuman))
	}
	return 0
}

// runAudited runs the loop with the host audited around it. The audit watches
// the machine, not the checkout (PRD §6.2): wd is where the run is supposed to
// write and is excluded, so anything else that moves while the loop runs is
// drift, and the run refuses rather than reporting a clean result over it.
func runAudited(ctx context.Context, wd, task string, llm agent.LLM, toolset agent.Toolset) (agent.Result, error) {
	before, err := snapshotHost(wd)
	if err != nil {
		return agent.Result{}, err
	}
	result, runErr := agent.Run(ctx, task, llm, agent.Options{}, toolset)
	after, err := snapshotHost(wd)
	if err != nil {
		return agent.Result{}, err
	}
	drift := audit.Compare(before, after)
	if len(drift) == 0 {
		return result, runErr
	}
	// The machine moving is the news the caller has to act on, so the refusal
	// wins over whatever the loop was about to say. The loop's own error, if it
	// had one, goes into the evidence instead of being dropped on the floor.
	return result, refuseDrift(drift, runErr)
}

// snapshotHost records every place a run must leave alone. runDir is the run's
// working directory, excluded together with everything under it. repo is the
// checkout around runDir, watched from its root so a write beside the run
// directory is caught, and passed to the watchlist so the one repository path
// the audit excuses is the .git/worktrees bookkeeping that `git worktree add`
// writes.
//
// When runDir is the checkout root itself the two are the same path and there
// is no repository path outside the run: "outside the run directory" would have
// nothing in the checkout to mean, so the repository adds no root of its own.
// That is a narrowing of the claim, not a silent exclusion — the audit never
// says it watched a repository when the whole of it was the run's to write.
func snapshotHost(runDir string) (audit.Snapshot, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory: %w", err)
	}
	stateBase, err := runrec.StateBase()
	if err != nil {
		return nil, err
	}
	repo, err := repoRoot(runDir)
	if err != nil {
		return nil, err
	}
	if repo != "" && sameDir(repo, runDir) {
		repo = ""
	}
	snap, err := audit.Take(audit.Watchlist(home, repo), audit.Ignores(home, stateBase, runDir, repo))
	if err != nil {
		return nil, err
	}
	// Take skips a root an ignore already covers without a word. A watchlist
	// that was excluded in full would then report no drift on nothing watched,
	// which is the shape of bug that made #54 split in the first place, so say
	// it instead of auditing an empty host.
	if len(snap) == 0 {
		return nil, errors.New("audit: every watchlist root was excluded; nothing was watched")
	}
	return snap, nil
}

// sameDir reports whether two paths name the same directory. git prints the
// physical path, which can differ from the one os.Getwd returns through a
// symlink; comparing the strings would then watch a repository that the run
// has already excluded under its other name.
func sameDir(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// repoRoot is the checkout dir belongs to, or "" when it is not inside one.
// The two answers stay apart: "there is no repository" means there is nothing
// to watch, while git failing to answer means the watch was never established —
// and a run that reports clean over that is #85 in a new coat.
func repoRoot(dir string) (string, error) {
	var stderr strings.Builder
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		root := strings.TrimSpace(string(out))
		if root == "" {
			return "", fmt.Errorf("git rev-parse --show-toplevel in %s printed nothing", dir)
		}
		return root, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return "", fmt.Errorf("run git rev-parse in %s: %w", dir, err)
	}
	// git's answer for "not inside a work tree" is fatal, exit 128. Any other
	// failure — a broken config, a missing object, a filesystem that refused —
	// is not that answer and must not read as one.
	msg := strings.TrimSpace(stderr.String())
	if exit.ExitCode() == 128 && strings.Contains(msg, "not a git repository") {
		return "", nil
	}
	return "", fmt.Errorf("git rev-parse --show-toplevel in %s: %v: %s", dir, err, msg)
}

// refuseDrift prints one line per drifted path and returns the refusal naming
// the audit as its stage. The paths are on stderr because that is the part a
// person acts on; the same paths go into the evidence with what moved, so a
// record of the run can be checked against them.
func refuseDrift(drift []audit.Drift, cause error) *refuse.Refusal {
	evidence := make(map[string]string, len(drift)+1)
	for _, d := range drift {
		what := d.Kind
		if d.Detail != "" {
			what += ": " + d.Detail
		}
		fmt.Fprintf(os.Stderr, "agent-runner: drift: %s (%s)\n", d.Path, what)
		evidence[d.Path] = what
	}
	if cause != nil {
		evidence["agent"] = cause.Error()
	}
	r := refuse.New(refuse.OutsideRunDir, "audit",
		fmt.Sprintf("%d path(s) outside the run directory changed during the run", len(drift)))
	r.Evidence = evidence
	return r
}

// work runs the M0 dry-run: it plans the change and prints the gate it would
// run, writing nothing except the run record when --run-dir asks for one. The
// issue URL is the leading positional argument, so it is consumed before the
// flag set, which stops at the first non-flag.
func work(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "agent-runner: work: exactly one issue URL is required")
		return 1
	}
	if args[0] == "-h" || args[0] == "--help" {
		usage(os.Stdout)
		return 0
	}
	ref := args[0]
	fs := flag.NewFlagSet("work", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "path to an existing checkout (unused in the dry run)")
	gate := fs.String("gate", "", "the command that decides whether the work is done")
	dryRun := fs.Bool("dry-run", false, "plan only; write nothing")
	runDir := fs.String("run-dir", "", "base directory for the run record; no record is written unless it is set")
	sandboxMode := fs.String("sandbox", sandbox.Auto, "isolation mode: auto, container or worktree")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout)
			return 0
		}
		fmt.Fprintf(os.Stderr, "agent-runner: work: %v\n", err)
		return 1
	}
	if len(fs.Args()) != 0 {
		fmt.Fprintln(os.Stderr, "agent-runner: work: exactly one issue URL is required")
		return 1
	}
	if !*dryRun {
		fmt.Fprintln(os.Stderr, "agent-runner: work: only --dry-run is implemented")
		return 1
	}

	// The mode is decided before anything is written, because it is part of what
	// the record claims the run was. A refusal that is not a refusal of this
	// ticket's kind — a mode that does not exist — is a usage error and gets no
	// record: nothing ran, and a record of nothing is noise.
	_, warn, modeErr := sandbox.Resolve(ctx, *sandboxMode, sandbox.Detect)
	if modeErr != nil && !errors.Is(modeErr, report.ErrRefused) {
		return fail(modeErr)
	}
	if warn != "" {
		fmt.Fprintf(os.Stderr, "agent-runner: warning: %s\n", warn)
	}
	if *runDir == "" {
		if modeErr != nil {
			return fail(modeErr)
		}
		code, planErr := planAndPrint(ctx, ref, *repo, *gate)
		if planErr != nil {
			fmt.Fprintf(os.Stderr, "agent-runner: %v\n", planErr)
		}
		return code
	}
	r, err := runrec.New(*runDir, runrec.Meta{Kind: "build", Subject: ref, Repo: *repo})
	if err != nil {
		return fail(err)
	}
	fmt.Printf("Run: %s\nDir: %s\n\n", r.ID, r.Dir)
	// A refusal is a finished run, so it gets a record with its own state rather
	// than a directory that says running for a process that already exited.
	if modeErr != nil {
		if err := r.Refuse(refusalForMode(modeErr)); err != nil {
			fmt.Fprintf(os.Stderr, "agent-runner: record refusal: %v\n", err)
			return 1
		}
		if err := r.Finish(report.ExitCode(modeErr)); err != nil {
			fmt.Fprintf(os.Stderr, "agent-runner: record run outcome: %v\n", err)
			return 1
		}
		return fail(modeErr)
	}
	code, planErr := planAndPrint(ctx, ref, *repo, *gate)
	if err := recordRefusal(r, planErr); err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: record refusal: %v\n", err)
		return 1
	}
	if planErr != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: %v\n", planErr)
	}
	if err := r.Finish(code); err != nil {
		// The run happened; the record of how it ended did not. Say so instead of
		// exiting on the result as though the record could be trusted.
		fmt.Fprintf(os.Stderr, "agent-runner: record run outcome: %v\n", err)
		return 1
	}
	return code
}

// planAndPrint plans the work, prints the plan, and returns the exit code for it
// with the refusal behind it, if any. Splitting it out lets the recorded path
// write the refusal into its record and finalize that record whatever the plan
// decided.
func planAndPrint(ctx context.Context, ref, repo, gate string) (int, error) {
	plan, err := report.Work(ctx, ref, repo, gate)
	fmt.Print(plan.String())
	if err != nil {
		return report.ExitCode(err), err
	}
	return 0, nil
}

// recordRefusal writes the typed refusal behind err into the run record. Only a
// *refuse.Refusal is recorded: an arbitrary error is not a refusal, and writing
// one as if it were would put a code on something no vocabulary covers.
func recordRefusal(r *runrec.Run, err error) error {
	var ref *refuse.Refusal
	if !errors.As(err, &ref) {
		return nil
	}
	return r.Refuse(ref)
}

// refusalForMode types the sandbox refusal, which is decided before any stage
// runs and so has no plan to refuse. The probe's own words are the reason: it
// is the part an operator can act on.
func refusalForMode(err error) *refuse.Refusal {
	var unavailable sandbox.ErrUnavailable
	if errors.As(err, &unavailable) {
		return refuse.New(refuse.SandboxUnavailable, "sandbox", unavailable.Reason)
	}
	return refuse.New(refuse.SandboxUnavailable, "sandbox", err.Error())
}

// reportRun prints one recorded run as REPORT.md. It reads the record and
// writes nothing, so it is safe to point at a run that is still going, at a
// finished one, and at one a resumed process is holding open. It is a view of
// the two files as they were read, not a snapshot of one instant: a stage that
// finishes between the reads can sit under a header that still says running, so
// render again rather than trust one mid-run render.
func reportRun(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "agent-runner: report: exactly one run id is required")
		return 1
	}
	if args[0] == "-h" || args[0] == "--help" {
		usage(os.Stdout)
		return 0
	}
	id := args[0]
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	runDir := fs.String("run-dir", "", "base directory the run record lives under")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout)
			return 0
		}
		fmt.Fprintf(os.Stderr, "agent-runner: report: %v\n", err)
		return 1
	}
	if len(fs.Args()) != 0 {
		fmt.Fprintln(os.Stderr, "agent-runner: report: exactly one run id is required")
		return 1
	}
	dir, err := runrec.Locate(*runDir, id)
	if err != nil {
		return fail(err)
	}
	b, err := runrec.RenderReport(dir)
	if err != nil {
		return fail(err)
	}
	if _, err := os.Stdout.Write(b); err != nil {
		return fail(err)
	}
	return 0
}

// replayRun proves a recorded run still plans what it planned: it recomputes
// the action set from the run's World snapshot and compares it with the trigger
// rows in the ledger. Writes are suppressed by construction — nothing on this
// path opens a file for writing — so it is safe on a live run's directory too.
//
// A record that no longer agrees with its own world exits 1: the run did not
// do what its record says, and that is a failure of the record, not a refusal
// or a gate. A run that has not finished has no verdict to give and exits 1 with
// an error instead: its ledger is partial because it is still going.
func replayRun(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "agent-runner: replay: exactly one run directory is required")
		return 1
	}
	if args[0] == "-h" || args[0] == "--help" {
		usage(os.Stdout)
		return 0
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "agent-runner: replay: exactly one run directory is required")
		return 1
	}
	ok, err := runrec.Replay(args[0], os.Stdout)
	if err != nil {
		return fail(err)
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "agent-runner: replay: the record's action set is not what its own world yields")
		return 1
	}
	return 0
}

// fail prints one line to stderr and returns the exit code for err.
func fail(err error) int {
	fmt.Fprintf(os.Stderr, "agent-runner: %v\n", err)
	return report.ExitCode(err)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `agent-runner — a step-capped autonomous agent runtime and the worker built on it.

Usage:
  agent-runner run <task>                                        run the offline mock agent
  agent-runner work <issue-url> --repo <path> [--gate <cmd>] [--run-dir <dir>]
                   [--sandbox auto|container|worktree] --dry-run
                                                                 plan the work, writing nothing but the run record
  agent-runner report <id> [--run-dir <dir>]                     print REPORT.md for a recorded run
  agent-runner replay <run-dir>                                  prove a finished run's action set from its record
                                                                 (needs a World snapshot and an account: a record written by
                                                                 Begin, not one from work --run-dir)
  agent-runner --help                                            show this help
  agent-runner --version                                         print the version
`)
}
