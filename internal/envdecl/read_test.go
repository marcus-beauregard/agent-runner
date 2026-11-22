package envdecl

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// writeRepo materialises files (name -> content) in a temp dir and returns
// its path. A nil content list makes an empty repo.
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// normalise replaces nil slices with empty ones, so a Decl read from a repo
// that declares nothing compares equal to the empty Decl written in a table.
func normalise(d Decl) Decl {
	if d.Toolchain == nil {
		d.Toolchain = []Tool{}
	}
	if d.Packages == nil {
		d.Packages = []string{}
	}
	if d.Services == nil {
		d.Services = []string{}
	}
	if d.Source == nil {
		d.Source = []string{}
	}
	return d
}

// TestReadProducesExpectedDecl is the table the issue asks for: each declaring
// file alone produces the expected Decl.
func TestReadProducesExpectedDecl(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  Decl
	}{
		{
			name:  "empty repo is an empty Decl, not an error",
			files: nil,
			want:  Decl{Toolchain: []Tool{}, Packages: []string{}, Services: []string{}, Source: []string{}},
		},
		{
			name:  "go.mod go directive",
			files: map[string]string{"go.mod": "module example.com/x\n\ngo 1.22\n"},
			want: Decl{
				Toolchain: []Tool{{Name: "go", Version: "1.22", Source: "go.mod#go"}},
				Packages:  []string{}, Services: []string{}, Source: []string{"go.mod"},
			},
		},
		{
			name:  "go.mod go and toolchain disagree: both kept, sources differ",
			files: map[string]string{"go.mod": "module example.com/x\n\ngo 1.22\ntoolchain go1.22.5\n"},
			want: Decl{
				Toolchain: []Tool{
					{Name: "go", Version: "1.22", Source: "go.mod#go"},
					{Name: "go", Version: "1.22.5", Source: "go.mod#toolchain"},
				},
				Packages: []string{}, Services: []string{}, Source: []string{"go.mod"},
			},
		},
		{
			name: "package.json engines and packageManager",
			files: map[string]string{
				"package.json": `{"engines": {"node": ">=20", "npm": "^10"}, "packageManager": "pnpm@9.1.0"}`,
			},
			want: Decl{
				Toolchain: []Tool{
					{Name: "node", Version: ">=20", Source: "package.json#engines.node"},
					{Name: "npm", Version: "^10", Source: "package.json#engines.npm"},
					{Name: "pnpm", Version: "9.1.0", Source: "package.json#packageManager"},
				},
				Packages: []string{}, Services: []string{}, Source: []string{"package.json"},
			},
		},
		{
			name: "lockfile presence lands in Packages",
			files: map[string]string{
				"package.json":      `{"engines": {"node": ">=20"}}`,
				"package-lock.json": `{"lockfileVersion": 3}`,
				"go.sum":            "h1:abc=\n",
			},
			want: Decl{
				Toolchain: []Tool{{Name: "node", Version: ">=20", Source: "package.json#engines.node"}},
				Packages:  []string{"go.sum", "package-lock.json"},
				Services:  []string{},
				Source:    []string{"go.sum", "package-lock.json", "package.json"},
			},
		},
		{
			name:  "rust-toolchain.toml channel",
			files: map[string]string{"rust-toolchain.toml": "[toolchain]\nchannel = \"1.80.0\"\n"},
			want: Decl{
				Toolchain: []Tool{{Name: "rust", Version: "1.80.0", Source: "rust-toolchain.toml#toolchain.channel"}},
				Packages:  []string{}, Services: []string{}, Source: []string{"rust-toolchain.toml"},
			},
		},
		{
			name:  "legacy rust-toolchain file",
			files: map[string]string{"rust-toolchain": "1.80.0\n"},
			want: Decl{
				Toolchain: []Tool{{Name: "rust", Version: "1.80.0", Source: "rust-toolchain"}},
				Packages:  []string{}, Services: []string{}, Source: []string{"rust-toolchain"},
			},
		},
		{
			name:  ".python-version",
			files: map[string]string{".python-version": "3.12.1\n"},
			want: Decl{
				Toolchain: []Tool{{Name: "python", Version: "3.12.1", Source: ".python-version"}},
				Packages:  []string{}, Services: []string{}, Source: []string{".python-version"},
			},
		},
		{
			name:  "pyproject requires-python",
			files: map[string]string{"pyproject.toml": "[project]\nname = \"x\"\nrequires-python = \">=3.11\"\n"},
			want: Decl{
				Toolchain: []Tool{{Name: "python", Version: ">=3.11", Source: "pyproject.toml#project.requires-python"}},
				Packages:  []string{}, Services: []string{}, Source: []string{"pyproject.toml"},
			},
		},
		{
			name:  "Dockerfile FROM with tag",
			files: map[string]string{"Dockerfile": "FROM node:18 AS build\nRUN npm ci\n"},
			want: Decl{
				Toolchain: []Tool{{Name: "node", Version: "18", Source: "Dockerfile#FROM node:18"}},
				Packages:  []string{}, Services: []string{}, Source: []string{"Dockerfile"},
			},
		},
		{
			name:  "Dockerfile multi-stage and tagless image",
			files: map[string]string{"Dockerfile": "FROM golang AS build\nFROM alpine:3.20\n"},
			want: Decl{
				Toolchain: []Tool{
					{Name: "alpine", Version: "3.20", Source: "Dockerfile#FROM alpine:3.20"},
					{Name: "golang", Version: "latest", Source: "Dockerfile#FROM golang"},
				},
				Packages: []string{}, Services: []string{}, Source: []string{"Dockerfile"},
			},
		},
		{
			name: "agent-runner.env.toml tools, packages, services",
			files: map[string]string{
				"agent-runner.env.toml": "[[tool]]\nname = \"go\"\nversion = \"1.23\"\n\n[[package]]\nname = \"postgresql-client\"\n\n[[service]]\nname = \"postgres\"\nimage = \"postgres:16\"\n",
			},
			want: Decl{
				Toolchain: []Tool{{Name: "go", Version: "1.23", Source: "agent-runner.env.toml#tool.go"}},
				Packages:  []string{"postgresql-client"},
				Services:  []string{"postgres (postgres:16)"},
				Source:    []string{"agent-runner.env.toml"},
			},
		},
		{
			name: "conflict: Dockerfile node 18 vs engines node >=20 — both kept",
			files: map[string]string{
				"Dockerfile":   "FROM node:18\n",
				"package.json": `{"engines": {"node": ">=20"}}`,
			},
			want: Decl{
				Toolchain: []Tool{
					{Name: "node", Version: "18", Source: "Dockerfile#FROM node:18"},
					{Name: "node", Version: ">=20", Source: "package.json#engines.node"},
				},
				Packages: []string{}, Services: []string{},
				Source: []string{"Dockerfile", "package.json"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRepo(t, tt.files)
			got, err := Read(dir)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			// files is unexported bookkeeping; compare the exported shape,
			// then check one hash per Source entry separately. nil and empty
			// slices are the same declaration, so normalise before comparing.
			gotFiles := got.files
			got.files = nil
			got = normalise(got)
			want := normalise(tt.want)
			want.files = nil
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Read mismatch:\n got: %+v\nwant: %+v", got, want)
			}
			if len(gotFiles) != len(tt.want.Source) {
				t.Fatalf("files hashed = %d, want one per Source entry %d", len(gotFiles), len(tt.want.Source))
			}
		})
	}
}

// TestReadSameTreeTwiceIsIdentical pins the determinism the issue asks for:
// the same tree read twice gives the same Decl and the same digest byte for
// byte.
func TestReadSameTreeTwiceIsIdentical(t *testing.T) {
	dir := writeRepo(t, map[string]string{
		"go.mod":       "module example.com/x\n\ngo 1.22\n",
		"package.json": `{"engines": {"node": ">=20"}, "packageManager": "pnpm@9.1.0"}`,
		"Dockerfile":   "FROM node:18 AS build\n",
		"go.sum":       "h1:abc=\n",
	})
	a, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("two reads of one tree differ:\n a: %+v\n b: %+v", a, b)
	}
	da, err := Digest(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Digest(b)
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Fatalf("digest changed between reads of one tree: %s vs %s", da, db)
	}
}

// TestVersionBumpChangesDigest pins the other half: a version bump in a
// declaring file changes the digest.
func TestVersionBumpChangesDigest(t *testing.T) {
	dir := writeRepo(t, map[string]string{"go.mod": "module example.com/x\n\ngo 1.22\n"})
	before, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	dBefore, err := Digest(before)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	dAfter, err := Digest(after)
	if err != nil {
		t.Fatal(err)
	}
	if dBefore == dAfter {
		t.Fatalf("version bump left the digest unchanged: %s", dBefore)
	}
	if !slices.Contains(after.Toolchain, Tool{Name: "go", Version: "1.23", Source: "go.mod#go"}) {
		t.Fatalf("bumped version not in Decl: %+v", after.Toolchain)
	}
}

// TestReadErrors pins the refusal cases: a missing repo and a malformed
// declaring file are errors, not silent empty declarations.
func TestReadErrors(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
		t.Fatal("Read of a missing repo did not fail")
	}
	dir := writeRepo(t, map[string]string{"package.json": `{"engines": {`})
	if _, err := Read(dir); err == nil {
		t.Fatal("Read of a malformed package.json did not fail")
	}
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(file); err == nil {
		t.Fatal("Read of a non-directory did not fail")
	}
}
