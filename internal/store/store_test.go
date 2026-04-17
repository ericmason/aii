package store

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"
)

// openTestDB opens a fresh DB under t.TempDir(). The modernc sqlite driver
// honors the ?mode=memory&cache=shared trick, but we use a file so the
// schema's WAL pragmas exercise the real open path.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seed inserts one session and its messages using the same API the indexer
// uses. It returns the session id so tests can assert against it.
func seed(t *testing.T, db *DB, s *Session, msgs []Message) int64 {
	t.Helper()
	id, err := db.UpsertSession(s)
	if err != nil {
		t.Fatalf("UpsertSession: %v", err)
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
	return id
}

func TestSearch_MatchesAndDedupes(t *testing.T) {
	db := openTestDB(t)
	sidA := seed(t, db, &Session{
		Agent: "claudecode", UID: "sess-a", Workspace: "repoA",
		Title: "session a", StartedAt: 100, EndedAt: 200,
		SourcePath: "/tmp/a.jsonl",
	}, []Message{
		{Ordinal: 1, Role: "user", TS: 100, Content: "How do I configure authentication?"},
		{Ordinal: 2, Role: "assistant", TS: 110, Content: "Use the webhookRetry handler for auth."},
		{Ordinal: 3, Role: "user", TS: 120, Content: "Totally unrelated content here."},
	})
	sidB := seed(t, db, &Session{
		Agent: "codex", UID: "sess-b", Workspace: "repoB",
		Title: "session b", StartedAt: 300, EndedAt: 400,
		SourcePath: "/tmp/b.jsonl",
	}, []Message{
		{Ordinal: 1, Role: "user", TS: 300, Content: "Nothing matches the target."},
	})

	// A natural-language query hits the porter index only.
	results, err := db.Search(Query{Text: "authentication"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	r := results[0]
	if r.SessionID != sidA {
		t.Errorf("want session %d, got %d", sidA, r.SessionID)
	}
	if r.Agent != "claudecode" || r.Workspace != "repoA" {
		t.Errorf("wrong session metadata: %+v", r)
	}
	if len(r.Excerpts) == 0 {
		t.Errorf("expected at least one excerpt")
	}

	// An identifier-substring hit goes through the trigram branch.
	results, err = db.Search(Query{Text: "webhookRetry"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].SessionID != sidA {
		t.Fatalf("trigram search should find session A, got %+v", results)
	}

	// Ensure the other session hasn't leaked into unrelated queries.
	_ = sidB
}

func TestSearch_FiltersByAgentWorkspaceRoleTime(t *testing.T) {
	db := openTestDB(t)
	sidA := seed(t, db, &Session{
		Agent: "claudecode", UID: "f-a", Workspace: "wsA",
		Title: "f-a", StartedAt: 100, EndedAt: 200,
		SourcePath: "/tmp/f-a",
	}, []Message{
		{Ordinal: 1, Role: "user", TS: 100, Content: "keyword alpha"},
		{Ordinal: 2, Role: "assistant", TS: 150, Content: "keyword reply"},
	})
	sidB := seed(t, db, &Session{
		Agent: "codex", UID: "f-b", Workspace: "wsB",
		Title: "f-b", StartedAt: 1000, EndedAt: 1100,
		SourcePath: "/tmp/f-b",
	}, []Message{
		{Ordinal: 1, Role: "user", TS: 1000, Content: "keyword beta"},
	})

	// Agent filter
	got, err := db.Search(Query{Text: "keyword", Agent: "codex"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != sidB {
		t.Errorf("agent filter: got %+v", sessionIDs(got))
	}

	// Workspace filter
	got, err = db.Search(Query{Text: "keyword", Workspace: "wsA"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != sidA {
		t.Errorf("workspace filter: got %+v", sessionIDs(got))
	}

	// Role filter — assistant only exists in session A
	got, err = db.Search(Query{Text: "keyword", Role: "assistant"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != sidA {
		t.Errorf("role filter: got %+v", sessionIDs(got))
	}

	// Time window — exclude session B (ts=1000) with UntilUnix=500
	got, err = db.Search(Query{Text: "keyword", UntilUnix: 500})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != sidA {
		t.Errorf("until filter: got %+v", sessionIDs(got))
	}

	// Time window — exclude session A (ts<=200) with SinceUnix=500
	got, err = db.Search(Query{Text: "keyword", SinceUnix: 500})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != sidB {
		t.Errorf("since filter: got %+v", sessionIDs(got))
	}

	// No filter — both sessions match.
	got, err = db.Search(Query{Text: "keyword"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := sessionIDs(got)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	want := []int64{sidA, sidB}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !equalInt64(ids, want) {
		t.Errorf("no filter: got %v, want %v", ids, want)
	}
}

func TestSearch_RespectsLimitAndOffset(t *testing.T) {
	db := openTestDB(t)
	var ids []int64
	for i := 0; i < 5; i++ {
		id := seed(t, db, &Session{
			Agent: "claudecode", UID: sprintf("lim-%d", i), Workspace: "w",
			Title: sprintf("s-%d", i), StartedAt: int64(100 + i), EndedAt: int64(200 + i),
			SourcePath: sprintf("/tmp/lim-%d", i),
		}, []Message{
			{Ordinal: 1, Role: "user", TS: int64(100 + i), Content: "uniquekeyword body"},
		})
		ids = append(ids, id)
	}

	got, err := db.Search(Query{Text: "uniquekeyword", Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("limit=2 returned %d results", len(got))
	}

	got, err = db.Search(Query{Text: "uniquekeyword", Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("offset=2 limit=2 returned %d results", len(got))
	}
}

func TestSearch_EmptyQueryReturnsNothing(t *testing.T) {
	db := openTestDB(t)
	seed(t, db, &Session{
		Agent: "claudecode", UID: "eq-a", Workspace: "w",
		Title: "eq", StartedAt: 100, SourcePath: "/tmp/eq",
	}, []Message{
		{Ordinal: 1, Role: "user", TS: 100, Content: "content that exists"},
	})
	got, err := db.Search(Query{Text: ""})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty query returned %d results, want 0", len(got))
	}
}

// helpers ---------------------------------------------------------------

func sessionIDs(rs []Result) []int64 {
	out := make([]int64, len(rs))
	for i, r := range rs {
		out[i] = r.SessionID
	}
	return out
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
