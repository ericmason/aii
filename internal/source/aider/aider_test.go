package aider

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

func readFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "history.md"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSplitSessions(t *testing.T) {
	sessions := splitSessions(readFixture(t), 0)
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}

	first := sessions[0]
	want := time.Date(2026, 3, 2, 9, 15, 4, 0, time.Local).Unix()
	if first.StartedAt != want {
		t.Errorf("StartedAt = %d, want %d", first.StartedAt, want)
	}
	var roles []string
	for _, m := range first.Messages {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "tool,user,assistant,tool" {
		t.Errorf("roles = %s", got)
	}
	if got := first.Messages[1].Content; got != "Add a subtract function to calc.py.\nKeep the style of add()." {
		t.Errorf("user message = %q", got)
	}
	asst := first.Messages[2].Content
	if !strings.HasPrefix(asst, "Here is the change") || !strings.Contains(asst, "\n\ncalc.py\n```python") ||
		!strings.HasSuffix(asst, "```") {
		t.Errorf("assistant message lost its Markdown: %q", asst)
	}
	if got := first.Messages[0].Content; !strings.HasPrefix(got, "[tool_result] /usr/local/bin/aider") ||
		!strings.Contains(got, "\nAdded calc.py to the chat.") || strings.Contains(got, "  ") {
		t.Errorf("tool message = %q", got)
	}

	second := sessions[1]
	if title(second.Messages) != "Add docstrings to every function." {
		t.Errorf("title skipped no slash command: %q", title(second.Messages))
	}
	if second.Offset <= first.Offset {
		t.Errorf("offsets not increasing: %d, %d", first.Offset, second.Offset)
	}
}

func TestSplitSessionsWithoutHeader(t *testing.T) {
	sessions := splitSessions([]byte("#### hello  \n\nhi there\n"), 0)
	if len(sessions) != 1 || len(sessions[0].Messages) != 2 {
		t.Fatalf("got %+v", sessions)
	}
}

// harness wires a temp DB, a temp project, and the Aider source.
type harness struct {
	t    *testing.T
	db   *store.DB
	dir  string
	file string
	src  *Source
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tmp := t.TempDir()
	db, err := store.Open(filepath.Join(tmp, "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dir := filepath.Join(tmp, "projects", "calc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &harness{
		t: t, db: db, dir: dir,
		file: filepath.Join(dir, HistoryFileName),
		src:  &Source{Roots: []string{filepath.Join(tmp, "projects")}},
	}
}

func (h *harness) write(data []byte) {
	h.t.Helper()
	if err := os.WriteFile(h.file, data, 0o644); err != nil {
		h.t.Fatal(err)
	}
	// Make sure the mtime moves even on coarse filesystems.
	future := time.Now().Add(time.Duration(len(data)) * time.Millisecond)
	os.Chtimes(h.file, future, future)
}

func (h *harness) index(full bool) indexer.Stats {
	h.t.Helper()
	stats, err := indexer.Run(context.Background(), h.db, []source.Source{h.src}, indexer.Options{Full: full})
	if err != nil {
		h.t.Fatal(err)
	}
	return stats
}

func (h *harness) count(q string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(q, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) messages() int {
	return h.count(`SELECT COUNT(*) FROM messages`)
}

func (h *harness) sessions() int {
	return h.count(`SELECT COUNT(*) FROM sessions WHERE agent = 'aider'`)
}

func TestIndexIsIncrementalAndIdempotent(t *testing.T) {
	h := newHarness(t)
	fixture := readFixture(t)
	h.write(fixture)

	h.index(false)
	if h.sessions() != 2 || h.messages() != 10 {
		t.Fatalf("first run: %d sessions, %d messages", h.sessions(), h.messages())
	}
	var ws string
	h.db.QueryRow(`SELECT workspace FROM sessions WHERE agent='aider' LIMIT 1`).Scan(&ws)
	if ws != h.dir {
		t.Errorf("workspace = %q, want %q", ws, h.dir)
	}

	// Unchanged file: nothing new.
	if st := h.index(false); st.MessagesAdded != 0 {
		t.Errorf("second run added %d messages", st.MessagesAdded)
	}
	if h.sessions() != 2 || h.messages() != 10 {
		t.Fatalf("second run: %d sessions, %d messages", h.sessions(), h.messages())
	}

	// Aider appends more tool output to the last message group of the
	// tail session, then starts a new session.
	grown := append([]byte{}, fixture...)
	grown = append(grown, []byte("> Commit 1a2b3c4 docs: add docstrings  \n\n# aider chat started at 2026-03-03 08:00:00\n\n#### Rename calc.py to mathutil.py.  \n\nDone.\n")...)
	h.write(grown)
	h.index(false)
	if h.sessions() != 3 || h.messages() != 12 {
		t.Fatalf("after append: %d sessions, %d messages", h.sessions(), h.messages())
	}
	var last string
	h.db.QueryRow(`
		SELECT m.content FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE s.title = 'Add docstrings to every function.' ORDER BY m.ordinal DESC LIMIT 1`).Scan(&last)
	if !strings.Contains(last, "Commit 1a2b3c4") {
		t.Errorf("tail message not refreshed: %q", last)
	}

	// The first session was not re-read: its row keeps the same id.
	if h.count(`SELECT COUNT(*) FROM sessions WHERE agent='aider' AND title LIKE 'Add a subtract%' AND id = 1`) != 1 {
		t.Error("first session was rewritten on an incremental run")
	}

	// --full rebuilds to the same totals.
	h.index(true)
	if h.sessions() != 3 || h.messages() != 12 {
		t.Fatalf("after full: %d sessions, %d messages", h.sessions(), h.messages())
	}
}

func TestShrunkFileDropsOldSessions(t *testing.T) {
	h := newHarness(t)
	h.write(readFixture(t))
	h.index(false)

	h.write([]byte("\n# aider chat started at 2026-04-01 12:00:00\n\n#### Start over.  \n\nOK.\n"))
	h.index(false)
	if h.sessions() != 1 || h.messages() != 2 {
		t.Fatalf("after shrink: %d sessions, %d messages", h.sessions(), h.messages())
	}
}

func TestTitleArrivesOnLaterRun(t *testing.T) {
	h := newHarness(t)
	// First read: Aider has printed its startup output but the user
	// hasn't typed anything yet.
	start := "# aider chat started at 2026-05-01 10:00:00\n\n> aider --model example/model  \n> Added calc.py to the chat.  \n\n"
	h.write([]byte(start))
	h.index(false)
	var title string
	h.db.QueryRow(`SELECT COALESCE(title,'') FROM sessions WHERE agent='aider'`).Scan(&title)
	if title != "" {
		t.Fatalf("title before any user text = %q", title)
	}

	h.write([]byte(start + "#### Add a multiply function.  \n\nDone.\n"))
	h.index(false)
	h.db.QueryRow(`SELECT COALESCE(title,'') FROM sessions WHERE agent='aider'`).Scan(&title)
	if title != "Add a multiply function." || h.sessions() != 1 || h.messages() != 3 {
		t.Errorf("after user text: title %q, %d sessions, %d messages", title, h.sessions(), h.messages())
	}
}

func TestUnterminatedLastLineIsReread(t *testing.T) {
	h := newHarness(t)
	// The write stops partway through a new session's header line.
	first := "# aider chat started at 2026-05-01 10:00:00\n\n#### One.  \n\nReply one.\n\n# aider chat sta"
	h.write([]byte(first))
	h.index(false)
	off := int64(-1)
	if err := h.db.QueryRow(`SELECT last_offset FROM index_state WHERE source_path = ?`, h.file).Scan(&off); err != nil {
		t.Fatal(err)
	}
	if off != 0 {
		t.Fatalf("saved offset %d moved past the last complete header", off)
	}

	h.write([]byte(first + "rted at 2026-05-01 11:00:00\n\n#### Two.  \n\nReply two.\n"))
	h.index(false)
	if h.sessions() != 2 || h.messages() != 4 {
		t.Fatalf("after completing the line: %d sessions, %d messages", h.sessions(), h.messages())
	}
	var last string
	h.db.QueryRow(`
		SELECT m.content FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE s.title = 'One.' ORDER BY m.ordinal DESC LIMIT 1`).Scan(&last)
	if last != "Reply one." {
		t.Errorf("partial header left in the first session: %q", last)
	}
}

func TestReadRangeStopsAtStatSize(t *testing.T) {
	h := newHarness(t)
	h.write([]byte("0123456789appended later"))
	got, err := readRange(h.file, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "3456789" {
		t.Errorf("readRange = %q, want bytes 3..10 only", got)
	}
}

// testdata/recorded.md is the history file from a real Aider 0.86.2 run
// (scripts/agent-fixtures/aider-run.sh), holding only the test prompts.
func TestRecordedHistory(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "recorded.md"))
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.write(data)
	h.index(false)
	if h.sessions() != 2 || h.messages() != 8 {
		t.Fatalf("%d sessions, %d messages", h.sessions(), h.messages())
	}
	rows, err := h.db.Query(`
		SELECT s.title, group_concat(m.role, ',') FROM sessions s
		JOIN (SELECT * FROM messages ORDER BY ordinal) m ON m.session_id = s.id
		GROUP BY s.id ORDER BY s.started_at, s.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []string{
		"Add a subtract(a, b) function to calc.py.|tool,user,assistant,tool",
		"Add a one-line docstring to every function in calc.py.|tool,user,assistant,tool",
	}
	var got []string
	for rows.Next() {
		var title, roles string
		rows.Scan(&title, &roles)
		got = append(got, title+"|"+roles)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sessions:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if st := h.index(false); st.MessagesAdded != 0 {
		t.Errorf("rerun added %d messages", st.MessagesAdded)
	}
}

func TestDiscoversKnownWorkspaces(t *testing.T) {
	h := newHarness(t)
	h.src.Roots = []string{} // no configured roots
	h.write(readFixture(t))

	// Another source recorded a session in this project.
	if _, err := h.db.UpsertSession(&store.Session{
		Agent: "claude_code", UID: "11111111-2222-3333-4444-555555555555",
		Workspace: filepath.Join(h.dir, "sub"), SourcePath: "/nowhere.jsonl",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	h.index(false)
	if h.sessions() != 2 {
		t.Fatalf("found %d sessions via workspace lookup, want 2", h.sessions())
	}

	// Once indexed, the file is found again even with no workspace hint.
	if _, err := h.db.Exec(`DELETE FROM sessions WHERE agent='claude_code'`); err != nil {
		t.Fatal(err)
	}
	if got := h.src.discover(context.Background(), h.db); len(got) != 1 || got[0] != h.file {
		t.Errorf("discover = %v", got)
	}
}

func roleContents(msgs []store.Message) string {
	var parts []string
	for _, m := range msgs {
		parts = append(parts, m.Role+":"+m.Content)
	}
	return strings.Join(parts, " | ")
}

// Aider writes user input as "#### text  " after a blank line (io.py
// user_input). A "#### " heading inside a reply is part of the reply.
func TestHeadingInReplyStaysAssistant(t *testing.T) {
	data := "# aider chat started at 2026-01-01 10:00:00\n\n" +
		"#### Explain the plan  \n" +
		"#### in two steps  \n\n" +
		"Here is the plan.\n\n#### Step one\nDo this.\n\n" +
		"#### thanks  \n\nYou're welcome.\n"
	s := splitSessions([]byte(data), 0)
	want := "user:Explain the plan\nin two steps | assistant:Here is the plan.\n\n#### Step one\nDo this. | user:thanks | assistant:You're welcome."
	if len(s) != 1 || roleContents(s[0].Messages) != want {
		t.Errorf("messages = %s\nwant %s", roleContents(s[0].Messages), want)
	}
}

// Aider prefixes only the first line of a multi-line tool message with
// "> " (io.py append_chat_history); the lines after it, up to a blank
// line, belong to the same message.
func TestMultiLineToolOutputStaysTool(t *testing.T) {
	data := "# aider chat started at 2026-01-01 10:00:00\n\n" +
		"#### run it  \n" +
		"> Running sh greet.sh\nhello\ngoodbye  \n" +
		"> Applied edit to greet.sh  \n\n" +
		"Done.\n"
	s := splitSessions([]byte(data), 0)
	want := "user:run it | tool:[tool_result] Running sh greet.sh\nhello\ngoodbye\nApplied edit to greet.sh | assistant:Done."
	if len(s) != 1 || roleContents(s[0].Messages) != want {
		t.Errorf("messages = %s\nwant %s", roleContents(s[0].Messages), want)
	}
}

// The same history file reached through a symlinked workspace is
// indexed once.
func TestSymlinkedWorkspaceIndexedOnce(t *testing.T) {
	h := newHarness(t)
	h.src.Roots = []string{}
	h.write(readFixture(t))
	link := filepath.Join(filepath.Dir(h.dir), "calc-link")
	if err := os.Symlink(h.dir, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	for i, ws := range []string{h.dir, link} {
		if _, err := h.db.UpsertSession(&store.Session{
			Agent: "claude_code", UID: "ws-" + string(rune('a'+i)), Workspace: ws, SourcePath: "/nowhere.jsonl",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.src.discover(context.Background(), h.db); len(got) != 1 {
		t.Errorf("discover = %v, want one file", got)
	}
	h.index(false)
	if h.sessions() != 2 {
		t.Errorf("indexed %d sessions, want 2", h.sessions())
	}
}

// A history file deleted and recreated larger has different bytes at
// the saved offset; it must be re-read from the start, not from there.
func TestRecreatedLargerFileIsReread(t *testing.T) {
	h := newHarness(t)
	fixture := readFixture(t)
	h.write(fixture)
	h.index(false)

	fresh := "\n# aider chat started at 2026-05-01 09:00:00\n\n#### " +
		strings.Repeat("Rewrite everything. ", len(fixture)/10) + " \n\nOK.\n"
	h.write([]byte(fresh))
	h.index(false)
	if h.sessions() != 1 || h.messages() != 2 {
		t.Errorf("after recreate: %d sessions, %d messages; want 1, 2", h.sessions(), h.messages())
	}
}

func TestRootsExpandTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(RootsEnv, strings.Join([]string{"~/src", "~" + string(filepath.Separator) + "work", "~"}, string(filepath.ListSeparator)))
	got := strings.Join(New().roots(), ",")
	want := strings.Join([]string{filepath.Join(home, "src"), filepath.Join(home, "work"), home}, ",")
	if got != want {
		t.Errorf("roots = %s, want %s", got, want)
	}
}

// A root that is the home directory skips system folders like Library.
func TestHomeRootSkipsSystemDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, d := range []string{"proj", filepath.Join("Library", "proj")} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, d, HistoryFileName), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var found []string
	walkRoot(context.Background(), home, func(p string) { found = append(found, p) })
	if len(found) != 1 || filepath.Base(filepath.Dir(found[0])) != "proj" || strings.Contains(found[0], "Library") {
		t.Errorf("found %v, want only proj", found)
	}
}

func TestTitleSkipsCommandsAndKeepsFirstLine(t *testing.T) {
	msgs := []store.Message{
		{Role: "user", Content: "/add calc.py"},
		{Role: "tool", Content: "[tool_result] Added calc.py to the chat"},
		{Role: "user", Content: "Add a subtract function.\nKeep the style of add()."},
	}
	if got := title(msgs); got != "Add a subtract function." {
		t.Errorf("title = %q", got)
	}
}

func TestSessionUIDIsHex(t *testing.T) {
	uid := sessionUID("/p/.aider.chat.history.md", 42)
	if len(uid) != 32 || strings.Trim(uid, "0123456789abcdef") != "" {
		t.Errorf("uid %q is not 32 hex chars", uid)
	}
}
