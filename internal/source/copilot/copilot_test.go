package copilot

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

// Fixtures follow ISerializableChatData (chatModel.ts) and the operation
// log written by ObjectMutationLog (objectMutationLog.ts) in
// microsoft/vscode. All content is synthetic.

const sid = "3f2b1c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"

const initial = `{"kind":0,"v":{"version":3,"sessionId":"` + sid + `","creationDate":1772359200000,"initialLocation":"panel","responderUsername":"GitHub Copilot","requests":[` +
	`{"requestId":"req_1","timestamp":1772359201000,"message":{"text":"Explain parse.go\nbriefly","parts":[]},"variableData":{"variables":[]},` +
	`"response":[{"value":"It parses ","supportThemeIcons":false},{"kind":"inlineReference","inlineReference":{"$mid":1,"path":"/work/demo-app/parse.go","scheme":"file"}},{"value":" input.","supportThemeIcons":false},{"kind":"progressMessage","content":{"value":"Searching"}}]}` +
	`]}}`

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
	mt := time.Date(2026, 3, 1, 12, 0, tick, 0, time.UTC)
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

func TestOperationLog(t *testing.T) {
	user := filepath.Join(t.TempDir(), "User")
	ws := filepath.Join(user, "workspaceStorage", "0123456789abcdef0123456789abcdef")
	writeFile(t, filepath.Join(ws, "workspace.json"), `{"folder":"file:///work/demo%20app"}`, 0)
	path := filepath.Join(ws, "chatSessions", sid+".jsonl")
	writeFile(t, path, initial+"\n", 1)

	db := openDB(t)
	src := &Source{UserDirs: []string{user}}
	runIndex(t, db, src, false)
	s, got := rows(t, db, sid)
	if s.Workspace != "/work/demo app" || s.Title != "Explain parse.go" || s.StartedAt != 1772359200 {
		t.Errorf("session = %+v", s)
	}
	check(t, got, []want{
		{"user", "Explain parse.go\nbriefly"},
		{"assistant", "It parses `parse.go` input."},
	})

	runIndex(t, db, src, false)
	_, got = rows(t, db, sid)
	if len(got) != 2 {
		t.Fatalf("rerun rows = %d, want 2", len(got))
	}

	// Append a second request, stream its response, rename the session,
	// then rewrite the first response by truncating and re-pushing.
	lines := []string{
		initial,
		`{"kind":2,"k":["requests"],"v":[{"requestId":"req_2","timestamp":1772359300000,"message":{"text":"Run the tests","parts":[]},"variableData":{"variables":[]},"response":[]}]}`,
		`{"kind":2,"k":["requests",1,"response"],"v":[{"kind":"thinking","value":["Run ","go test."]},{"kind":"toolInvocationSerialized","toolId":"run_in_terminal","toolCallId":"tc1","invocationMessage":"Running go test","pastTenseMessage":"Ran go test","isConfirmed":true,"isComplete":true,"source":null,"presentation":null,"resultDetails":{"input":"{\n  \"command\": \"go test ./...\"\n}","output":[{"type":"embed","isText":true,"value":"ok  demo 0.1s"}]}}]}`,
		`{"kind":2,"k":["requests",1,"response"],"v":[{"value":"All tests pass."}]}`,
		`{"kind":2,"k":["requests",1,"response"],"v":[{"kind":"toolInvocationSerialized","toolId":"read_file","toolCallId":"tc2","invocationMessage":{"value":"Reading missing.go"},"isComplete":true,"isConfirmed":true,"source":null,"presentation":null,"resultDetails":{"input":"{\"path\":\"missing.go\"}","output":[],"isError":true},"pastTenseMessage":{"value":"File not found"}}]}`,
		`{"kind":1,"k":["customTitle"],"v":"Parser walkthrough"}`,
		`{"kind":2,"k":["requests",0,"response"],"v":[{"value":"It reads input."}],"i":0}`,
		`{"kind":3,"k":["requests",0,"followups"]}`,
	}
	writeFile(t, path, strings.Join(lines, "\n")+"\n", 2)
	runIndex(t, db, src, false)
	runIndex(t, db, src, false)
	s, got = rows(t, db, sid)
	if s.Title != "Parser walkthrough" || s.EndedAt != 1772359300 {
		t.Errorf("session = %+v", s)
	}
	check(t, got, []want{
		{"user", "Explain parse.go\nbriefly"},
		{"assistant", "It reads input."},
		{"user", "Run the tests"},
		{"thinking", "Run go test."},
		{"tool", `[tool_call name=run_in_terminal] {"command":"go test ./..."}`},
		{"tool", "[tool_result name=run_in_terminal] ok  demo 0.1s"},
		{"assistant", "All tests pass."},
		{"tool", `[tool_call name=read_file] {"path":"missing.go"}`},
		{"tool", "[tool_result error name=read_file] File not found"},
	})

	runIndex(t, db, src, true)
	if _, got = rows(t, db, sid); len(got) != 9 {
		t.Fatalf("full reindex rows = %d, want 9", len(got))
	}
}

func TestLegacyJSONAndEmptyWindow(t *testing.T) {
	user := filepath.Join(t.TempDir(), "User")
	legacy := `{"version":2,"sessionId":"aaaaaaaa-0000-4000-8000-000000000001","creationDate":1700000000000,"lastMessageDate":1700000060000,"computedTitle":"Old chat","requesterUsername":"dev","responderUsername":"GitHub Copilot","requests":[{"message":{"text":"hello","parts":[]},"response":[{"value":"Hi there."}],"isCanceled":false}]}`
	writeFile(t, filepath.Join(user, "globalStorage", "emptyWindowChatSessions", "aaaaaaaa-0000-4000-8000-000000000001.json"), legacy, 1)
	// A .json file with a .jsonl sibling is superseded.
	dir := filepath.Join(user, "workspaceStorage", "fedcba9876543210fedcba9876543210", "chatSessions")
	writeFile(t, filepath.Join(dir, "bbbbbbbb.json"), strings.Replace(legacy, "aaaaaaaa", "bbbbbbbb", 1), 1)
	writeFile(t, filepath.Join(dir, "bbbbbbbb.jsonl"), `{"kind":0,"v":{"version":3,"sessionId":"bbbbbbbb-0000-4000-8000-000000000001","creationDate":1700000000000,"requests":[{"timestamp":1700000001000,"message":{"text":"newer"},"response":[]}]}}`+"\n", 1)

	db := openDB(t)
	runIndex(t, db, &Source{UserDirs: []string{user}}, false)
	s, got := rows(t, db, "aaaaaaaa-0000-4000-8000-000000000001")
	if s.Title != "Old chat" || s.Workspace != "" || s.EndedAt != 1700000060 {
		t.Errorf("session = %+v", s)
	}
	check(t, got, []want{{"user", "hello"}, {"assistant", "Hi there."}})
	_, got = rows(t, db, "bbbbbbbb-0000-4000-8000-000000000001")
	check(t, got, []want{{"user", "newer"}})
	st, _ := db.Stats()
	if len(st) != 1 || st[0].Sessions != 2 {
		t.Errorf("stats = %+v", st)
	}
}

func TestDefaultUserDirsPortable(t *testing.T) {
	t.Setenv("VSCODE_PORTABLE", "/opt/vscode-data")
	d := DefaultUserDirs()
	if len(d) != 1 || d[0] != filepath.Join("/opt/vscode-data", "user-data", "User") {
		t.Errorf("DefaultUserDirs = %v", d)
	}
}

func TestURIPath(t *testing.T) {
	if got := uriPath("file:///Users/dev/my%20proj"); got != filepath.FromSlash("/Users/dev/my proj") {
		t.Errorf("uriPath = %q", got)
	}
	if got := uriPath("vscode-remote://ssh-remote%2Bbox/home/dev"); got != "vscode-remote://ssh-remote%2Bbox/home/dev" {
		t.Errorf("remote uriPath = %q", got)
	}
}
