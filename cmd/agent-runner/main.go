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
	"github.com/marcus-beauregard/agent-runner/internal/report"
	runrec "github.com/marcus-beauregard/agent-runner/internal/run"
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
// run, writing nothing. The issue URL is the leading positional argument, so it
// is consumed before the flag set, which stops at the first non-flag.
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
	runDir := fs.String("run-dir", "", "base directory for the run record (default: the user state directory)")
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

	if *runDir == "" {
		return planAndPrint(ctx, ref, *repo, *gate)
	}
	r, err := runrec.New(*runDir, runrec.Meta{Kind: "build", Subject: ref, Repo: *repo})
	if err != nil {
		return fail(err)
	}
	fmt.Printf("Run: %s\nDir: %s\n\n", r.ID, r.Dir)
	code := planAndPrint(ctx, ref, *repo, *gate)
	if err := r.Finish(code); err != nil {
		// The run happened; the record of how it ended did not. Say so instead of
		// exiting on the result as though the record could be trusted.
		fmt.Fprintf(os.Stderr, "agent-runner: record run outcome: %v\n", err)
		return 1
	}
	return code
}

// planAndPrint plans the work, prints the plan, and returns the exit code for it.
// Splitting it out lets the recorded path finalize its record whatever the
// plan decided, refusals included.
func planAndPrint(ctx context.Context, ref, repo, gate string) int {
	plan, err := report.Work(ctx, ref, repo, gate)
	fmt.Print(plan.String())
	if err != nil {
		return fail(err)
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
  agent-runner work <issue-url> --repo <path> [--gate <cmd>] [--run-dir <dir>] --dry-run
                                                                 plan the work, write nothing
  agent-runner --help                                            show this help
  agent-runner --version                                         print the version
`)
}
