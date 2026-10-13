package gemini

import (
	"path/filepath"
	"strings"
	"testing"
)

// A record larger than any line buffer (here 65 MiB of tool output) is
// replayed, and so are the records after it.
func TestOversizedRecordReplayed(t *testing.T) {
	big := `{"id":"g1","type":"gemini","content":"` + strings.Repeat("x", 65<<20) + `"}`
	c := parseConversation([]byte(strings.Join([]string{
		`{"sessionId":"s1","projectHash":"p"}`,
		`{"id":"u1","type":"user","content":"one"}`,
		big,
		`{"id":"u2","type":"user","content":"two"}`,
	}, "\n")))
	var ids []string
	for _, m := range c.Messages {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "u1,g1,u2" {
		t.Fatalf("ids = %v, want u1,g1,u2", ids)
	}
}

// When Gemini CLI rewrites a .json session as .jsonl, the session's rows
// come from the new file only.
func TestJSONToJSONLMigration(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".gemini", "tmp")
	chats := filepath.Join(root, "demo-app", "chats")
	legacy := `{"sessionId":"` + sessionID + `","projectHash":"demo-app","messages":[{"id":"m1","type":"user","content":"old question"},{"id":"m2","type":"gemini","content":"Old answer."}]}`
	writeFile(t, filepath.Join(chats, "session-a.json"), legacy, 1)
	db := openDB(t)
	src := &Source{Roots: []string{root}}
	runIndex(t, db, src, false)

	writeFile(t, filepath.Join(chats, "session-a.jsonl"), `{"sessionId":"`+sessionID+`","projectHash":"demo-app"}`+"\n"+
		`{"id":"m1","type":"user","content":"old question"}`+"\n"+
		`{"id":"m2","type":"gemini","content":"Old answer, migrated."}`+"\n", 2)
	runIndex(t, db, src, false)
	runIndex(t, db, src, false)
	s, got := rows(t, db, sessionID)
	check(t, got, []want{{"user", "old question"}, {"assistant", "Old answer, migrated."}})
	if !strings.HasSuffix(s.SourcePath, ".jsonl") {
		t.Errorf("source_path = %q", s.SourcePath)
	}
}
