package tools

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/report"
)

// creds is a complete, uninteresting identity: enough for Env to build an
// environment, with a token short enough to spot in a dump.
func creds() Credentials {
	return Credentials{Token: "tok-abc", Name: "Marcus", Email: "marcus@example.com"}
}

// noGH points gh's config lookup at an empty directory, so a developer's real
// hosts.yml cannot decide a test's outcome.
func noGH(t *testing.T) {
	t.Helper()
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
}

// envOf turns an environment list into a map for assertions.
func envOf(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		if name, v, ok := strings.Cut(entry, "="); ok {
			out[name] = v
		}
	}
	return out
}

// TestEnvRefusesIncompleteCall covers the inputs Env will not guess at: no run
// directory to put TMPDIR in, and no identity for git to commit under.
func TestEnvRefusesIncompleteCall(t *testing.T) {
	noGH(t)
	full := creds()
	for _, tc := range []struct {
		name string
		dir  string
		in   Credentials
	}{
		{"no run directory", "", full},
		{"no name", t.TempDir(), Credentials{Token: "tok", Email: "marcus@example.com"}},
		{"no email", t.TempDir(), Credentials{Token: "tok", Name: "Marcus"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Env(tc.dir, tc.in); !errors.Is(err, report.ErrRefused) {
				t.Fatalf("error = %v, want a refusal", err)
			}
		})
	}
}

// TestEnvRefusesWithoutGhCredential is the §11 sentence: a run with no token
// and no hosts.yml refuses instead of making an anonymous call it would then
// record as work.
func TestEnvRefusesWithoutGhCredential(t *testing.T) {
	noGH(t)
	_, err := Env(t.TempDir(), Credentials{Name: "Marcus", Email: "marcus@example.com"})
	if !errors.Is(err, report.ErrRefused) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "no gh credential") {
		t.Fatalf("error = %q, want it to name the missing credential", err)
	}
}

// TestEnvChildSeesTMPDIRAndNoParentSecrets is the ticket's done-when: a real
// child prints a TMPDIR under the run directory, and a secret planted in the
// parent does not appear in what the child sees.
func TestEnvChildSeesTMPDIRAndNoParentSecrets(t *testing.T) {
	printer, err := exec.LookPath("env")
	if err != nil {
		t.Skip("no env(1) to print a child environment")
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "planted-secret")
	dir := t.TempDir()
	env, err := Env(dir, creds())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(printer)
	cmd.Env = env
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("run child: %v", err)
	}
	child := envOf(strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"))
	if got, want := child["TMPDIR"], filepath.Join(dir, "tmp"); got != want {
		t.Fatalf("child TMPDIR = %q, want %q", got, want)
	}
	if _, ok := child["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Fatal("child inherited AWS_SECRET_ACCESS_KEY; the allowlist is not doing its job")
	}
	if strings.Contains(string(raw), "planted") {
		t.Fatalf("a planted value reached the child:\n%s", raw)
	}
}

// TestEnvAllowlist covers what does and does not cross the boundary, and the
// fixed values that do not depend on the parent at all.
func TestEnvAllowlist(t *testing.T) {
	t.Setenv("LANG", "en_ZA.UTF-8")
	t.Setenv("TZ", "Africa/Johannesburg")
	dir := t.TempDir()
	env, err := Env(dir, creds())
	if err != nil {
		t.Fatal(err)
	}
	got := envOf(env)
	want := map[string]string{
		"PATH":                os.Getenv("PATH"),
		"LANG":                "en_ZA.UTF-8",
		"TZ":                  "Africa/Johannesburg",
		"HOME":                filepath.Join(dir, "home"),
		"TMPDIR":              filepath.Join(dir, "tmp"),
		"GH_TOKEN":            "tok-abc",
		"GITHUB_TOKEN":        "tok-abc",
		"GIT_AUTHOR_NAME":     "Marcus",
		"GIT_AUTHOR_EMAIL":    "marcus@example.com",
		"GIT_COMMITTER_NAME":  "Marcus",
		"GIT_COMMITTER_EMAIL": "marcus@example.com",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_TERMINAL_PROMPT": "0",
	}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("%s = %q, want %q", name, got[name], v)
		}
	}
	// Anything outside the allowlist is the bug this test exists for, so the
	// check is on the names present rather than on how many there are.
	allowed := map[string]bool{
		"PATH": true, "LANG": true, "LC_ALL": true, "TZ": true,
		"HOME": true, "TMPDIR": true, "GH_TOKEN": true, "GITHUB_TOKEN": true,
		"GIT_AUTHOR_NAME": true, "GIT_AUTHOR_EMAIL": true,
		"GIT_COMMITTER_NAME": true, "GIT_COMMITTER_EMAIL": true,
		"GIT_CONFIG_NOSYSTEM": true, "GIT_TERMINAL_PROMPT": true,
	}
	for name := range got {
		if !allowed[name] {
			t.Errorf("environment carries %s, which the allowlist does not justify", name)
		}
	}
}

// TestEnvCopiesHosts is the second way gh authenticates: no token in the
// environment, but the run's HOME carries a private copy of hosts.yml.
func TestEnvCopiesHosts(t *testing.T) {
	config := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", config)
	body := "github.com:\n    oauth_token: x\n"
	if err := os.WriteFile(filepath.Join(config, "hosts.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env, err := Env(dir, Credentials{Name: "Marcus", Email: "marcus@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envOf(env)["GH_TOKEN"]; ok {
		t.Fatal("a token appeared although the caller passed none")
	}
	copied := filepath.Join(dir, "home", ".config", "gh", "hosts.yml")
	info, err := os.Stat(copied)
	if err != nil {
		t.Fatalf("hosts.yml was not copied: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("copied hosts.yml mode = %o, want 0600", info.Mode().Perm())
	}
}

// TestEnvWritesGitIdentity proves git can commit without the parent's config:
// the identity lives in the run's own HOME.
func TestEnvWritesGitIdentity(t *testing.T) {
	dir := t.TempDir()
	first, err := Env(dir, creds())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "home", ".gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name = Marcus", "email = marcus@example.com"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf(".gitconfig = %q, want it to contain %q", data, want)
		}
	}
	// A resumed run calls Env again over its own directories; that must not be
	// the failure that ends the resume.
	second, err := Env(dir, creds())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatalf("second call differs:\n%v\n%v", second, first)
	}
}

// TestEnvDirectoriesArePrivate covers the mode gc and the record rely on: the
// run's scratch space belongs to the run.
func TestEnvDirectoriesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful on windows")
	}
	dir := t.TempDir()
	if _, err := Env(dir, creds()); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"tmp", "home"} {
		info, err := os.Stat(filepath.Join(dir, sub))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s mode = %o, want 0700", sub, got)
		}
	}
}

// TestRedactArgv covers the record's side of the rule: a secret in an argv is
// replaced, by name and by value, before the ledger sees it.
func TestRedactArgv(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want []string
	}{
		{"assignment by name", []string{"gh", "GH_TOKEN=secret", "pr", "list"}, []string{"gh", "GH_TOKEN=REDACTED", "pr", "list"}},
		{"value repeated", []string{"git", "https://x:secret@example.com/r.git"}, []string{"git", "https://x:REDACTED@example.com/r.git"}},
		{"nothing to redact", []string{"ls", "-1"}, []string{"ls", "-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactArgv(tc.argv, []string{"secret"})
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("redactArgv(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

// TestRegistryRecordsScrubbedEnv ties the two halves together: a registry
// handed an environment runs commands with it and says so in the record, and
// one that was not handed one still inherits. The record alone would not carry
// that: dropping `cmd.Env = r.env` from runCommand would leave the ledger
// saying "scrubbed" while the child inherited everything, so the child is made
// to report its own environment and the assertion is on what it saw.
func TestRegistryRecordsScrubbedEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in child is a shell script")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh(1) to build a stand-in child")
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "planted-secret")
	// A cat that prints the environment it was handed. env(1) is not on the
	// command allowlist — a run could dump its own gh token — so an allowed
	// binary that reports what it saw is how the child gets a voice.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "cat"), []byte("#!"+sh+"\nexec env\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	runDir := t.TempDir()
	env, err := Env(runDir, creds())
	if err != nil {
		t.Fatal(err)
	}
	scrubbed := newRegistry(t)
	led := &fakeLedger{}
	scrubbed.SetLedger(led)
	scrubbed.SetEnv(env)
	out, err := run(t, scrubbed, "cat")
	if err != nil {
		t.Fatal(err)
	}
	child := envOf(strings.Split(strings.TrimSpace(out), "\n"))
	if got, want := child["TMPDIR"], filepath.Join(runDir, "tmp"); got != want {
		t.Fatalf("child TMPDIR = %q, want %q: the command did not run under Env", got, want)
	}
	if _, ok := child["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Fatal("the child inherited AWS_SECRET_ACCESS_KEY; SetEnv did not reach the process")
	}
	if entry, ok := led.events[0].data.(commandExit); !ok || entry.Env != scrubbedEnv {
		t.Fatalf("recorded %T, want commandExit with env = %q", led.events[0].data, scrubbedEnv)
	}

	plain := newRegistry(t)
	led = &fakeLedger{}
	plain.SetLedger(led)
	if _, err := run(t, plain, "ls -1"); err != nil {
		t.Fatal(err)
	}
	if entry, ok := led.events[0].data.(commandExit); !ok || entry.Env != "" {
		t.Fatalf("recorded env = %q, want empty for an inherited environment", entry.Env)
	}
}
