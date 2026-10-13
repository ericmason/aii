package copilot

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// requestHiddenFromTranscript hides the user's message but keeps the
// response; hiddenFromTranscript hides the whole turn.
func TestRequestHiddenFromTranscript(t *testing.T) {
	var s chatSession
	if err := json.Unmarshal([]byte(`{"sessionId":"x","requests":[
		{"timestamp":1000,"message":{"text":"internal prompt"},"requestHiddenFromTranscript":true,"response":[{"value":"Visible answer."}]},
		{"timestamp":2000,"message":{"text":"hidden turn"},"hiddenFromTranscript":true,"response":[{"value":"Hidden answer."}]},
		{"timestamp":3000,"message":{"text":"normal"},"response":[{"value":"Normal answer."}]}
	]}`), &s); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range sessionMessages(s) {
		got = append(got, m.Role+":"+m.Content)
	}
	want := "assistant:Visible answer.|user:normal|assistant:Normal answer."
	if strings.Join(got, "|") != want {
		t.Fatalf("messages = %q, want %q", strings.Join(got, "|"), want)
	}
}

// A file that fails to parse (for example while VS Code is writing it)
// leaves the indexed rows in place and is retried on the next run.
func TestParseFailureKeepsRows(t *testing.T) {
	user := filepath.Join(t.TempDir(), "User")
	path := filepath.Join(user, "workspaceStorage", "0123456789abcdef0123456789abcdef", "chatSessions", sid+".json")
	full := `{"version":2,"sessionId":"` + sid + `","creationDate":1700000000000,"requests":[{"message":{"text":"question"},"response":[{"value":"Answer."}]}]}`
	writeFile(t, path, full, 1)
	db := openDB(t)
	src := &Source{UserDirs: []string{user}}
	runIndex(t, db, src, false)

	// Half-written file.
	writeFile(t, path, full[:60], 2)
	runIndex(t, db, src, false)
	s, err := db.SessionByUID(agentName, sid)
	if err != nil || s == nil {
		t.Fatalf("session removed after parse failure: %v, %v", s, err)
	}
	if _, got := rows(t, db, sid); len(got) != 2 {
		t.Fatalf("rows after parse failure = %d, want 2", len(got))
	}

	writeFile(t, path, strings.Replace(full, "Answer.", "Answer, edited.", 1), 3)
	runIndex(t, db, src, false)
	_, got := rows(t, db, sid)
	check(t, got, []want{{"user", "question"}, {"assistant", "Answer, edited."}})
}

// A log line longer than any line buffer (here 65 MiB) is replayed
// instead of failing the whole file.
func TestOversizedLogLine(t *testing.T) {
	big := `{"kind":2,"k":["requests"],"v":[{"timestamp":1772359300000,"message":{"text":"` + strings.Repeat("x", 65<<20) + `"},"response":[]}]}`
	raw, err := replayLog([]byte(initial + "\n" + big + "\n" + `{"kind":1,"k":["customTitle"],"v":"After"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := raw.(map[string]any)
	if m["customTitle"] != "After" || len(m["requests"].([]any)) != 2 {
		t.Fatalf("replay stopped early: title=%v requests=%d", m["customTitle"], len(m["requests"].([]any)))
	}
}

// When VS Code migrates <id>.json to <id>.jsonl and removes the old
// file, the session's rows come from the new file only.
func TestJSONToJSONLMigration(t *testing.T) {
	user := filepath.Join(t.TempDir(), "User")
	dir := filepath.Join(user, "workspaceStorage", "0123456789abcdef0123456789abcdef", "chatSessions")
	legacy := `{"version":2,"sessionId":"` + sid + `","creationDate":1700000000000,"requests":[{"message":{"text":"old question"},"response":[{"value":"Old answer."}]}]}`
	writeFile(t, filepath.Join(dir, sid+".json"), legacy, 1)
	db := openDB(t)
	src := &Source{UserDirs: []string{user}}
	runIndex(t, db, src, false)

	writeFile(t, filepath.Join(dir, sid+".jsonl"), `{"kind":0,"v":{"version":3,"sessionId":"`+sid+`","creationDate":1700000000000,"requests":[{"timestamp":1700000001000,"message":{"text":"old question"},"response":[{"value":"Old answer, migrated."}]}]}}`+"\n", 2)
	runIndex(t, db, src, false)
	runIndex(t, db, src, false)
	s, got := rows(t, db, sid)
	check(t, got, []want{{"user", "old question"}, {"assistant", "Old answer, migrated."}})
	if !strings.HasSuffix(s.SourcePath, ".jsonl") {
		t.Errorf("source_path = %q", s.SourcePath)
	}
}
