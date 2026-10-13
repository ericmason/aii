package gemini

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ericmason/aii/internal/indexer"
	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

// Fixtures follow the record shapes in Gemini CLI's
// packages/core/src/services/chatRecordingService.ts. All content is
// synthetic.
const sessionID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"

var jsonlLines = []string{
	`{"sessionId":"` + sessionID + `","projectHash":"demo-app","startTime":"2026-04-02T09:00:00.000Z","lastUpdated":"2026-04-02T09:00:00.000Z","kind":"main"}`,
	`{"id":"u1","timestamp":"2026-04-02T09:00:01.000Z","type":"user","content":[{"text":"Summarize README.md\n--- Content from referenced files ---\n# Demo"}],"displayContent":[{"text":"Summarize @README.md"}]}`,
	`{"id":"i1","timestamp":"2026-04-02T09:00:01.500Z","type":"info","content":"Loaded 1 file"}`,
	`{"id":"g1","timestamp":"2026-04-02T09:00:02.000Z","type":"gemini","content":"","thoughts":[{"subject":"Planning","description":"Read the file first.","timestamp":"2026-04-02T09:00:02.000Z"}],"toolCalls":[{"id":"t1","name":"read_file","args":{"path":"README.md"},"status":"executing","timestamp":"2026-04-02T09:00:02.000Z"}]}`,
	`{"$patch":{"id":"g1","toolCalls":[{"id":"t1","result":[{"functionResponse":{"id":"t1","name":"read_file","response":{"output":"# Demo\nA sample project."}}}]}]}}`,
	`{"id":"g1","timestamp":"2026-04-02T09:00:02.000Z","type":"gemini","content":[{"text":"thinking aloud","thought":true},{"text":"It describes a sample project."}],"thoughts":[{"subject":"Planning","description":"Read the file first.","timestamp":"2026-04-02T09:00:02.000Z"}],"toolCalls":[{"id":"t1","name":"read_file","args":{"path":"README.md"},"status":"success","result":[{"functionResponse":{"id":"t1","name":"read_file","response":{"output":"# Demo\nA sample project."}}}],"timestamp":"2026-04-02T09:00:02.000Z"}]}`,
	`{"$set":{"lastUpdated":"2026-04-02T09:00:03.000Z","summary":"Summarize the README\nsecond line"}}`,
}

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func runIndex(t *testing.T, db *store.DB, src source.Source, full bool) {
	t.Helper()
	if _, err := indexer.Run(context.Background(), db, []source.Source{src}, indexer.Options{Full: full}); err != nil {
		t.Fatalf("indexer.Run: %v", err)
	}
}

func writeFile(t *testing.T, path, content string, tick int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2026, 4, 2, 12, 0, tick, 0, time.UTC)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func rows(t *testing.T, db *store.DB, uid string) (*store.Session, []store.Row) {
	t.Helper()
	s, err := db.SessionByUID(agentName, uid)
	if err != nil || s == nil {
		t.Fatalf("SessionByUID = %v, %v", s, err)
	}
	r, err := db.SessionMessages(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

type want struct{ role, content string }

func check(t *testing.T, got []store.Row, w []want) {
	t.Helper()
	if len(got) != len(w) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(w), got)
	}
	for i := range w {
		if got[i].Ordinal != i || got[i].Role != w[i].role || got[i].Content != w[i].content {
			t.Errorf("row %d = {%d %q %q}, want {%d %q %q}", i, got[i].Ordinal, got[i].Role, got[i].Content, i, w[i].role, w[i].content)
		}
	}
}

func TestJSONLReplay(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".gemini", "tmp")
	path := filepath.Join(root, "demo-app", "chats", "session-2026-04-02T09-00-7c9e6679.jsonl")
	writeFile(t, filepath.Join(root, "demo-app", ".project_root"), "/work/demo-app\n", 0)
	writeFile(t, path, strings.Join(jsonlLines, "\n")+"\n", 1)

	db := openDB(t)
	src := &Source{Roots: []string{root}}
	runIndex(t, db, src, false)
	s, got := rows(t, db, sessionID)
	if s.Workspace != "/work/demo-app" || s.Title != "Summarize the README" {
		t.Errorf("session = %+v", s)
	}
	if s.EndedAt != time.Date(2026, 4, 2, 9, 0, 3, 0, time.UTC).Unix() {
		t.Errorf("ended_at = %d", s.EndedAt)
	}
	first := []want{
		{"user", "Summarize @README.md"},
		{"thinking", "Planning: Read the file first."},
		{"assistant", "It describes a sample project."},
		{"tool", `[tool_call name=read_file] {"path":"README.md"}`},
		{"tool", "[tool_result name=read_file] # Demo\nA sample project."},
	}
	check(t, got, first)

	runIndex(t, db, src, false)
	_, got = rows(t, db, sessionID)
	check(t, got, first)

	// Rewind to the gemini reply, then continue with a new turn.
	more := append(append([]string{}, jsonlLines...),
		`{"$rewindTo":"g1"}`,
		`{"id":"u2","timestamp":"2026-04-02T09:01:00.000Z","type":"user","content":"Try again, shorter"}`,
		`{"id":"g2","timestamp":"2026-04-02T09:01:01.000Z","type":"gemini","content":"A sample.","toolCalls":[{"id":"t2","name":"run_shell_command","args":{"command":"false"},"status":"error","result":[{"functionResponse":{"id":"t2","name":"run_shell_command","response":{"error":"exit 1"}}}]}]}`,
	)
	writeFile(t, path, strings.Join(more, "\n")+"\n", 2)
	runIndex(t, db, src, false)
	runIndex(t, db, src, false)
	_, got = rows(t, db, sessionID)
	check(t, got, []want{
		{"user", "Summarize @README.md"},
		{"user", "Try again, shorter"},
		{"assistant", "A sample."},
		{"tool", `[tool_call name=run_shell_command] {"command":"false"}`},
		{"tool", "[tool_result error name=run_shell_command] exit 1"},
	})

	runIndex(t, db, src, true)
	if _, got = rows(t, db, sessionID); len(got) != 5 {
		t.Fatalf("full reindex rows = %d, want 5", len(got))
	}
}

func TestSetMessagesAndRemove(t *testing.T) {
	c := parseConversation([]byte(strings.Join([]string{
		`{"sessionId":"s1","projectHash":"p"}`,
		`{"id":"a","type":"user","content":"one"}`,
		`{"id":"b","type":"user","content":"two"}`,
		`{"id":"c","type":"user","content":"three"}`,
		`{"$patch":{"removeIds":["b"],"orderIds":["a"]}}`,
	}, "\n")))
	var ids []string
	for _, m := range c.Messages {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "c,a" {
		t.Errorf("after patch ids = %v, want c,a", ids)
	}

	c = parseConversation([]byte(`{"sessionId":"s1","projectHash":"p"}` + "\n" +
		`{"id":"a","type":"user","content":"one"}` + "\n" +
		`{"$set":{"messages":[{"id":"z","type":"user","content":"replaced"}]}}` + "\n"))
	if len(c.Messages) != 1 || c.Messages[0].ID != "z" {
		t.Errorf("after $set messages = %+v", c.Messages)
	}

	c = parseConversation([]byte(`{"sessionId":"s1","projectHash":"p"}` + "\n" +
		`{"id":"a","type":"user","content":"one"}` + "\n" +
		`{"$rewindTo":"missing"}` + "\n"))
	if len(c.Messages) != 0 {
		t.Errorf("rewind to unknown id kept %d messages", len(c.Messages))
	}
}

func TestLegacyJSON(t *testing.T) {
	gemHome := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", gemHome)
	root := filepath.Join(gemHome, ".gemini", "tmp")
	writeFile(t, filepath.Join(gemHome, ".gemini", "projects.json"), `{"projects":{"/work/legacy":"legacy-app"}}`, 0)
	legacy := `{
  "sessionId": "11111111-2222-4333-8444-555555555555",
  "projectHash": "legacy-app",
  "startTime": "2026-01-05T08:00:00.000Z",
  "lastUpdated": "2026-01-05T08:05:00.000Z",
  "messages": [
    {"id": "m1", "timestamp": "2026-01-05T08:00:01.000Z", "type": "user", "content": "Hello there"},
    {"id": "m2", "timestamp": "2026-01-05T08:00:02.000Z", "type": "gemini", "content": "Hi."},
    {"id": "m3", "timestamp": "2026-01-05T08:00:03.000Z", "type": "error", "content": "quota"}
  ]
}`
	writeFile(t, filepath.Join(root, "legacy-app", "chats", "session-2026-01-05T08-00-11111111.json"), legacy, 1)
	// A .json file with a .jsonl sibling is superseded and skipped.
	writeFile(t, filepath.Join(root, "legacy-app", "chats", "session-old.json"), strings.Replace(legacy, "11111111-2222", "99999999-2222", 1), 1)
	writeFile(t, filepath.Join(root, "legacy-app", "chats", "session-old.jsonl"), `{"sessionId":"99999999-2222-4333-8444-555555555555","projectHash":"legacy-app"}`+"\n"+`{"id":"x","type":"user","content":"resumed"}`+"\n", 1)

	db := openDB(t)
	runIndex(t, db, New(), false)
	s, got := rows(t, db, "11111111-2222-4333-8444-555555555555")
	if s.Workspace != "/work/legacy" || s.Title != "Hello there" {
		t.Errorf("session = %+v", s)
	}
	check(t, got, []want{{"user", "Hello there"}, {"assistant", "Hi."}})
	_, got = rows(t, db, "99999999-2222-4333-8444-555555555555")
	check(t, got, []want{{"user", "resumed"}})
}

func TestDefaultRootsEnv(t *testing.T) {
	t.Setenv("GEMINI_CLI_HOME", "/tmp/gem")
	r := DefaultRoots()
	if len(r) != 2 || r[0] != filepath.Join("/tmp/gem", ".gemini", "tmp") {
		t.Errorf("DefaultRoots = %v", r)
	}
}
