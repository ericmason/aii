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

// tryLockFile takes an advisory lock on f without blocking, exclusive
// when exclusive is set and shared otherwise. It reports:
//
//	ok=true, err=nil    we hold the lock
//	ok=false, err=nil   someone else holds a conflicting lock
//	ok=false, err!=nil  the lock could not be evaluated — a filesystem
//	                    that does not support flock (some NFS, SMB, and
//	                    FUSE mounts) reports ENOLCK, ENOTSUP, or EINVAL
//	                    here, and the caller must not read that as
//	                    "someone else is indexing".
func tryLockFile(f *os.File, exclusive bool) (ok bool, err error) {
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	err = unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
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

// processAlive reports whether a PID is still running. Signal 0 is the
// portable "are you there?" probe: ESRCH means the process is gone, and
// EPERM means it exists under another user. Only the transitional check
// in acquireIndexLock uses this; ownership comes from the lock.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(unix.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, unix.EPERM)
}
