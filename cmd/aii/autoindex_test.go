package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withTempDataDir points dataDir() at a fresh temp dir by overriding
// AII_DB (which store.DefaultPath reads). Restored on cleanup.
func withTempDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AII_DB", filepath.Join(dir, "aii.db"))
	return dir
}

func TestMarkIndexed(t *testing.T) {
	withTempDataDir(t)

	if _, err := os.Stat(stampPath()); !os.IsNotExist(err) {
		t.Fatalf("stamp should not exist yet, stat err = %v", err)
	}
	markIndexed()
	if _, err := os.Stat(stampPath()); err != nil {
		t.Fatalf("stamp should exist after markIndexed: %v", err)
	}
}

func TestAcquireIndexLock_Contention(t *testing.T) {
	withTempDataDir(t)

	release, ok, _ := acquireIndexLock()
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	t.Cleanup(release)

	// A second acquirer sees the advisory lock and fails, reporting the
	// PID the holder recorded.
	release2, ok2, otherPID := acquireIndexLock()
	if ok2 {
		release2()
		t.Fatal("second acquire should fail while first is held")
	}
	if otherPID != os.Getpid() {
		t.Errorf("otherPID = %d, want our pid %d", otherPID, os.Getpid())
	}

	// Releasing makes the lock available again.
	release()
	release3, ok3, _ := acquireIndexLock()
	if !ok3 {
		t.Fatal("acquire after release should succeed")
	}
	release3()
}

// writeStaleLockFile leaves a lock file behind that nobody holds, the
// way a crashed indexer does. 2^31 - 2 is well past any real PID on
// macOS and Linux, so nothing can mistake it for a live process.
func writeStaleLockFile(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(lockPath(), []byte(fmt.Sprint(2147483646)), 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}
}

func TestAcquireIndexLock_StaleFile(t *testing.T) {
	withTempDataDir(t)
	writeStaleLockFile(t)

	release, ok, _ := acquireIndexLock()
	if !ok {
		t.Fatal("a lock file nobody holds should not block acquiring")
	}
	t.Cleanup(release)

	// The lock now records OUR pid.
	if readLockPID() != os.Getpid() {
		t.Errorf("lock PID = %d, want %d", readLockPID(), os.Getpid())
	}
}

// TestAcquireIndexLock_HeldWhileFilePIDIsDead covers the race the
// advisory lock closes: the lock file names a PID that no longer
// exists, yet a live indexer holds the lock. That is what both sides of
// the old PID race saw — each read the dead PID, deleted the file, and
// recreated it, so both ran as indexer. Ownership comes from the lock,
// not from the recorded PID.
func TestAcquireIndexLock_HeldWhileFilePIDIsDead(t *testing.T) {
	withTempDataDir(t)
	writeStaleLockFile(t)

	// Stand in for another indexer: hold the advisory lock on the file
	// while the PID written in it is long gone.
	holder, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer holder.Close()
	held, err := tryLockFile(holder)
	if err != nil {
		t.Fatalf("lock lock file: %v", err)
	}
	if !held {
		t.Fatal("nothing should hold the lock yet")
	}

	release, ok, otherPID := acquireIndexLock()
	if ok {
		release()
		t.Fatal("acquire should fail while another process holds the lock")
	}
	if otherPID != 2147483646 {
		t.Errorf("otherPID = %d, want the PID recorded in the file (2147483646)", otherPID)
	}
	if locked, _ := indexLocked(); !locked {
		t.Error("indexLocked should report the held lock")
	}

	// Once the holder lets go, the next acquirer wins.
	if err := unlockFile(holder); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	release2, ok2, _ := acquireIndexLock()
	if !ok2 {
		t.Fatal("acquire after the holder released should succeed")
	}
	release2()
}

// TestAcquireIndexLock_ConcurrentAcquirersSingleWinner runs many
// acquirers at once against a stale lock file and checks that no two
// ever hold the lock together. Each index run trims cron.log, so a
// second concurrent indexer costs log lines.
func TestAcquireIndexLock_ConcurrentAcquirersSingleWinner(t *testing.T) {
	withTempDataDir(t)
	writeStaleLockFile(t)

	const acquirers = 24
	var holders, maxHolders, wins atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range acquirers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for attempt := 0; attempt < 20; attempt++ {
				release, ok, _ := acquireIndexLock()
				if !ok {
					time.Sleep(time.Millisecond)
					continue
				}
				wins.Add(1)
				n := holders.Add(1)
				for {
					m := maxHolders.Load()
					if n <= m || maxHolders.CompareAndSwap(m, n) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				holders.Add(-1)
				release()
				return
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := maxHolders.Load(); got != 1 {
		t.Errorf("concurrent lock holders = %d, want 1", got)
	}
	if wins.Load() == 0 {
		t.Error("no acquirer ever got the lock")
	}
}

func TestIndexLocked_Live(t *testing.T) {
	withTempDataDir(t)

	// No lock file → not locked.
	if locked, _ := indexLocked(); locked {
		t.Error("no lock file should not be locked")
	}

	release, ok, _ := acquireIndexLock()
	if !ok {
		t.Fatal("acquire: failed")
	}
	defer release()

	locked, pid := indexLocked()
	if !locked {
		t.Error("live lock should report locked")
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}
}

func TestIndexLocked_StaleFile(t *testing.T) {
	withTempDataDir(t)
	writeStaleLockFile(t)

	locked, _ := indexLocked()
	if locked {
		t.Error("a lock file nobody holds should be reported free")
	}
	// The file stays: unlinking it would let one process lock the
	// unlinked inode while another locks a new file at the same path.
	if _, err := os.Stat(lockPath()); err != nil {
		t.Errorf("lock file should still exist: %v", err)
	}
}

func TestIndexLocked_AfterRelease(t *testing.T) {
	withTempDataDir(t)

	release, ok, _ := acquireIndexLock()
	if !ok {
		t.Fatal("acquire: failed")
	}
	release()

	locked, pid := indexLocked()
	if locked {
		t.Error("released lock should be reported free")
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0 after release clears it", pid)
	}
}
