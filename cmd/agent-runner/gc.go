package main

// gcCommand is two of PRD §6.3's three tiers: the environment store and a run's
// checkout. The record tier is not here yet, and a command that quietly did the
// third while its flags suggested three would be worse than one that says which
// it is.
//
// A dry run is the default because the command's whole value is that a person
// can see what it thinks before anything is gone: it prints every candidate with
// its size and the reason it qualifies, and deletes nothing without --apply.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/marcus-beauregard/agent-runner/internal/envstore"
	runrec "github.com/marcus-beauregard/agent-runner/internal/run"
)

// gcReport is --json's shape: what this invocation found, what it collected or
// would collect, and the one number the disk question is asked in.
type gcReport struct {
	// Apply is false for a dry run, so a caller reading the file knows whether
	// the numbers describe something that happened.
	Apply bool `json:"apply"`
	// FreedBytes is what this run freed, or what --apply would free, across both
	// tiers: the disk question is asked once, not per tier.
	FreedBytes int64 `json:"freed_bytes"`
	// Candidates are the environment directories collected, each with its own
	// bytes.
	Candidates []envstore.Entry `json:"candidates"`
	// Kept are the environment directories that stay, with the reason they stay.
	Kept []envstore.Entry `json:"kept"`
	// Worktrees are every run's checkout with its own bytes and decision: the
	// second tier's report, one entry per run. A run whose checkout is already
	// gone is here too, saying so, because "nothing to collect" is an answer the
	// disk question needs as much as a number is.
	Worktrees []runrec.WorktreeEntry `json:"worktrees"`
	// OrphansPruned is how many registrations pointed into a run directory that
	// is gone. With --apply they are dropped; without it, they would be.
	OrphansPruned int `json:"orphans_pruned"`
}

// maxEnvDays is the largest --envs the age multiplication can hold, which is a
// limit of time.Duration rather than of policy: 106751 days is longer than any
// store has existed, and anything above it wraps instead of meaning "forever".
const maxEnvDays = int((1<<63 - 1) / int64(24*time.Hour))

func gcCommand(args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		usage(os.Stdout)
		return 0
	}
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	apply := fs.Bool("apply", false, "collect the candidates instead of listing them")
	envDays := fs.Int("envs", 30, "days an environment must be unused before it qualifies")
	worktreeDays := fs.Int("worktrees", 14, "days a run must have been started before its checkout qualifies")
	keep := fs.Int("keep", 0, "keep the newest N published environments whatever their age")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: gc: %v\n", err)
		return 1
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "agent-runner: gc takes flags only, got %q\n", fs.Args())
		return 1
	}
	if *envDays < 0 || *keep < 0 || *worktreeDays < 0 {
		fmt.Fprintln(os.Stderr, "agent-runner: gc: --envs, --keep and --worktrees cannot be negative")
		return 1
	}
	// A day count is multiplied into a Duration below, so the flag needs the
	// bound the multiplication has: past it the product wraps, and a negative
	// age matches every environment in the store.
	if *envDays > maxEnvDays {
		fmt.Fprintf(os.Stderr, "agent-runner: gc: --envs is %d days, more than the %d it can hold\n", *envDays, maxEnvDays)
		return 1
	}
	if *worktreeDays > maxEnvDays {
		fmt.Fprintf(os.Stderr, "agent-runner: gc: --worktrees is %d days, more than the %d it can hold\n", *worktreeDays, maxEnvDays)
		return 1
	}

	base, err := runrec.StateBase()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: gc: %v\n", err)
		return 1
	}
	policy := envstore.Policy{
		Age:  time.Duration(*envDays) * 24 * time.Hour,
		Keep: *keep,
	}
	entries, err := envstore.Collect(base, policy, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: gc: %v\n", err)
		return 1
	}

	report := gcReport{Apply: *apply}
	for _, e := range entries {
		if e.Collect {
			report.Candidates = append(report.Candidates, e)
		} else {
			report.Kept = append(report.Kept, e)
		}
	}

	// Collect first, then report what actually happened: a candidate that lost
	// its lock to a reader between the listing and the deletion stays, and the
	// report says so rather than a number that describes an intention.
	failed := false
	gone := make([]envstore.Entry, 0, len(report.Candidates))
	for _, e := range report.Candidates {
		if !*apply {
			report.FreedBytes += e.Bytes
			gone = append(gone, e)
			continue
		}
		n, err := envstore.Remove(base, e)
		switch {
		case errors.Is(err, envstore.ErrLocked):
			e.Collect = false
			e.Reason = "held by a reader"
			report.Kept = append(report.Kept, e)
		case err != nil:
			fmt.Fprintf(os.Stderr, "agent-runner: gc: %v\n", err)
			failed = true
		default:
			report.FreedBytes += n
			gone = append(gone, e)
		}
	}
	report.Candidates = gone

	// The second tier: a run's checkout and its cache. It runs after the
	// environment tier because the two share no directory, and before the report
	// because --json's freed_bytes is one number for the whole command.
	worktrees, err := runrec.CollectWorktrees(base, time.Duration(*worktreeDays)*24*time.Hour, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: gc: %v\n", err)
		return 1
	}
	report.Worktrees = worktrees
	for i := range report.Worktrees {
		e := &report.Worktrees[i]
		if !e.Collect {
			continue
		}
		if !*apply {
			report.FreedBytes += e.Bytes
			continue
		}
		n, err := runrec.RemoveWorktreeEntry(*e)
		if err != nil {
			// A closed-cache directory or a parent git cannot rewrite is not a
			// reason to stop over every other run: the entry stays in the report,
			// saying it did not go, and the exit code carries the failure.
			e.Collect = false
			e.Reason = "left in place: " + err.Error()
			fmt.Fprintf(os.Stderr, "agent-runner: gc: %v\n", err)
			failed = true
			continue
		}
		report.FreedBytes += n
	}
	orphans, err := runrec.PruneOrphans(base, *apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: gc: %v\n", err)
		failed = true
	}
	report.OrphansPruned = orphans

	if *asJSON {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-runner: gc: encode report: %v\n", err)
			return 1
		}
		fmt.Printf("%s\n", b)
	} else {
		printGC(report, len(entries))
	}
	if failed {
		return 1
	}
	return 0
}

// printGC is the reading a person gets before they pass --apply, and the receipt
// they get after. Each line is a name, a size and the reason, in the order the
// command decided them.
func printGC(r gcReport, total int) {
	var envBytes int64
	for _, e := range r.Candidates {
		envBytes += e.Bytes
	}
	if r.Apply {
		fmt.Printf("gc: collected %d of %d environment directories, freeing %s\n", len(r.Candidates), total, humanBytes(envBytes))
	} else if len(r.Candidates) == 0 {
		fmt.Printf("gc: nothing to collect in %d environment directories\n", total)
	} else {
		fmt.Printf("gc: dry run, %d of %d environment directories qualify, %s would be freed\n", len(r.Candidates), total, humanBytes(envBytes))
	}
	for _, e := range r.Candidates {
		fmt.Printf("  %10s  %s  %s\n", humanBytes(e.Bytes), e.Name, e.Reason)
	}
	if len(r.Kept) > 0 {
		fmt.Printf("kept %d:\n", len(r.Kept))
		for _, e := range r.Kept {
			fmt.Printf("  %10s  %s  %s\n", humanBytes(e.Bytes), e.Name, e.Reason)
		}
	}
	printWorktrees(r)
	if !r.Apply && (len(r.Candidates) > 0 || collectedWorktrees(r) > 0 || r.OrphansPruned > 0) {
		fmt.Println("gc: run again with --apply to collect them")
	}
}

// printWorktrees is the second tier's lines, in the same shape as the first: a
// person reading a dry run gets one report, not two reports bolted together.
func printWorktrees(r gcReport) {
	total, collected := len(r.Worktrees), collectedWorktrees(r)
	var bytes int64
	for _, e := range r.Worktrees {
		if e.Collect {
			bytes += e.Bytes
		}
	}
	switch {
	case total == 0:
		fmt.Println("gc: no run checkouts")
	case r.Apply:
		fmt.Printf("gc: collected %d of %d run checkouts, freeing %s\n", collected, total, humanBytes(bytes))
	case collected == 0:
		fmt.Printf("gc: nothing to collect in %d run checkouts\n", total)
	default:
		fmt.Printf("gc: dry run, %d of %d run checkouts qualify, %s would be freed\n", collected, total, humanBytes(bytes))
	}
	for _, e := range r.Worktrees {
		if e.Collect {
			fmt.Printf("  %10s  %s  %s\n", humanBytes(e.Bytes), e.ID, e.Reason)
		}
	}
	for _, e := range r.Worktrees {
		if !e.Collect && e.Bytes > 0 {
			fmt.Printf("  %10s  %s  kept: %s\n", humanBytes(e.Bytes), e.ID, e.Reason)
		}
	}
	if r.OrphansPruned > 0 {
		verb := "pruned"
		if !r.Apply {
			verb = "would prune"
		}
		fmt.Printf("gc: %s %d orphan worktree registrations\n", verb, r.OrphansPruned)
	}
}

// collectedWorktrees is the count both the hint and the report line need.
func collectedWorktrees(r gcReport) int {
	n := 0
	for _, e := range r.Worktrees {
		if e.Collect {
			n++
		}
	}
	return n
}

// humanBytes is the size a person reads, one decimal and a unit that fits the
// number: the point of the command is that the disk question has an answer, not
// an exact byte count nobody compares to anything.
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PB", v/unit)
}
