//go:build unix

package main

// Advisory whole-file locking on unix, used by the indexer lock in
// autoindex.go. flock locks belong to the open file description, so the
// kernel drops the lock when the process exits or is killed — no PID
// liveness probing and no stale-lock reaping.

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLockFile takes an exclusive advisory lock on f without blocking.
// It reports ok=false with a nil error when another open file
// description already holds the lock.
func tryLockFile(f *os.File) (ok bool, err error) {
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EWOULDBLOCK), errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EACCES):
		return false, nil
	}
	return false, err
}

// unlockFile releases the lock tryLockFile took. Closing f releases it
// too; this makes the release explicit and ordered.
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
