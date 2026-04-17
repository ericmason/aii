package main

// Background auto-indexing. Most commands fork off `aii index --quiet`
// before doing their own work so new sessions land in the DB without
// the user remembering to run `aii index`. A PID lockfile prevents
// concurrent indexers (cron, manual, several shells) from racing each
// other on the single SQLite writer.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ericmason/aii/internal/store"
)

// autoIndexStaleAfter is how recently `aii index` must have completed
// for us to skip kicking a new one. SQLite scans are cheap when nothing
// has changed but they still stat every source file, so this throttles
// chatty back-to-back invocations like `aii search foo && aii show ...`.
const autoIndexStaleAfter = 60 * time.Second

// dataDir is the parent of the DB — also where the lock and stamp live.
func dataDir() string { return filepath.Dir(store.DefaultPath()) }

func lockPath() string  { return filepath.Join(dataDir(), ".index.lock") }
func stampPath() string { return filepath.Join(dataDir(), ".last-index") }

// autoIndexDisabled lets users (and our own spawned children) opt out
// without per-command flag plumbing. AII_NO_AUTO_INDEX=1 wins.
func autoIndexDisabled() bool {
	v := strings.TrimSpace(os.Getenv("AII_NO_AUTO_INDEX"))
	return v != "" && v != "0" && strings.ToLower(v) != "false"
}

// kickBackgroundIndex spawns `aii index --quiet` as a detached child
// process and returns immediately. Safe to call from short-lived CLI
// commands — the child survives our exit. No-ops if:
//   - AII_NO_AUTO_INDEX is set,
//   - another indexer is already running (lock held by live PID),
//   - the last successful index finished within autoIndexStaleAfter.
func kickBackgroundIndex() {
	if autoIndexDisabled() {
		return
	}
	if !indexStale() {
		return
	}
	if locked, _ := indexLocked(); locked {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "index", "--quiet")
	// Detach from this process group so signals to us (e.g. ctrl-c on a
	// search) don't bring the indexer down with us.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Suppress recursion: the spawned child must not turn around and
	// try to spawn another one (defensive — `aii index` doesn't kick,
	// but we may add commands later).
	cmd.Env = append(os.Environ(), "AII_NO_AUTO_INDEX=1")

	if err := cmd.Start(); err != nil {
		return
	}
	// Release the OS process handle so we don't accumulate zombies if
	// the parent stays alive long enough to reap. This is the key bit
	// for "fire and forget."
	_ = cmd.Process.Release()
}

// indexStale returns true if the last successful index finished longer
// ago than autoIndexStaleAfter, OR if there's no stamp at all.
func indexStale() bool {
	info, err := os.Stat(stampPath())
	if err != nil {
		return true
	}
	return time.Since(info.ModTime()) > autoIndexStaleAfter
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
// process — without trying to acquire it. Used by kickBackgroundIndex
// to skip spawning when an indexer is already running.
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
