package gate_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/gate"
)

// pathWith puts exactly these names on PATH, each an executable that does
// nothing, so the answer comes from the host the test built rather than from
// whatever this machine happens to have installed.
func pathWith(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir)
}

// TestMissingNamesOnlyWhatTheHostLacks is the package's one claim: the first
// word of each command segment is looked up, and everything that is not a
// program word — a quoted argument, a substitution, an assignment, a
// redirection, a shell builtin — is left alone. A false refusal would stop a
// run whose gate works, so the cases that must stay silent outnumber the ones
// that must speak.
func TestMissingNamesOnlyWhatTheHostLacks(t *testing.T) {
	tests := []struct {
		name string
		path []string
		cmd  string
		want []string
	}{
		{
			name: "no gate",
			path: []string{"go"},
			cmd:  "",
			want: nil,
		},
		{
			name: "every program present",
			path: []string{"go"},
			cmd:  "go vet ./... && go test -race ./...",
			want: nil,
		},
		{
			name: "one program missing",
			path: []string{"go"},
			cmd:  "go vet ./... && cloc .",
			want: []string{"cloc"},
		},
		{
			name: "order kept, repeats dropped",
			path: []string{"go"},
			cmd:  "cloc -v && go vet ./... && cloc .",
			want: []string{"cloc"},
		},
		{
			name: "a pipeline names two programs",
			path: []string{"go"},
			cmd:  "go test ./... | tee out.txt",
			want: []string{"tee"},
		},
		{
			name: "a quoted separator is an argument",
			path: []string{"go"},
			cmd:  "go test -run 'a|b' ./...",
			want: nil,
		},
		{
			name: "a substitution is not unravelled",
			path: []string{"go"},
			cmd:  "test -z \"$(gofmt -l .)\" && go vet ./...",
			want: nil,
		},
		{
			name: "an assignment is not a program",
			path: []string{"go"},
			cmd:  "CGO_ENABLED=0 go test ./...",
			want: nil,
		},
		{
			name: "a redirection is not a program",
			path: []string{"go"},
			cmd:  "go test ./... 2>&1",
			want: nil,
		},
		{
			name: "a builtin needs nothing on PATH",
			path: nil,
			cmd:  "test -f go.mod",
			want: nil,
		},
		{
			name: "a keyword is not the program",
			path: []string{"go"},
			cmd:  "if go test ./...; then echo ok; fi",
			want: nil,
		},
		{
			name: "two programs missing, in order",
			path: nil,
			cmd:  "cloc . && misspell .",
			want: []string{"cloc", "misspell"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pathWith(t, tc.path...)
			if got := gate.Missing(tc.cmd); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Missing(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}
