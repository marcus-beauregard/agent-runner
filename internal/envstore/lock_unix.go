//go:build unix

package envstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked is what TryLock answers when someone already holds the lock: the
// environment is in use, and a collector that deleted it would pull the tools
// out from under a command that is running.
var ErrLocked = errors.New("environment is locked")

// Lock is a held flock on <name>.lock. The kernel drops it when the holder's
// process exits, so a reader that was killed does not leave a lock that gc
// respects forever — the failure mode a lock file alone would have.
type Lock struct {
	f    *os.File
	path string
}

// TryLock takes the lock beside the environment without waiting for it. The file
// sits beside the directory, never inside it, because the directory is read-only
// to the runs that hold it (§6.4).
//
// A reader that finds the environment gone after it takes the lock has lost a
// race with gc, and answers "not ready"; the lock keeps them from overlapping in
// the other direction, which is the one that could hand a command half a
// toolchain.
func TryLock(base, name string) (*Lock, error) {
	root, err := Root(base)
	if err != nil {
		return nil, err
	}
	if err := validName(name); err != nil {
		return nil, err
	}
	path := filepath.Join(root, name+lockSuffix)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock for %s: %w", name, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", name, err)
	}
	return &Lock{f: f, path: path}, nil
}

// buildLock is TryLock under the name of its one other caller: Create holds it
// for as long as a build runs, so a collector cannot take a directory it is
// still writing into.
func buildLock(base, name string) (*Lock, error) { return TryLock(base, name) }

// Unlock releases the lock and leaves the file: a second reader should not have
// to recreate it, and the file is removed with the environment it guards.
func (l *Lock) Unlock() error {
	if l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// Discard releases the lock and removes the file, which only gc does: the lock
// guarded an environment that no longer exists, and leaving it behind would make
// the store's contents a lie (§6.4: the only other file under envs/ is a lock).
//
// The caller deletes the directory first, so a reader that opened the lock file
// but has not taken it yet finds no environment on the other side.
func (l *Lock) Discard() error {
	err := l.Unlock()
	if rerr := os.Remove(l.path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}
