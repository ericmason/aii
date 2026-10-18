package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// Sessions indexed before Claude Code tool output got its own role keep
// it as role "user". Reopening the DB relabels them, since the source
// transcript may be gone and a reindex can't fix them.
func TestOpenRelabelsLegacyClaudeCodeToolResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aii.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cc := seed(t, db, &Session{Agent: "claude_code", UID: "cc-1", Title: "[tool_result id=toolu_01] ls output", SourcePath: "/gone.jsonl"}, []Message{
		{Ordinal: 0, Role: "user", Content: "[tool_result id=toolu_01] ls output"},
		{Ordinal: 1, Role: "user", Content: "fix the tabs\nplease"},
		{Ordinal: 2, Role: "assistant", Content: "[tool_result mentioned by assistant]"},
	})
	other := seed(t, db, &Session{Agent: "codex", UID: "cdx-1", Title: "[tool_result] keep", SourcePath: "/c.jsonl"}, []Message{
		{Ordinal: 0, Role: "user", Content: "[tool_result] keep"},
	})
	// Simulate a DB written before the migration existed.
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// The migration runs once: a legacy-shaped row written after it (say
	// by an old binary) is left alone on the next Open.
	late := seed(t, db, &Session{Agent: "claude_code", UID: "cc-2", SourcePath: "/late.jsonl"}, []Message{
		{Ordinal: 0, Role: "user", Content: "[tool_result id=toolu_02] late"},
	})
	db.Close()
	if db, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if rows, err := db.SessionMessages(late); err != nil || len(rows) != 1 || rows[0].Role != "user" {
		t.Errorf("row written after migration = %+v (err %v), want untouched role user", rows, err)
	}

	roles := func(sid int64) []string {
		rows, err := db.SessionMessages(sid)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.Role)
		}
		return out
	}
	if got := roles(cc); len(got) != 3 || got[0] != "tool" || got[1] != "user" || got[2] != "assistant" {
		t.Errorf("claude_code roles = %v, want [tool user assistant]", got)
	}
	if got := roles(other); len(got) != 1 || got[0] != "user" {
		t.Errorf("codex roles = %v, want untouched [user]", got)
	}

	var title string
	if err := db.QueryRow(`SELECT title FROM sessions WHERE id = ?`, cc).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "fix the tabs" {
		t.Errorf("title = %q, want first line of the first typed message", title)
	}

	// Search filters role on the messages table, so the relabelled row
	// must come back under role=tool and no longer under role=user.
	for role, want := range map[string]int{"tool": 1, "user": 0} {
		res, err := db.Search(Query{Text: "ls output", Agent: "claude_code", Role: role})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != want {
			t.Errorf("role=%s search returned %d sessions, want %d", role, len(res), want)
		}
	}
}

// Injected Claude Code text (skill bodies, command caveats, agent
// hand-offs) indexed before isMeta was honoured is relabelled "system",
// and sessions titled by it are retitled from a typed prompt.
func TestOpenRelabelsLegacyClaudeCodeMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aii.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cc := seed(t, db, &Session{Agent: "claude_code", UID: "cc-1", Title: "<local-command-caveat>Caveat", SourcePath: "/gone.jsonl"}, []Message{
		{Ordinal: 0, Role: "user", Content: "<local-command-caveat>Caveat</local-command-caveat>"},
		{Ordinal: 1, Role: "user", Content: "<command-name>/output-style</command-name>"},
		{Ordinal: 2, Role: "user", Content: "Base directory for this skill: /x\n# skill"},
		{Ordinal: 3, Role: "user", Content: "Another Claude session sent a message:\n<agent-message>"},
		{Ordinal: 4, Role: "user", Content: "alright so back to aii"},
	})
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.SessionMessages(cc)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, r := range rows {
		roles = append(roles, r.Role)
	}
	if got := strings.Join(roles, ","); got != "system,user,system,system,user" {
		t.Errorf("roles = %s, want system,user,system,system,user", got)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM sessions WHERE id = ?`, cc).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "alright so back to aii" {
		t.Errorf("title = %q, want the first typed prompt", title)
	}
}
