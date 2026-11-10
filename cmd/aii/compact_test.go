package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericmason/aii/internal/store"
)

// disableAutomerge stops FTS5 from merging small runs of segments as
// they are written. Without it these fixtures are too small for the
// automatic merges to miss anything, and a test could not tell a run
// that merges from one that does not. A large database is the case
// where the automatic merges never catch up.
func disableAutomerge(t *testing.T, path string) {
	t.Helper()
	db, err := store.OpenForIndex(path)
	if err != nil {
		t.Fatalf("OpenForIndex: %v", err)
	}
	defer db.Close()
	for _, table := range store.FTSTables {
		q := fmt.Sprintf(`INSERT INTO %s(%s, rank) VALUES('automerge', 0)`, table, table)
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("disable automerge on %s: %v", table, err)
		}
	}
}

// ftsTotalBytes is how much both full-text indexes occupy, stale text
// and delete markers included.
func ftsTotalBytes(t *testing.T, path string) int64 {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	var total int64
	for _, table := range store.FTSTables {
		var n sql.NullInt64
		if err := db.QueryRow(fmt.Sprintf(`SELECT sum(length(block)) FROM %s_data`, table)).Scan(&n); err != nil {
			t.Fatalf("size of %s: %v", table, err)
		}
		total += n.Int64
	}
	return total
}

// writeTranscript puts a Claude Code transcript under a temporary HOME,
// large enough that a generation left behind in the index shows up as
// size. Each generation's text is distinct, so a replaced one is
// visible rather than hidden inside terms the live copy shares.
func writeTranscript(t *testing.T, home string, generation int) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	for i := range 200 {
		fmt.Fprintf(&b, `{"type":"user","sessionId":"s1","cwd":"/w","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":%q}}`+"\n",
			generatedText(generation, i))
	}
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}

// generatedText is one message's content. Its tokens carry the
// generation, so text a merge should have dropped is visible as size
// rather than hidden inside terms the live copy also holds.
func generatedText(generation, i int) string {
	var body strings.Builder
	fmt.Fprintf(&body, "generation %d message %d", generation, i)
	for w := range 30 {
		fmt.Fprintf(&body, " tok%x%x%x", generation, i, w)
	}
	return body.String()
}

// bloatIndex writes the same transcript over and over the way a
// reindex does — delete what is stored for the file, insert it all
// again — so the full-text indexes end up holding every replaced copy.
// It goes through the store rather than `aii index` because the
// sources skip a file whose size and modification time say nothing
// changed, and this needs the replacement, not the scan.
func bloatIndex(t *testing.T, path string, generations int) {
	t.Helper()
	db, err := store.OpenForIndex(path)
	if err != nil {
		t.Fatalf("OpenForIndex: %v", err)
	}
	defer db.Close()
	const src = "/tmp/bloat.jsonl"
	for g := range generations {
		if err := db.DeleteBySourcePath(src); err != nil {
			t.Fatalf("DeleteBySourcePath: %v", err)
		}
		id, err := db.UpsertSession(&store.Session{
			Agent: "claudecode", UID: "bloat", Workspace: "/w",
			Title: "bloat", StartedAt: 1, EndedAt: 2, SourcePath: src,
		})
		if err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
		msgs := make([]store.Message, 0, 200)
		for i := range 200 {
			msgs = append(msgs, store.Message{
				Ordinal: i, Role: "user", TS: int64(i),
				Content: generatedText(g, i),
			})
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := db.InsertMessages(tx, id, msgs); err != nil {
			tx.Rollback()
			t.Fatalf("InsertMessages: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

// TestIndexFull_LeavesTheFullTextIndexesMerged is the regression test
// for the bug: `aii index --full` deletes and reinserts every message,
// and FTS5 keeps the replaced text until a merge drops it, so two full
// runs used to leave the index holding both copies.
//
// The check is that a merge run straight afterwards has nothing left
// to do. Remove the merge from the index run and the second full pass
// leaves a generation behind, which that merge then takes out.
func TestIndexFull_LeavesTheFullTextIndexesMerged(t *testing.T) {
	dir := withTempDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(dir, "aii.db")
	disableAutomerge(t, dbPath)

	writeTranscript(t, home, 0)
	if err := cmdIndex(t.Context(), []string{"--full", "--source", "cc", "--quiet"}); err != nil {
		t.Fatalf("first full index: %v", err)
	}
	writeTranscript(t, home, 1)
	if err := cmdIndex(t.Context(), []string{"--full", "--source", "cc", "--quiet"}); err != nil {
		t.Fatalf("second full index: %v", err)
	}

	before := ftsTotalBytes(t, dbPath)
	if before == 0 {
		t.Fatal("the full-text indexes are empty, so this test would prove nothing")
	}
	db, err := store.OpenForIndex(dbPath)
	if err != nil {
		t.Fatalf("OpenForIndex: %v", err)
	}
	if _, err := db.OptimizeFTS(t.Context()); err != nil {
		db.Close()
		t.Fatalf("OptimizeFTS: %v", err)
	}
	db.Close()

	if after := ftsTotalBytes(t, dbPath); after != before {
		t.Errorf("merging after `aii index --full` changed the indexes from %d to %d bytes; the run left them unmerged",
			before, after)
	}
}

// TestIndexIncremental_DoesNotMerge keeps the merge off the five-minute
// scheduled run, which reindexes little and would pay minutes for it on
// a large database.
func TestIndexIncremental_DoesNotMerge(t *testing.T) {
	dir := withTempDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(dir, "aii.db")
	disableAutomerge(t, dbPath)
	bloatIndex(t, dbPath, 5)

	writeTranscript(t, home, 0)
	if err := cmdIndex(t.Context(), []string{"--source", "cc", "--quiet"}); err != nil {
		t.Fatalf("incremental index: %v", err)
	}

	before := ftsTotalBytes(t, dbPath)
	db, err := store.OpenForIndex(dbPath)
	if err != nil {
		t.Fatalf("OpenForIndex: %v", err)
	}
	if _, err := db.OptimizeFTS(t.Context()); err != nil {
		db.Close()
		t.Fatalf("OptimizeFTS: %v", err)
	}
	db.Close()

	if after := ftsTotalBytes(t, dbPath); after >= before {
		t.Errorf("the incremental run left nothing for a merge to drop (%d → %d bytes); it should not be merging", before, after)
	}
}

// TestCompact_MergesAndShrinksTheFile covers `aii compact` end to end
// on a database that reindexing has filled with replaced text.
func TestCompact_MergesAndShrinksTheFile(t *testing.T) {
	dir := withTempDataDir(t)
	dbPath := filepath.Join(dir, "aii.db")
	disableAutomerge(t, dbPath)
	bloatIndex(t, dbPath, 5)

	beforeFTS := ftsTotalBytes(t, dbPath)
	beforeFile := dbFileBytes(dbPath)
	if err := cmdCompact(t.Context(), []string{"--quiet"}); err != nil {
		t.Fatalf("cmdCompact: %v", err)
	}
	if after := ftsTotalBytes(t, dbPath); after >= beforeFTS {
		t.Errorf("full-text indexes hold %d bytes after compact, want fewer than %d", after, beforeFTS)
	}
	if after := dbFileBytes(dbPath); after >= beforeFile {
		t.Errorf("database file is %d bytes after compact, want smaller than %d", after, beforeFile)
	}
	if free := freelistPages(t, dbPath); free != 0 {
		t.Errorf("free list holds %d pages after compact, want 0", free)
	}

	// Compacting must not change what the index answers.
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n != 200 {
		t.Errorf("messages = %d after compact, want 200", n)
	}
	var hits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '"generation 4"'`).Scan(&hits); err != nil {
		t.Fatalf("match live generation: %v", err)
	}
	if hits != 200 {
		t.Errorf("messages_fts matches %d rows for the live generation after compact, want 200", hits)
	}
	var stale int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '"generation 0"'`).Scan(&stale); err != nil {
		t.Fatalf("match replaced generation: %v", err)
	}
	if stale != 0 {
		t.Errorf("messages_fts matches %d rows for a replaced generation after compact, want 0", stale)
	}
}

// TestCompact_NoVacuumKeepsTheFreedPagesInTheFile covers --no-vacuum:
// the merge still runs, and the pages it frees stay in the file for
// later writes to reuse instead of being handed back to the
// filesystem. Plain `aii compact` hands them back, so its free list
// ends up empty.
func TestCompact_NoVacuumKeepsTheFreedPagesInTheFile(t *testing.T) {
	dir := withTempDataDir(t)
	dbPath := filepath.Join(dir, "aii.db")
	disableAutomerge(t, dbPath)
	bloatIndex(t, dbPath, 5)

	beforeFTS := ftsTotalBytes(t, dbPath)
	if err := cmdCompact(t.Context(), []string{"--no-vacuum", "--quiet"}); err != nil {
		t.Fatalf("cmdCompact --no-vacuum: %v", err)
	}
	if after := ftsTotalBytes(t, dbPath); after >= beforeFTS {
		t.Errorf("full-text indexes hold %d bytes after compact --no-vacuum, want fewer than %d", after, beforeFTS)
	}
	if free := freelistPages(t, dbPath); free == 0 {
		t.Error("compact --no-vacuum left no free pages; it should not have rewritten the file")
	}
}

func freelistPages(t *testing.T, path string) int64 {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(`PRAGMA freelist_count`).Scan(&n); err != nil {
		t.Fatalf("freelist_count: %v", err)
	}
	return n
}

// TestCompact_RefusesWhileAnIndexerHoldsTheLock keeps both heavy writes
// behind the same lock the indexer takes. Running them against a live
// index run would cost it batches to SQLITE_BUSY.
func TestCompact_RefusesWhileAnIndexerHoldsTheLock(t *testing.T) {
	withTempDataDir(t)
	release := mustAcquire(t)
	defer release()

	err := cmdCompact(t.Context(), nil)
	if err == nil {
		t.Fatal("cmdCompact should fail while another process holds the index lock")
	}
	if !strings.Contains(err.Error(), "indexer is running") {
		t.Errorf("error = %q, want it to name the running indexer", err)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{5 * 1024 * 1024, "5.0 MiB"},
		{13673586688, "12.7 GiB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
