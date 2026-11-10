package claudecode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ericmason/aii/internal/indexer"
	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

// asstLine is one transcript line of an assistant response. Claude Code
// writes one such line per content block, repeating the ids and usage.
func asstLine(sess, msgID, reqID, ts string, in, out, cw, cr int64) string {
	return fmt.Sprintf(`{"type":"assistant","sessionId":%q,"requestId":%q,"cwd":"/w","timestamp":%q,"message":{"id":%q,"model":"claude-test-1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}}`,
		sess, reqID, ts, msgID, in, out, cw, cr) + "\n"
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runIndex(t *testing.T, db *store.DB, root string, full bool) {
	t.Helper()
	if _, err := indexer.Run(context.Background(), db, []source.Source{&Source{Root: root}}, indexer.Options{Full: full}); err != nil {
		t.Fatal(err)
	}
}

func sessionUsage(t *testing.T, db *store.DB) map[string]store.UsageRow {
	t.Helper()
	rows, err := db.UsageReport(store.UsageFilter{By: []string{"session"}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.UsageRow{}
	for _, r := range rows {
		r.Title = "" // compare token counts only
		out[r.SessionUID] = r
	}
	return out
}

func TestUsageDedupeSubagentsAndRetention(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	main := filepath.Join(root, "proj", "sess-a.jsonl")
	writeFile(t, main,
		userLine("sess-a", "hello")+
			// One response written as three lines; the first carries an
			// early output count, as Claude Code does while streaming.
			asstLine("sess-a", "msg_1", "req_1", "2026-01-02T10:00:00Z", 3, 1, 100, 0)+
			asstLine("sess-a", "msg_1", "req_1", "2026-01-02T10:00:01Z", 3, 40, 100, 0)+
			asstLine("sess-a", "msg_1", "req_1", "2026-01-02T10:00:01Z", 3, 40, 100, 0)+
			asstLine("sess-a", "msg_2", "req_2", "2026-01-02T10:01:00Z", 5, 20, 0, 100))
	// A subagent transcript carries the parent's sessionId.
	writeFile(t, filepath.Join(root, "proj", "sess-a", "subagents", "agent-x.jsonl"),
		asstLine("sess-a", "msg_3", "req_3", "2026-01-02T10:02:00Z", 7, 9, 50, 0))
	writeFile(t, filepath.Join(root, "proj", "sess-a", "subagents", "workflows", "wf_1", "agent-y.jsonl"),
		asstLine("sess-a", "msg_4", "req_4", "2026-01-02T10:03:00Z", 1, 1, 0, 0))
	// A resumed session copies earlier responses into its own file.
	writeFile(t, filepath.Join(root, "proj", "sess-b.jsonl"),
		asstLine("sess-b", "msg_2", "req_2", "2026-01-02T10:01:00Z", 5, 20, 0, 100)+
			asstLine("sess-b", "msg_5", "req_5", "2026-01-03T09:00:00Z", 2, 2, 0, 0))

	runIndex(t, db, root, false)

	got := sessionUsage(t, db)
	a := got["sess-a"]
	// msg_1 (3+40+100) + msg_2 (5+20+100) + msg_3 (7+9+50) + msg_4 (1+1)
	if a.Requests != 4 || a.Input != 16 || a.Output != 70 || a.CacheCreation != 150 || a.CacheRead != 100 {
		t.Fatalf("sess-a usage = %+v", a)
	}
	if b := got["sess-b"]; b.Requests != 1 || b.Total != 4 {
		t.Fatalf("sess-b usage = %+v, want only msg_5", b)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions with usage, want 2 (subagents roll into the parent)", len(got))
	}

	// The subagent's messages are not indexed as sessions of their own.
	if n, _ := db.CountSessions(store.SessionFilter{}); n != 2 {
		t.Fatalf("sessions = %d, want 2", n)
	}

	before := sessionUsage(t, db)

	// Re-indexing in full adds nothing.
	runIndex(t, db, root, true)
	if after := sessionUsage(t, db); after["sess-a"] != before["sess-a"] || after["sess-b"] != before["sess-b"] {
		t.Fatalf("full re-index changed usage: %+v -> %+v", before, after)
	}

	// A transcript that shrinks is re-indexed from scratch, and one that
	// disappears is never read again; recorded usage stays either way.
	writeFile(t, main, userLine("sess-a", "rewritten"))
	if err := os.RemoveAll(filepath.Join(root, "proj", "sess-a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "proj", "sess-b.jsonl")); err != nil {
		t.Fatal(err)
	}
	runIndex(t, db, root, false)
	runIndex(t, db, root, true)
	if after := sessionUsage(t, db); after["sess-a"] != before["sess-a"] || after["sess-b"] != before["sess-b"] {
		t.Fatalf("usage lost after transcripts shrank or vanished: %+v -> %+v", before, after)
	}
}

func TestUsageAppendedAcrossRuns(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	path := filepath.Join(root, "proj", "s.jsonl")
	first := asstLine("s", "msg_1", "req_1", "2026-01-02T10:00:00Z", 1, 1, 0, 0)
	writeFile(t, path, first)
	runIndex(t, db, root, false)

	// The next run reads only the appended bytes: a later copy of msg_1
	// with its final output count, and a new response.
	writeFile(t, path, first+
		asstLine("s", "msg_1", "req_1", "2026-01-02T10:00:02Z", 1, 30, 0, 0)+
		asstLine("s", "msg_2", "req_2", "2026-01-02T10:00:05Z", 2, 2, 0, 0))
	runIndex(t, db, root, false)

	s := sessionUsage(t, db)["s"]
	if s.Requests != 2 || s.Output != 32 || s.Input != 3 {
		t.Fatalf("usage = %+v, want 2 requests, output 32, input 3", s)
	}
}

// A response with no request id, copied into a resumed session's file
// under a new session id, counts once.
func TestUsageWithoutRequestIDCountsOnceAcrossResume(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	writeFile(t, filepath.Join(root, "proj", "old.jsonl"),
		asstLine("old", "msg_1", "", "2026-01-02T10:00:00Z", 1, 10, 0, 0))
	writeFile(t, filepath.Join(root, "proj", "new.jsonl"),
		asstLine("new", "msg_1", "", "2026-01-02T10:00:00Z", 1, 10, 0, 0)+
			asstLine("new", "msg_2", "", "2026-01-02T10:05:00Z", 2, 20, 0, 0))
	runIndex(t, db, root, false)

	rows, err := db.UsageReport(store.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 2 || rows[0].Total != 33 {
		t.Fatalf("usage = %+v, want 2 requests and 33 tokens", rows)
	}
}

func TestLineUsageAdvisorAndSynthetic(t *testing.T) {
	line := []byte(`{"type":"assistant","sessionId":"s","requestId":"r","timestamp":"2026-01-02T10:00:00Z","message":{"id":"m","model":"main-model","usage":{"input_tokens":1,"output_tokens":2,"speed":"fast","iterations":[{"type":"message","input_tokens":1,"output_tokens":2},{"type":"advisor_message","model":"advisor-model","input_tokens":10,"output_tokens":3}]}}}`)
	got := lineUsage(line, 0, "p")
	if len(got) != 2 {
		t.Fatalf("got %d usage rows, want 2", len(got))
	}
	if got[0].Model != "main-model-fast" || got[0].Key != "m|r" || got[0].Total() != 3 {
		t.Fatalf("main row = %+v", got[0])
	}
	if got[1].Model != "advisor-model" || got[1].Total() != 13 {
		t.Fatalf("advisor row = %+v", got[1])
	}

	synthetic := []byte(`{"type":"assistant","sessionId":"s","message":{"id":"m2","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}}`)
	if got := lineUsage(synthetic, 0, "p"); len(got) != 0 {
		t.Fatalf("zero-usage synthetic line recorded: %+v", got)
	}
}
