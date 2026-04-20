package main

// Indexer locking and stamp primitives. The indexer is a single-writer
// process (SQLite) so both the cron job and manual `aii index` runs
// share this PID lockfile to avoid racing each other. The stamp file
// records the last successful index so `aii cron status` can report
// freshness.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// acquireIndexLock atomically claims the per-DB index lock. Returns a
// release function (idempotent) and ok=true on success. ok=false means
// another indexer holds it; the returned PID is informational.
//
// Stale locks (lockfile present, owning PID dead) are silently reaped
// and re-acquired — common after a crash or kill -9.
func acquireIndexLock() (release func(), ok bool, otherPID int) {
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		return func() {}, false, 0
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			released := false
			return func() {
				if released {
					return
				}
				released = true
				_ = os.Remove(lockPath())
			}, true, 0
		}
		if !errors.Is(err, os.ErrExist) {
			return func() {}, false, 0
		}
		// Lock exists. Check if owner is alive.
		pid := readLockPID()
		if pid > 0 && processAlive(pid) {
			return func() {}, false, pid
		}
		// Stale — owner gone. Remove and retry once.
		_ = os.Remove(lockPath())
	}
	return func() {}, false, 0
}

// indexLocked reports whether the lock is currently held by a live
// process — without trying to acquire it. Used by `aii cron status`.
func indexLocked() (bool, int) {
	pid := readLockPID()
	if pid <= 0 {
		return false, 0
	}
	if !processAlive(pid) {
		_ = os.Remove(lockPath())
		return false, 0
	}
	return true, pid
}

func readLockPID() int {
	b, err := os.ReadFile(lockPath())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// processAlive checks whether a PID is still live. signal 0 is the
// portable "are you there?" probe — it returns ESRCH if the process is
// gone and EPERM if it exists but we don't own it (still alive).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
