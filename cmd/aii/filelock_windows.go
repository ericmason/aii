//go:build windows

package main

// Advisory whole-file locking on Windows, used by the indexer lock in
// autoindex.go. LockFileEx locks belong to the file handle, so Windows
// drops the lock when the process exits or is killed — the same
// lifecycle the unix flock path gets.

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// allBytes locks the whole possible file range, the way
// cmd/go/internal/lockedfile does. The range may extend past EOF.
const allBytes = ^uint32(0)

// tryLockFile takes an exclusive lock on f without blocking. It reports
// ok=false with a nil error when another handle already holds the lock.
func tryLockFile(f *os.File) (ok bool, err error) {
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, allBytes, allBytes, new(windows.Overlapped))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return false, nil
	}
	return false, err
}

// unlockFile releases the lock tryLockFile took.
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()),
		0, allBytes, allBytes, new(windows.Overlapped))
}
