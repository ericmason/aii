package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// mustAcquire takes the index lock or fails the test.
func mustAcquire(t *testing.T) func() {
	t.Helper()
	release, ok, pid, err := acquireIndexLock()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !ok {
		t.Fatalf("acquire: lock reported held by pid %d", pid)
	}
	return release
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

// TestDataDirIsAbsolute covers a relative AII_DB. A scheduled job runs
// from a different directory than the install did — launchd from / and
// cron from $HOME — so every path derived from the DB has to be
// absolute before it is written into a schedule.
func TestDataDirIsAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("AII_DB", filepath.Join("sub", "aii.db"))

	for name, got := range map[string]string{
		"dataDir":     dataDir(),
		"lockPath":    lockPath(),
		"stampPath":   stampPath(),
		"cronLogPath": cronLogPath(),
	} {
		if !filepath.IsAbs(got) {
			t.Errorf("%s() = %s, want an absolute path", name, got)
		}
	}
	if want := filepath.Join(dir, "sub"); dataDir() != want {
		// On macOS t.TempDir() is under /var, a symlink to /private/var;
		// compare what filepath.Abs would produce, not the real path.
		t.Errorf("dataDir() = %s, want %s", dataDir(), want)
	}
}

func TestAcquireIndexLock_Contention(t *testing.T) {
	withTempDataDir(t)

	release := mustAcquire(t)
	t.Cleanup(release)

	// A second acquirer sees the advisory lock and fails, reporting the
	// PID the holder recorded.
	release2, ok2, otherPID, err := acquireIndexLock()
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if ok2 {
		release2()
		t.Fatal("second acquire should fail while first is held")
	}
	if otherPID != os.Getpid() {
		t.Errorf("otherPID = %d, want our pid %d", otherPID, os.Getpid())
	}

	// Releasing makes the lock available again.
	release()
	release3 := mustAcquire(t)
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

	release := mustAcquire(t)
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
	held, err := tryLockFile(holder, true)
	if err != nil {
		t.Fatalf("lock lock file: %v", err)
	}
	if !held {
		t.Fatal("nothing should hold the lock yet")
	}

	release, ok, otherPID, err := acquireIndexLock()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if ok {
		release()
		t.Fatal("acquire should fail while another process holds the lock")
	}
	if otherPID != 2147483646 {
		t.Errorf("otherPID = %d, want the PID recorded in the file (2147483646)", otherPID)
	}
	locked, _, err := indexLocked()
	if err != nil {
		t.Fatalf("indexLocked: %v", err)
	}
	if !locked {
		t.Error("indexLocked should report the held lock")
	}

	// Once the holder lets go, the next acquirer wins.
	if err := unlockFile(holder); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	release2 := mustAcquire(t)
	release2()
}

// TestReadLockPIDWhileHeld keeps the lock readable while it is held.
// On Windows a byte-range lock is mandatory, so locking the whole file
// would make this read fail and `aii cron status` would report pid 0
// for every running indexer. On unix flock does not restrict reads, so
// this only bites on Windows — where the lock covers a single byte far
// past the PID instead.
func TestReadLockPIDWhileHeld(t *testing.T) {
	withTempDataDir(t)

	release := mustAcquire(t)
	defer release()

	if got := readLockPID(); got != os.Getpid() {
		t.Errorf("readLockPID while held = %d, want %d", got, os.Getpid())
	}
	locked, pid, err := indexLocked()
	if err != nil {
		t.Fatalf("indexLocked: %v", err)
	}
	if !locked || pid != os.Getpid() {
		t.Errorf("indexLocked = (%t, %d), want (true, %d)", locked, pid, os.Getpid())
	}
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
				release, ok, _, err := acquireIndexLock()
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
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

// TestAcquireIndexLock_UnreadableLockPath covers the third outcome: the
// lock could not be evaluated. A directory where the lock file belongs
// is one way to get there; a filesystem whose locks do not work is the
// real one. Reporting it as "another indexer is running" would stop
// scheduled indexing for good, silently, because the message is hidden
// under --quiet and the exit status is 0.
func TestAcquireIndexLock_UnreadableLockPath(t *testing.T) {
	withTempDataDir(t)
	if err := os.MkdirAll(lockPath(), 0o755); err != nil {
		t.Fatalf("mkdir lock path: %v", err)
	}

	release, ok, pid, err := acquireIndexLock()
	if err == nil {
		release()
		t.Fatal("acquire should report an error when the lock file cannot be opened")
	}
	if ok {
		release()
		t.Fatal("acquire should not report success")
	}
	if pid != 0 {
		t.Errorf("otherPID = %d, want 0: there is no holder to name", pid)
	}

	// `aii index` turns that into a non-zero exit, not a quiet skip.
	if err := cmdIndex(t.Context(), []string{"--quiet"}); err == nil {
		t.Error("aii index --quiet should fail when the lock cannot be evaluated")
	}

	// `aii cron status` must not call it free either. A directory opens
	// and flocks fine on unix, so only the file type gives it away.
	locked, pid, err := indexLocked()
	if err == nil {
		t.Error("indexLocked should report an error for a lock path that is not a regular file")
	}
	if locked || pid != 0 {
		t.Errorf("indexLocked() = (%v, %d), want (false, 0) alongside the error", locked, pid)
	}
	if line := lockStatusLine(); !strings.HasPrefix(line, "Lock:  unknown") {
		t.Errorf("status line = %q, want a Lock:  unknown line", line)
	}
}

func TestIndexLocked_Live(t *testing.T) {
	withTempDataDir(t)

	// No lock file → not locked, no error.
	locked, _, err := indexLocked()
	if err != nil {
		t.Fatalf("indexLocked with no file: %v", err)
	}
	if locked {
		t.Error("no lock file should not be locked")
	}

	release := mustAcquire(t)
	defer release()

	locked, pid, err := indexLocked()
	if err != nil {
		t.Fatalf("indexLocked: %v", err)
	}
	if !locked {
		t.Error("live lock should report locked")
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}
}

// TestIndexLocked_SharedProbe keeps `aii cron status` out of the
// indexer's way: the probe takes a shared lock, so another reader's
// shared lock does not read as a running indexer.
func TestIndexLocked_SharedProbe(t *testing.T) {
	withTempDataDir(t)
	writeStaleLockFile(t)

	reader, err := os.Open(lockPath())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer reader.Close()
	got, err := tryLockFile(reader, false)
	if err != nil {
		t.Fatalf("shared lock: %v", err)
	}
	if !got {
		t.Fatal("a shared lock should be available on an unheld file")
	}
	defer unlockFile(reader)

	locked, _, err := indexLocked()
	if err != nil {
		t.Fatalf("indexLocked: %v", err)
	}
	if locked {
		t.Error("another reader's shared lock should not read as a held lock")
	}
}

// TestAcquireIndexLock_WaitsOutAStatusProbe: a status probe holds a
// shared lock for a moment, and a scheduled run that starts in that
// moment must not skip its whole interval. Acquire retries briefly
// before concluding that someone else is indexing.
func TestAcquireIndexLock_WaitsOutAStatusProbe(t *testing.T) {
	withTempDataDir(t)
	writeStaleLockFile(t)

	probe, err := os.Open(lockPath())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer probe.Close()
	if ok, err := tryLockFile(probe, false); err != nil || !ok {
		t.Fatalf("shared lock: ok=%t err=%v", ok, err)
	}
	unlocked := make(chan struct{})
	go func() {
		time.Sleep(lockRetryPause * 2)
		_ = unlockFile(probe)
		close(unlocked)
	}()

	release, ok, pid, err := acquireIndexLock()
	// Wait for the goroutine before the deferred Close, so the two
	// never touch the file at once.
	<-unlocked
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !ok {
		t.Fatalf("acquire should wait out a momentary probe, got held by pid %d", pid)
	}
	release()
}

func TestIndexLocked_StaleFile(t *testing.T) {
	withTempDataDir(t)
	writeStaleLockFile(t)

	locked, _, err := indexLocked()
	if err != nil {
		t.Fatalf("indexLocked: %v", err)
	}
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

	release := mustAcquire(t)
	release()

	locked, pid, err := indexLocked()
	if err != nil {
		t.Fatalf("indexLocked: %v", err)
	}
	if locked {
		t.Error("released lock should be reported free")
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0 after release clears it", pid)
	}
	// Release truncates the file and leaves it in place. Unlinking it
	// would let one process lock the unlinked inode while another locks
	// a new file at the same path, and both would index.
	if _, err := os.Stat(lockPath()); err != nil {
		t.Errorf("release should keep the lock file: %v", err)
	}
}

// liveForeignPID starts a process that outlives the test body and
// returns its PID, standing in for an indexer this process did not
// start.
func liveForeignPID(t *testing.T) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sleep")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

// TestAcquireIndexLock_YieldsToPreUpgradeIndexer is the transitional
// guard. aii 0.6.0 and earlier claimed the lock by writing a PID and
// took no advisory lock, so an indexer started by an older binary is
// invisible to the lock. While the file names a live process that is
// not us, the run yields to it instead of indexing alongside it.
func TestAcquireIndexLock_YieldsToPreUpgradeIndexer(t *testing.T) {
	withTempDataDir(t)
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pid := liveForeignPID(t)
	// Exactly what an old binary leaves: a PID, no lock.
	if err := os.WriteFile(lockPath(), []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	release, ok, otherPID, err := acquireIndexLock()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if ok {
		release()
		t.Fatal("acquire should yield while the file names a live process")
	}
	if otherPID != pid {
		t.Errorf("otherPID = %d, want %d", otherPID, pid)
	}
	// The yielding run must leave the lock free for the holder it
	// yielded to, and the PID untouched.
	if got := readLockPID(); got != pid {
		t.Errorf("lock PID = %d, want %d left in place", got, pid)
	}
	// cron status agrees, so the two never disagree about who is running.
	locked, statusPID, err := indexLocked()
	if err != nil {
		t.Fatalf("indexLocked: %v", err)
	}
	if !locked || statusPID != pid {
		t.Errorf("indexLocked = (%t, %d), want (true, %d)", locked, statusPID, pid)
	}
	if got, want := lockStatusLine(), fmt.Sprintf("Lock:  held by pid %d (indexer running now)", pid); got != want {
		t.Errorf("status line = %q, want %q", got, want)
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("our own PID should be alive")
	}
	if processAlive(0) {
		t.Error("PID 0 should not be alive")
	}
	if processAlive(2147483646) {
		t.Error("impossibly-high PID should not be alive")
	}
	pid := liveForeignPID(t)
	if !processAlive(pid) {
		t.Errorf("pid %d should be alive", pid)
	}
}

func TestLockStatusLine(t *testing.T) {
	withTempDataDir(t)

	if got, want := lockStatusLine(), "Lock:  free"; got != want {
		t.Errorf("no lock file: %q, want %q", got, want)
	}

	release := mustAcquire(t)
	if got, want := lockStatusLine(), fmt.Sprintf("Lock:  held by pid %d (indexer running now)", os.Getpid()); got != want {
		t.Errorf("held: %q, want %q", got, want)
	}
	release()
	if got, want := lockStatusLine(), "Lock:  free"; got != want {
		t.Errorf("after release: %q, want %q", got, want)
	}

	// A lock state that cannot be read is neither held nor free.
	if os.Geteuid() == 0 {
		t.Skip("root can read an unreadable file")
	}
	if err := os.Chmod(lockPath(), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockPath(), 0o600) })
	if got := lockStatusLine(); !strings.HasPrefix(got, "Lock:  unknown (") {
		t.Errorf("unreadable lock: %q, want an unknown line", got)
	}
}
