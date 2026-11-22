package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunReadsWhatTheShellSaid is the table from the issue, measured rather
// than reasoned about: each row is a real /bin/sh with a PATH that holds only
// the programs the row allows, and the answer comes back from the shell that
// ran it. The shapes matter because a reader of the command line got every one
// of them wrong: an assignment, a reserved word, a guard, a pipe, a fallback.
func TestRunReadsWhatTheShellSaid(t *testing.T) {
	cases := []struct {
		name            string
		command         string
		goMod           bool
		path            []string
		wantExit        int
		wantMissing     []string
		wantUnavailable bool
	}{
		{
			name:            "a program this host lacks",
			command:         "go test ./...",
			wantExit:        127,
			wantMissing:     []string{"go"},
			wantUnavailable: true,
		},
		{
			name:            "an assignment in front of the program",
			command:         "GOFLAGS=\"-mod=vendor -tags ci\" go test ./...",
			wantExit:        127,
			wantMissing:     []string{"go"},
			wantUnavailable: true,
		},
		{
			name:            "a word the shell runs itself",
			command:         "time -p go test ./...",
			wantExit:        127,
			wantMissing:     []string{"time"},
			wantUnavailable: true,
		},
		{
			name:            "a guard that holds",
			command:         "if [ -f go.mod ]; then go test; fi",
			goMod:           true,
			wantExit:        127,
			wantMissing:     []string{"go"},
			wantUnavailable: true,
		},
		{
			name:            "a guard that does not hold",
			command:         "if [ -f nope ]; then go test; fi",
			wantExit:        0,
			wantUnavailable: false,
		},
		{
			name:            "a fallback that swallows the failure",
			command:         "cloc . || true",
			wantExit:        0,
			wantMissing:     []string{"cloc"},
			wantUnavailable: false,
		},
		{
			name:            "two gates on one line",
			command:         "go vet ./... && go test ./...",
			wantExit:        127,
			wantMissing:     []string{"go"},
			wantUnavailable: true,
		},
		{
			name:            "a program named by path",
			command:         "./gradlew check",
			wantExit:        127,
			wantMissing:     []string{"./gradlew"},
			wantUnavailable: true,
		},
		{
			name:            "a program inside a substitution",
			command:         "echo \"$(cloc .)\"",
			wantExit:        0,
			wantMissing:     []string{"cloc"},
			wantUnavailable: false,
		},
		{
			name:        "a pipe whose status is not the program's",
			command:     "go test ./... | tail -1",
			path:        []string{"tail"},
			wantExit:    0,
			wantMissing: []string{"go"},
		},
		{
			name:            "a shell that words it differently",
			command:         "bash -c \"go test ./...\"",
			path:            []string{"bash"},
			wantExit:        127,
			wantMissing:     []string{"go"},
			wantUnavailable: true,
		},
		{
			name:            "a gate that prints the words itself",
			command:         "echo \"go: not found\"; exit 1",
			wantExit:        1,
			wantUnavailable: false,
		},
		{
			name:            "a gate that writes 127 for its own reasons",
			command:         "exit 127",
			wantExit:        127,
			wantUnavailable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.goMod {
				if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test\n"), 0o600); err != nil {
					t.Fatalf("write go.mod: %v", err)
				}
			}
			t.Setenv("PATH", onlyPath(t, tc.path...))
			got, err := Run(context.Background(), dir, tc.command)
			if err != nil {
				t.Fatalf("run %q: %v", tc.command, err)
			}
			if got.Command != tc.command {
				t.Errorf("command = %q, want the gate as given", got.Command)
			}
			if got.Exit != tc.wantExit {
				t.Errorf("exit = %d, want %d (tail: %q)", got.Exit, tc.wantExit, got.Tail)
			}
			if strings.Join(got.Missing, ",") != strings.Join(tc.wantMissing, ",") {
				t.Errorf("missing = %q, want %q (tail: %q)", got.Missing, tc.wantMissing, got.Tail)
			}
			if unavailable := got.Unavailable(); unavailable != tc.wantUnavailable {
				t.Errorf("unavailable = %v, want %v", unavailable, tc.wantUnavailable)
			}
		})
	}
}

// TestRunTailIsTheEnd: a gate that prints a log gets its last lines into the
// record, not its first. A tail that began with the top of the log would send a
// reader to the part of the output where nothing went wrong.
func TestRunTailIsTheEnd(t *testing.T) {
	if _, err := exec.LookPath("seq"); err != nil {
		t.Skip("seq is not installed here")
	}
	t.Setenv("PATH", onlyPath(t, "seq"))
	got, err := Run(context.Background(), t.TempDir(), "seq 1 200")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(got.Tail, "\n")
	if len(lines) != tailLines {
		t.Fatalf("tail has %d lines, want %d", len(lines), tailLines)
	}
	if lines[0] != "161" || lines[len(lines)-1] != "200" {
		t.Errorf("tail runs %q..%q, want the last %d lines, 161..200", lines[0], lines[len(lines)-1], tailLines)
	}
}

// TestRunGateThatCannotStartIsAnError: the exit code is the gate's answer, so
// a gate that never started has none. Reporting -1 as if the command had said
// something would put a refusal's evidence on a shell that was never there.
func TestRunGateThatCannotStartIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if _, err := Run(context.Background(), missing, "true"); err == nil {
		t.Fatal("run in a directory that does not exist, want an error")
	}
}

// TestRunEndedContextIsAnError: a canceled gate has no answer either, and the
// caller has to know the difference between a gate that failed and one that was
// stopped.
func TestRunEndedContextIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, t.TempDir(), "true"); err == nil {
		t.Fatal("run with a canceled context, want an error")
	}
}

// onlyPath returns a PATH holding the named programs and nothing else, so a
// gate in the test finds exactly what the case allows it to find.
func onlyPath(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not installed here", name)
		}
		if err := os.Symlink(real, filepath.Join(dir, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	return dir
}
