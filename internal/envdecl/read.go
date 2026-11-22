package envdecl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// lockfiles are the dependency-pinning files recognised at a repo root. Their
// presence is itself a declaration: a repo with go.sum pins its Go modules,
// and env-init can say why it wants the module cache warm.
var lockfiles = []string{
	"go.sum",
	"package-lock.json",
	"npm-shrinkwrap.json",
	"yarn.lock",
	"pnpm-lock.yaml",
	"bun.lockb",
	"Cargo.lock",
	"uv.lock",
	"poetry.lock",
	"Pipfile.lock",
	"requirements.txt",
	"composer.lock",
	"Gemfile.lock",
}

// Read parses the declarations of the repository at repo and returns them as
// a Decl. It reads files only: no network, no execution, no guessing at what
// a file might have meant.
//
// A repo that declares nothing yields an empty Decl and no error. A repo
// path that is not a directory, or a declaring file that cannot be read or
// parsed, is an error — a silent skip would make the digest lie about what
// was read.
func Read(repo string) (Decl, error) {
	info, err := os.Stat(repo)
	if err != nil {
		return Decl{}, fmt.Errorf("envdecl: read repo: %w", err)
	}
	if !info.IsDir() {
		return Decl{}, fmt.Errorf("envdecl: read repo: %s is not a directory", repo)
	}

	var d Decl
	d.files = map[string]string{}

	// Each reader parses one file and appends what it declares. A file that
	// does not exist contributes nothing; one that exists is read and hashed
	// even if it declares nothing, so the digest sees it.
	read := func(name string, parse func(content string) error) error {
		path := filepath.Join(repo, name)
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("envdecl: read %s: %w", name, err)
		}
		sum := sha256.Sum256(b)
		d.files[name] = hex.EncodeToString(sum[:])
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

	// Lockfiles are declarations of presence, not content: the file's bytes
	// are hashed in d.files when it is read, so a dependency bump moves the
	// digest even though only the name is listed here.
	for _, name := range lockfiles {
		path := filepath.Join(repo, name)
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Decl{}, fmt.Errorf("envdecl: read %s: %w", name, err)
		}
		sum := sha256.Sum256(b)
		d.files[name] = hex.EncodeToString(sum[:])
		d.Source = append(d.Source, name)
		d.Packages = append(d.Packages, name)
	}

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
