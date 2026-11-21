// Package tools holds the safe-by-default tool registry the agent runs
// against: read_file, write_file, and an allowlisted command runner.
package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/marcus-beauregard/agent-runner/internal/policy"
	"github.com/marcus-beauregard/agent-runner/internal/refuse"
	"github.com/marcus-beauregard/agent-runner/internal/sandbox"
)

const (
	// defaultCommandTimeout bounds a single command unless the caller
	// overrides the tool's timeout.
	defaultCommandTimeout = 10 * time.Minute
	// toolRunCommand is the registry's command-running tool.
	toolRunCommand = "run_command"
	// kindCommandExit is the event one completed command leaves in the run
	// ledger.
	kindCommandExit = "cmd:exit"
	// outputCap bounds stdout and stderr of a command, matching the JS maxBuffer.
	outputCap = 1 << 20 // 1 MiB
	// readFileCap is the largest file read_file will return.
	readFileCap = 2 << 20 // 2 MiB
)

// shellChars are the metacharacters a shell would interpret. Encountering one
// in a command means the model is trying to reach a shell; we refuse rather
// than silently treat it as part of a filename.
const shellChars = ";&|<>$`*?()[]{}~!\\"

// Ledger is the part of a run's event stream the registry writes to. It is
// implemented by *run.Ledger; a Registry with no ledger records nothing.
type Ledger interface {
	Append(kind, stage string, data any) error
}

// commandExit is the payload of a cmd:exit event: the command's name, the argv
// that was executed (never the environment), and how it ended. Killed names
// why the group was killed — "timeout", "canceled", or a signal — and ExitCode
// is -1 then, because a killed process has no exit status of its own. Refused
// names why the tool call failed after the process ran — "output-limit",
// "wait-delay", or "kill-failed" — so a record never reads as success for a
// call that failed. "kill-failed" wins when more than one applies: the others
// say why the call ended, it says the group may still be running.
// Orphans says the command exited on its own and its group still held a member,
// which the kill after Wait then reached — the case Killed cannot describe,
// because the command itself was not stopped.
type commandExit struct {
	Name       string   `json:"name"`
	Argv       []string `json:"argv"`
	DurationMS int64    `json:"duration_ms"`
	ExitCode   int      `json:"exit_code"`
	Killed     string   `json:"killed,omitempty"`
	Refused    string   `json:"refused,omitempty"`
	Orphans    bool     `json:"orphans,omitempty"`
	// Env names the environment policy the command ran under, never a value.
	// "scrubbed" says the child got Env's allowlist; empty says it inherited,
	// which is what a registry nobody configured still does.
	Env string `json:"env,omitempty"`
}

// Registry is a set of tools confined to a single working directory.
type Registry struct {
	root     string
	names    []string
	tools    map[string]func(context.Context, map[string]any) (string, error)
	ledger   Ledger
	timeouts map[string]time.Duration
	// env is the child environment from Env; nil means inherit. secrets are
	// the values inside it that a recorded argv must not repeat.
	env     []string
	secrets []string
	// isolation is the argv every command is prefixed with: bubblewrap and its
	// mounts when the run got a container, nil on the host. It is built once,
	// because the mounts do not change between commands in one run.
	isolation []string
}

// New resolves root to an absolute, symlink-free path and returns a Registry
// whose tools all operate inside it.
func New(root string) (*Registry, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	r := &Registry{root: real, timeouts: make(map[string]time.Duration)}
	r.names = []string{"read_file", "write_file", toolRunCommand}
	r.tools = map[string]func(context.Context, map[string]any) (string, error){
		"read_file":    r.readFile,
		"write_file":   r.writeFile,
		toolRunCommand: r.runCommand,
	}
	return r, nil
}

// SetLedger attaches the run's event stream, so every command appends one
// cmd:exit event. A nil ledger, the default, records nothing.
func (r *Registry) SetLedger(l Ledger) { r.ledger = l }

// SetEnv gives every command the environment built by Env. A nil slice
// restores inheritance, which is the default; an empty non-nil slice is an
// environment with nothing in it, so the two are not the same call.
func (r *Registry) SetEnv(env []string) {
	r.env = env
	r.secrets = secretValues(env)
}

// Isolation is where commands run: the mode the run was granted and the
// directory the mode's scratch space lives in. RunDir is only read for
// container mode, which needs a tmp and a home to mount.
type Isolation struct {
	Mode   sandbox.Mode
	RunDir string
}

// namespaceProbeTimeout bounds the one bwrap run that proves the namespace can
// be built with this run's mounts.
const namespaceProbeTimeout = 10 * time.Second

// SetIsolation puts every later command inside the isolation it names. A
// worktree run — and the zero Isolation — runs on the host unchanged, because
// that is what the weaker mode is. Container mode prepends bubblewrap and the
// mounts from sandbox to every argv.
//
// The registry's own root is what /work binds, never a path guessed from
// RunDir: checkPath already confines every tool to that root, so mounting
// anything else would report a confinement the runner does not honour.
//
// A host that cannot run bwrap is a refusal wrapping report.ErrRefused, which
// the pipeline maps to exit 3. It is decided here, before the first command,
// rather than discovered as a failed command in the middle of a run.
func (r *Registry) SetIsolation(iso Isolation) error {
	r.isolation = nil
	switch iso.Mode {
	case "", sandbox.Worktree:
		return nil
	case sandbox.Container:
	default:
		return fmt.Errorf("unknown sandbox mode %q", iso.Mode)
	}
	args, err := sandbox.ArgsWithWorktree(r.root, iso.RunDir, sandbox.Config{})
	if err != nil {
		// The mounts are the claim, so a source that will not bind is a
		// refusal with its own words — not a plain error that the record
		// would have to drop for want of a code.
		return refuse.New(refuse.SandboxUnavailable, "sandbox",
			fmt.Sprintf("container mounts: %v", err))
	}
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		return refuse.New(refuse.SandboxUnavailable, "sandbox",
			fmt.Sprintf("container sandbox unavailable: %v", err))
	}
	prefix := append([]string{bin}, args...)
	// Detect proves bwrap can build a namespace; it cannot prove it can build
	// this one. Running the exact argv once with a command that does nothing
	// turns a mount or namespace failure into a refusal before the first
	// command, instead of a command that failed for a reason the record reads
	// as the model's own work.
	ctx, cancel := context.WithTimeout(context.Background(), namespaceProbeTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, append(append([]string{}, args...), "true")...).CombinedOutput(); err != nil {
		reason := strings.TrimSpace(string(out))
		if reason == "" {
			reason = err.Error()
		}
		if i := strings.IndexByte(reason, '\n'); i >= 0 {
			reason = reason[:i]
		}
		return refuse.New(refuse.SandboxUnavailable, "sandbox",
			fmt.Sprintf("container sandbox unavailable: %s", reason))
	}
	r.isolation = prefix
	return nil
}

// SetTimeout overrides the timeout for one tool. A non-positive duration
// restores the default.
func (r *Registry) SetTimeout(tool string, d time.Duration) {
	if d <= 0 {
		delete(r.timeouts, tool)
		return
	}
	r.timeouts[tool] = d
}

// timeout returns the tool's timeout, or the default when none is set.
func (r *Registry) timeout(tool string) time.Duration {
	if d, ok := r.timeouts[tool]; ok {
		return d
	}
	return defaultCommandTimeout
}

// Names returns the tool names in declared order, for prompts and parseAction.
func (r *Registry) Names() []string { return r.names }

// Has reports whether name is a known tool.
func (r *Registry) Has(name string) bool {
	_, ok := r.tools[name]
	return ok
}

// Run executes a tool by name and returns the observation string on success,
// or a descriptive error the agent feeds back to the model.
func (r *Registry) Run(ctx context.Context, name string, args map[string]any) (string, error) {
	fn, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return fn(ctx, args)
}

// strArg extracts a required string argument from the JSON the model produced.
func strArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("%s is required", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}

// tokenize splits a command line into argv without invoking a shell. Quoting
// groups arguments; anything a shell would interpret is rejected with a clear
// error rather than silently becoming part of a filename.
func tokenize(command string) ([]string, error) {
	tokens := make([]string, 0)
	var current strings.Builder
	var quote rune
	started := false
	for _, ch := range command {
		if ch == '\x00' || ch == '\n' || ch == '\r' {
			return nil, errors.New("invalid character in command")
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				current.WriteRune(ch)
			}
			continue
		}
		switch {
		case ch == '"' || ch == '\'':
			quote = ch
			started = true
		case unicode.IsSpace(ch):
			if started {
				tokens = append(tokens, current.String())
				current.Reset()
				started = false
			}
		case strings.ContainsRune(shellChars, ch):
			return nil, fmt.Errorf("shell syntax is not supported: %c", ch)
		default:
			current.WriteRune(ch)
			started = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in command")
	}
	if started {
		tokens = append(tokens, current.String())
	}
	return tokens, nil
}

// checkFlag enforces a binary's flag allowlist. A `name=value` form is only
// accepted when the name is allowed and the value is a plain token.
func checkFlag(bin string, flags []string, arg string) error {
	eq := strings.IndexByte(arg, '=')
	name := arg
	if eq >= 0 {
		name = arg[:eq]
	}
	if !contains(flags, name) {
		return fmt.Errorf("%s: argument not allowed: %s", bin, arg)
	}
	if eq >= 0 && !isFlagValue(arg[eq+1:]) {
		return fmt.Errorf("%s: argument not allowed: %s", bin, arg)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func isFlagValue(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if !(ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' ||
			ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

// escapes rejects a resolved path that is not the root or a child of it.
func (r *Registry) escapes(arg, resolved, root string) error {
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return fmt.Errorf("path escapes the working directory: %s", arg)
	}
	return nil
}

// checkPath rejects absolute paths and ".." components, then resolves the
// deepest existing ancestor of the path and confirms it stays inside root.
// Resolving an existing ancestor defeats symlinks that would otherwise smuggle
// a path outside the working directory. This guards against a model naming the
// wrong path; it is not a sandbox against a concurrent local writer, since the
// file is reopened by name after the check and a swapped symlink would still
// win the race.
func (r *Registry) checkPath(arg string) error {
	if strings.HasPrefix(arg, "/") || hasDotDot(arg) {
		return fmt.Errorf("path escapes the working directory: %s", arg)
	}
	probe := filepath.Join(r.root, arg)
	missing := make([]string, 0)
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			return r.escapes(arg, filepath.Join(append([]string{real}, missing...)...), r.root)
		}
		if !isNotFound(err) {
			return err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("path cannot be resolved: %s", arg)
		}
		missing = append([]string{filepath.Base(probe)}, missing...)
		probe = parent
	}
}

// hasDotDot reports whether any path component, on either separator, is "..".
func hasDotDot(arg string) bool {
	return contains(strings.FieldsFunc(arg, func(c rune) bool { return c == '/' || c == '\\' }), "..")
}

func isNotFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// parseCommand validates a command line and returns the argv to execute.
func (r *Registry) parseCommand(command string) ([]string, error) {
	argv, err := tokenize(command)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	bin := argv[0]
	p, ok := policy.Commands[bin]
	if !ok {
		return nil, fmt.Errorf("command not allowed: %s", bin)
	}
	args := argv[1:]
	flags := p.Flags
	if p.Subcommands != nil {
		sub := ""
		if len(args) > 0 {
			sub = args[0]
		}
		subFlags, ok := p.Subcommands[sub]
		if !ok {
			if sub == "" {
				sub = "(none)"
			}
			return nil, fmt.Errorf("%s: subcommand not allowed: %s", bin, sub)
		}
		flags = subFlags
	}
	for i, arg := range args {
		if strings.HasPrefix(arg, "-") {
			if err := checkFlag(bin, flags, arg); err != nil {
				return nil, err
			}
		} else if p.Subcommands != nil {
			// args[0] is the subcommand; later non-flag operands are paths,
			// except rev:path forms, which name objects inside the repository.
			if i > 0 && !strings.Contains(arg, ":") {
				if err := r.checkPath(arg); err != nil {
					return nil, err
				}
			}
		} else {
			if err := r.checkPath(arg); err != nil {
				return nil, err
			}
		}
	}
	return argv, nil
}

func (r *Registry) readFile(ctx context.Context, args map[string]any) (string, error) {
	name, err := strArg(args, "path")
	if err != nil {
		return "", err
	}
	if err := r.checkPath(name); err != nil {
		return "", err
	}
	full := filepath.Join(r.root, name)
	info, err := os.Stat(full)
	if err != nil {
		return "", err
	}
	if info.Size() > readFileCap {
		return "", fmt.Errorf("file too large to read: %s", name)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (r *Registry) writeFile(ctx context.Context, args map[string]any) (string, error) {
	name, err := strArg(args, "path")
	if err != nil {
		return "", err
	}
	content, err := strArg(args, "content")
	if err != nil {
		return "", err
	}
	if err := r.checkPath(name); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(r.root, name), []byte(content), 0o644); err != nil {
		return "", err
	}
	return "wrote " + name, nil
}

// killGroup is killProcessGroup behind a variable so a test can make the kill
// fail without needing a real EPERM.
var killGroup = killProcessGroup

func (r *Registry) runCommand(ctx context.Context, args map[string]any) (string, error) {
	command, err := strArg(args, "command")
	if err != nil {
		return "", err
	}
	argv, err := r.parseCommand(command)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout(toolRunCommand))
	defer cancel()
	// The recorded argv stays the command the model asked for: the mounts are
	// the run's fixed confinement, not something the model chose, and a record
	// that made it read as if the model had typed --unshare-all would be wrong
	// about who decided what.
	bin, cmdArgv := argv[0], argv[1:]
	if r.isolation != nil {
		bin = r.isolation[0]
		cmdArgv = append(append(make([]string, 0, len(r.isolation)+len(argv)), r.isolation[1:]...), argv...)
	}
	cmd := exec.CommandContext(ctx, bin, cmdArgv...)
	cmd.Dir = r.root
	if r.env != nil {
		cmd.Env = r.env
	}
	// A child that holds the output pipe open would otherwise keep Wait
	// blocked after the command itself has exited. WaitDelay closes the pipe
	// and lets the group kill below run.
	cmd.WaitDelay = time.Second
	setProcessGroup(cmd)
	stdout, stderr := newCappedBuffer(), newCappedBuffer()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)
	// The command is over. Anything still in its group was left behind by it:
	// a backgrounded child, or the whole group when the deadline fired.
	orphans, killErr := killGroup(cmd)
	code, killed := exitStatus(cmd.ProcessState)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		killed = "timeout"
	case ctx.Err() != nil:
		killed = "canceled"
	}
	if killed != "" {
		code = -1
		// A stopped command's group was killed on purpose and Killed already
		// says so; this field is for the group that outlived a command that
		// finished by itself.
		orphans = false
	}
	// Decide the outcome before recording it. The ledger is the run's evidence:
	// an event that asserts a clean exit for a call the tool refused would be a
	// lie, and the exit code alone cannot carry that — the process may well have
	// exited 0 while its output was thrown away.
	var outErr error
	refused := ""
	switch {
	case stdout.exceeded || stderr.exceeded:
		refused, outErr = "output-limit", errors.New("command output exceeds 1 MiB limit")
	case killed == "timeout":
		outErr = errors.New("command timed out")
	case errors.Is(runErr, exec.ErrWaitDelay):
		refused, outErr = "wait-delay", errors.New("command left a child holding its output; the process group was killed")
	case runErr != nil:
		outErr = fmt.Errorf("command failed: %w", runErr)
	}
	if killErr != nil {
		// The group may still be running. A record that said "nothing left
		// behind" would invert on exactly the path where the evidence matters,
		// so the failed kill is named and the call fails. It is named on every
		// path, timeout and cancel included: Killed says the group was
		// signalled on purpose, so a kill that failed cannot be left implied
		// to have worked just because the command had already stopped.
		refused = "kill-failed"
		if outErr == nil {
			outErr = fmt.Errorf("kill process group: %w", killErr)
		} else {
			outErr = fmt.Errorf("%w (kill process group: %v)", outErr, killErr)
		}
	}
	entry := commandExit{
		Name:       argv[0],
		Argv:       redactArgv(argv, r.secrets),
		DurationMS: duration.Milliseconds(),
		ExitCode:   code,
		Killed:     killed,
		Refused:    refused,
		Orphans:    orphans,
	}
	if r.env != nil {
		entry.Env = scrubbedEnv
	}
	recordErr := r.record(entry)
	if outErr != nil {
		if recordErr != nil {
			return "", fmt.Errorf("%w (record %s: %v)", outErr, kindCommandExit, recordErr)
		}
		return "", outErr
	}
	if recordErr != nil {
		return "", fmt.Errorf("record %s: %w", kindCommandExit, recordErr)
	}
	return stdout.String(), nil
}

// record appends one event to the run ledger, if one is attached.
func (r *Registry) record(exit commandExit) error {
	if r.ledger == nil {
		return nil
	}
	return r.ledger.Append(kindCommandExit, "", exit)
}

// cappedBuffer keeps at most outputCap bytes, flagging overflow so the caller
// can refuse rather than hand the model unbounded output.
type cappedBuffer struct {
	buf      bytes.Buffer
	exceeded bool
}

func newCappedBuffer() *cappedBuffer { return &cappedBuffer{} }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.exceeded {
		return len(p), nil
	}
	room := outputCap - c.buf.Len()
	if len(p) > room {
		c.buf.Write(p[:room])
		c.exceeded = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string { return c.buf.String() }
