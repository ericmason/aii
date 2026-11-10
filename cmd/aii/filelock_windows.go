//go:build windows

package main

// Whole-run locking on Windows, used by the indexer lock in
// autoindex.go. LockFileEx locks belong to the file handle, so Windows
// drops the lock when the process exits or is killed — the same
// lifecycle the unix flock path gets.

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Windows byte-range locks are mandatory: a locked range cannot be read
// through another handle. Lock a single byte far past the end of the
// file instead of the whole range, so readLockPID can still read the
// PID at offset 0 while an indexer holds the lock. The range may extend
// past EOF, and no two aii processes pick different offsets.
const (
	lockByteOffsetLow  = 0         // offset 4 GiB, byte 1 of 1
	lockByteOffsetHigh = 1         // high dword of the offset
	lockByteCountLow   = uint32(1) // one byte
	lockByteCountHigh  = uint32(0)
)

// tryLockFile takes a lock on f's reserved byte without blocking,
// exclusive when exclusive is set and shared otherwise. It reports:
//
//	ok=true, err=nil    we hold the lock
//	ok=false, err=nil   someone else holds a conflicting lock
//	ok=false, err!=nil  the lock could not be evaluated, and the caller
//	                    must not read that as "someone else is indexing"
func tryLockFile(f *os.File, exclusive bool) (ok bool, err error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	ol := &windows.Overlapped{Offset: lockByteOffsetLow, OffsetHigh: lockByteOffsetHigh}
	err = windows.LockFileEx(windows.Handle(f.Fd()), flags, 0,
		lockByteCountLow, lockByteCountHigh, ol)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return false, nil
	}
	return false, err
}

// unlockFile releases the lock tryLockFile took.
func unlockFile(f *os.File) error {
	ol := &windows.Overlapped{Offset: lockByteOffsetLow, OffsetHigh: lockByteOffsetHigh}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0,
		lockByteCountLow, lockByteCountHigh, ol)
}

// processAlive reports whether a PID is still running. Only the
// transitional check in acquireIndexLock uses this; ownership comes
// from the lock.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied means the process exists under another user.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

// stillActive is the exit status GetExitCodeProcess reports for a
// process that has not terminated: "the status returned is STILL_ACTIVE
// ... STILL_ACTIVE (259)".
// https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getexitcodeprocess
const stillActive = 259
