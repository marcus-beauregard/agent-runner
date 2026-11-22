package envdecl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Digest returns the hex SHA-256 over the normalised declaration plus the
// content of every declaring file.
//
// The hashed value is built, not borrowed, for the same reason as
// world.Digest: a struct names the fields in a fixed order, every slice is
// sorted first, and no map is marshalled — Go randomises map iteration, and a
// digest that changes from run to run is worse than none. The same tree read
// twice therefore produces the same digest byte for byte, and a version bump
// in any declaring file changes it twice over: once in the parsed entry, once
// in the file's content hash.
func Digest(d Decl) (string, error) {
	b, err := json.Marshal(d.canonical())
	if err != nil {
		return "", fmt.Errorf("envdecl: marshal digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalDecl is the exact shape the digest hashes. It is a type of its own
// rather than Decl itself, so a field added to Decl cannot silently change
// digests already written down: a new field joins the digest by being added
// here, on purpose, with a test that pins the result.
type canonicalDecl struct {
	Toolchain []canonicalTool `json:"toolchain"`
	Packages  []string        `json:"packages"`
	Services  []string        `json:"services"`
	Source    []string        `json:"source"`
	Files     []canonicalFile `json:"files"`
}

type canonicalTool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Source  string `json:"source"`
}

type canonicalFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// canonical builds the digestable value: every slice sorted and non-nil, so
// that an absent list and an empty one encode alike — a nil slice marshals as
// null, and null next to [] is two digests for one declaration.
func (d Decl) canonical() canonicalDecl {
	c := canonicalDecl{
		Toolchain: make([]canonicalTool, 0, len(d.Toolchain)),
		Packages:  nonNil(d.Packages),
		Services:  nonNil(d.Services),
		Source:    nonNil(d.Source),
		Files:     make([]canonicalFile, 0, len(d.files)),
	}
	for _, t := range d.Toolchain {
		c.Toolchain = append(c.Toolchain, canonicalTool{Name: t.Name, Version: t.Version, Source: t.Source})
	}
	sort.Slice(c.Toolchain, func(i, j int) bool {
		a, b := c.Toolchain[i], c.Toolchain[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Source < b.Source
	})
	sort.Strings(c.Packages)
	sort.Strings(c.Services)
	sort.Strings(c.Source)
	for path, sum := range d.files {
		c.Files = append(c.Files, canonicalFile{Path: path, SHA256: sum})
	}
	sort.Slice(c.Files, func(i, j int) bool { return c.Files[i].Path < c.Files[j].Path })
	return c
}

// nonNil returns ss sorted, as a non-nil slice so that it encodes as [].
func nonNil(ss []string) []string {
	out := make([]string, 0, len(ss))
	out = append(out, ss...)
	sort.Strings(out)
	return out
}
