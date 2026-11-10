package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// reindexGenerations is how many times reindexSession writes the same
// transcript. Every pass but the last leaves text in the full-text
// indexes that no query can return, which is what OptimizeFTS drops.
const reindexGenerations = 5

const reindexPath = "/tmp/reindexed.jsonl"

// reindexSession writes one session's messages the way the indexer
// does on a reindex: delete what is stored for the file, then insert
// the whole transcript again.
func reindexSession(t *testing.T, db *DB, generation int) {
	t.Helper()
	if err := db.DeleteBySourcePath(reindexPath); err != nil {
		t.Fatalf("DeleteBySourcePath: %v", err)
	}
	msgs := make([]Message, 0, 300)
	for i := range 300 {
		// Distinct text per generation, so a generation the index still
		// holds after a merge shows up as size rather than hiding
		// inside terms the live copy shares with it.
		var body strings.Builder
		for w := range 40 {
			fmt.Fprintf(&body, "tok%x%x%x ", generation, i, w)
		}
		msgs = append(msgs, Message{
			Ordinal: i,
			Role:    "assistant",
			TS:      int64(i),
			Content: fmt.Sprintf("generation %d message %d: retryWebhookHandler configures authentication %s",
				generation, i, body.String()),
		})
	}
	seed(t, db, &Session{
		Agent: "claudecode", UID: "reindexed", Workspace: "repo",
		Title: "reindexed", StartedAt: 1, EndedAt: 2, SourcePath: reindexPath,
	}, msgs)
}

// ftsBytes is how much the named full-text index occupies in its
// storage table. It counts stale text and delete markers too, which is
// the point.
func ftsBytes(t *testing.T, db *DB, table string) int64 {
	t.Helper()
	var n sql.NullInt64
	q := fmt.Sprintf(`SELECT sum(length(block)) FROM %s_data`, table)
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("size of %s: %v", table, err)
	}
	return n.Int64
}

func countMatches(t *testing.T, db *DB, table, match string) int {
	t.Helper()
	var n int
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s MATCH ?`, table, table)
	if err := db.QueryRow(q, match).Scan(&n); err != nil {
		t.Fatalf("match %q in %s: %v", match, table, err)
	}
	return n
}

// TestOptimizeFTS_DropsTextLeftByAReindex is the regression test for a
// full reindex that left both indexes holding a copy of every message
// it had replaced. FTS5 answers a delete by appending a marker, not by
// erasing terms, so the old text stays on disk until a merge drops it.
func TestOptimizeFTS_DropsTextLeftByAReindex(t *testing.T) {
	db := openTestDB(t)
	// FTS5 merges small runs of segments on its own as they are
	// written, which at this scale would hide most of what a reindex
	// leaves behind. Turning that off reproduces the large database
	// this fixes, where the automatic merges never reach the segments
	// holding the replaced text.
	for _, table := range FTSTables {
		q := fmt.Sprintf(`INSERT INTO %s(%s, rank) VALUES('automerge', 0)`, table, table)
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("disable automerge on %s: %v", table, err)
		}
	}
	for g := range reindexGenerations {
		reindexSession(t, db, g)
	}

	before := map[string]int64{}
	var totalBefore int64
	for _, table := range FTSTables {
		before[table] = ftsBytes(t, db, table)
		if before[table] == 0 {
			t.Fatalf("%s is empty before the merge", table)
		}
		totalBefore += before[table]
	}

	if _, err := db.OptimizeFTS(t.Context()); err != nil {
		t.Fatalf("OptimizeFTS: %v", err)
	}

	// Every generation but the last is unreachable, so the merge has
	// to take storage out. How much depends on how FTS5 happened to
	// lay the segments out, and the bound is on the total of the two
	// because the porter index on a fixture this small can arrive
	// already merged; the trigram index, the one that grows on a real
	// corpus, does not.
	var totalAfter int64
	for _, table := range FTSTables {
		after := ftsBytes(t, db, table)
		if after > before[table] {
			t.Errorf("%s grew from %d to %d bytes over the merge", table, before[table], after)
		}
		totalAfter += after
	}
	if totalAfter >= totalBefore {
		t.Errorf("the full-text indexes hold %d bytes after the merge, want fewer than the %d they held before",
			totalAfter, totalBefore)
	}

	// Each index separately, because a bound on the two added together
	// passes over code that merges one and skips the other: the one it
	// merges frees more than enough bytes to cover the one it does not.
	// Merging an index that is already merged changes nothing, so each
	// one is merged again on its own here.
	for _, table := range FTSTables {
		size := ftsBytes(t, db, table)
		q := fmt.Sprintf(`INSERT INTO %s(%s) VALUES('optimize')`, table, table)
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("optimize %s: %v", table, err)
		}
		if again := ftsBytes(t, db, table); again != size {
			t.Errorf("merging %s changed it from %d to %d bytes; OptimizeFTS left it unmerged", table, size, again)
		}
	}

	// The merge must drop only what was deleted. The last generation's
	// text is still indexed, and the earlier ones are gone from the
	// index as well as from messages.
	last := fmt.Sprintf(`"generation %d"`, reindexGenerations-1)
	if n := countMatches(t, db, "messages_fts", last); n != 300 {
		t.Errorf("messages_fts matches %d rows for the live generation, want 300", n)
	}
	if n := countMatches(t, db, "messages_fts_tri", "retrywebhookhandler"); n != 300 {
		t.Errorf("messages_fts_tri matches %d rows for the live generation, want 300", n)
	}
	for g := range reindexGenerations - 1 {
		if n := countMatches(t, db, "messages_fts", fmt.Sprintf(`"generation %d"`, g)); n != 0 {
			t.Errorf("messages_fts still matches %d rows from replaced generation %d", n, g)
		}
	}
}

// TestOptimizeFTS_IsIdempotent guards the cost of running it on every
// `aii index --full`: a merged index must not grow when merged again.
func TestOptimizeFTS_IsIdempotent(t *testing.T) {
	db := openTestDB(t)
	for g := range reindexGenerations {
		reindexSession(t, db, g)
	}
	if _, err := db.OptimizeFTS(t.Context()); err != nil {
		t.Fatalf("first OptimizeFTS: %v", err)
	}
	first := ftsBytes(t, db, "messages_fts_tri")
	if _, err := db.OptimizeFTS(t.Context()); err != nil {
		t.Fatalf("second OptimizeFTS: %v", err)
	}
	if second := ftsBytes(t, db, "messages_fts_tri"); second > first {
		t.Errorf("messages_fts_tri grew from %d to %d on a second merge", first, second)
	}
}

// TestOptimizeFTS_ReportsOneDurationPerIndex keeps the report the CLI
// prints tied to FTSTables, which it indexes by position.
func TestOptimizeFTS_ReportsOneDurationPerIndex(t *testing.T) {
	db := openTestDB(t)
	reindexSession(t, db, 0)
	took, err := db.OptimizeFTS(t.Context())
	if err != nil {
		t.Fatalf("OptimizeFTS: %v", err)
	}
	if len(took) != len(FTSTables) {
		t.Fatalf("OptimizeFTS returned %d durations, want one per table (%d)", len(took), len(FTSTables))
	}
}

// TestVacuum_ReturnsFreedPagesToTheFile covers the second half of
// `aii compact`: a merge moves pages to the free list, and only a
// rewrite gives the space back to the filesystem.
func TestVacuum_ReturnsFreedPagesToTheFile(t *testing.T) {
	db := openTestDB(t)
	for g := range reindexGenerations {
		reindexSession(t, db, g)
	}
	if _, err := db.OptimizeFTS(t.Context()); err != nil {
		t.Fatalf("OptimizeFTS: %v", err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	pages := func() (count, free int64) {
		if err := db.QueryRow(`PRAGMA page_count`).Scan(&count); err != nil {
			t.Fatalf("page_count: %v", err)
		}
		if err := db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
			t.Fatalf("freelist_count: %v", err)
		}
		return count, free
	}

	beforeCount, beforeFree := pages()
	if beforeFree == 0 {
		t.Fatal("the merge freed no pages, so this test would prove nothing about the vacuum")
	}
	if err := db.Vacuum(t.Context()); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	afterCount, afterFree := pages()
	if afterFree != 0 {
		t.Errorf("free list holds %d pages after the vacuum, want 0", afterFree)
	}
	if afterCount >= beforeCount {
		t.Errorf("database is %d pages after the vacuum, want fewer than the %d before", afterCount, beforeCount)
	}
}
