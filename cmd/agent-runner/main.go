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
	"os/signal"
	"strings"
	"syscall"

	"github.com/marcus-beauregard/agent-runner/internal/agent"
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
	result, err := agent.Run(ctx, task, agent.MockLLM{}, agent.Options{}, registry)
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
