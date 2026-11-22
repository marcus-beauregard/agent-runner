package envdecl

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// Read parses the declarations read from fsys (paths relative to its root) and
// returns them as a Decl. It reads files only: no network, no execution, no
// guessing at what a file might have meant.
//
// Taking an fs.FS keeps this package pure and puts the ref where it belongs:
// the caller materialises the declaration at a ref — `git show <ref>:go.mod`
// into an fstest.MapFS, or os.DirFS of a worktree checked out at that ref —
// and the digest then answers for that ref, not for whatever a directory
// happens to hold (PRD §6.4: "the declaration is what a ref says").
//
// A repo that declares nothing yields an empty Decl and no error. A declaring
// file that cannot be read or parsed is an error — a silent skip would make
// the digest lie about what was read.
func Read(fsys fs.FS) (Decl, error) {
	var d Decl

	// Each reader parses one file and appends what it declares. A file that
	// does not exist contributes nothing; one that exists is parsed even if
	// it declares nothing, so the digest sees its name in Source.
	read := func(name string, parse func(content string) error) error {
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("envdecl: read %s: %w", name, err)
		}
		d.Source = append(d.Source, name)
		if err := parse(string(b)); err != nil {
			return fmt.Errorf("envdecl: parse %s: %w", name, err)
		}
		return nil
	}

	if err := read("go.mod", func(c string) error { return parseGoMod(c, &d) }); err != nil {
		return Decl{}, err
	}
	if err := read("package.json", func(c string) error { return parsePackageJSON(c, &d) }); err != nil {
		return Decl{}, err
	}
	if err := read("rust-toolchain.toml", func(c string) error { return parseRustToolchainTOML(c, &d) }); err != nil {
		return Decl{}, err
	}
	if err := read("rust-toolchain", func(c string) error { return parseRustToolchain(c, &d) }); err != nil {
		return Decl{}, err
	}
	if err := read(".python-version", func(c string) error { return parsePythonVersion(c, &d) }); err != nil {
		return Decl{}, err
	}
	if err := read("pyproject.toml", func(c string) error { return parsePyproject(c, &d) }); err != nil {
		return Decl{}, err
	}
	if err := read("Dockerfile", func(c string) error { return parseDockerfile(c, &d) }); err != nil {
		return Decl{}, err
	}
	if err := read("agent-runner.env.toml", func(c string) error { return parseEnvTOML(c, &d) }); err != nil {
		return Decl{}, err
	}

	// Lockfiles declare nothing here: dependencies are the run's, not the
	// environment's (PRD §6.4), so go.sum / package-lock.json & co. neither
	// enter the Decl nor move the digest. The deps stage reads the tree and
	// the lockfile chooses the resolver command; it never the digest.

	d.sort()
	return d, nil
}

// sort puts a Decl in its canonical order, so two reads of the same tree are
// DeepEqual and not merely similar.
func (d *Decl) sort() {
	sort.Slice(d.Toolchain, func(i, j int) bool {
		a, b := d.Toolchain[i], d.Toolchain[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Source < b.Source
	})
	sort.Strings(d.Packages)
	sort.Strings(d.Services)
	sort.Strings(d.Source)
}

// addTool appends a toolchain entry. Disagreeing declarations are all kept:
// the conflict is surfaced by the differing Source values, never resolved
// here.
func (d *Decl) addTool(name, version, source string) {
	d.Toolchain = append(d.Toolchain, Tool{Name: name, Version: version, Source: source})
}

// firstWord returns the first whitespace-separated field of line, with any
// trailing comment stripped. It is the shared shape of the line-oriented
// formats (go.mod, Dockerfile, .python-version).
func firstField(line string) string {
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i]
	}
	return line
}
