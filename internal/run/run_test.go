package run

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNewCreatesLayout checks the tree every later stage writes into exists and
// is private: a record holds paths and reasoning.
func TestNewCreatesLayout(t *testing.T) {
	base := t.TempDir()
	r, err := New(base, Meta{Kind: "build", Subject: "https://example.test/issues/1", Repo: "/src/x"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wantDir := filepath.Join(base, "agent-runner", "runs", r.ID)
	if r.Dir != wantDir {
		t.Errorf("Dir = %q, want %q", r.Dir, wantDir)
	}
	for _, path := range []string{
		wantDir,
		filepath.Join(wantDir, "stages"),
		filepath.Join(wantDir, "run.json"),
		filepath.Join(base, "agent-runner", "runs"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing %s: %v", path, err)
		}
	}
	for _, path := range []string{wantDir, filepath.Join(wantDir, "stages"), filepath.Join(base, "agent-runner", "runs")} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s is %04o, want 0700", path, perm)
		}
	}
}

// TestNewRecordsMetadata checks run.json says what the caller said, in the
// documented field order, with no terminal state yet.
func TestNewRecordsMetadata(t *testing.T) {
	base := t.TempDir()
	meta := Meta{
		Kind:    "resolve",
		Subject: "https://example.test/pulls/7",
		Repo:    "/src/x",
		Branch:  "agent/fix-it",
		BaseSHA: "abc123",
		Budget:  "--budget 45m",
	}
	r, err := New(base, meta)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(r.Dir, "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("run.json is not JSON: %v", err)
	}
	want := map[string]any{
		"id":        r.ID,
		"kind":      meta.Kind,
		"subject":   meta.Subject,
		"repo":      meta.Repo,
		"branch":    meta.Branch,
		"base_sha":  meta.BaseSHA,
		"budget":    meta.Budget,
		"state":     stateRunning,
		"exit_code": nil,
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v", key, got[key], value)
		}
	}
	if created, ok := got["created_at"].(string); !ok || created == "" {
		t.Errorf("created_at = %v, want a timestamp", got["created_at"])
	}

	ordered := []string{"id", "created_at", "kind", "subject", "repo", "branch", "base_sha", "budget", "state", "exit_code"}
	for i := 1; i < len(ordered); i++ {
		prev := strings.Index(string(b), "\""+ordered[i-1]+"\"")
		next := strings.Index(string(b), "\""+ordered[i]+"\"")
		if prev < 0 || next < 0 || prev > next {
			t.Fatalf("field %q must follow %q in run.json", ordered[i], ordered[i-1])
		}
	}
}

// TestNewIDsSortNewestLast is the ordering the record index depends on: ids
// sort lexicographically in the order the runs started.
func TestNewIDsSortNewestLast(t *testing.T) {
	base := t.TempDir()
	var ids []string
	for i := 0; i < 3; i++ {
		r, err := New(base, Meta{Kind: "build", Subject: "https://example.test/issues/1"})
		if err != nil {
			t.Fatalf("New %d: %v", i, err)
		}
		ids = append(ids, r.ID)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if sorted[i] != ids[i] {
			t.Fatalf("ids do not sort newest-last: created %v, sorted %v", ids, sorted)
		}
	}
}

// TestNewTightensLooseRoot covers the directory that already exists with the
// wrong mode: it is tightened, not accepted.
func TestNewTightensLooseRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "agent-runner", "runs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("prepare root: %v", err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	if _, err := New(base, Meta{Kind: "build"}); err != nil {
		t.Fatalf("New: %v", err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("root is %04o after New, want 0700", perm)
	}
}

// TestNewResolvesBase covers the two ways the base is chosen when no --run-dir
// is given: the environment, and a refusal for a relative value.
func TestNewResolvesBase(t *testing.T) {
	t.Run("XDG_STATE_HOME", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv("XDG_STATE_HOME", base)
		r, err := New("", Meta{Kind: "build"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		want := filepath.Join(base, "agent-runner", "runs")
		if !strings.HasPrefix(r.Dir, want+string(filepath.Separator)) {
			t.Errorf("Dir = %q, want under %q", r.Dir, want)
		}
	})

	t.Run("relative XDG_STATE_HOME", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "relative/state")
		if _, err := New("", Meta{Kind: "build"}); err == nil {
			t.Fatal("New accepted a relative XDG_STATE_HOME")
		}
	})
}

// TestNewRefusesFileBase is the failure a caller can actually hit: the base
// names something that is not a directory.
func TestNewRefusesFileBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(base, []byte("x"), 0o600); err != nil {
		t.Fatalf("prepare file: %v", err)
	}
	if _, err := New(base, Meta{Kind: "build"}); err == nil {
		t.Fatal("New accepted a base that is a regular file")
	}
}

// TestLoad covers what counts as a record. An empty run.json is the shape a
// crash between mkdir and rename leaves, and it must read as absent.
func TestLoad(t *testing.T) {
	valid := `{"id":"20261120-213458.000001-a1b2c3","state":"running"}`
	tests := []struct {
		name    string
		content string
		write   bool
		wantErr bool
	}{
		{name: "valid", content: valid, write: true},
		{name: "missing", write: false, wantErr: true},
		{name: "empty", content: "", write: true, wantErr: true},
		{name: "whitespace only", content: "\n  \n", write: true, wantErr: true},
		{name: "truncated", content: `{"id":"20261120-213458`, write: true, wantErr: true},
		{name: "no id", content: `{"state":"running"}`, write: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.write {
				if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte(tt.content), 0o600); err != nil {
					t.Fatalf("prepare run.json: %v", err)
				}
			}
			r, err := Load(dir)
			if tt.wantErr {
				if !errors.Is(err, ErrNoRecord) {
					t.Fatalf("err = %v, want ErrNoRecord", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if r.ID != "20261120-213458.000001-a1b2c3" {
				t.Errorf("ID = %q", r.ID)
			}
			if r.Dir != dir {
				t.Errorf("Dir = %q, want %q", r.Dir, dir)
			}
		})
	}
}
