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

// model is the planner the run command drives. The deterministic mock is the
// default and the binary has no flag that could replace it; a test swaps this
// to make the loop attempt one specific thing, which is the only way to attack
// the pipeline with a model that does not plan the same run every time.
var model agent.LLM = agent.MockLLM{}

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
	case "gc":
		return gcCommand(args[1:])
	case "replay":
		return replayRun(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "agent-runner: unknown command: %s\n", args[0])
		return 1
	}
}

// runAgent runs the offline mock loop in the current directory and prints the
// same JSON the JavaScript entry point printed.
//
// It is the one command that executes children, so it is where --sandbox has
// teeth: container mode runs every command inside the namespace, and the mode
// the run got is recorded when --run-dir asks for a record. The directory the
// run writes is the one it was started in; there is no worktree stage here.
func runAgent(ctx context.Context, args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		usage(os.Stdout)
		return 0
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sandboxMode := fs.String("sandbox", sandbox.Auto, "isolation mode: auto, container or worktree")
	runDir := fs.String("run-dir", "", "base directory for the run record; no record is written unless it is set")
	// Flags come before the task, as the usage line says. The flag package stops
	// at the first non-flag, so everything from there is task text, kept word for
	// word: a sentence that happens to contain "--sandbox" cannot switch the mode
	// and cannot lose a word out of the middle of itself. A task that starts with
	// a dash word is written behind "--", the only terminator this command honours.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout)
			return 0
		}
		fmt.Fprintf(os.Stderr, "agent-runner: run: %v\n", err)
		return 1
	}
	task := strings.Join(fs.Args(), " ")
	if task == "" {
		task = "List the files in this project"
	}
	wd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}

	// The mode is decided before the record is written, because it is part of
	// what the record claims the run was. A mode that does not exist is a usage
	// error and gets no record: nothing ran, and a record of nothing is noise.
	mode, warn, modeErr := sandbox.Resolve(ctx, *sandboxMode, sandbox.Detect)
	if modeErr != nil && !errors.Is(modeErr, report.ErrRefused) {
		return fail(modeErr)
	}
	if warn != "" {
		fmt.Fprintf(os.Stderr, "agent-runner: warning: %s\n", warn)
	}

	var rec *runrec.Run
	var led *runrec.Ledger
	if *runDir != "" {
		rec, err = runrec.New(*runDir, runrec.Meta{Kind: "run", Subject: task, Repo: wd, Sandbox: string(mode)})
		if err != nil {
			return fail(err)
		}
		fmt.Printf("Run: %s\nDir: %s\n\n", rec.ID, rec.Dir)
		// The stream is opened with the record rather than with the first
		// command, because a refusal is an event too: a run that refuses before
		// it ever ran a command still has to say so in the file a reader follows.
		// It is closed after the last command, so a run killed mid-command still
		// leaves the events of the commands before it.
		led, err = runrec.OpenLedger(filepath.Join(rec.Dir, "run.ndjson"))
		if err != nil {
			// The run is over before it began, and its record must say so: a
			// run.json left at running for a process that exited is the state
			// Finish exists to prevent.
			if code := finishRun(rec, report.ExitCode(err)); code != 0 {
				return code
			}
			return fail(fmt.Errorf("open record: %w", err))
		}
		defer closeRecord(led)
		rec.SetLedger(led)
	}
	// A refusal is a finished run, so it gets a record with its own state rather
	// than a directory that says running for a process that already exited.
	if modeErr != nil {
		if !refusalRecorded(rec, refusalForMode(modeErr)) {
			return 1
		}
		if code := finishRun(rec, report.ExitCode(modeErr)); code != 0 {
			return code
		}
		return fail(modeErr)
	}

	registry, err := tools.New(wd)
	if err != nil {
		return fail(err)
	}
	if mode == sandbox.Container {
		// The mounts need a tmp and a home that exist before the first command.
		// A recorded run keeps them beside its record; an unrecorded one gets a
		// scratch directory that goes away with the process.
		layout := ""
		if rec != nil {
			layout = rec.Dir
		} else {
			dir, err := os.MkdirTemp("", "agent-runner-sandbox-")
			if err != nil {
				return fail(err)
			}
			defer os.RemoveAll(dir)
			layout = dir
		}
		for _, name := range []string{"tmp", "home"} {
			if err := os.MkdirAll(filepath.Join(layout, name), 0o700); err != nil {
				return fail(fmt.Errorf("sandbox scratch: %w", err))
			}
		}
		if err := registry.SetIsolation(tools.Isolation{Mode: mode, RunDir: layout}); err != nil {
			if !refusalRecorded(rec, refusalOf(err)) {
				return 1
			}
			if code := finishRun(rec, report.ExitCode(err)); code != 0 {
				return code
			}
			return fail(err)
		}
	}
	// A command is the run's most consequential act, so its record belongs to
	// the run rather than to the process that happened to run it. An unrecorded
	// run has nowhere to put its events and, as before, keeps nothing.
	if led != nil {
		registry.SetLedger(led)
	}
	result, err := runAudited(ctx, wd, task, model, registry)
	if err != nil {
		if !refusalRecorded(rec, refusalOf(err)) {
			return 1
		}
		if code := finishRun(rec, report.ExitCode(err)); code != 0 {
			return code
		}
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
		stopped := fmt.Errorf("%w: agent stopped at the step cap", report.ErrNeedsHuman)
		if code := finishRun(rec, report.ExitCode(stopped)); code != 0 {
			return code
		}
		return fail(stopped)
	}
	if code := finishRun(rec, 0); code != 0 {
		return code
	}
	return 0
}

// closeRecord closes the run's stream once the run is over. Every event is
// synced as it is written, so a close that fails has cost no event: it is said
// out loud instead of becoming a second exit code over a run whose outcome is
// already on disk.
func closeRecord(l *runrec.Ledger) {
	if err := l.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: close record: %v\n", err)
	}
}

// finishRun writes a run's terminal state when it has a record. The result is
// the process exit code: a record that cannot be written after the run happened
// is not a success, so it becomes 1.
func finishRun(rec *runrec.Run, code int) int {
	if rec == nil {
		return 0
	}
	if err := rec.Finish(code); err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: record run outcome: %v\n", err)
		return 1
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
	// os.Getwd keeps the spelling the caller walked through, git prints the
	// physical one, and $HOME is often a symlink of its own. The watchlist and
	// the ignores only line up when every path they are built from is the same
	// physical spelling the walk produces, so each one is resolved once here:
	// a run directory under a symlinked home otherwise has its own writes read
	// as drift.
	resolved, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		return nil, fmt.Errorf("resolve run directory %s: %w", runDir, err)
	}
	runDir = resolved
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory: %w", err)
	}
	home = canonical(home)
	stateBase, err := runrec.StateBase()
	if err != nil {
		return nil, err
	}
	stateBase = canonical(stateBase)
	repo, err := repoRoot(runDir)
	if err != nil {
		return nil, err
	}
	if repo != "" {
		repo = canonical(repo)
	}
	if repo != "" && sameDir(repo, runDir) {
		repo = ""
	}
	snap, err := audit.Take(audit.Watchlist(home, repo), audit.Ignores(home, stateBase, runDir, repo))
	if err != nil {
		return nil, err
	}
	// Take skips a root an ignore already covers without a word, and a root
	// that does not exist without a word too. A watchlist excluded in full
	// would then report no drift on nothing watched — the shape of bug that
	// made #54 split — and a home that silently contributed nothing is the
	// same lie one step quieter, so name both instead of auditing a machine
	// that was only partly sampled.
	if len(snap) == 0 {
		return nil, errors.New("audit: every watchlist root was excluded; nothing was watched")
	}
	if _, ok := snap[home]; !ok {
		return nil, fmt.Errorf("audit: %s was not watched", home)
	}
	if repo != "" {
		if _, ok := snap[repo]; !ok {
			return nil, fmt.Errorf("audit: the checkout %s was not watched", repo)
		}
	}
	return snap, nil
}

// sameDir reports whether two paths name the same directory. snapshotHost has
// already resolved the run directory, so this is the belt to that braces: git
// may still print the checkout in a spelling of its own, and a repository the
// run has already excluded under another name must not be watched again.
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
	// The answer has to be about dir, so the caller's git environment must not
	// change it: GIT_CEILING_DIRECTORIES makes a directory inside a checkout
	// answer "not a git repository", GIT_DIR answers for another tree entirely,
	// and LC_ALL decides which sentence the answer arrives in — which the
	// classification below reads.
	cmd.Env = gitReadEnv()
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		root := strings.TrimSpace(string(out))
		if root == "" {
			return "", fmt.Errorf("git rev-parse --show-toplevel in %s printed nothing", dir)
		}
		// A root that does not contain dir is an answer about somewhere else;
		// watching it would report on a tree this run is not in.
		if !contains(root, dir) {
			return "", fmt.Errorf("git rev-parse --show-toplevel in %s printed %s, which does not contain it", dir, root)
		}
		return root, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return "", fmt.Errorf("run git rev-parse in %s: %w", dir, err)
	}
	// git's answer for "discovery found no repository" names its search scope in
	// parentheses and exits 128. A message that names a path after the colon —
	// "not a git repository: <path>", which a broken submodule gitdir prints
	// with the same exit code — is a repository git refused to open, and a
	// repository it refused to describe has to stop the run, not read as
	// nothing to watch.
	msg := strings.TrimSpace(stderr.String())
	if exit.ExitCode() == 128 && strings.Contains(msg, "not a git repository") && !strings.Contains(msg, "not a git repository: ") {
		return "", nil
	}
	return "", fmt.Errorf("git rev-parse --show-toplevel in %s: %v: %s", dir, err, msg)
}

// gitReadEnv is the environment for a read-only git question about a directory:
// the caller's repository selection is removed and the locale is pinned, so the
// answer is about dir on a host whose shell was set up some other way.
func gitReadEnv() []string {
	drop := []string{
		"GIT_DIR=", "GIT_WORK_TREE=", "GIT_COMMON_DIR=",
		"GIT_CEILING_DIRECTORIES=", "GIT_DISCOVERY_ACROSS_FILESYSTEM=",
		"LC_ALL=", "LC_MESSAGES=",
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		skip := false
		for _, prefix := range drop {
			if strings.HasPrefix(kv, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			env = append(env, kv)
		}
	}
	return append(env, "LC_ALL=C")
}

// canonical is the physical spelling of path, symlinks followed. A path that
// does not exist yet — the state directory before the first run — is resolved
// as far as its parent, so the answer still lines up with the walk once the
// directory appears.
func canonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(canonical(parent), filepath.Base(path))
}

// contains reports whether root is dir or one of its ancestors, by path
// component, so /a/bc does not read as containing /a/b.
func contains(root, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(dir))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
	mode, warn, modeErr := sandbox.Resolve(ctx, *sandboxMode, sandbox.Detect)
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
	// The mode goes into the record as the mode, never as the request: a run
	// that asked for auto and got worktree must not read as a container run in
	// the one document a later person checks it against.
	r, err := runrec.New(*runDir, runrec.Meta{Kind: "build", Subject: ref, Repo: *repo, Sandbox: string(mode)})
	if err != nil {
		return fail(err)
	}
	fmt.Printf("Run: %s\nDir: %s\n\n", r.ID, r.Dir)
	// Every recorded run has one stream, opened with the record. This command
	// writes no command events yet, so what the stream holds today is the
	// refusal — and a refusal that only existed in run.json would leave the
	// events file a reader follows with no account of why the run stopped.
	led, err := runrec.OpenLedger(filepath.Join(r.Dir, "run.ndjson"))
	if err != nil {
		if err := r.Finish(report.ExitCode(err)); err != nil {
			fmt.Fprintf(os.Stderr, "agent-runner: record run outcome: %v\n", err)
			return 1
		}
		return fail(fmt.Errorf("open record: %w", err))
	}
	defer closeRecord(led)
	r.SetLedger(led)
	// A refusal is a finished run, so it gets a record with its own state rather
	// than a directory that says running for a process that already exited.
	if modeErr != nil {
		if !refusalRecorded(r, refusalForMode(modeErr)) {
			return 1
		}
		if err := r.Finish(report.ExitCode(modeErr)); err != nil {
			fmt.Fprintf(os.Stderr, "agent-runner: record run outcome: %v\n", err)
			return 1
		}
		return fail(modeErr)
	}
	code, planErr := planAndPrint(ctx, ref, *repo, *gate)
	if !refusalRecorded(r, refusalOf(planErr)) {
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

// refusalOf returns the refusal carried by err, or nil when err is not one: a
// stage that failed for its own reasons has nothing to record here. Only a
// *refuse.Refusal is recorded — writing an arbitrary error as if it were a
// refusal would put a code on something no vocabulary covers.
func refusalOf(err error) *refuse.Refusal {
	var ref *refuse.Refusal
	if errors.As(err, &ref) {
		return ref
	}
	return nil
}

// refusalRecorded records the refusal and reports whether the run may go on to
// Finish. A refusal the record holds is enough: a lost event is said out loud
// and the run is still refused, because a record left at running for a process
// that has already exited is the state Finish exists to prevent. An error that
// means the record does not hold the refusal leaves nothing for Finish to
// carry, so the caller stops instead of writing an outcome with no reason.
func refusalRecorded(r *runrec.Run, ref *refuse.Refusal) bool {
	if r == nil || ref == nil {
		return true
	}
	err := r.Refuse(ref)
	if err == nil {
		return true
	}
	fmt.Fprintf(os.Stderr, "agent-runner: record refusal: %v\n", err)
	return errors.Is(err, runrec.ErrEventLost)
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
  agent-runner run [--sandbox auto|container|worktree] [--run-dir <dir>] [--] <task>
                                                                 run the offline mock agent
                                                                 (flags come before the task; "--" lets a task start with a dash)
  agent-runner work <issue-url> --repo <path> [--gate <cmd>] [--run-dir <dir>]
                   [--sandbox auto|container|worktree] --dry-run
                                                                 plan the work, writing nothing but the run record
  agent-runner report <id> [--run-dir <dir>]                     print REPORT.md for a recorded run
  agent-runner replay <run-dir>                                  prove a finished run's action set from its record
                                                                 (needs a World snapshot and an account: a record written by
                                                                 Begin, not one from work --run-dir)
  agent-runner gc [--apply] [--envs <days>] [--keep <n>] [--json]
                                                                 collect the environment tier of PRD §6.3; a dry run
                                                                 unless --apply (the run and worktree tiers are not
                                                                 implemented yet)
  agent-runner --help                                            show this help
  agent-runner --version                                         print the version
`)
}
