package envstore

// This file is the store's second reader: the collection §6.3 describes. It
// answers two questions and nothing else — which directories may go, and why
// the rest stay — because a cleanup command that also decides policy is a
// cleanup command nobody can review.
//
// Age is measured from the manifest. §6.3 wants the newest run record naming a
// digest to be the clock, and that field (run.json's env_digest) lands with the
// run binding; until it does no record can name an environment, so the manifest
// is both the build time and the only use evidence there is. That is exactly
// the state the PRD already calls "a digest no run has named".

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// lockSuffix is the file a reader holds beside an environment, never inside the
// read-only directory (§6.4). It is not an environment and never a candidate;
// gc is the only thing that removes one.
const lockSuffix = ".lock"

// tmpMark separates a published digest from the temporary sibling a build is
// written in ("built in a temporary sibling and renamed on success", §6.4), so a
// name carrying it is a build that stopped rather than an environment.
const tmpMark = ".tmp-"

// Policy is what a collection keeps and what it may remove. Age is counted from
// the newest evidence of use the store has; Keep is the second half of the
// tier's rule — "30 days since the newest run record naming it, or all but the
// newest few digests" (§6.3) — and zero keeps no digest for its newness alone.
type Policy struct {
	Age  time.Duration
	Keep int
}

// Entry is one directory under envs/: what it is, what it holds, and whether
// the policy collects it. A caller prints it and, on --apply, hands a candidate
// back to Remove.
type Entry struct {
	// Name is the directory's name: the digest, or <digest>.tmp-<rand> for a
	// build that stopped. It is the name gc deletes, so the report carries it
	// exactly as it appears on disk.
	Name string `json:"name"`
	// Digest is the digest the directory answers for, which a temporary sibling
	// only answers for if it had finished.
	Digest string `json:"digest,omitempty"`
	// Dir is the absolute path collected or kept.
	Dir string `json:"-"`
	// Bytes is what the directory holds: the number the disk question gets
	// instead of a feeling.
	Bytes int64 `json:"bytes"`
	// Modified is the manifest's build time, or the directory's when the
	// manifest cannot be read.
	Modified time.Time `json:"modified"`
	// Collect is true when the policy would remove it now.
	Collect bool `json:"collect"`
	// Reason is one sentence: why it qualifies, or why it stays.
	Reason string `json:"reason"`

	// published separates an environment from a build that stopped. A published
	// directory can be an environment; a sibling only ever was going to be one.
	published bool
	// ready is a published directory whose manifest says the build finished.
	ready bool
}

// Collect lists every directory under the store with the reason the policy
// collects or keeps it. It changes nothing: taking the lock and deleting are
// Remove's job, so a dry run cannot be the run that removed something.
func Collect(base string, p Policy, now time.Time) ([]Entry, error) {
	root, err := Root(base)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read environment store: %w", err)
	}
	entries := make([]Entry, 0, len(ents))
	for _, de := range ents {
		name := de.Name()
		// A lock is ours to remove, never ours to count, and a symlink is not a
		// directory this store made: neither is an environment.
		if strings.HasSuffix(name, lockSuffix) || !de.IsDir() {
			continue
		}
		e, err := inspect(root, name, p, now)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if p.Keep > 0 {
		keepNewest(entries, p.Keep)
	}
	for i := range entries {
		if !entries[i].Collect {
			continue
		}
		// The lock is the only thing that can make the answer stale between the
		// listing and the deletion, so it is asked here as well as in Remove:
		// a dry run that called a live environment collectable would be a lie.
		l, err := TryLock(base, entries[i].Name)
		switch {
		case errors.Is(err, ErrLocked):
			entries[i].Collect = false
			entries[i].Reason = "held by a reader"
		case err != nil:
			return nil, err
		default:
			l.Unlock()
		}
	}
	return entries, nil
}

// inspect describes one directory and decides whether the age policy reaches it.
// A directory the store cannot describe — no manifest, a manifest naming another
// digest, a published name whose build never finished — is kept and says so: gc
// deletes what it can account for, not what it found.
func inspect(root, name string, p Policy, now time.Time) (Entry, error) {
	e := Entry{Name: name, Dir: filepath.Join(root, name)}
	if i := strings.Index(name, tmpMark); i > 0 {
		e.Digest = name[:i]
	} else {
		e.published = true
		e.Digest = name
	}
	n, err := dirBytes(e.Dir)
	if err != nil {
		return Entry{}, fmt.Errorf("measure %s: %w", name, err)
	}
	e.Bytes = n

	m, err := readManifest(e.Dir)
	if err != nil {
		e.Modified = dirTime(e.Dir)
		e.Reason = "no readable env.json, so a person decides"
		return e, nil
	}
	built, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		built = dirTime(e.Dir)
	}
	e.Modified = built.UTC()

	switch {
	case !e.published:
		e.Reason = "a build that stopped " + since(now.Sub(e.Modified)) + " ago"
	case m.Digest != e.Digest:
		e.Reason = "env.json names another digest, so a person decides"
	case m.State != StateReady:
		e.Reason = "env.json says " + m.State + ", not " + StateReady + ", so a person decides"
	default:
		e.ready = true
		e.Reason = "unused for " + since(now.Sub(e.Modified))
	}
	// Only a directory the store can describe is collected: a torn one is
	// evidence, and a person is allowed to look at it before it goes.
	describable := e.ready || !e.published
	if !describable {
		return e, nil
	}
	if now.Sub(e.Modified) < p.Age {
		e.Reason += ", under the " + days(p.Age) + " age"
		return e, nil
	}
	e.Collect = true
	return e, nil
}

// keepNewest spares the Keep most recently built published environments, which
// is the tier's other rule: keeping a handful is cheaper than provisioning one
// again, and it is the answer a person gives when the machine is small.
func keepNewest(entries []Entry, keep int) {
	var idx []int
	for i, e := range entries {
		if e.ready {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool {
		ea, eb := entries[idx[a]], entries[idx[b]]
		if !ea.Modified.Equal(eb.Modified) {
			return ea.Modified.After(eb.Modified)
		}
		return ea.Name < eb.Name
	})
	for n, i := range idx {
		if n >= keep {
			break
		}
		entries[i].Collect = false
		entries[i].Reason = fmt.Sprintf("one of the newest %d", keep)
	}
}

// Remove collects one entry, returning the bytes freed. It takes the lock the
// entry was reported under first: a reader that arrived after the listing wins,
// and ErrLocked says nothing moved. The lock file goes with the directory, since
// gc is the only thing that removes one.
func Remove(base string, e Entry) (int64, error) {
	l, err := TryLock(base, e.Name)
	if err != nil {
		return 0, err
	}
	defer l.Discard()
	n, err := dirBytes(e.Dir)
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", e.Name, err)
	}
	if err := os.RemoveAll(e.Dir); err != nil {
		return 0, fmt.Errorf("remove %s: %w", e.Name, err)
	}
	return n, nil
}

// validName is the check a caller-supplied name gets before it is joined to
// the store root: the store only ever makes names out of a digest and a
// temporary suffix, so anything carrying a separator is not one of ours and
// could point out of the store.
func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("environment name %q is not a name in the store", name)
	}
	return nil
}

// dirBytes is what a directory holds, regular files only: the number is for the
// disk question, so a symlink is not followed out of the store and a device
// file is not counted as the space it might occupy.
func dirBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func dirTime(dir string) time.Time {
	if fi, err := os.Stat(dir); err == nil {
		return fi.ModTime().UTC()
	}
	return time.Time{}
}

// since and days are the strings a person reads. They stay coarse on purpose: a
// cleanup report does not need seconds, and "41 days" is the number someone
// checks against a policy.
func since(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func days(d time.Duration) string { return fmt.Sprintf("%d days", int(d.Hours()/24)) }
