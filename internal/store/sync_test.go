package store

import (
	"path/filepath"
	"testing"
)

func TestOpenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aii.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SyncMetaSet("repo_id", "abc"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Second open must run the same DDL without complaint and keep data.
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	v, err := db2.SyncMetaGet("repo_id")
	if err != nil || v != "abc" {
		t.Fatalf("SyncMetaGet after reopen = %q, %v", v, err)
	}
}

func TestUpsertSessionFromSyncFreshInsert(t *testing.T) {
	db := openTestDB(t)
	id, err := db.UpsertSessionFromSync(&Session{
		Agent: "claude_code", UID: "uid-1", Workspace: "/w", Title: "t", StartedAt: 100, EndedAt: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := db.SessionByUID("claude_code", "uid-1")
	if err != nil || s == nil {
		t.Fatalf("SessionByUID: %v %v", s, err)
	}
	if s.ID != id {
		t.Fatalf("id mismatch: %d vs %d", s.ID, id)
	}
	if !IsSyntheticSourcePath(s.SourcePath) {
		t.Fatalf("fresh sync insert source_path = %q, want synthetic", s.SourcePath)
	}
}

func TestUpsertSessionFromSyncNeverClobbersRealPath(t *testing.T) {
	db := openTestDB(t)
	// A locally indexed session with a real transcript path.
	_, err := db.UpsertSession(&Session{
		Agent: "claude_code", UID: "uid-1", Workspace: "/local/ws", Title: "local title",
		StartedAt: 150, EndedAt: 200, SourcePath: "/home/u/.claude/projects/x/uid-1.jsonl",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Pull merges the same session from a peer.
	if _, err := db.UpsertSessionFromSync(&Session{
		Agent: "claude_code", UID: "uid-1", Workspace: "/peer/ws", Title: "peer title",
		Summary: "peer summary", StartedAt: 100, EndedAt: 300,
	}); err != nil {
		t.Fatal(err)
	}
	s, _ := db.SessionByUID("claude_code", "uid-1")
	if s.SourcePath != "/home/u/.claude/projects/x/uid-1.jsonl" {
		t.Fatalf("real source_path clobbered: %q", s.SourcePath)
	}
	if s.Workspace != "/local/ws" || s.Title != "local title" {
		t.Fatalf("local metadata lost: ws=%q title=%q", s.Workspace, s.Title)
	}
	if s.Summary != "peer summary" {
		t.Fatalf("remote should fill blank summary, got %q", s.Summary)
	}
	if s.StartedAt != 100 || s.EndedAt != 300 {
		t.Fatalf("time range not widened: %d..%d", s.StartedAt, s.EndedAt)
	}
}

func TestIndexerUpsertReplacesSyntheticPath(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.UpsertSessionFromSync(&Session{Agent: "claude_code", UID: "uid-1", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	// The real transcript shows up locally; the indexer's upsert must win.
	if _, err := db.UpsertSession(&Session{
		Agent: "claude_code", UID: "uid-1", SourcePath: "/real/path.jsonl",
	}); err != nil {
		t.Fatal(err)
	}
	s, _ := db.SessionByUID("claude_code", "uid-1")
	if s.SourcePath != "/real/path.jsonl" {
		t.Fatalf("synthetic path not replaced by real one: %q", s.SourcePath)
	}
}

func TestReplaceSessionMessagesFTSIntegrity(t *testing.T) {
	db := openTestDB(t)
	id, err := db.UpsertSession(&Session{Agent: "claude_code", UID: "uid-1", SourcePath: "/p"})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := db.Begin()
	if err := db.InsertMessages(tx, id, []Message{
		{Ordinal: 0, Role: "user", Content: "oldword alpha"},
		{Ordinal: 1, Role: "assistant", Content: "oldword beta"},
	}); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	tx, _ = db.Begin()
	if err := db.ReplaceSessionMessages(tx, id, []Message{
		{Ordinal: 0, Role: "user", Content: "newword gamma"},
	}); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	for query, want := range map[string]int{"oldword": 0, "newword": 1} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("FTS match %q = %d rows, want %d", query, n, want)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts_tri WHERE messages_fts_tri MATCH ?`, query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("trigram FTS match %q = %d rows, want %d", query, n, want)
		}
	}
}

func TestSyncStateRoundtrip(t *testing.T) {
	db := openTestDB(t)
	if s, err := db.GetSyncState("claude_code", "uid-1"); err != nil || s != nil {
		t.Fatalf("GetSyncState empty = %v, %v", s, err)
	}
	in := SyncState{Agent: "claude_code", UID: "uid-1", Epoch: 3, ChainCount: 42, ChainHash: []byte{1, 2, 3}, Excluded: true}
	if err := db.SaveSyncState(in); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetSyncState("claude_code", "uid-1")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Epoch != 3 || got.ChainCount != 42 || !got.Excluded || len(got.ChainHash) != 3 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	all, err := db.ListSyncStates()
	if err != nil || len(all) != 1 {
		t.Fatalf("ListSyncStates = %v, %v", all, err)
	}
}
