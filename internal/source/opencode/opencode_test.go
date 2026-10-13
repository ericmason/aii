package opencode

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericmason/aii/internal/indexer"
	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

// The V1 DDL matches opencode 1.4.x. The V2 DDL keeps the columns aii
// reads from session_v2 and session_message. All rows are synthetic.
const v1DDL = "CREATE TABLE `session` (" +
	"`id` text PRIMARY KEY, `project_id` text NOT NULL, `parent_id` text, `slug` text NOT NULL," +
	"`directory` text NOT NULL, `title` text NOT NULL, `version` text NOT NULL, `share_url` text," +
	"`summary_additions` integer, `summary_deletions` integer, `summary_files` integer, `summary_diffs` text," +
	"`revert` text, `permission` text, `time_created` integer NOT NULL, `time_updated` integer NOT NULL," +
	"`time_compacting` integer, `time_archived` integer, `workspace_id` text);" +
	"CREATE TABLE `message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `time_created` integer NOT NULL," +
	"`time_updated` integer NOT NULL, `data` text NOT NULL);" +
	"CREATE TABLE `part` (`id` text PRIMARY KEY, `message_id` text NOT NULL, `session_id` text NOT NULL," +
	"`time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL);"

const v2DDL = "CREATE TABLE `session_v2` (`id` text PRIMARY KEY, `project_id` text, `directory` text NOT NULL," +
	"`title` text, `time_created` integer NOT NULL, `time_updated` integer NOT NULL);" +
	"CREATE TABLE `session_message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `type` text NOT NULL," +
	"`seq` integer NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL);"

const sid = "ses_0f1e2d3c4b5aAbCdEfGhIjKlMn"

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func fixture(t *testing.T, ddl string) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	w, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	if _, err := w.Exec(ddl); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	return path, w
}

func exec(t *testing.T, w *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := w.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func runIndex(t *testing.T, db *store.DB, src source.Source, full bool) {
	t.Helper()
	if _, err := indexer.Run(context.Background(), db, []source.Source{src}, indexer.Options{Full: full}); err != nil {
		t.Fatalf("indexer.Run: %v", err)
	}
}

func rows(t *testing.T, db *store.DB) (*store.Session, []store.Row) {
	t.Helper()
	s, err := db.SessionByUID(agentName, SessionUID(sid))
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

func TestV1(t *testing.T) {
	path, w := fixture(t, v1DDL)
	exec(t, w, "INSERT INTO session VALUES (?, 'prj_1', NULL, 'calm-otter', '/work/demo-app', 'Fix the parser', '1.4.2', NULL, NULL, NULL, NULL, NULL, NULL, NULL, 1772359200000, 1772359260000, NULL, NULL, NULL)", sid)
	exec(t, w, "INSERT INTO message VALUES ('msg_a', ?, 1772359201000, 1772359201000, '{\"role\":\"user\"}')", sid)
	exec(t, w, "INSERT INTO part VALUES ('prt_a1', 'msg_a', ?, 1772359201000, 1772359201000, '{\"type\":\"text\",\"text\":\"Why does the parser crash?\"}')", sid)
	exec(t, w, "INSERT INTO part VALUES ('prt_a2', 'msg_a', ?, 1772359201000, 1772359201000, '{\"type\":\"text\",\"text\":\"<file>injected</file>\",\"synthetic\":true}')", sid)
	exec(t, w, "INSERT INTO message VALUES ('msg_b', ?, 1772359202000, 1772359210000, '{\"role\":\"assistant\"}')", sid)
	exec(t, w, "INSERT INTO part VALUES ('prt_b0', 'msg_b', ?, 1772359202000, 1772359202000, '{\"type\":\"step-start\"}')", sid)
	exec(t, w, "INSERT INTO part VALUES ('prt_b1', 'msg_b', ?, 1772359202000, 1772359202000, '{\"type\":\"reasoning\",\"text\":\"Check the input handling.\"}')", sid)
	exec(t, w, "INSERT INTO part VALUES ('prt_b2', 'msg_b', ?, 1772359202000, 1772359202000, '{\"type\":\"tool\",\"tool\":\"read\",\"callID\":\"c1\",\"state\":{\"status\":\"completed\",\"input\":{\"filePath\":\"parser.go\"},\"output\":\"package parser\"}}')", sid)
	exec(t, w, "INSERT INTO part VALUES ('prt_b3', 'msg_b', ?, 1772359202000, 1772359202000, '{\"type\":\"tool\",\"tool\":\"bash\",\"callID\":\"c2\",\"state\":{\"status\":\"error\",\"input\":{\"command\":\"make\"},\"error\":\"exit 2\"}}')", sid)
	exec(t, w, "INSERT INTO part VALUES ('prt_b4', 'msg_b', ?, 1772359202000, 1772359202000, '{\"type\":\"text\",\"text\":\"The nil check is missing.\"}')", sid)
	exec(t, w, "INSERT INTO message VALUES ('msg_c', ?, 1772359220000, 1772359220000, '{\"role\":\"assistant\",\"summary\":true}')", sid)
	exec(t, w, "INSERT INTO part VALUES ('prt_c1', 'msg_c', ?, 1772359220000, 1772359220000, '{\"type\":\"text\",\"text\":\"Investigated a parser crash.\"}')", sid)

	db := openDB(t)
	src := &Source{DBPath: path}
	runIndex(t, db, src, false)
	s, got := rows(t, db)
	if s.Title != "Fix the parser" || s.Workspace != "/work/demo-app" || s.Summary != "Investigated a parser crash." {
		t.Errorf("session = %+v", s)
	}
	if s.StartedAt != 1772359200 || s.EndedAt != 1772359260 {
		t.Errorf("times = %d..%d", s.StartedAt, s.EndedAt)
	}
	check(t, got, []want{
		{"user", "Why does the parser crash?"},
		{"thinking", "Check the input handling."},
		{"tool", `[tool_call name=read] {"filePath":"parser.go"}`},
		{"tool", "[tool_result name=read] package parser"},
		{"tool", `[tool_call name=bash] {"command":"make"}`},
		{"tool", "[tool_result error name=bash] exit 2"},
		{"assistant", "The nil check is missing."},
	})

	// Unchanged database: no rows added or replaced.
	runIndex(t, db, src, false)
	_, got = rows(t, db)
	if len(got) != 7 {
		t.Fatalf("rerun rows = %d, want 7", len(got))
	}

	// A streamed part update plus a new turn re-reads the session once.
	exec(t, w, "UPDATE part SET data = '{\"type\":\"text\",\"text\":\"The nil check is missing in Parse.\"}', time_updated = 1772359230000 WHERE id = 'prt_b4'")
	exec(t, w, "INSERT INTO message VALUES ('msg_d', ?, 1772359240000, 1772359240000, '{\"role\":\"user\"}')", sid)
	exec(t, w, "INSERT INTO part VALUES ('prt_d1', 'msg_d', ?, 1772359240000, 1772359240000, '{\"type\":\"text\",\"text\":\"Add a test too.\"}')", sid)
	runIndex(t, db, src, false)
	runIndex(t, db, src, false)
	_, got = rows(t, db)
	if len(got) != 8 {
		t.Fatalf("rows after update = %d, want 8", len(got))
	}
	if got[6].Content != "The nil check is missing in Parse." || got[7].Content != "Add a test too." {
		t.Errorf("updated rows = %q, %q", got[6].Content, got[7].Content)
	}

	runIndex(t, db, src, true)
	if _, got = rows(t, db); len(got) != 8 {
		t.Fatalf("full reindex rows = %d, want 8", len(got))
	}
}

func TestV2PreferredOverV1(t *testing.T) {
	path, w := fixture(t, v1DDL+v2DDL)
	// A stale V1 copy that must be ignored once V2 exists.
	exec(t, w, "INSERT INTO session VALUES (?, 'prj_1', NULL, 'calm-otter', '/work/old', 'Old title', '1.4.2', NULL, NULL, NULL, NULL, NULL, NULL, NULL, 1772359200000, 1772359260000, NULL, NULL, NULL)", sid)
	exec(t, w, "INSERT INTO message VALUES ('msg_a', ?, 1772359201000, 1772359201000, '{\"role\":\"user\"}')", sid)
	exec(t, w, "INSERT INTO part VALUES ('prt_a1', 'msg_a', ?, 1772359201000, 1772359201000, '{\"type\":\"text\",\"text\":\"stale\"}')", sid)

	exec(t, w, "INSERT INTO session_v2 VALUES (?, 'prj_1', '/work/demo-app', NULL, 1772359200000, 1772359300000)", sid)
	msgs := []struct {
		id, typ string
		seq     int
		data    string
	}{
		{"m1", "system", 0, `{"text":"system prompt"}`},
		{"m2", "user", 1, `{"text":"List the open TODOs\nin the repo"}`},
		{"m3", "assistant", 2, `{"content":[{"type":"reasoning","text":"Search for TODO."},{"type":"tool","name":"grep","state":{"status":"completed","input":{"pattern":"TODO"},"content":[{"type":"text","text":"a.go:3: TODO"}]}},{"type":"tool","name":"read","state":{"status":"error","input":{"path":"b.go"},"error":{"message":"not found"}}},{"type":"text","text":"One TODO in a.go."}]}`},
		{"m4", "shell", 3, `{"command":"git log -1","output":{"output":"commit abc"}}`},
		{"m5", "compaction", 4, `{"status":"completed","summary":"Listed TODOs."}`},
		{"m6", "model-switched", 5, `{}`},
	}
	for _, m := range msgs {
		exec(t, w, "INSERT INTO session_message VALUES (?, ?, ?, ?, ?, ?, ?)", m.id, sid, m.typ, m.seq, 1772359201000+int64(m.seq)*1000, 1772359201000+int64(m.seq)*1000, m.data)
	}

	db := openDB(t)
	src := &Source{DBPath: path}
	runIndex(t, db, src, false)
	s, got := rows(t, db)
	if s.Title != "List the open TODOs" || s.Workspace != "/work/demo-app" || s.Summary != "Listed TODOs." {
		t.Errorf("session = %+v", s)
	}
	check(t, got, []want{
		{"user", "List the open TODOs\nin the repo"},
		{"thinking", "Search for TODO."},
		{"tool", `[tool_call name=grep] {"pattern":"TODO"}`},
		{"tool", "[tool_result name=grep] a.go:3: TODO"},
		{"tool", `[tool_call name=read] {"path":"b.go"}`},
		{"tool", "[tool_result error name=read] not found"},
		{"assistant", "One TODO in a.go."},
		{"tool", "[bash] git log -1\ncommit abc"},
	})

	exec(t, w, "INSERT INTO session_message VALUES ('m7', ?, 'user', 6, 1772359400000, 1772359400000, '{\"text\":\"Thanks\"}')", sid)
	runIndex(t, db, src, false)
	runIndex(t, db, src, false)
	if _, got = rows(t, db); len(got) != 9 {
		t.Fatalf("rows after append = %d, want 9", len(got))
	}
}

// opencode 1.19 has an empty session_message table that references
// `session`, while messages are still written to message and part.
func TestEmptySessionMessageTableFallsBackToV1(t *testing.T) {
	ddl := v1DDL + "CREATE TABLE `session_message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `type` text NOT NULL," +
		"`seq` integer NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL);"
	path, w := fixture(t, ddl)
	exec(t, w, "INSERT INTO session VALUES (?, 'prj_1', NULL, 'calm-otter', '/work/demo-app', 'Hello', '1.19.0', NULL, NULL, NULL, NULL, NULL, NULL, NULL, 1772359200000, 1772359260000, NULL, NULL, NULL)", sid)
	exec(t, w, "INSERT INTO message VALUES ('msg_a', ?, 1772359201000, 1772359201000, '{\"role\":\"user\"}')", sid)
	exec(t, w, "INSERT INTO part VALUES ('prt_a1', 'msg_a', ?, 1772359201000, 1772359201000, '{\"type\":\"text\",\"text\":\"hi\"}')", sid)
	db := openDB(t)
	runIndex(t, db, &Source{DBPath: path}, false)
	_, got := rows(t, db)
	check(t, got, []want{{"user", "hi"}})
}

// testdata/opencode-1.19.0.sql is a dump of the session tables recorded
// by scripts/agent-fixtures/run.sh with opencode 1.19.0.
func TestRecordedSession(t *testing.T) {
	dump, err := os.ReadFile(filepath.Join("testdata", "opencode-1.19.0.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path, w := fixture(t, "SELECT 1")
	exec(t, w, string(dump))
	db := openDB(t)
	src := &Source{DBPath: path}
	runIndex(t, db, src, false)
	runIndex(t, db, src, false)
	st, err := db.Stats()
	if err != nil || len(st) != 1 || st[0].Sessions != 1 {
		t.Fatalf("stats = %+v, %v", st, err)
	}
	s, err := db.SessionByUIDAny("")
	if err != nil || s == nil {
		t.Fatalf("session = %v, %v", s, err)
	}
	got, err := db.SessionMessages(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, r := range got {
		roles = append(roles, r.Role)
	}
	if want := "user,tool,tool,assistant,user,tool,tool"; strings.Join(roles, ",") != want {
		t.Errorf("roles = %s, want %s", strings.Join(roles, ","), want)
	}
	if s.Workspace != "/work/lantern" {
		t.Errorf("workspace = %q", s.Workspace)
	}
}

func TestReadOnlyOpen(t *testing.T) {
	path, _ := fixture(t, v1DDL)
	odb, err := openRO(path)
	if err != nil {
		t.Fatal(err)
	}
	defer odb.Close()
	if _, err := odb.Exec("INSERT INTO message VALUES ('x', 'y', 0, 0, '{}')"); err == nil {
		t.Fatal("write through read-only handle succeeded")
	}
}

func TestDBPaths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENCODE_DB", "custom.db")
	got := (&Source{DataDir: dir}).dbPaths()
	if len(got) != 1 || got[0] != filepath.Join(dir, "custom.db") {
		t.Errorf("dbPaths with OPENCODE_DB = %v", got)
	}
	t.Setenv("OPENCODE_DB", "")
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg")
	if d := DefaultDataDir(); d != filepath.Join("/tmp/xdg", "opencode") {
		t.Errorf("DefaultDataDir = %q", d)
	}
}

func TestSessionUID(t *testing.T) {
	if got := SessionUID("ses_abc"); got != "abc" {
		t.Errorf("SessionUID = %q", got)
	}
}
