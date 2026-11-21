// The environment a child gets is part of the cage, not a detail of the call:
// a process that inherits the parent's environment can read every credential
// on the box, and one that inherits its TMPDIR writes outside the run. This
// file builds the environment from an allowlist and puts scratch space inside
// the run directory.
//
// It is a boundary drawn in the child's environment, not a sandbox. A tool
// that hardcodes /tmp, or reads a secret from somewhere other than the
// environment, still escapes both rules. The container sandbox is what closes
// that gap; this is the part that stops the ordinary case of a child being
// handed the whole machine's environment by default.
package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/marcus-beauregard/agent-runner/internal/report"
)

// Credentials is what a run may hand a child: the token gh authenticates with
// and the identity git commits under. It is passed in rather than read from
// the parent's environment, so what a child sees is a decision the caller
// made and can record — not whatever happened to be exported.
type Credentials struct {
	// Token becomes GH_TOKEN and GITHUB_TOKEN. When it is empty the run falls
	// back to a copy of gh's hosts.yml, and with neither there is no honest
	// way to run gh at all.
	Token string
	// Name and Email are the git identity. A child that cannot commit is not
	// a build run, so an empty pair is a refusal rather than an anonymous
	// commit under whatever the machine defaults to.
	Name  string
	Email string
}

// envPassthrough is every name carried over from the parent when it is set.
// PATH is not optional — a child with no search path cannot find the binary it
// was asked to run — and the rest are the locale and clock a tool needs to
// spell a filename and a timestamp the way the run expects.
var envPassthrough = []string{"PATH", "LANG", "LC_ALL", "TZ"}

const (
	// scrubbedEnv is what the record says about the environment of a command
	// that ran under Env. The record names the policy and never a value.
	scrubbedEnv = "scrubbed"
	// redacted is what a secret becomes in a recorded argv.
	redacted = "REDACTED"
)

// secretNames are the environment names whose values a record must never
// carry. An argv entry like GH_TOKEN=... is a mistake, not a technique, and
// the ledger is not the place to discover it.
var secretNames = []string{"GH_TOKEN", "GITHUB_TOKEN"}

// Env builds the child environment: an allowlist of the parent's variables, a
// HOME and TMPDIR inside the run directory, and the credentials the caller
// passed in. Nothing else is inherited — a parent's AWS_SECRET_ACCESS_KEY is
// not a child's business, and the test that plants one is the point.
//
// It refuses rather than guessing. An empty run directory, a missing git
// identity, no PATH, or no way for gh to authenticate are all refusals
// (PRD §11), because every one of them would otherwise produce a command that
// runs while being unable to do the job it was recorded as doing.
func Env(runDir string, creds Credentials) ([]string, error) {
	if runDir == "" {
		return nil, refuse("env: no run directory")
	}
	if creds.Name == "" || creds.Email == "" {
		return nil, refuse("env: no git identity; a child that cannot commit is not a build run")
	}
	abs, err := filepath.Abs(runDir)
	if err != nil {
		return nil, fmt.Errorf("env: resolve run directory: %w", err)
	}
	tmp, home := filepath.Join(abs, "tmp"), filepath.Join(abs, "home")
	for _, dir := range []string{tmp, home} {
		if err := ensurePrivate(dir); err != nil {
			return nil, err
		}
	}
	if err := writeGitConfig(home, creds); err != nil {
		return nil, err
	}

	env := make([]string, 0, 16)
	for _, name := range envPassthrough {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			env = append(env, name+"="+v)
		}
	}
	if !hasName(env, "PATH") {
		return nil, refuse("env: PATH is not set; a child with no search path cannot run")
	}
	// TMPDIR is the rule PRD §6.2 states: scratch space lands inside the run,
	// so a tool that asks the system where to write gets an answer that gc can
	// count and the host never sees.
	env = append(env, "HOME="+home, "TMPDIR="+tmp)
	// git reads /etc/gitconfig unless told not to, and that file can name
	// hooks and URL rewrites the run never chose. A prompt would hang a run
	// with no terminal; a refusal is the honest failure.
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	env = append(env,
		"GIT_AUTHOR_NAME="+creds.Name, "GIT_AUTHOR_EMAIL="+creds.Email,
		"GIT_COMMITTER_NAME="+creds.Name, "GIT_COMMITTER_EMAIL="+creds.Email)

	if creds.Token != "" {
		return append(env, "GH_TOKEN="+creds.Token, "GITHUB_TOKEN="+creds.Token), nil
	}
	if err := copyHosts(home); err != nil {
		return nil, err
	}
	return env, nil
}

// ensurePrivate creates dir if needed and leaves it 0700 either way. MkdirAll
// does not repair the mode of a directory that already exists, and a run
// directory handed over by a previous process is exactly that case.
func ensurePrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("env: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("env: protect %s: %w", dir, err)
	}
	return nil
}

// writeGitConfig writes the run's own .gitconfig: the identity git commits
// under, and nothing the host could have put there. A commit made in this run
// is attributable to the account the run acted as, which is the same account
// the record names.
func writeGitConfig(home string, creds Credentials) error {
	body := "[user]\n\tname = " + creds.Name + "\n\temail = " + creds.Email + "\n"
	path := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("env: write %s: %w", path, err)
	}
	return nil
}

// copyHosts copies gh's hosts.yml into the run's HOME. gh authenticates from
// that file exactly as it does for a person, and the copy means the run's
// credential cannot be rewritten by a concurrent run in the same account.
// GH_CONFIG_DIR is honoured because it is how gh itself is pointed elsewhere.
func copyHosts(home string) error {
	dir := os.Getenv("GH_CONFIG_DIR")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return refuse("env: no gh credential: %v", err)
		}
		dir = filepath.Join(base, "gh")
	}
	src := filepath.Join(dir, "hosts.yml")
	data, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return refuse("env: no gh credential: set GH_TOKEN or sign gh in (%s does not exist)", src)
	}
	if err != nil {
		return fmt.Errorf("env: read %s: %w", src, err)
	}
	dst := filepath.Join(home, ".config", "gh")
	if err := ensurePrivate(dst); err != nil {
		return err
	}
	path := filepath.Join(dst, "hosts.yml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("env: write %s: %w", path, err)
	}
	return nil
}

// hasName reports whether env already carries the named variable.
func hasName(env []string, name string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, name+"=") {
			return true
		}
	}
	return false
}

// secretValues returns the values of the secret-bearing names in env, so a
// recorded argv can be checked against what it must never repeat.
func secretValues(env []string) []string {
	values := make([]string, 0, len(secretNames))
	for _, entry := range env {
		name, v, ok := strings.Cut(entry, "=")
		if ok && v != "" && contains(secretNames, name) {
			values = append(values, v)
		}
	}
	return values
}

// redactArgv returns argv with secrets removed before it reaches the record: a
// NAME=VALUE whose name is a secret becomes NAME=REDACTED, and a token value
// spelled anywhere else is replaced too. The ledger shows that the command ran
// and what it was asked to do, never what it carried.
func redactArgv(argv, secrets []string) []string {
	out := make([]string, len(argv))
	for i, arg := range argv {
		if name, _, ok := strings.Cut(arg, "="); ok && contains(secretNames, name) {
			out[i] = name + "=" + redacted
			continue
		}
		value := arg
		for _, secret := range secrets {
			value = strings.ReplaceAll(value, secret, redacted)
		}
		out[i] = value
	}
	return out
}

// refuse wraps report.ErrRefused, the sentinel report.ExitCode maps to 3.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", report.ErrRefused, fmt.Sprintf(format, args...))
}
