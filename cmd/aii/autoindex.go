package main

// Indexer locking and stamp primitives. The indexer is a single-writer
// process (SQLite) so both the cron job and manual `aii index` runs
// take an exclusive advisory lock on the same lock file to avoid racing
// each other; the file also carries the holder's PID for display. The
// stamp file records the last successful index so `aii cron status` can
// report freshness.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/store"
)

// dataDir is the parent of the DB — also where the lock and stamp live.
func dataDir() string { return filepath.Dir(store.DefaultPath()) }

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

// acquireIndexLock claims the per-DB index lock. Returns a release
// function (idempotent) and ok=true on success. ok=false means another
// indexer holds it; the returned PID is informational and is 0 when the
// holder wrote no PID.
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
func acquireIndexLock() (release func(), ok bool, otherPID int) {
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		return func() {}, false, 0
	}
	f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, false, 0
	}
	locked, err := tryLockFile(f)
	if err != nil || !locked {
		// Read the holder's PID for the message, not for a decision.
		pid := readLockPID()
		f.Close()
		return func() {}, false, pid
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
	}, true, 0
}

// indexLocked reports whether an indexer holds the lock right now,
// along with the PID it recorded. Used by `aii cron status`. It answers
// by trying to take the lock and releasing it again, so it never blocks
// and never reports a crashed indexer as running.
func indexLocked() (bool, int) {
	f, err := os.OpenFile(lockPath(), os.O_RDWR, 0o600)
	if err != nil {
		return false, 0
	}
	defer f.Close()
	locked, err := tryLockFile(f)
	if err != nil {
		return false, 0
	}
	if locked {
		_ = unlockFile(f)
		return false, 0
	}
	return true, readLockPID()
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
