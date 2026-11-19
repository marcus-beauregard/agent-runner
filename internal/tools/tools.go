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
)

const (
	// commandTimeout bounds a single run_command invocation.
	commandTimeout = 10 * time.Second
	// outputCap bounds stdout and stderr of a command, matching the JS maxBuffer.
	outputCap = 1 << 20 // 1 MiB
	// readFileCap is the largest file read_file will return.
	readFileCap = 2 << 20 // 2 MiB
)

// shellChars are the metacharacters a shell would interpret. Encountering one
// in a command means the model is trying to reach a shell; we refuse rather
// than silently treat it as part of a filename.
const shellChars = ";&|<>$`*?()[]{}~!\\"

// Registry is a set of tools confined to a single working directory.
type Registry struct {
	root  string
	names []string
	tools map[string]func(context.Context, map[string]any) (string, error)
}

// New resolves root to an absolute, symlink-free path and returns a Registry
// whose tools all operate inside it.
func New(root string) (*Registry, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	r := &Registry{root: real}
	r.names = []string{"read_file", "write_file", "run_command"}
	r.tools = map[string]func(context.Context, map[string]any) (string, error){
		"read_file":   r.readFile,
		"write_file":  r.writeFile,
		"run_command": r.runCommand,
	}
	return r, nil
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

func (r *Registry) runCommand(ctx context.Context, args map[string]any) (string, error) {
	command, err := strArg(args, "command")
	if err != nil {
		return "", err
	}
	argv, err := r.parseCommand(command)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = r.root
	stdout, stderr := newCappedBuffer(), newCappedBuffer()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return "", errors.New("command output exceeds 1 MiB limit")
	}
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", errors.New("command timed out")
		}
		return "", fmt.Errorf("command failed: %w", runErr)
	}
	return stdout.String(), nil
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
