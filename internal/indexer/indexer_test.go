package indexer

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"

	_ "modernc.org/sqlite"
)

// shortTimeouts makes every wait in this package short enough for a
// test: the store's busy_timeout, the per-batch retries, and the
// post-run marks.
func shortTimeouts(t *testing.T) {
	t.Helper()
	oldBusy, oldAttempts, oldBackoff := store.BusyTimeout, applyAttempts, applyBackoff
	oldMarkAttempts, oldMarkBackoff := markAttempts, markBackoff
	store.BusyTimeout = 100 * time.Millisecond
	applyAttempts = 3
	applyBackoff = []time.Duration{10 * time.Millisecond}
	markAttempts = 10
	markBackoff = []time.Duration{100 * time.Millisecond}
	t.Cleanup(func() {
		store.BusyTimeout, applyAttempts, applyBackoff = oldBusy, oldAttempts, oldBackoff
		markAttempts, markBackoff = oldMarkAttempts, oldMarkBackoff
	})
}

// holdWriteLock takes SQLite's write lock on path, as another process
// would, and returns a function that releases it.
func holdWriteLock(t *testing.T, path string) (release func()) {
	t.Helper()
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(100)")
	if err != nil {
		t.Fatalf("open locker: %v", err)
	}
	ctx := context.Background()
	conn, err := other.Conn(ctx)
	if err != nil {
		other.Close()
		t.Fatalf("locker conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		conn.Close()
		other.Close()
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
			t.Errorf("ROLLBACK: %v", err)
		}
		conn.Close()
		other.Close()
	}
	t.Cleanup(release)
	return release
}

// fakeSource emits the batches it was given, once.
type fakeSource struct {
	name    string
	batches []source.Batch
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Scan(ctx context.Context, _ *store.DB, _ bool, out chan<- source.Batch) error {
	for _, b := range f.batches {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- b:
		}
	}
	return nil
}

// batchFor builds a batch that replaces whatever is stored for path,
// the shape `aii index --full` produces.
func batchFor(path, uid string, msgs ...string) source.Batch {
	b := source.Batch{
		Session: store.Session{
			Agent: "claudecode", UID: uid, Workspace: "/repo", Title: "title",
			StartedAt: 100, EndedAt: 200, SourcePath: path,
			SourceMtimeNs: 4242, SourceSize: 99,
		},
		State:    store.IndexState{SourcePath: path, MtimeNs: 4242, Size: 99, LastOffset: 99},
		Truncate: true,
		Usage: []store.Usage{{
			Agent: "claudecode", SessionUID: uid, Key: uid + "-req-1",
			TS: 150, Model: "claude", Input: 10, Output: 20,
		}},
	}
	for i, m := range msgs {
		b.Messages = append(b.Messages, store.Message{Ordinal: i, Role: "user", TS: int64(100 + i), Content: m})
	}
	return b
}

func countRows(t *testing.T, db *store.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func run(t *testing.T, db *store.DB, batches ...source.Batch) (Stats, error) {
	t.Helper()
	src := &fakeSource{name: "claudecode", batches: batches}
	return Run(context.Background(), db, []source.Source{src}, Options{Full: true})
}

// A batch that hits the write lock is retried, not dropped.
func TestRun_RetriesBatchWhileWriteLockHeld(t *testing.T) {
	shortTimeouts(t)
	path := filepath.Join(t.TempDir(), "aii.db")
	db, err := store.OpenForIndex(path)
	if err != nil {
		t.Fatalf("OpenForIndex: %v", err)
	}
	defer db.Close()

	release := holdWriteLock(t, path)
	go func() {
		time.Sleep(250 * time.Millisecond)
		release()
	}()

	stats, err := run(t, db, batchFor("/transcripts/a.jsonl", "sess-a", "hello", "world"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Deferred != 0 || stats.Errors != 0 {
		t.Errorf("stats = %+v, want no errors and nothing deferred", stats)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM messages`); got != 2 {
		t.Errorf("stored %d messages, want 2", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM token_usage`); got != 1 {
		t.Errorf("stored %d usage rows, want 1", got)
	}
}

// A batch that never gets the lock leaves nothing half-written, leaves
// the stored state alone, and marks the file so a later run reparses
// it. Before this, the batch was logged and dropped: its messages and
// usage were never indexed, and nothing recorded that.
func TestRun_DroppedBatchMarksFileForReindex(t *testing.T) {
	shortTimeouts(t)
	path := filepath.Join(t.TempDir(), "aii.db")
	db, err := store.OpenForIndex(path)
	if err != nil {
		t.Fatalf("OpenForIndex: %v", err)
	}
	defer db.Close()

	// What an earlier run indexed for this file.
	const transcript = "/transcripts/b.jsonl"
	if _, err := run(t, db, batchFor(transcript, "sess-b", "first")); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	before, err := db.GetState(transcript)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}

	// The lock outlasts every retry of the batch (three attempts of
	// 100ms each, 10ms apart) and is gone well inside the budget the
	// marks retry on.
	release := holdWriteLock(t, path)
	go func() {
		time.Sleep(800 * time.Millisecond)
		release()
	}()

	stats, err := run(t, db, batchFor(transcript, "sess-b", "first", "second", "third"))
	if err == nil {
		t.Fatal("Run should report that a batch could not be written")
	}
	if !strings.Contains(err.Error(), "marked for reindex") {
		t.Errorf("Run error = %q, want it to name the deferral", err)
	}
	if stats.Deferred != 1 || stats.Errors != 1 {
		t.Errorf("stats = %+v, want 1 error and 1 deferred", stats)
	}

	// The earlier messages survive: a batch that cannot be written must
	// not delete what it was going to replace.
	if got := countRows(t, db, `SELECT COUNT(*) FROM messages`); got != 1 {
		t.Errorf("stored %d messages, want the 1 from the earlier run", got)
	}
	after, err := db.GetState(transcript)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if after == nil {
		t.Fatal("the dropped batch deleted the file's index state")
	}
	if after.MtimeNs != before.MtimeNs || after.Size != before.Size || after.LastOffset != before.LastOffset {
		t.Errorf("state advanced to %+v, want %+v", after, before)
	}
	if !after.NeedsReindex {
		t.Error("the file should be marked for reindex, or the next run will skip it as unchanged")
	}
}

// A batch that fails partway through leaves the file exactly as it was.
// The delete a truncating batch starts with used to commit on its own,
// so a failure after it wiped the sessions the batch was going to
// replace.
func TestRun_FailedBatchLeavesStoredRowsIntact(t *testing.T) {
	db := openTestStore(t)
	const transcript = "/transcripts/e.jsonl"
	if _, err := run(t, db, batchFor(transcript, "sess-e", "first")); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	before, err := db.GetState(transcript)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}

	// Fail the batch at its usage insert, after the delete and the
	// messages it would otherwise have committed.
	if _, err := db.Exec(`DROP TABLE token_usage`); err != nil {
		t.Fatalf("drop token_usage: %v", err)
	}

	stats, err := run(t, db, batchFor(transcript, "sess-e", "first", "second"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Errors != 1 {
		t.Errorf("stats = %+v, want 1 error", stats)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM messages`); got != 1 {
		t.Errorf("stored %d messages, want the 1 from the earlier run", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM sessions WHERE source_path = ?`, transcript); got != 1 {
		t.Errorf("stored %d sessions for the file, want 1", got)
	}
	after, err := db.GetState(transcript)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if after == nil || *after != *before {
		t.Errorf("state = %+v, want it unchanged at %+v", after, before)
	}
}

// When even the mark cannot be written, the run says which files it
// will keep skipping and what to run instead. Silence here is what cost
// 436 sessions.
func TestRun_ReportsFilesItCouldNotMark(t *testing.T) {
	shortTimeouts(t)
	markAttempts = 2
	path := filepath.Join(t.TempDir(), "aii.db")
	db, err := store.OpenForIndex(path)
	if err != nil {
		t.Fatalf("OpenForIndex: %v", err)
	}
	defer db.Close()

	const transcript = "/transcripts/d.jsonl"
	if _, err := run(t, db, batchFor(transcript, "sess-d", "first")); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	holdWriteLock(t, path) // held for the whole run

	_, err = run(t, db, batchFor(transcript, "sess-d", "first", "second"))
	if err == nil {
		t.Fatal("Run should report the file it could not mark")
	}
	if !strings.Contains(err.Error(), transcript) || !strings.Contains(err.Error(), "aii index --full") {
		t.Errorf("Run error = %q, want the path and the command that recovers it", err)
	}
}

// A marked file is read from the start on the next incremental run,
// even though its mtime and size are unchanged.
func TestFullForFile_MarkedFileIsReadAgain(t *testing.T) {
	db := openTestStore(t)
	const transcript = "/transcripts/c.jsonl"
	state := store.IndexState{SourcePath: transcript, MtimeNs: 1, Size: 2, LastOffset: 2}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	prev, err := db.GetState(transcript)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if source.FullForFile(false, prev) {
		t.Error("an unmarked, unchanged file should not be reread")
	}
	if err := db.MarkNeedsReindex(transcript); err != nil {
		t.Fatalf("MarkNeedsReindex: %v", err)
	}
	prev, err = db.GetState(transcript)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if !source.FullForFile(false, prev) {
		t.Error("a marked file should be read from the start")
	}
}

func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.OpenForIndex(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatalf("OpenForIndex: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
