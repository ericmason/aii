package main

import (
	"fmt"
	"os"
	"path/filepath"
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

func TestAutoIndexDisabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"False", false},
		{"1", true},
		{"true", true},
		{"anything-nonempty", true},
	}
	for _, c := range cases {
		t.Setenv("AII_NO_AUTO_INDEX", c.val)
		if got := autoIndexDisabled(); got != c.want {
			t.Errorf("AII_NO_AUTO_INDEX=%q -> %v, want %v", c.val, got, c.want)
		}
	}
}

func TestIndexStale(t *testing.T) {
	withTempDataDir(t)

	// No stamp → stale.
	if !indexStale() {
		t.Fatal("no stamp should be stale")
	}

	// Fresh stamp → not stale.
	markIndexed()
	if indexStale() {
		t.Fatal("fresh stamp should not be stale")
	}

	// Ancient stamp → stale. mtime 1h ago beats 60s threshold.
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(stampPath(), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if !indexStale() {
		t.Fatal("old stamp should be stale")
	}
}

func TestAcquireIndexLock_Contention(t *testing.T) {
	withTempDataDir(t)

	release, ok, _ := acquireIndexLock()
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	t.Cleanup(release)

	// Second attempt with the same live PID in the file fails.
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

func TestAcquireIndexLock_ReapsStale(t *testing.T) {
	withTempDataDir(t)

	// PID 1 on unix exists but reaping is about "process we claim owns
	// this is gone." Use an obviously-dead PID instead.
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 2^31 - 1 is well past any real PID on macOS/linux.
	if err := os.WriteFile(lockPath(), []byte(fmt.Sprint(2147483646)), 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}

	release, ok, _ := acquireIndexLock()
	if !ok {
		t.Fatal("should reap stale lock and acquire")
	}
	t.Cleanup(release)

	// The lock now records OUR pid.
	if readLockPID() != os.Getpid() {
		t.Errorf("lock PID = %d, want %d", readLockPID(), os.Getpid())
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

func TestIndexLocked_ReapsDead(t *testing.T) {
	withTempDataDir(t)

	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Impossibly-high PID that won't collide with a real process.
	if err := os.WriteFile(lockPath(), []byte("2147483646"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	locked, _ := indexLocked()
	if locked {
		t.Error("dead-PID lock should be reported unlocked")
	}
	// And the stale file should have been removed.
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Errorf("stale lock should be removed, stat err = %v", err)
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("our own PID should be alive")
	}
	if processAlive(0) {
		t.Error("PID 0 should not be alive")
	}
	// A very high PID won't be in use.
	if processAlive(2147483646) {
		t.Error("impossibly-high PID should not be alive")
	}
}
