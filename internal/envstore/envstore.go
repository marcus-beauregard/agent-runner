// Package envstore owns the on-disk shape of a prepared environment: the
// directory under the state dir keyed by its digest, and the env.json manifest
// that says what was put in it and whether the build finished.
//
// It knows nothing about declarations and nothing about how a digest is
// computed: a caller that has a digest hands it over, and the store keys on it.
// That is what lets two repos whose declarations happen to be identical share
// one directory (PRD §6.3), and it keeps the store buildable while the code
// that computes the digest is still in review.
//
// The directory is the environment. It is created once, with a manifest that
// says whether it is finished, so a crash during init leaves evidence a later
// env-check reads as "not ready" rather than a toolchain missing the one
// program a gate needs.
package envstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The two states a manifest records. A directory is only an environment once
// the process that built it says so; anything else is a build that stopped.
const (
	StateBuilding = "building"
	StateReady    = "ready"
)

// Tool is one program in the environment, with the version it was installed at
// and the file that justified that version. Path is relative to the environment
// directory, usually "bin/<name>".
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"`
	Source  string `json:"source,omitempty"`
}

// Manifest is env.json: the digest this directory answers for, whether the
// build that made it finished, when it started, what the tools are, and the
// non-secret variables a child is given (PRD §6.4).
type Manifest struct {
	Digest    string            `json:"digest"`
	State     string            `json:"state"`
	CreatedAt string            `json:"created_at"`
	Bytes     int64             `json:"bytes,omitempty"`
	Tools     []Tool            `json:"tools"`
	Vars      map[string]string `json:"vars,omitempty"`
}

// Env is an open environment directory and its manifest as it was read.
type Env struct {
	// Dir is the absolute path of the environment directory.
	Dir string

	// Manifest is the manifest last read or written.
	Manifest Manifest
}

// Root returns <base>/agent-runner/envs, where every environment lives, beside
// the run records the same base holds. Resolving an empty base is the caller's
// job: that is the binary's state-directory policy, not a directory's shape.
func Root(base string) (string, error) {
	if base == "" {
		return "", errors.New("no state directory")
	}
	root := filepath.Join(base, "agent-runner", "envs")
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return root, nil
}

// Dir returns the environment directory for digest.
//
// The digest is the whole name of the directory, so it is validated rather than
// trusted: a caller that hands over a path instead of a hash gets an error, not
// a directory outside the store.
func Dir(base, digest string) (string, error) {
	if err := validDigest(digest); err != nil {
		return "", err
	}
	root, err := Root(base)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, digest), nil
}

// Create makes the environment directory for m.Digest with its bin, lib, share
// and tmp subdirectories, and writes env.json with state building. A digest
// that already exists fails rather than being reused: two processes building
// the same digest must not write over each other, and a caller that wants the
// existing one opens it.
//
// The directory is created in place, not under a temporary name, because a
// build that stops has to leave something a later env-check can name. MarkReady
// is what turns it into an environment.
func Create(base string, m Manifest) (*Env, error) {
	dir, err := Dir(base, m.Digest)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("create environment store: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("environment %s already exists: %w", m.Digest, err)
		}
		return nil, fmt.Errorf("create environment directory: %w", err)
	}
	for _, sub := range []string{"bin", "lib", "share", "tmp"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("create %s: %w", sub, err)
		}
	}
	m.State = StateBuilding
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	// A manifest with no tools says [], not null: the file is read by people as
	// well as by code, and "nothing was installed" is a fact worth spelling.
	if m.Tools == nil {
		m.Tools = []Tool{}
	}
	e := &Env{Dir: dir, Manifest: m}
	if err := e.write(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return e, nil
}

// Open reads the environment at digest without changing it.
func Open(base, digest string) (*Env, error) {
	dir, err := Dir(base, digest)
	if err != nil {
		return nil, err
	}
	m, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	return &Env{Dir: dir, Manifest: m}, nil
}

// MarkReady records that the build finished, with the number of bytes the
// environment holds. It is the store's only transition: a manifest that is
// already ready stays ready, so finishing a resumed build costs one write
// rather than a second install.
func (e *Env) MarkReady(bytes int64) error {
	e.Manifest.State = StateReady
	e.Manifest.Bytes = bytes
	return e.write()
}

// write publishes env.json with a rename, so a reader never sees half a
// manifest, and syncs the directory so the rename itself survives a crash.
func (e *Env) write() error {
	b, err := json.MarshalIndent(e.Manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode env.json: %w", err)
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(e.Dir, ".env.json-")
	if err != nil {
		return fmt.Errorf("create env.json: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("write env.json: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync env.json: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close env.json: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(e.Dir, "env.json")); err != nil {
		return fmt.Errorf("publish env.json: %w", err)
	}
	tmp = ""
	d, err := os.Open(e.Dir)
	if err != nil {
		return fmt.Errorf("open environment directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync environment directory: %w", err)
	}
	return nil
}

func readManifest(dir string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dir, "env.json"))
	if err != nil {
		return m, fmt.Errorf("read env.json: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parse env.json: %w", err)
	}
	return m, nil
}

// validDigest accepts the names a digest may have: lowercase hex, long enough
// to be a hash of something and short enough to be one. The store does not
// decide the algorithm and so does not count characters precisely; it refuses
// what is not a hex string, because that is the only kind of name it creates.
func validDigest(digest string) error {
	if len(digest) < 32 || len(digest) > 128 {
		return fmt.Errorf("digest %q is %d characters, want 32 to 128 hex digits", digest, len(digest))
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("digest %q is not lowercase hex", digest)
		}
	}
	return nil
}
