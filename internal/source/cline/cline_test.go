package cline

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ericmason/aii/internal/indexer"
	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

const (
	legacyTask = "1760000000000"
	xmlTask    = "1750000000000"
	sdkSession = "1760000500000_ab3cd"
	rooTask    = "019a1b2c-3d4e-7f60-8a9b-0c1d2e3f4a5b"
)

// copyTree copies testdata/<name> into a temp dir so tests can edit it.
func copyTree(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", name)
	dst := filepath.Join(t.TempDir(), name)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func run(t *testing.T, db *store.DB, full bool, srcs ...source.Source) indexer.Stats {
	t.Helper()
	st, err := indexer.Run(context.Background(), db, srcs, indexer.Options{Full: full})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

type row struct {
	Role    string
	Content string
}

func sessionRows(t *testing.T, db *store.DB, agent, uid string) (store.Session, []row) {
	t.Helper()
	s, err := db.SessionByUID(agent, uid)
	if err != nil || s == nil {
		t.Fatalf("session %s/%s: %v", agent, uid, err)
	}
	rows, err := db.Query(`SELECT role, content FROM messages WHERE session_id = ? ORDER BY ordinal`, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.Role, &r.Content); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return *s, out
}

func roles(rs []row) string {
	var parts []string
	for _, r := range rs {
		parts = append(parts, r.Role)
	}
	return strings.Join(parts, ",")
}

func count(t *testing.T, db *store.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestClineLayouts(t *testing.T) {
	root := copyTree(t, "cline")
	db := openDB(t)
	run(t, db, false, &Source{agent: clineAgent, Roots: []string{root}})

	// Legacy task with native tool calls and taskHistory.json metadata.
	s, rs := sessionRows(t, db, "cline", legacyTask)
	if got := roles(rs); got != "user,thinking,assistant,tool,tool,assistant" {
		t.Errorf("legacy roles = %s", got)
	}
	if rs[0].Content != "Add a --verbose flag to the greeter CLI." {
		t.Errorf("user text not cleaned: %q", rs[0].Content)
	}
	if rs[3].Content != `[tool_call name=read_file] {"path":"main.go"}` {
		t.Errorf("tool call = %q", rs[3].Content)
	}
	if !strings.HasPrefix(rs[4].Content, "[tool_result] package main") {
		t.Errorf("tool result = %q", rs[4].Content)
	}
	if s.Workspace != "/home/dev/projects/greeter" || s.Title != "Add a --verbose flag to the greeter CLI." {
		t.Errorf("metadata = %q, %q", s.Workspace, s.Title)
	}
	if s.StartedAt != 1760000000 || s.EndedAt != 1760000042 {
		t.Errorf("times from ui_messages = %d..%d", s.StartedAt, s.EndedAt)
	}

	// Older task with text-format tools and no history entry.
	s, rs = sessionRows(t, db, "cline", xmlTask)
	if got := roles(rs); got != "user,assistant,tool,assistant" {
		t.Errorf("xml-tool roles = %s", got)
	}
	if want := "[tool_result] [read_file for 'notes.txt'] Result:\nBuy oat milk."; !strings.HasPrefix(rs[2].Content, want) {
		t.Errorf("legacy tool result = %q", rs[2].Content)
	}
	if s.Workspace != "/home/dev/projects/notes" {
		t.Errorf("workspace from environment_details = %q", s.Workspace)
	}
	if s.StartedAt != 1750000000 {
		t.Errorf("start time from task ID = %d", s.StartedAt)
	}
	if s.Title != "Summarize notes.txt in one sentence." {
		t.Errorf("title = %q", s.Title)
	}

	// SDK session.
	s, rs = sessionRows(t, db, "cline", sdkSession)
	if got := roles(rs); got != "user,assistant,tool,tool,tool,tool,assistant" {
		t.Errorf("sdk roles = %s", got)
	}
	if rs[0].Content != "Create greet.sh that prints hello, then run it." {
		t.Errorf("user_input wrapper not removed: %q", rs[0].Content)
	}
	if rs[5].Content != "[tool_result] [error] exit 127: sh: not found" {
		t.Errorf("error result = %q", rs[5].Content)
	}
	if s.Workspace != "/home/dev/projects/scripts" || s.StartedAt != 1760000500 || s.EndedAt != 1760000510 {
		t.Errorf("sdk metadata = %q %d..%d", s.Workspace, s.StartedAt, s.EndedAt)
	}
}

func TestRooCode(t *testing.T) {
	root := copyTree(t, "roo")
	db := openDB(t)
	src := NewRooCode()
	src.Roots = []string{root}
	run(t, db, false, src)

	s, rs := sessionRows(t, db, "roo_code", rooTask)
	if got := roles(rs); got != "user,thinking,assistant,tool,tool,assistant" {
		t.Errorf("roo roles = %s", got)
	}
	if rs[1].Content != "February never has 30 days." {
		t.Errorf("reasoning item = %q", rs[1].Content)
	}
	if s.Workspace != "/home/dev/projects/dates" || s.StartedAt != 1761000000 || s.EndedAt != 1761000004 {
		t.Errorf("roo metadata = %q %d..%d", s.Workspace, s.StartedAt, s.EndedAt)
	}
	if count(t, db, `SELECT COUNT(*) FROM sessions WHERE agent != 'roo_code'`) != 0 {
		t.Error("Roo Code tasks indexed under another agent name")
	}
}

func TestReindexAddsNoDuplicates(t *testing.T) {
	root := copyTree(t, "cline")
	db := openDB(t)
	src := &Source{agent: clineAgent, Roots: []string{root}}

	first := run(t, db, false, src)
	if first.MessagesAdded != 17 {
		t.Fatalf("first run added %d messages, want 17", first.MessagesAdded)
	}
	second := run(t, db, false, src)
	if second.MessagesAdded != 0 || second.SessionsTouch != 0 {
		t.Errorf("unchanged rerun touched %d sessions, added %d messages", second.SessionsTouch, second.MessagesAdded)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM messages`); n != 17 {
		t.Errorf("messages = %d, want 17", n)
	}

	// --full rebuilds to the same total.
	run(t, db, true, src)
	if n := count(t, db, `SELECT COUNT(*) FROM messages`); n != 17 {
		t.Errorf("after --full, messages = %d, want 17", n)
	}
}

// rewrite replaces a task's message file and bumps its mtime.
func rewrite(t *testing.T, path string, msgs []map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
}

func readRaw(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestAppendAndRewrite(t *testing.T) {
	root := copyTree(t, "roo")
	db := openDB(t)
	src := NewRooCode()
	src.Roots = []string{root}
	path := filepath.Join(root, "tasks", rooTask, apiHistoryFile)

	run(t, db, false, src)
	s, _ := sessionRows(t, db, "roo_code", rooTask)

	// Append a follow-up turn: only the two new rows are inserted and
	// the session row is kept.
	msgs := readRaw(t, path)
	msgs = append(msgs,
		map[string]any{"role": "user", "content": "<user_message>\nAnd 2026-02-29?\n</user_message>", "ts": 1761000100000},
		map[string]any{"role": "assistant", "content": "That fails too: 2026 is not a leap year.", "ts": 1761000101000},
	)
	rewrite(t, path, msgs)
	st := run(t, db, false, src)
	if st.MessagesAdded != 2 {
		t.Errorf("append added %d messages, want 2", st.MessagesAdded)
	}
	s2, rs := sessionRows(t, db, "roo_code", rooTask)
	if len(rs) != 8 || rs[6].Content != "And 2026-02-29?" || s2.ID != s.ID {
		t.Errorf("after append: %d rows, row 6 = %q, id %d -> %d", len(rs), rs[6].Content, s.ID, s2.ID)
	}

	// The user edits the first message and the extension drops the rest:
	// the task is re-indexed from scratch instead of keeping stale rows.
	rewrite(t, path, []map[string]any{
		{"role": "user", "content": "<user_message>\nWhy does the parser accept 2026-02-28?\n</user_message>", "ts": 1761000200000},
		{"role": "assistant", "content": "Because February 28 is a valid date.", "ts": 1761000201000},
	})
	run(t, db, false, src)
	_, rs = sessionRows(t, db, "roo_code", rooTask)
	if len(rs) != 2 || rs[0].Content != "Why does the parser accept 2026-02-28?" {
		t.Errorf("after rewrite: %+v", rs)
	}
}

func TestSameTaskInTwoRootsIndexedOnce(t *testing.T) {
	a := copyTree(t, "roo")
	b := copyTree(t, "roo")
	db := openDB(t)
	src := NewRooCode()
	src.Roots = []string{a, b}
	run(t, db, false, src)
	if n := count(t, db, `SELECT COUNT(*) FROM messages`); n != 6 {
		t.Errorf("messages = %d, want 6", n)
	}
	var path string
	db.QueryRow(`SELECT source_path FROM sessions`).Scan(&path)
	if !strings.HasPrefix(path, a) {
		t.Errorf("source_path %q is not from the first root", path)
	}
}

func TestDefaultRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLINE_DATA_DIR", "")
	t.Setenv("CLINE_DIR", "")

	cl := New().roots()
	if cl[0] != filepath.Join(home, ".cline", "data") {
		t.Errorf("first Cline root = %q", cl[0])
	}
	if !containsSuffix(cl, filepath.Join("User", "globalStorage", clineExtensionID)) {
		t.Errorf("Cline roots miss VS Code globalStorage: %v", cl)
	}

	roo := NewRooCode().roots()
	if roo[0] != filepath.Join(home, ".vscode-mock", "global-storage") {
		t.Errorf("first Roo Code root = %q", roo[0])
	}
	if !containsSuffix(roo, filepath.Join("User", "globalStorage", rooCodeExtensionID)) {
		t.Errorf("Roo Code roots miss VS Code globalStorage: %v", roo)
	}

	t.Setenv("CLINE_DIR", filepath.Join(home, "alt"))
	if got := New().roots()[0]; got != filepath.Join(home, "alt", "data") {
		t.Errorf("CLINE_DIR root = %q", got)
	}
}

func TestIDTime(t *testing.T) {
	cases := map[string]int64{
		"1760000000000":                        1760000000,
		"1760000500000_ab3cd":                  1760000500,
		"019a1b2c-3d4e-7f60-8a9b-0c1d2e3f4a5b": 1761392475,
		"0b5f2c1e-1111-4222-8333-444455556666": 0, // UUIDv4
		"42":                                   0,
	}
	for id, want := range cases {
		if got := idTime(id); got != want {
			t.Errorf("idTime(%q) = %d, want %d", id, got, want)
		}
	}
}

func containsSuffix(list []string, suffix string) bool {
	for _, s := range list {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}
