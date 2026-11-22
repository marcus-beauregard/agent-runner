// Package gate runs one gate command and reads the shell's answer to it.
//
// A gate that could not run is not a gate that failed. When the shell reports a
// program it could not find, the code was never given the chance to be wrong,
// so the run refuses (PRD §11) rather than blaming the patch for the machine
// (PRD §13). The answer is observed from what the shell printed, never
// predicted from the command line: a command line is not a program list, and
// every reader of one written by hand has refused a gate that runs.
package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// shell is the interpreter a gate's command line runs under. It is an
	// absolute path, because the PATH being questioned is the one /bin/sh will
	// search for the gate's own programs.
	shell = "/bin/sh"
	// tailLines and tailBytes bound the output a Result carries: the end of
	// what the gate printed, never the whole transcript. A person deciding what
	// to do next needs the last lines, not a build log in the record.
	tailLines = 40
	tailBytes = 8 << 10
	// maxLine bounds the piece of a line the finder holds while it waits for a
	// newline. A shell's not-found line is short; anything longer is output, and
	// dropping it costs a name at worst, never the exit code.
	maxLine = 4 << 10
	// waitDelay is how long Wait keeps reading a stream after the shell has
	// exited or its context has ended. A gate that leaves a background child
	// holding the pipe would otherwise block Run past the deadline it was given,
	// and a caller cannot recover from a function that never returns.
	waitDelay = time.Second
)

// notFound matches a shell's own report that a program is not on PATH, in the
// wordings the Linux shells use:
//
//	/bin/sh: 1: go: not found
//	bash: line 1: go: command not found
//
// The prefix is what makes it the shell talking. A program that prints
// "go: not found" itself has the words and not the shape, and a gate whose own
// output contains them must not read as a refusal.
var notFound = regexp.MustCompile(`^[^:\n]+: (?:line )?[0-9]+: ([^:\n]+): (?:command )?not found$`)

// Result is what one gate command did.
type Result struct {
	// Command is the gate as it was given, not a normalised copy: a record
	// quoting the repo's own command back is the point of quoting it.
	Command string
	// Exit is the shell's exit status, or -1 when the shell did not finish.
	Exit int
	// Duration is wall clock, as a person reads it rather than as a statistic.
	Duration time.Duration
	// Tail is the end of what the gate printed — both streams, in the order each
	// stream wrote them — without a trailing newline. Two streams are copied by
	// two readers, so the order holds within a stream, not across the two.
	Tail string
	// Missing names the programs the shell said it could not find, in the order
	// it said so, without repeats.
	Missing []string
}

// Unavailable reports whether the gate never ran the programs it names: the
// shell exited 127 or 126, it reported a program it could not find and the gate
// failed as well, or the shell itself did not finish. 127 is a program the shell
// could not find; 126 is one it found and could not execute, which a lost exec
// bit or a noexec mount makes of a checkout's own script. Either way the code
// was never given the chance to be wrong. An exit below zero is a shell killed
// from outside — an OOM kill or an operator — which is the host's answer, not
// the patch's, and must not be recorded as a gate that failed. An exit of 0 with
// a missing program is not unavailable either — the caller wrote a gate whose
// status is not that program's — and belongs in the record as a warning rather
// than a refusal.
func (r Result) Unavailable() bool {
	return r.Exit < 0 || r.Exit == 126 || r.Exit == 127 || (r.Exit != 0 && len(r.Missing) > 0)
}

// Run executes one gate command under /bin/sh in dir and reports what it did.
//
// A gate that exits non-zero is a Result, not an error: the exit code is the
// gate's answer, and what it means for the run is the caller's decision. The
// error is reserved for a gate that could not be started at all, for a context
// that ended, and for a group that could not be killed, where there is no
// answer to report or no clean host to leave. A child the gate left behind is
// not an error either: killProcessGroup reaps it after the shell exits, and the
// shell's exit status still stands.
func Run(ctx context.Context, dir, command string) (Result, error) {
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	cmd.Dir = dir
	// The gate's streams are writers, so the shell's output reaches them through
	// copying goroutines that a grandchild inheriting the pipe keeps alive.
	// WaitDelay bounds that wait: after the shell exits or the context ends, the
	// pipe is closed and Run returns with the answer it has.
	cmd.WaitDelay = waitDelay
	setProcessGroup(cmd)
	var out tailWriter
	var said finder
	cmd.Stdout = &out
	cmd.Stderr = io.MultiWriter(&out, &said)
	start := time.Now()
	runErr := cmd.Run()
	// Whatever is still in the group outlived the shell: a background child the
	// gate started. It is killed here, not at the run's exit, because PRD §12
	// says a run leaves no process behind, and this is the last moment the gate
	// can be held to it.
	_, killErr := killProcessGroup(cmd)
	result := Result{Command: command, Exit: -1, Duration: time.Since(start)}
	if cmd.ProcessState != nil {
		result.Exit = cmd.ProcessState.ExitCode()
	}
	said.flush()
	result.Missing = said.missing
	result.Tail = out.tail()
	if killErr != nil {
		return result, fmt.Errorf("kill gate process group: %w", killErr)
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("run gate: %w", err)
	}
	var exit *exec.ExitError
	switch {
	case runErr == nil, errors.As(runErr, &exit):
	case errors.Is(runErr, exec.ErrWaitDelay):
		// The shell exited and a child kept its pipe open past waitDelay. The
		// wait is over and the gate still answered, so this is not a failure to
		// start: the exit code in result is the answer.
	default:
		return result, fmt.Errorf("run gate: %w", runErr)
	}
	return result, nil
}

// tailWriter keeps the end of everything written to it, so a gate that prints a
// build log costs its tail and not the log. The two streams are copied by two
// goroutines, so the buffer is guarded: a gate that writes to stdout and stderr
// at once is ordinary, and a record built from an unguarded buffer would be a
// record of a race.
type tailWriter struct {
	mu   sync.Mutex
	kept []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.kept = append(w.kept, p...)
	if over := len(w.kept) - tailBytes; over > 0 {
		// Cut at the next newline when the window holds one: a tail that began
		// with the second half of a line would read as output the gate never
		// wrote. A single line longer than the window has no newline to cut at,
		// so it is cut where the window starts.
		if next := bytes.IndexByte(w.kept[over:], '\n'); next >= 0 {
			over += next + 1
		}
		w.kept = w.kept[over:]
	}
	return len(p), nil
}

// tail renders the kept bytes as the last tailLines whole lines.
func (w *tailWriter) tail() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	lines := strings.Split(strings.TrimRight(string(w.kept), "\n"), "\n")
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	return strings.Join(lines, "\n")
}

// finder reads stderr as it arrives and keeps the programs the shell reported
// as not found. It scans line by line, so the names it holds are bounded by the
// commands the gate ran rather than by how much it printed.
type finder struct {
	line    []byte
	dropped bool // the current line was longer than maxLine and cannot be matched
	missing []string
	seen    map[string]bool
}

func (f *finder) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != '\n' {
			if len(f.line) < maxLine {
				f.line = append(f.line, b)
			} else {
				f.dropped = true
			}
			continue
		}
		f.endLine()
	}
	return len(p), nil
}

// flush matches a last line the shell wrote without a newline after it.
func (f *finder) flush() {
	if len(f.line) > 0 {
		f.endLine()
	}
}

// endLine matches one complete line and clears the buffer for the next.
func (f *finder) endLine() {
	if !f.dropped {
		if m := notFound.FindSubmatch(f.line); m != nil {
			f.add(string(m[1]))
		}
	}
	f.line, f.dropped = f.line[:0], false
}

// add keeps one name once, in the order the shell first said it.
func (f *finder) add(name string) {
	if f.seen == nil {
		f.seen = make(map[string]bool)
	}
	if f.seen[name] {
		return
	}
	f.seen[name] = true
	f.missing = append(f.missing, name)
}
