package main

// `aii compact` reclaims the space a reindex leaves behind. It runs the
// same two steps `aii index --full` finishes with, plus the VACUUM that
// returns the freed pages to the filesystem, and it takes the index
// lock so neither step can stall a scheduled index run.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ericmason/aii/internal/store"
)

func cmdCompact(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("compact", flag.ExitOnError)
	noVacuum := fs.Bool("no-vacuum", false, "merge the full-text indexes but skip the rewrite, leaving the freed pages inside the file for later writes to reuse")
	quiet := fs.Bool("quiet", false, "suppress the per-step lines")
	fs.Parse(reorderFlags(args))

	// One writer at a time, same lock the indexer takes: both steps
	// hold SQLite's write lock for minutes on a large database, and a
	// scheduled index run that overlapped would lose its batches.
	release, ok, otherPID, err := acquireIndexLock()
	if err != nil {
		return fmt.Errorf("index lock: %w", err)
	}
	if !ok {
		return fmt.Errorf("an indexer is running (pid %d); run `aii compact` when it has finished", otherPID)
	}
	defer release()

	path := store.DefaultPath()
	before := dbFileBytes(path)

	// OpenForIndex, not Open: this process holds the index lock, so it
	// is the one that may run the rebuilds that scan every message.
	db, err := store.OpenForIndex(path)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := compactDB(ctx, db, *noVacuum, *quiet); err != nil {
		return err
	}

	after := dbFileBytes(path)
	if !*quiet {
		switch {
		case *noVacuum:
			fmt.Printf("merged: %s on disk, with the freed pages kept inside the file for later writes\n", humanBytes(after))
		case after < before:
			fmt.Printf("compacted: %s → %s on disk (%s reclaimed)\n",
				humanBytes(before), humanBytes(after), humanBytes(before-after))
		default:
			// A database with little to merge can come out slightly
			// larger, because the merge writes its new segment before
			// the vacuum drops the old ones.
			fmt.Printf("compacted: %s → %s on disk\n", humanBytes(before), humanBytes(after))
		}
	}
	return nil
}

// compactDB merges the full-text indexes and, unless the caller opted
// out, rewrites the file without the pages that merge released. The
// caller holds the index lock.
func compactDB(ctx context.Context, db *store.DB, noVacuum, quiet bool) error {
	took, err := db.OptimizeFTS(ctx)
	if err != nil {
		return err
	}
	if !quiet {
		for i, d := range took {
			fmt.Printf("merged %s in %s\n", store.FTSTables[i], d.Truncate(time.Millisecond))
		}
	}

	// Checkpoint first: the merge's rewrites are still in the
	// write-ahead log, and VACUUM would otherwise copy a database that
	// does not yet include them back into the log a second time.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if noVacuum {
		return nil
	}

	// VACUUM writes a full second copy of the database before it
	// replaces the original, so it needs free space about the size of
	// the file. It rolls back if it runs out, leaving the original
	// intact.
	start := time.Now()
	if err := db.Vacuum(ctx); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}

	// Checkpoint again. In WAL mode the rewritten database lands in the
	// write-ahead log, so until the log is emptied the two files
	// together are larger than the database was before the vacuum, and
	// the space the vacuum reclaimed is still spoken for.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint after vacuum: %w", err)
	}
	if !quiet {
		fmt.Printf("vacuumed in %s\n", time.Since(start).Truncate(time.Millisecond))
	}
	return nil
}

// dbFileBytes is the database's size on disk, its write-ahead log
// included. A file it cannot stat counts as zero, because this only
// feeds a report.
func dbFileBytes(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal"} {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
