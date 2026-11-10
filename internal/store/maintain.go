package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FTSTables are the two full-text indexes the message triggers keep in
// sync with the messages table.
var FTSTables = []string{"messages_fts", "messages_fts_tri"}

// OptimizeFTS merges each full-text index down to a single segment and
// returns how long each one took, in the order of FTSTables.
//
// FTS5 does not erase a deleted row's terms. The delete trigger appends
// a marker instead, and the terms and the marker both stay on disk
// until a merge drops them. Reindexing a transcript deletes and
// reinserts every message in it, so the index keeps a second copy of
// text no query can ever return; `aii index --full` does that for the
// whole corpus at once and can more than double the index on disk.
// Merging to one segment discards every superseded term.
//
// This reads and rewrites the whole index, which takes minutes on a
// large database and holds SQLite's single write lock throughout. Call
// it only from a process holding the index lock, the same rule that
// governs the rebuilds in OpenForIndex.
//
// The freed pages go on the database's free list; the file does not
// shrink until something rewrites it. Call Vacuum for that.
func (d *DB) OptimizeFTS(ctx context.Context) ([]time.Duration, error) {
	took := make([]time.Duration, 0, len(FTSTables))
	for _, t := range FTSTables {
		start := time.Now()
		// The table name comes from FTSTables, never from a caller.
		stmt := fmt.Sprintf(`INSERT INTO %s(%s) VALUES('optimize')`, t, t)
		if _, err := d.ExecContext(ctx, stmt); err != nil {
			return took, fmt.Errorf("optimize %s: %w", t, err)
		}
		took = append(took, time.Since(start))
	}
	return took, nil
}

// Vacuum rewrites the database file without its free pages, returning
// the space an optimize or a delete released to the operating system.
//
// VACUUM builds a second copy of the database and a temporary file
// alongside it, so it can need up to twice the size of the database in
// free space. Part of that goes to the temporary directory, which may
// be on a different volume than the database; see
// https://www.sqlite.org/lang_vacuum.html. It holds the write lock for
// the whole rewrite, so like OptimizeFTS it belongs to a process
// holding the index lock.
func (d *DB) Vacuum(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `VACUUM`)
	return err
}

// ErrCheckpointBusy reports that a reader kept the write-ahead log from
// being emptied. The database is intact and every write is committed;
// only the space the log holds is still spoken for.
var ErrCheckpointBusy = errors.New("another aii process is reading the database, so the write-ahead log could not be emptied; the space comes back at the next index run or `aii compact`")

// checkpointAttempts and checkpointWait bound the retry. A reader that
// holds the log for longer than this is doing real work, and waiting on
// it would be worse than reporting it, because the caller holds the
// index lock and SQLite's write lock throughout.
const (
	checkpointAttempts = 10
	checkpointWait     = 500 * time.Millisecond
)

// CheckpointTruncate empties the write-ahead log and shrinks it to
// nothing, so the space its frames hold goes back to the filesystem.
//
// A truncating checkpoint needs every reader out of the way. When one
// is not, SQLite does not fail: the pragma returns a row whose first
// column is 1 and leaves the log where it is. Running it through Exec
// discards that row and reads as success, which is how a compact can
// report that it shrank a database while the log still holds a full
// copy of it. So this reads the row, retries while a reader holds the
// log, and returns ErrCheckpointBusy when the reader outlasts the
// retries.
func (d *DB) CheckpointTruncate(ctx context.Context) error {
	var lastBusy, lastLog, lastCheckpointed int64
	for attempt := 0; attempt < checkpointAttempts; attempt++ {
		if attempt > 0 {
			t := time.NewTimer(checkpointWait)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
		row := d.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		if err := row.Scan(&lastBusy, &lastLog, &lastCheckpointed); err != nil {
			return fmt.Errorf("checkpoint: %w", err)
		}
		// busy is 0 and the log is empty only when the checkpoint ran
		// to the end of the log and truncated it.
		if lastBusy == 0 && lastLog == 0 {
			return nil
		}
	}
	return fmt.Errorf("%w (log holds %d frames, %d checkpointed)", ErrCheckpointBusy, lastLog, lastCheckpointed)
}
