package pi

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

// All fixture content below is synthetic.
const (
	piHeader  = `{"type":"session","version":3,"id":"0190aaaa-1111-7000-8000-000000000001","timestamp":"2026-03-01T10:00:00.000Z","cwd":"/work/demo-app"}` + "\n"
	piSystem  = `{"type":"message","id":"a0","parentId":null,"timestamp":"2026-03-01T10:00:00.500Z","message":{"role":"system","content":"You are a helpful agent."}}` + "\n"
	piUser    = `{"type":"message","id":"a1","parentId":"a0","timestamp":"2026-03-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"Rename the widget module\nplease"},{"type":"image","data":"AAAA","mimeType":"image/png"}]}}` + "\n"
	piModel   = `{"type":"model_change","id":"a2","parentId":"a1","timestamp":"2026-03-01T10:00:01.500Z","provider":"demo","modelId":"demo-1"}` + "\n"
	piAsst    = `{"type":"message","id":"a3","parentId":"a2","timestamp":"2026-03-01T10:00:02.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Need to find the module."},{"type":"text","text":"Looking for it now."},{"type":"toolCall","id":"call_1","name":"bash","arguments":{"command":"ls src"}}]}}` + "\n"
	piResult  = `{"type":"message","id":"a4","parentId":"a3","timestamp":"2026-03-01T10:00:03.000Z","message":{"role":"toolResult","toolCallId":"call_1","toolName":"bash","content":[{"type":"text","text":"widget.go"}],"isError":false}}` + "\n"
	piInfo    = `{"type":"session_info","id":"a5","parentId":"a4","timestamp":"2026-03-01T10:00:04.000Z","name":"Widget rename"}` + "\n"
	piBash    = `{"type":"message","id":"a6","parentId":"a5","timestamp":"2026-03-01T10:00:05.000Z","message":{"role":"bashExecution","command":"git status","output":"clean","exitCode":0}}` + "\n"
	piCompact = `{"type":"compaction","id":"a7","parentId":"a6","timestamp":"2026-03-01T10:00:06.000Z","summary":"Renamed the widget module.","firstKeptEntryId":"a4","tokensBefore":1000}` + "\n"
	piErr     = `{"type":"message","id":"a8","parentId":"a7","timestamp":"2026-03-01T10:00:07.000Z","message":{"role":"toolResult","toolCallId":"call_2","toolName":"read","content":[{"type":"text","text":"no such file"}],"isError":true}}` + "\n"
)

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

func messages(t *testing.T, db *store.DB, uid string) (*store.Session, []store.Row) {
	t.Helper()
	s, err := LookupSession(db, uid)
	if err != nil || s == nil {
		t.Fatalf("SessionByUID(%q) = %v, %v", uid, s, err)
	}
	rows, err := db.SessionMessages(s.ID)
	if err != nil {
		t.Fatalf("SessionMessages: %v", err)
	}
	return s, rows
}

// writeFile writes content and bumps the mtime so size+mtime checks see
// a change even within the filesystem's timestamp granularity.
func writeFile(t *testing.T, path, content string, tick int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2026, 3, 1, 12, 0, tick, 0, time.UTC)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (root, path string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "--work-demo-app--")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(dir, "2026-03-01T10-00-00-000Z_0190aaaa-1111-7000-8000-000000000001.jsonl")
}

const uid = "0190aaaa-1111-7000-8000-000000000001"

func TestScanMapsEntries(t *testing.T) {
	root, path := setup(t)
	writeFile(t, path, piHeader+piSystem+piUser+piModel+piAsst+piResult+piInfo+piBash+piCompact+piErr, 1)
	db := openDB(t)
	runIndex(t, db, &Source{Root: root}, false)

	s, rows := messages(t, db, uid)
	if s.Workspace != "/work/demo-app" {
		t.Errorf("workspace = %q", s.Workspace)
	}
	if s.Title != "Widget rename" {
		t.Errorf("title = %q", s.Title)
	}
	if s.Summary != "Renamed the widget module." {
		t.Errorf("summary = %q", s.Summary)
	}
	if s.StartedAt != time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("started_at = %d", s.StartedAt)
	}
	want := []struct{ role, content string }{
		{"user", "Rename the widget module\nplease"},
		{"thinking", "Need to find the module."},
		{"assistant", "Looking for it now."},
		{"tool", `[tool_call name=bash] {"command":"ls src"}`},
		{"tool", "[tool_result name=bash] widget.go"},
		{"tool", "[bash] git status\nclean"},
		{"tool", "[tool_result error name=read] no such file"},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i, w := range want {
		if rows[i].Ordinal != i || rows[i].Role != w.role || rows[i].Content != w.content {
			t.Errorf("row %d = {%d %q %q}, want {%d %q %q}", i, rows[i].Ordinal, rows[i].Role, rows[i].Content, i, w.role, w.content)
		}
	}
}

func TestIncrementalAppendNoDuplicates(t *testing.T) {
	root, path := setup(t)
	db := openDB(t)
	src := &Source{Root: root}

	writeFile(t, path, piHeader+piUser+piAsst, 1)
	runIndex(t, db, src, false)
	_, rows := messages(t, db, uid)
	if len(rows) != 4 {
		t.Fatalf("first pass rows = %d, want 4", len(rows))
	}

	// Unchanged file: nothing new.
	runIndex(t, db, src, false)
	if _, rows = messages(t, db, uid); len(rows) != 4 {
		t.Fatalf("rerun rows = %d, want 4", len(rows))
	}

	// Append a half-written line: it must not be consumed yet.
	partial := strings.TrimSuffix(piResult, "\n")
	writeFile(t, path, piHeader+piUser+piAsst+partial[:40], 2)
	runIndex(t, db, src, false)
	if _, rows = messages(t, db, uid); len(rows) != 4 {
		t.Fatalf("partial-line rows = %d, want 4", len(rows))
	}

	// Finish the line and append one more entry.
	writeFile(t, path, piHeader+piUser+piAsst+piResult+piBash, 3)
	runIndex(t, db, src, false)
	_, rows = messages(t, db, uid)
	if len(rows) != 6 {
		t.Fatalf("append rows = %d, want 6", len(rows))
	}
	for i, r := range rows {
		if r.Ordinal != i {
			t.Errorf("row %d has ordinal %d", i, r.Ordinal)
		}
	}
	if rows[4].Content != "[tool_result name=bash] widget.go" {
		t.Errorf("row 4 = %q", rows[4].Content)
	}

	// A full reindex rebuilds the same rows.
	runIndex(t, db, src, true)
	if _, rows = messages(t, db, uid); len(rows) != 6 {
		t.Fatalf("full reindex rows = %d, want 6", len(rows))
	}
}

func TestShrunkFileIsReread(t *testing.T) {
	root, path := setup(t)
	db := openDB(t)
	src := &Source{Root: root}
	writeFile(t, path, piHeader+piUser+piAsst+piResult, 1)
	runIndex(t, db, src, false)
	writeFile(t, path, piHeader+piUser, 2)
	runIndex(t, db, src, false)
	if _, rows := messages(t, db, uid); len(rows) != 1 {
		t.Fatalf("rows after shrink = %d, want 1", len(rows))
	}
}

func TestNonSessionFileIgnored(t *testing.T) {
	root, path := setup(t)
	writeFile(t, path, `{"type":"something_else"}`+"\n"+piUser, 1)
	db := openDB(t)
	runIndex(t, db, &Source{Root: root}, false)
	st, err := db.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 0 {
		t.Fatalf("stats = %+v, want none", st)
	}
}

// testdata/pi-1.2.0-session.jsonl was recorded by
// scripts/agent-fixtures/run.sh with pi 1.2.0 (system prompt removed).
func TestRecordedSession(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "pi-1.2.0-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "--work-lantern--", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data), 1)
	db := openDB(t)
	src := &Source{Root: root}
	runIndex(t, db, src, false)
	runIndex(t, db, src, false)
	s, rows := messages(t, db, "01a13709-e475-7495-b150-8c1b17465d47")
	if s.Workspace != "/work/lantern" {
		t.Errorf("workspace = %q", s.Workspace)
	}
	var roles []string
	for _, r := range rows {
		roles = append(roles, r.Role)
	}
	want := "user,thinking,tool,tool,assistant,user,thinking,tool,tool,assistant"
	if got := strings.Join(roles, ","); got != want {
		t.Errorf("roles = %s, want %s", got, want)
	}
	if !strings.HasPrefix(rows[2].Content, "[tool_call name=read] ") || !strings.HasPrefix(rows[3].Content, "[tool_result name=read] # Lantern") {
		t.Errorf("tool rows = %q, %q", rows[2].Content, rows[3].Content)
	}
}

func TestDefaultRootEnv(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", "/tmp/pi-agent")
	if got := DefaultRoot(); got != filepath.Join("/tmp/pi-agent", "sessions") {
		t.Errorf("DefaultRoot with PI_CODING_AGENT_DIR = %q", got)
	}
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "/tmp/pi-sessions")
	if got := DefaultRoot(); got != "/tmp/pi-sessions" {
		t.Errorf("DefaultRoot with PI_CODING_AGENT_SESSION_DIR = %q", got)
	}
}
