package envstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// digest is a fixed 64-character hex string: what the store needs of a digest
// is its shape, and a constant keeps a failure message readable.
const digest = "3f786850e387550fdab836ed7e6dc881de23001b1b4b1c5f5a3a0d0f37b7a1e2"

func otherDigest() string { return strings.Repeat("ab", 32) }

func TestCreateLaysOutAnEnvironment(t *testing.T) {
	base := t.TempDir()
	e, err := Create(base, Manifest{
		Digest: digest,
		Tools:  []Tool{{Name: "go", Version: "1.22.5", Path: "bin/go", Source: "go.mod#toolchain"}},
		Vars:   map[string]string{"CGO_ENABLED": "0"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := filepath.Join(base, "agent-runner", "envs", digest)
	// The build runs in a sibling of the published name, never at it.
	if e.Dir == final {
		t.Fatalf("Create wrote the environment at its published name %q", e.Dir)
	}
	if base := filepath.Base(e.Dir); !strings.HasPrefix(base, digest+".tmp-") || filepath.Dir(e.Dir) != filepath.Dir(final) {
		t.Errorf("build directory = %q, want %s.tmp-* beside %q", e.Dir, digest, final)
	}
	for _, sub := range []string{"bin", "lib", "share", "tmp"} {
		fi, err := os.Stat(filepath.Join(e.Dir, sub))
		if err != nil {
			t.Errorf("stat %s: %v", sub, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}
	if e.Manifest.State != StateBuilding {
		t.Errorf("state = %q, want %q", e.Manifest.State, StateBuilding)
	}
	if e.Manifest.CreatedAt == "" {
		t.Error("CreatedAt is empty: the manifest should say when the build started")
	}

	// Nothing is published yet, so a reader gets nothing: Open must never answer
	// for a tree that is still being built.
	if _, err := Open(base, digest); err == nil {
		t.Fatal("Open succeeded while the build was still running")
	}
	if err := e.MarkReady(2048); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if e.Dir != final {
		t.Errorf("Dir after MarkReady = %q, want %q", e.Dir, final)
	}

	// The file is the contract with a later reader, so read it back rather than
	// trusting the struct in memory.
	m, err := Open(base, digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if m.Manifest.Digest != digest || m.Manifest.State != StateReady || m.Manifest.Bytes != 2048 {
		t.Errorf("reopened manifest = %+v", m.Manifest)
	}
	if len(m.Manifest.Tools) != 1 || m.Manifest.Tools[0].Source != "go.mod#toolchain" {
		t.Errorf("tools = %+v, want go from go.mod#toolchain", m.Manifest.Tools)
	}
	if m.Manifest.Vars["CGO_ENABLED"] != "0" {
		t.Errorf("vars = %+v, want CGO_ENABLED=0", m.Manifest.Vars)
	}
}

func TestCreateRefusesAnExistingDigest(t *testing.T) {
	base := t.TempDir()
	first, err := Create(base, Manifest{Digest: digest})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := first.MarkReady(1); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if _, err := Create(base, Manifest{Digest: digest}); err == nil {
		t.Fatal("second Create succeeded; a digest that exists must not be reused")
	}
}

// A build that stops leaves the temporary sibling and nothing at the digest,
// which is what lets gc collect it and env-check call the digest absent.
func TestStoppedBuildLeavesOnlyASibling(t *testing.T) {
	base := t.TempDir()
	if _, err := Create(base, Manifest{Digest: digest}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := filepath.Join(base, "agent-runner", "envs", digest)
	if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s: %v, want not exist", final, err)
	}
	entries, err := os.ReadDir(filepath.Dir(final))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var siblings []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), digest+".tmp-") {
			siblings = append(siblings, entry.Name())
		}
	}
	if len(siblings) != 1 {
		t.Fatalf("store holds %v, want exactly one %s.tmp-* sibling", siblings, digest)
	}
}

func TestCreateKeepsDigestsApart(t *testing.T) {
	base := t.TempDir()
	first, err := Create(base, Manifest{Digest: digest})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := Create(base, Manifest{Digest: otherDigest()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if first.Dir == second.Dir {
		t.Fatalf("two digests share one directory: %q", first.Dir)
	}
	// A store keyed by digest, not by repo: a second declaration that hashes
	// the same costs nothing, which is what §6.3 promises.
	if _, err := os.Stat(second.Dir); err != nil {
		t.Errorf("stat second environment: %v", err)
	}
}

func TestMarkReady(t *testing.T) {
	base := t.TempDir()
	e, err := Create(base, Manifest{Digest: digest})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := e.MarkReady(1024); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	m, err := Open(base, digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if m.Manifest.State != StateReady || m.Manifest.Bytes != 1024 {
		t.Errorf("manifest = %+v, want ready with 1024 bytes", m.Manifest)
	}
	// Repeating the last step of a build is how a resumed init finishes; it must
	// not fail on an environment that is already there, and it must not lose the
	// creation time the first write recorded.
	created := m.Manifest.CreatedAt
	if err := m.MarkReady(1024); err != nil {
		t.Errorf("second MarkReady: %v", err)
	}
	if m.Manifest.CreatedAt != created {
		t.Errorf("CreatedAt = %q after a repeat, want %q", m.Manifest.CreatedAt, created)
	}
	if m.Dir != filepath.Join(base, "agent-runner", "envs", digest) {
		t.Errorf("Dir = %q after a repeat, want the published name", m.Dir)
	}
}

func TestOpenMissing(t *testing.T) {
	base := t.TempDir()
	if _, err := Open(base, digest); err == nil {
		t.Fatal("Open of a digest that was never built succeeded")
	}
}

func TestCreateLeavesNoTemporaryManifest(t *testing.T) {
	base := t.TempDir()
	e, err := Create(base, Manifest{Digest: digest})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := e.MarkReady(1); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	entries, err := os.ReadDir(e.Dir)
	if err != nil {
		t.Fatalf("read environment: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".env.json-") {
			t.Errorf("temporary manifest left behind: %s", entry.Name())
		}
	}
	// The published environment is the only thing left in the store.
	root, err := os.ReadDir(filepath.Dir(e.Dir))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	for _, entry := range root {
		if strings.HasPrefix(entry.Name(), digest+".tmp-") {
			t.Errorf("build directory left behind: %s", entry.Name())
		}
	}
}

func TestManifestEncodesToolsAsAnArray(t *testing.T) {
	base := t.TempDir()
	e, err := Create(base, Manifest{Digest: digest})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(e.Dir, "env.json"))
	if err != nil {
		t.Fatalf("read env.json: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("env.json is not an object: %v", err)
	}
	if got := string(raw["tools"]); got != "[]" {
		t.Errorf("tools = %s, want []", got)
	}
}

func TestDir(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		name   string
		digest string
		base   string
		want   string
		errHas string
	}{
		{name: "hex digest", digest: digest, base: base, want: filepath.Join(base, "agent-runner", "envs", digest)},
		{name: "empty digest", digest: "", base: base, errHas: "want 32 to 128"},
		{name: "too short", digest: "abc123", base: base, errHas: "want 32 to 128"},
		{name: "too long", digest: strings.Repeat("a", 129), base: base, errHas: "want 32 to 128"},
		{name: "uppercase", digest: strings.Repeat("A", 64), base: base, errHas: "not lowercase hex"},
		{name: "a short path, not a hash", digest: "../../etc", base: base, errHas: "want 32 to 128"},
		{name: "a path long enough to pass the length check", digest: strings.Repeat("a", 62) + "/x", base: base, errHas: "not lowercase hex"},
		{name: "no state directory", digest: digest, base: "", errHas: "no state directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Dir(tc.base, tc.digest)
			if tc.errHas != "" {
				if err == nil {
					t.Fatalf("Dir = %q, want an error containing %q", got, tc.errHas)
				}
				if !strings.Contains(err.Error(), tc.errHas) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.errHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("Dir: %v", err)
			}
			if got != tc.want {
				t.Errorf("Dir = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRootIsAbsolute(t *testing.T) {
	root, err := Root("relative/state")
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if !filepath.IsAbs(root) {
		t.Errorf("Root = %q, want an absolute path", root)
	}
}
