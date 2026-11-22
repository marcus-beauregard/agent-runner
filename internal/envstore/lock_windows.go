//go:build windows

package envstore

import (
	"errors"
	"os"
)

// ErrLocked is what TryLock answers when someone already holds the lock.
var ErrLocked = errors.New("environment is locked")

// Lock is a lock this build cannot take. Windows has no flock in the standard
// library, and the alternative — creating a lock file exclusively — leaves a
// stale lock behind a killed reader, which is a directory gc could never
// collect and nobody could explain.
//
// So the stub is honest rather than silent: TryLock fails, and gc refuses to
// delete an environment it cannot prove is unused.
type Lock struct{ f *os.File }

// TryLock reports that this platform cannot take the lock the store is built
// around, which a collector must treat as "not collectable".
func TryLock(base, name string) (*Lock, error) {
	return nil, errors.New("environment locks are not supported on windows")
}

// Unlock has nothing to release.
func (l *Lock) Unlock() error { return nil }

// Discard has nothing to release or remove.
func (l *Lock) Discard() error { return nil }
