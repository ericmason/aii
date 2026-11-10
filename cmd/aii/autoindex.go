package main

// Indexer locking and stamp primitives. The indexer is a single-writer
// process (SQLite) so both the cron job and manual `aii index` runs
// take an exclusive advisory lock on the same lock file to avoid racing
// each other; the file also carries the holder's PID for display. The
// stamp file records the last successful index so `aii cron status` can
// report freshness.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/store"
)

// dataDir is the parent of the DB — also where the lock, the stamp, and
// the scheduler's log live. The path is absolute even when AII_DB is
// relative, because a scheduled job runs from a different directory
// than the install did: launchd starts from / and cron from $HOME.
func dataDir() string {
	dir := filepath.Dir(store.DefaultPath())
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

func lockPath() string  { return filepath.Join(dataDir(), ".index.lock") }
func stampPath() string { return filepath.Join(dataDir(), ".last-index") }

// warnStaleAfter is the grace period before query commands nag the
// user about a stale index. Cron default is 5 min, so a healthy install
// stamps well under this threshold; 30 min means something's off.
const warnStaleAfter = 30 * time.Minute

// warnIfIndexStale writes a one-line hint to stderr when the stamp file
// is missing or older than warnStaleAfter. Read-only — safe to call
// from any query-path command. Stays silent on healthy installs.
func warnIfIndexStale() {
	info, err := os.Stat(stampPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "aii: index has not been built yet — run `aii index` (or `aii cron install` for automatic updates)")
		return
	}
	age := time.Since(info.ModTime())
	if age <= warnStaleAfter {
		return
	}
	fmt.Fprintf(os.Stderr, "aii: index is %s stale — run `aii index` (or `aii cron install` for automatic updates)\n", humanDuration(age))
}

// markIndexed touches the stamp file. Called by cmdIndex on success.
func markIndexed() {
	_ = os.MkdirAll(dataDir(), 0o755)
	f, err := os.OpenFile(stampPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	_, _ = io.WriteString(f, time.Now().Format(time.RFC3339Nano))
	f.Close()
}

// acquireIndexLock claims the per-DB index lock. It reports exactly one
// of three outcomes:
//
//	ok=true, err=nil      we own the run; call release when done
//	ok=false, err=nil     another indexer holds the lock, and otherPID
//	                      names it when it recorded a PID
//	ok=false, err!=nil    the lock could not be evaluated
//
// The third case must never be reported as the second. A filesystem
// without working locks, or a lock file we cannot create, is a problem
// to surface, not a reason to skip the run quietly: `aii index` returns
// the error and exits non-zero so launchd, cron, and the user see it.
//
// Ownership is an exclusive advisory lock (flock on unix, LockFileEx on
// Windows) held on the lock file for the life of the run. The kernel
// owns the lock's lifecycle, so a crashed or `kill -9`'d indexer
// releases it with no stale-lock reaping and no window where two
// processes both decide the other is dead.
//
// The lock file itself is never removed. Unlinking it would let one
// process lock the unlinked inode while another locks a freshly created
// file at the same path, and both would run. Release truncates the file
// instead, which leaves it present and empty.
func acquireIndexLock() (release func(), ok bool, otherPID int, err error) {
	noop := func() {}
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		return noop, false, 0, fmt.Errorf("create data dir %s: %w", dataDir(), err)
	}
	f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return noop, false, 0, fmt.Errorf("open lock file %s: %w", lockPath(), err)
	}
	locked, err := lockWithRetry(f)
	if err != nil {
		f.Close()
		return noop, false, 0, fmt.Errorf("lock %s: %w", lockPath(), err)
	}
	if !locked {
		// Read the holder's PID for the message, not for a decision.
		pid := readLockPID()
		f.Close()
		return noop, false, pid, nil
	}

	// Transitional, for one release: aii 0.6.0 and earlier claimed the
	// lock by writing a PID and took no advisory lock, so our lock says
	// nothing about an indexer started by an older binary. While the
	// file names some other live process, assume that is what it is and
	// let it finish. Drop this block once no pre-0.7.0 binary is in
	// use; the PID becomes display-only again.
	//
	// "Live" here means the kernel still has a process table entry, so
	// an indexer that exited but has not been reaped by its parent — a
	// zombie — reads as running and holds off a new run until the
	// parent reaps it. That costs at most a skipped interval, cron and
	// launchd reap their children, and the whole block goes away next
	// release, so it is not worth a platform-specific zombie check.
	if pid := readLockPID(); pid > 0 && pid != os.Getpid() && processAlive(pid) {
		_ = unlockFile(f)
		f.Close()
		return noop, false, pid, nil
	}

	// We own the run. Record our PID so `aii cron status` and anything
	// else reading the file can name the process.
	_ = f.Truncate(0)
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err == nil {
		_ = f.Sync()
	}

	released := false
	return func() {
		if released {
			return
		}
		released = true
		// Clear the PID first: once the lock is gone the file is up for
		// grabs, and a leftover PID would misreport the holder.
		_ = f.Truncate(0)
		_ = unlockFile(f)
		f.Close()
	}, true, 0, nil
}

// A run that conflicts with the lock skips indexing, so it is worth a
// short wait first: `aii cron status` holds a shared lock on the file
// for a moment to read its state, and a scheduled run that started in
// that moment would otherwise skip a whole interval. A real indexer
// holds the lock for seconds to minutes, so waiting this long never
// turns a genuine conflict into a false acquire.
const (
	lockRetryWindow = 150 * time.Millisecond
	lockRetryPause  = 10 * time.Millisecond
)

// lockWithRetry takes the exclusive lock, retrying briefly while it
// conflicts. An error is returned as is, without retrying: a
// filesystem that cannot lock will not start being able to.
func lockWithRetry(f *os.File) (bool, error) {
	deadline := time.Now().Add(lockRetryWindow)
	for {
		locked, err := tryLockFile(f, true)
		if err != nil || locked {
			return locked, err
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(lockRetryPause)
	}
}

// indexLocked reports whether an indexer holds the lock right now,
// along with the PID it recorded. Used by `aii cron status`. A non-nil
// error means the lock could not be evaluated, which is neither "held"
// nor "free"; the caller says so rather than guessing.
//
// The probe takes a *shared* lock, so running `aii cron status` cannot
// make a scheduled run skip: an exclusive holder still blocks the
// probe, but the probe never blocks an indexer.
func indexLocked() (locked bool, pid int, err error) {
	f, err := os.Open(lockPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("open lock file %s: %w", lockPath(), err)
	}
	defer f.Close()
	free, err := tryLockFile(f, false)
	if err != nil {
		return false, 0, fmt.Errorf("lock %s: %w", lockPath(), err)
	}
	if !free {
		return true, readLockPID(), nil
	}
	_ = unlockFile(f)
	// Transitional, matching acquireIndexLock: an indexer started by a
	// pre-0.7.0 binary holds no advisory lock, so the PID is all there
	// is to go on. Drop this with that block.
	if p := readLockPID(); p > 0 && p != os.Getpid() && processAlive(p) {
		return true, p, nil
	}
	return false, 0, nil
}

// readLockPID returns the PID the current lock holder recorded, or 0
// when the file is missing or empty. It says nothing about whether the
// lock is held — only tryLockFile answers that.
func readLockPID() int {
	b, err := os.ReadFile(lockPath())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}
