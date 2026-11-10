package store

import (
	"context"
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
// VACUUM builds a second copy of the database before it replaces the
// original, so it needs free disk space about the size of the file, and
// it holds the write lock for the whole rewrite. Like OptimizeFTS, it
// belongs to a process holding the index lock.
func (d *DB) Vacuum(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `VACUUM`)
	return err
}
