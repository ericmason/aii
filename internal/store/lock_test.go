package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// holdWriteLock takes SQLite's write lock on path, as a second process
// would, and returns a function that releases it.
func holdWriteLock(t *testing.T, path string) (release func()) {
	t.Helper()
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
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

// trigramDocs counts the documents the trigram index holds.
func trigramDocs(t *testing.T, db *DB) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts_tri_docsize`).Scan(&n); err != nil {
		t.Fatalf("count trigram docs: %v", err)
	}
	return n
}

// seedWithEmptyTrigramIndex returns a database whose messages are not in
// the trigram index, the state a pre-trigram database is in after an
// upgrade.
func seedWithEmptyTrigramIndex(t *testing.T, path string, msgs int) {
	t.Helper()
	db, err := OpenForIndex(path)
	if err != nil {
		t.Fatalf("OpenForIndex: %v", err)
	}
	defer db.Close()
	var rows []Message
	for i := 0; i < msgs; i++ {
		rows = append(rows, Message{Ordinal: i, Role: "user", TS: int64(100 + i),
			Content: fmt.Sprintf("message %d mentions the OAuthToken refresh", i)})
	}
	seed(t, db, &Session{
		Agent: "claudecode", UID: "sess-trigram", Workspace: "repo", Title: "trigram",
		StartedAt: 100, EndedAt: 200, SourcePath: "/tmp/trigram.jsonl",
	}, rows)
	if _, err := db.Exec(`INSERT INTO messages_fts_tri(messages_fts_tri) VALUES('delete-all')`); err != nil {
		t.Fatalf("empty trigram index: %v", err)
	}
	if got := trigramDocs(t, db); got != 0 {
		t.Fatalf("trigram index holds %d docs, want 0", got)
	}
}

// A query command must not rebuild the trigram index. The rebuild reads
// every message and holds the write lock for minutes on a large
// database, which is long enough for a concurrent index run to lose
// batches to SQLITE_BUSY.
func TestOpen_LeavesTrigramRebuildToTheIndexRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aii.db")
	seedWithEmptyTrigramIndex(t, path, 20)

	release := holdWriteLock(t, path)

	start := time.Now()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open while the write lock is held: %v", err)
	}
	defer db.Close()
	elapsed := time.Since(start)
	if elapsed > BusyTimeout/2 {
		t.Errorf("Open took %s; it waited for the write lock instead of skipping the rebuild", elapsed)
	}
	if got := trigramDocs(t, db); got != 0 {
		t.Errorf("Open rebuilt the trigram index (%d docs); only an index run should", got)
	}

	release()

	idx, err := OpenForIndex(path)
	if err != nil {
		t.Fatalf("OpenForIndex: %v", err)
	}
	defer idx.Close()
	if got := trigramDocs(t, idx); got != 20 {
		t.Errorf("trigram index holds %d docs after an index run, want 20", got)
	}
}

// The rebuild check reads both counts from one snapshot. Read in
// separate statements, a writer committing between them makes a current
// index look stale, and the rebuild that follows blocks every writer.
func TestTrigramIndexStale_IgnoresConcurrentInserts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aii.db")
	reader, err := OpenForIndex(path)
	if err != nil {
		t.Fatalf("OpenForIndex: %v", err)
	}
	defer reader.Close()
	sid := seed(t, reader, &Session{
		Agent: "claudecode", UID: "sess-race", Workspace: "repo", Title: "race",
		StartedAt: 100, EndedAt: 200, SourcePath: "/tmp/race.jsonl",
	}, []Message{{Ordinal: 0, Role: "user", TS: 100, Content: "first message"}})

	writer, err := Open(path)
	if err != nil {
		t.Fatalf("Open writer: %v", err)
	}
	defer writer.Close()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			tx, err := writer.Begin()
			if err != nil {
				return
			}
			err = writer.InsertMessages(tx, sid, []Message{
				{Ordinal: i, Role: "assistant", TS: int64(100 + i), Content: fmt.Sprintf("reply %d", i)},
			})
			if err != nil {
				tx.Rollback()
				return
			}
			if err := tx.Commit(); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 400; i++ {
		stale, err := trigramIndexStale(reader.DB)
		if err != nil {
			close(stop)
			<-done
			t.Fatalf("trigramIndexStale: %v", err)
		}
		if stale {
			close(stop)
			<-done
			t.Fatalf("trigram index reported stale on iteration %d while a writer was inserting; the counts came from different snapshots", i)
		}
	}
	close(stop)
	<-done
}

// A file marked for reindex keeps its stored offsets; a later successful
// batch clears the mark.
func TestMarkNeedsReindex(t *testing.T) {
	db := openTestDB(t)
	state := IndexState{SourcePath: "/tmp/one.jsonl", MtimeNs: 7, Size: 11, LastOffset: 11}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := db.MarkNeedsReindex(state.SourcePath); err != nil {
		t.Fatalf("MarkNeedsReindex: %v", err)
	}
	got, err := db.GetState(state.SourcePath)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if !got.NeedsReindex {
		t.Error("state should be marked for reindex")
	}
	if got.MtimeNs != state.MtimeNs || got.Size != state.Size || got.LastOffset != state.LastOffset {
		t.Errorf("mark changed the stored offsets: %+v, want %+v", got, state)
	}

	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState after mark: %v", err)
	}
	got, err = db.GetState(state.SourcePath)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if got.NeedsReindex {
		t.Error("a successful batch should clear the mark")
	}
}

// Marking a file that was never indexed is a no-op: the next run reads
// it from the start anyway.
func TestMarkNeedsReindex_UnknownFile(t *testing.T) {
	db := openTestDB(t)
	if err := db.MarkNeedsReindex("/tmp/never-seen.jsonl"); err != nil {
		t.Fatalf("MarkNeedsReindex: %v", err)
	}
	got, err := db.GetState("/tmp/never-seen.jsonl")
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if got != nil {
		t.Errorf("GetState = %+v, want no row", got)
	}
}
