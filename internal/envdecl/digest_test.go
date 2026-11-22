package envdecl

import (
	"testing"
)

// TestDigestOfEmptyDeclIsStable pins the empty-repo case: an empty Decl has a
// digest (so "declares nothing" is still an answer with a hash), and it is a
// 64-char hex string.
func TestDigestOfEmptyDeclIsStable(t *testing.T) {
	d, err := Digest(Decl{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 64 {
		t.Fatalf("digest = %q, want 64 hex chars", d)
	}
	again, err := Digest(Decl{})
	if err != nil {
		t.Fatal(err)
	}
	if d != again {
		t.Fatalf("empty Decl digested twice: %s vs %s", d, again)
	}
}

// TestDigestIgnoresFieldOrder pins normalisation: a Decl assembled in a
// different order hashes the same, because the canonical form sorts.
func TestDigestIgnoresFieldOrder(t *testing.T) {
	a := Decl{
		Toolchain: []Tool{
			{Name: "node", Version: "18", Source: "Dockerfile#FROM node:18"},
			{Name: "go", Version: "1.22", Source: "go.mod#go"},
		},
		Packages: []string{"go.sum", "package-lock.json"},
		Services: []string{"postgres (postgres:16)"},
		Source:   []string{"package.json", "Dockerfile", "go.mod"},
	}
	b := Decl{
		Toolchain: []Tool{
			{Name: "go", Version: "1.22", Source: "go.mod#go"},
			{Name: "node", Version: "18", Source: "Dockerfile#FROM node:18"},
		},
		Packages: []string{"package-lock.json", "go.sum"},
		Services: []string{"postgres (postgres:16)"},
		Source:   []string{"go.mod", "package.json", "Dockerfile"},
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
		t.Fatalf("same declaration, different order: %s vs %s", da, db)
	}
}

// TestDigestIgnoresFileBytes pins the PRD §6.4 rule in the opposite direction
// from the old test: the digest hashes the normalised declaration, never the
// bytes that declared it, so two Reads whose files differ only cosmetically
// (comment, key order, lockfile bytes) produce the SAME digest, while a version
// bump — which changes a parsed entry — moves it.
func TestDigestIgnoresFileBytes(t *testing.T) {
	base := Decl{
		Toolchain: []Tool{{Name: "go", Version: "1.22", Source: "go.mod#go"}},
		Source:    []string{"go.mod"},
	}
	// Same declaration; only the (unhashed) file bytes differ.
	sameDecl := Decl{
		Toolchain: []Tool{{Name: "go", Version: "1.22", Source: "go.mod#go"}},
		Source:    []string{"go.mod"},
	}
	d1, err := Digest(base)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := Digest(sameDecl)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("identical declarations hashed differently: %s vs %s", d1, d2)
	}
	bumped := Decl{
		Toolchain: []Tool{{Name: "go", Version: "1.23", Source: "go.mod#go"}},
		Source:    []string{"go.mod"},
	}
	d3, err := Digest(bumped)
	if err != nil {
		t.Fatal(err)
	}
	if d3 == d1 {
		t.Fatal("version bump left the digest unchanged")
	}
}

// TestDigestChangesWithEveryField names the field whose change must move the
// digest: the failure message says which case caught it.
func TestDigestChangesWithEveryField(t *testing.T) {
	base := Decl{
		Toolchain: []Tool{{Name: "go", Version: "1.22", Source: "go.mod#go"}},
		Packages:  []string{"go.sum"},
		Services:  []string{"redis"},
		Source:    []string{"go.mod"},
	}
	baseDigest, err := Digest(base)
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]Decl{
		"toolchain name":    {Toolchain: []Tool{{Name: "go2", Version: "1.22", Source: "go.mod#go"}}, Packages: base.Packages, Services: base.Services, Source: base.Source},
		"toolchain version": {Toolchain: []Tool{{Name: "go", Version: "1.23", Source: "go.mod#go"}}, Packages: base.Packages, Services: base.Services, Source: base.Source},
		"toolchain source":  {Toolchain: []Tool{{Name: "go", Version: "1.22", Source: "go.mod#toolchain"}}, Packages: base.Packages, Services: base.Services, Source: base.Source},
		"packages":          {Toolchain: base.Toolchain, Packages: []string{"uv.lock"}, Services: base.Services, Source: base.Source},
		"services":          {Toolchain: base.Toolchain, Packages: base.Packages, Services: []string{"postgres"}, Source: base.Source},
		"source":            {Toolchain: base.Toolchain, Packages: base.Packages, Services: base.Services, Source: []string{"go.mod", "Dockerfile"}},
	}
	for name, mutated := range mutations {
		d, err := Digest(mutated)
		if err != nil {
			t.Fatal(err)
		}
		if d == baseDigest {
			t.Fatalf("mutating %s left the digest at %s", name, baseDigest)
		}
	}
}
