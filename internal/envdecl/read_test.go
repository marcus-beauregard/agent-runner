package envdecl

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"testing/fstest"
)

// mapFS builds an fs.FS from files (name -> content). A nil map makes an
// empty repo. Read takes an fs.FS, so tests run on fstest.MapFS and the git
// side (`git show <ref>:<file>`) stays in the command that calls this.
func mapFS(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for name, content := range files {
		m[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
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
			// PRD §6.4: dependencies are the run's, not the environment's —
			// lockfiles neither enter the Decl nor move the digest.
			name: "lockfiles declare nothing (go.sum / package-lock.json ignored)",
			files: map[string]string{
				"package.json":      `{"engines": {"node": ">=20"}}`,
				"package-lock.json": `{"lockfileVersion": 3}`,
				"go.sum":            "h1:abc=\n",
			},
			want: Decl{
				Toolchain: []Tool{{Name: "node", Version: ">=20", Source: "package.json#engines.node"}},
				Packages:  []string{},
				Services:  []string{},
				Source:    []string{"package.json"},
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
			name:  "Dockerfile FROM with a --platform flag takes the image, not the flag",
			files: map[string]string{"Dockerfile": "FROM --platform=linux/amd64 node:18 AS build\n"},
			want: Decl{
				Toolchain: []Tool{{Name: "node", Version: "18", Source: "Dockerfile#FROM node:18"}},
				Packages:  []string{}, Services: []string{}, Source: []string{"Dockerfile"},
			},
		},
		{
			name:  "a line starting with FROM but not the directive (FROMAGE) is ignored",
			files: map[string]string{"Dockerfile": "FROMAGE is not a directive\nFROM alpine:3.20\n"},
			want: Decl{
				Toolchain: []Tool{{Name: "alpine", Version: "3.20", Source: "Dockerfile#FROM alpine:3.20"}},
				Packages:  []string{}, Services: []string{}, Source: []string{"Dockerfile"},
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
			got, err := Read(mapFS(tt.files))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			// nil and empty slices are the same declaration, so normalise
			// before comparing.
			got = normalise(got)
			want := normalise(tt.want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Read mismatch:\n got: %+v\nwant: %+v", got, want)
			}
		})
	}
}

// TestReadSameTreeTwiceIsIdentical pins the determinism the issue asks for:
// the same tree read twice gives the same Decl and the same digest byte for
// byte.
func TestReadSameTreeTwiceIsIdentical(t *testing.T) {
	fsys := mapFS(map[string]string{
		"go.mod":       "module example.com/x\n\ngo 1.22\n",
		"package.json": `{"engines": {"node": ">=20"}, "packageManager": "pnpm@9.1.0"}`,
		"Dockerfile":   "FROM node:18 AS build\n",
		"go.sum":       "h1:abc=\n",
	})
	a, err := Read(fsys)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Read(fsys)
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
	before, err := Read(mapFS(map[string]string{"go.mod": "module example.com/x\n\ngo 1.22\n"}))
	if err != nil {
		t.Fatal(err)
	}
	dBefore, err := Digest(before)
	if err != nil {
		t.Fatal(err)
	}

	after, err := Read(mapFS(map[string]string{"go.mod": "module example.com/x\n\ngo 1.23\n"}))
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

// TestCosmeticEditsLeaveDigestAlone is the PRD §6.4 rule pinned: a comment
// added to go.mod, package.json keys reordered by a formatter, and a lockfile
// byte change are NOT different environments — the digest must not move.
func TestCosmeticEditsLeaveDigestAlone(t *testing.T) {
	base := mapFS(map[string]string{
		"go.mod":       "module example.com/x\n\ngo 1.22\n",
		"package.json": `{"engines": {"node": ">=20"}, "packageManager": "npm@10.2.3"}`,
		"go.sum":       "h1:abc=\n",
	})
	cosmetic := mapFS(map[string]string{
		"go.mod":       "// a comment added later\nmodule example.com/x\n\ngo 1.22\n",
		"package.json": `{"packageManager": "npm@10.2.3", "engines": {"node": ">=20"}}`,
		"go.sum":       "h1:def=\n", // dependency bytes moved, declaration did not
	})
	dBase, err := Digest(mustRead(t, base))
	if err != nil {
		t.Fatal(err)
	}
	dCosmetic, err := Digest(mustRead(t, cosmetic))
	if err != nil {
		t.Fatal(err)
	}
	if dBase != dCosmetic {
		t.Fatalf("cosmetic edit moved the digest: %s vs %s", dBase, dCosmetic)
	}
}

func mustRead(t *testing.T, fsys fs.FS) Decl {
	t.Helper()
	d, err := Read(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestReadErrors pins the refusal cases: a missing repo and a malformed
// declaring file are errors, not silent empty declarations.
func TestReadErrors(t *testing.T) {
	if _, err := Read(mapFS(map[string]string{"package.json": `{"engines": {`})); err == nil {
		t.Fatal("Read of a malformed package.json did not fail")
	}
	// An fs rooted at a file is not a repo: reading go.mod under it fails
	// with something other than ErrNotExist, so Read must not swallow it.
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(os.DirFS(file)); err == nil {
		t.Fatal("Read of a non-directory did not fail")
	}
}
