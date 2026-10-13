package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
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

func TestSearch_BooleanOperators(t *testing.T) {
	db := openTestDB(t)
	sidA := seed(t, db, &Session{
		Agent: "claudecode", UID: "sess-a", Workspace: "repoA",
		Title: "session a", StartedAt: 100, EndedAt: 200, SourcePath: "/tmp/a.jsonl",
	}, []Message{{Ordinal: 1, Role: "user", TS: 100, Content: "download the archive"}})
	sidB := seed(t, db, &Session{
		Agent: "claudecode", UID: "sess-b", Workspace: "repoB",
		Title: "session b", StartedAt: 300, EndedAt: 400, SourcePath: "/tmp/b.jsonl",
	}, []Message{{Ordinal: 1, Role: "user", TS: 300, Content: "extract into scratch"}})
	sidC := seed(t, db, &Session{
		Agent: "claudecode", UID: "sess-c", Workspace: "repoC",
		Title: "session c", StartedAt: 500, EndedAt: 600, SourcePath: "/tmp/c.jsonl",
	}, []Message{{Ordinal: 1, Role: "user", TS: 500, Content: "download ab archive"}})

	sessions := func(q string) map[int64]bool {
		t.Helper()
		results, err := db.Search(Query{Text: q})
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		got := map[int64]bool{}
		for _, r := range results {
			got[r.SessionID] = true
		}
		return got
	}

	if got := sessions("download OR extract OR scratch OR app"); !got[sidA] || !got[sidB] {
		t.Errorf("OR query should match both sessions, got %v", got)
	}
	if got := sessions("extract NOT download"); got[sidA] || !got[sidB] {
		t.Errorf("NOT query should match only session B, got %v", got)
	}
	if got := sessions("OR"); len(got) != 0 {
		t.Errorf("bare operator should match nothing, got %v", got)
	}
	// A short excluded term must not let the trigram branch bring it back.
	if got := sessions("download NOT ab"); !got[sidA] || got[sidC] {
		t.Errorf("download NOT ab should match A but not C, got %v", got)
	}
	if got := sessions("NOT download"); len(got) != 0 {
		t.Errorf("leading NOT should match nothing, got %v", got)
	}
}

// A message matched by both indexes counts once, and every excerpt carries
// a highlighted snippet from the index that scored it.
func TestSearch_SnippetsAndMatchCount(t *testing.T) {
	db := openTestDB(t)
	sid := seed(t, db, &Session{
		Agent: "claudecode", UID: "sess-snip", Workspace: "repo",
		Title: "snippets", StartedAt: 100, EndedAt: 200,
		SourcePath: "/tmp/snip.jsonl",
	}, []Message{
		// porter (auth* -> authentication) and trigram ("auth") both match
		{Ordinal: 1, Role: "user", TS: 100, Content: "The authentication flow is broken."},
		// trigram only: porter sees the single token "oauthtoken"
		{Ordinal: 2, Role: "assistant", TS: 110, Content: "Refresh the OAuthToken first."},
		{Ordinal: 3, Role: "user", TS: 120, Content: "Unrelated text."},
	})
	// Many other sessions so the snippet pass has to pick its rows out of
	// a wider id range.
	for i := 0; i < 5; i++ {
		seed(t, db, &Session{
			Agent: "codex", UID: fmt.Sprintf("other-%d", i), Workspace: "repo",
			Title: "other", StartedAt: int64(300 + i), EndedAt: int64(400 + i),
			SourcePath: fmt.Sprintf("/tmp/other-%d.jsonl", i),
		}, []Message{
			{Ordinal: 1, Role: "user", TS: int64(300 + i), Content: "auth handler mentioned once, with a lot of other words around it"},
		})
	}

	results, err := db.Search(Query{Text: "auth", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var got *Result
	for i := range results {
		if results[i].SessionID == sid {
			got = &results[i]
		}
		for _, ex := range results[i].Excerpts {
			if !strings.Contains(strings.ToLower(ex.Snippet), "«auth") {
				t.Errorf("session %d ordinal %d: snippet %q lacks a highlighted match", results[i].SessionID, ex.Ordinal, ex.Snippet)
			}
		}
	}
	if got == nil {
		t.Fatalf("session %d missing from %+v", sid, results)
	}
	if got.MatchCount != 2 {
		t.Errorf("match_count: want 2 (one per message), got %d", got.MatchCount)
	}
	if len(got.Excerpts) != 2 {
		t.Fatalf("want 2 excerpts, got %+v", got.Excerpts)
	}
	ordinals := map[int]bool{}
	for _, ex := range got.Excerpts {
		ordinals[ex.Ordinal] = true
	}
	if !ordinals[1] || !ordinals[2] {
		t.Errorf("want excerpts for ordinals 1 and 2, got %+v", got.Excerpts)
	}
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

func TestLatestSessionsByWorkspace(t *testing.T) {
	db := openTestDB(t)
	seed(t, db, &Session{
		Agent: "claudecode", UID: "old-a", Workspace: "/ws/foo",
		StartedAt: 100, EndedAt: 200, SourcePath: "/tmp/old-a.jsonl",
	}, nil)
	seed(t, db, &Session{
		Agent: "claudecode", UID: "new-a", Workspace: "/ws/foo",
		StartedAt: 1000, EndedAt: 1100, SourcePath: "/tmp/new-a.jsonl",
	}, nil)
	seed(t, db, &Session{
		Agent: "claudecode", UID: "other-ws", Workspace: "/ws/bar",
		StartedAt: 2000, EndedAt: 2100, SourcePath: "/tmp/other.jsonl",
	}, nil)

	got, err := db.LatestSessionsByWorkspace("/ws/foo", 5)
	if err != nil {
		t.Fatalf("LatestSessionsByWorkspace: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (only /ws/foo)", len(got))
	}
	if got[0].UID != "new-a" || got[1].UID != "old-a" {
		t.Errorf("got order %s,%s — want newest first (new-a,old-a)", got[0].UID, got[1].UID)
	}

	got, err = db.LatestSessionsByWorkspace("/ws/foo", 1)
	if err != nil {
		t.Fatalf("LatestSessionsByWorkspace limit=1: %v", err)
	}
	if len(got) != 1 || got[0].UID != "new-a" {
		t.Errorf("limit=1: got %+v, want [new-a]", got)
	}

	got, err = db.LatestSessionsByWorkspace("/ws/nonexistent", 5)
	if err != nil {
		t.Fatalf("LatestSessionsByWorkspace empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nonexistent workspace returned %d sessions, want 0", len(got))
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

// Open must not fail while another process holds the write lock, for
// example while it builds the search index on a large database, and
// Search must work until the index exists.
func TestOpen_SearchIndexWhileDatabaseLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aii.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sid := seed(t, db, &Session{
		Agent: "claudecode", UID: "sess-lock", Workspace: "repo",
		Title: "lock", StartedAt: 100, EndedAt: 200, SourcePath: "/tmp/lock.jsonl",
	}, []Message{
		{Ordinal: 1, Role: "user", TS: 100, Content: "authentication is broken"},
		{Ordinal: 2, Role: "assistant", TS: 110, Content: "Refresh the OAuthToken first."},
	})
	want, err := db.Search(Query{Text: "auth"})
	if err != nil {
		t.Fatalf("Search with index: %v", err)
	}
	if _, err := db.Exec(`DROP INDEX ` + searchIndexName); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	db.Close()

	// A second connection holds the write lock, as another process would.
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open locker: %v", err)
	}
	defer other.Close()
	ctx := context.Background()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatalf("locker conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open while locked: %v", err)
	}
	defer db2.Close()
	if ok, err := indexExists(db2, searchIndexName); err != nil || ok {
		t.Fatalf("index should still be missing: exists=%v err=%v", ok, err)
	}

	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}

	got, err := db2.Search(Query{Text: "auth"})
	if err != nil {
		t.Fatalf("Search without index: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != sid {
		t.Fatalf("want session %d, got %+v", sid, got)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("results differ without the index:\n got %+v\nwant %+v", got, want)
	}

	// A later Open builds the index, and the earlier handle starts using it.
	db3, err := Open(path)
	if err != nil {
		t.Fatalf("Open after unlock: %v", err)
	}
	db3.Close()
	if ok, err := indexExists(db2, searchIndexName); err != nil || !ok {
		t.Fatalf("index should exist after unlocked Open: exists=%v err=%v", ok, err)
	}
	got, err = db2.Search(Query{Text: "auth"})
	if err != nil {
		t.Fatalf("Search after index build: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("results differ after index build:\n got %+v\nwant %+v", got, want)
	}
	if !db2.hasSearchIndex.Load() {
		t.Errorf("Search should have noticed the new index")
	}
}

// Snippets come out the same when the page's ids span several snippet
// queries.
func TestSearch_SnippetsAcrossBatches(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 6; i++ {
		seed(t, db, &Session{
			Agent: "codex", UID: fmt.Sprintf("batch-%d", i), Workspace: "repo",
			Title: "batch", StartedAt: int64(100 + i), EndedAt: int64(200 + i),
			SourcePath: fmt.Sprintf("/tmp/batch-%d.jsonl", i),
		}, []Message{
			{Ordinal: 1, Role: "user", TS: int64(100 + i), Content: "authentication fails on login"},
			{Ordinal: 2, Role: "assistant", TS: int64(101 + i), Content: "check the OAuthToken expiry"},
			{Ordinal: 3, Role: "user", TS: int64(102 + i), Content: fmt.Sprintf("auth retry %d worked", i)},
		})
	}

	want, err := db.Search(Query{Text: "auth", Limit: 50})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	excerpts := 0
	for _, r := range want {
		excerpts += len(r.Excerpts)
	}
	old := snippetBatchSize
	snippetBatchSize = 2
	t.Cleanup(func() { snippetBatchSize = old })
	if excerpts <= snippetBatchSize {
		t.Fatalf("need more than %d excerpts to force several batches, got %d", snippetBatchSize, excerpts)
	}

	got, err := db.Search(Query{Text: "auth", Limit: 50})
	if err != nil {
		t.Fatalf("Search with small batches: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("results differ with small batches:\n got %+v\nwant %+v", got, want)
	}
	for _, r := range got {
		for _, ex := range r.Excerpts {
			if !strings.Contains(ex.Snippet, "«") {
				t.Errorf("session %d ordinal %d: snippet %q lacks a highlighted match", r.SessionID, ex.Ordinal, ex.Snippet)
			}
		}
	}
}
