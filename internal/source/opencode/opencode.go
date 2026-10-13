// Package opencode parses opencode sessions out of its SQLite database
// and emits a Batch per session.
//
// opencode keeps everything in one SQLite file (WAL mode) under its data
// directory, normally ~/.local/share/opencode/opencode.db. Two schemas
// exist in the wild:
//
//   - V1 (opencode 1.x through mid-2026): `session` rows, `message` rows
//     whose `data` JSON carries the role and metadata, and `part` rows
//     whose `data` JSON carries the content (text, reasoning, tool, file,
//     step markers, ...).
//
//   - V2 (opencode's development branch): `session_v2` rows and
//     `session_message` rows ordered by `seq`, each a typed record (user,
//     assistant, shell, compaction, ...). opencode copies V1 history into
//     V2 on upgrade and leaves the V1 tables behind, so a session with
//     session_message rows is read from them. opencode 1.19 already has
//     an empty session_message table next to `session`, and still writes
//     message and part rows, so the choice is made per session.
//
// Message and part rows are updated in place while a response streams,
// so there is no append-only offset to resume from. Instead the parser
// fingerprints each session (row counts plus the latest time_updated)
// and re-reads the whole session whenever the fingerprint changes,
// replacing what the index held for it.
//
// The database is opened read-only (mode=ro) and never checkpointed.
// It is not opened with immutable=1 because opencode may be writing to
// it, and an immutable open would ignore the WAL.
package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"

	_ "modernc.org/sqlite"
)

const (
	agentName = "opencode"

	maxToolResult  = 4 * 1024
	maxToolInput   = 1 * 1024
	maxTitleRunes  = 200
	maxSummaryRune = 4096
)

// Source implements source.Source for opencode.
type Source struct {
	// DataDir overrides the opencode data directory. Empty means
	// $XDG_DATA_HOME/opencode, falling back to ~/.local/share/opencode.
	DataDir string
	// DBPath overrides the database file. Empty means $OPENCODE_DB
	// (resolved against DataDir when relative), or every opencode*.db
	// file in DataDir.
	DBPath string
}

// New returns a default Source.
func New() *Source { return &Source{} }

// Name returns the agent identifier used for sessions.agent.
func (s *Source) Name() string { return agentName }

// DefaultDataDir returns the directory opencode stores its database in.
func DefaultDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "opencode")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode")
}

// dbPaths lists the database files to scan. opencode names the file
// opencode.db, or opencode-<channel>.db for non-release channels, and
// honors $OPENCODE_DB.
func (s *Source) dbPaths() []string {
	dataDir := s.DataDir
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	p := s.DBPath
	if p == "" {
		p = os.Getenv("OPENCODE_DB")
	}
	if p == ":memory:" {
		return nil
	}
	if p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(dataDir, p)
		}
		return []string{p}
	}
	var out []string
	if _, err := os.Stat(filepath.Join(dataDir, "opencode.db")); err == nil {
		out = append(out, filepath.Join(dataDir, "opencode.db"))
	}
	more, _ := filepath.Glob(filepath.Join(dataDir, "opencode-*.db"))
	return append(out, more...)
}

// Scan opens each opencode database and emits a Batch for every session
// that is new or changed since the last run.
func (s *Source) Scan(ctx context.Context, db *store.DB, full bool, out chan<- source.Batch) error {
	paths := s.dbPaths()
	if len(paths) == 0 {
		log.Printf("opencode: no database found, nothing to scan")
		return nil
	}
	for _, p := range paths {
		if err := s.scanDB(ctx, db, p, full, out); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("opencode: %s: %v", p, err)
		}
	}
	return nil
}

// sessionRow is one session plus its change fingerprint.
type sessionRow struct {
	ID          string
	Directory   string
	Title       string
	Created     int64 // ms
	Updated     int64 // ms
	V2Rows      int64 // session_message rows; when > 0 they are authoritative
	Fingerprint string
}

func (s *Source) scanDB(ctx context.Context, db *store.DB, path string, full bool, out chan<- source.Batch) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	odb, err := openRO(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer odb.Close()

	schema, err := detectSchema(ctx, odb)
	if err != nil {
		return err
	}
	if schema.sessions == "" {
		log.Printf("opencode: %s has no session tables, skipping", path)
		return nil
	}

	sessions, err := listSessions(ctx, odb, schema)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	var emitted, skipped int
	for _, sr := range sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		sourcePath := sessionSourcePath(path, sr.ID)
		prev, err := db.GetState(sourcePath)
		if err != nil {
			return fmt.Errorf("get state: %w", err)
		}
		if !full && prev != nil && prev.Fingerprint == sr.Fingerprint {
			skipped++
			continue
		}

		var msgs []store.Message
		var summary string
		if sr.V2Rows > 0 || !schema.v1 {
			msgs, summary, err = readSessionV2(ctx, odb, sr.ID)
		} else {
			msgs, summary, err = readSessionV1(ctx, odb, sr.ID)
		}
		if err != nil {
			log.Printf("opencode: session %s: %v", sr.ID, err)
			continue
		}

		title := strings.TrimSpace(sr.Title)
		if title == "" {
			title = firstUserText(msgs)
		}
		sess := store.Session{
			Agent:      agentName,
			UID:        SessionUID(sr.ID),
			Workspace:  sr.Directory,
			Title:      truncateRunes(title, maxTitleRunes),
			Summary:    truncateRunes(summary, maxSummaryRune),
			StartedAt:  sr.Created / 1000,
			EndedAt:    sr.Updated / 1000,
			SourcePath: sourcePath,
		}
		batch := source.Batch{
			Session:  sess,
			Messages: msgs,
			State:    store.IndexState{SourcePath: sourcePath, Fingerprint: sr.Fingerprint},
			// The whole session is re-read, so replace what we had.
			Truncate: prev != nil || full,
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- batch:
		}
		emitted++
	}
	log.Printf("opencode: %s: sessions=%d emitted=%d unchanged=%d", filepath.Base(path), len(sessions), emitted, skipped)
	return nil
}

// schemaInfo records which opencode tables a database has.
type schemaInfo struct {
	sessions string // "session_v2", "session", or "" when neither exists
	v1       bool   // message and part tables exist
	v2       bool   // session_message table exists
}

// detectSchema inspects the tables. opencode 1.19 ships the
// session_message table but still writes to message and part, so the
// choice between the two is made per session, by which one holds rows.
func detectSchema(ctx context.Context, odb *sql.DB) (schemaInfo, error) {
	var si schemaInfo
	rows, err := odb.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return si, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return si, err
		}
		have[n] = true
	}
	if err := rows.Err(); err != nil {
		return si, err
	}
	switch {
	case have["session_v2"]:
		si.sessions = "session_v2"
	case have["session"]:
		si.sessions = "session"
	}
	si.v1 = have["message"] && have["part"]
	si.v2 = have["session_message"]
	if !si.v1 && !si.v2 {
		si.sessions = ""
	}
	return si, nil
}

// listSessions returns every session with a fingerprint that changes
// whenever a message or part is added or updated, or the title changes.
func listSessions(ctx context.Context, odb *sql.DB, si schemaInfo) ([]sessionRow, error) {
	v1 := `'', ''`
	if si.v1 {
		v1 = `(SELECT COUNT(*) || ':' || COALESCE(MAX(time_updated), 0) FROM message WHERE session_id = s.id),
		      (SELECT COUNT(*) || ':' || COALESCE(MAX(time_updated), 0) FROM part WHERE session_id = s.id)`
	}
	v2 := `0, ''`
	if si.v2 {
		v2 = `(SELECT COUNT(*) FROM session_message WHERE session_id = s.id),
		      (SELECT COALESCE(MAX(seq), -1) || ':' || COALESCE(MAX(time_updated), 0) FROM session_message WHERE session_id = s.id)`
	}
	q := `SELECT s.id, s.directory, COALESCE(s.title, ''), s.time_created, s.time_updated, ` +
		v1 + `, ` + v2 + ` FROM ` + si.sessions + ` s`
	rows, err := odb.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		var m, p, v2tail string
		if err := rows.Scan(&r.ID, &r.Directory, &r.Title, &r.Created, &r.Updated, &m, &p, &r.V2Rows, &v2tail); err != nil {
			return nil, err
		}
		r.Fingerprint = fmt.Sprintf("%s:%s:%d:%s:%s", m, p, r.V2Rows, v2tail, r.Title)
		out = append(out, r)
	}
	return out, rows.Err()
}

// -------------------------------------------------------------------
// V1: message + part tables.
// -------------------------------------------------------------------

// v1Message is the subset of message.data we read.
type v1Message struct {
	Role    string `json:"role"`
	Summary bool   `json:"summary"` // assistant message holding a compaction summary
}

// v1Part is the subset of part.data we read.
type v1Part struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Synthetic bool   `json:"synthetic"`
	Ignored   bool   `json:"ignored"`
	Tool      string `json:"tool"`
	State     struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output string          `json:"output"`
		Error  string          `json:"error"`
	} `json:"state"`
}

// readSessionV1 returns the session's messages in order, plus the text
// of the latest compaction summary.
func readSessionV1(ctx context.Context, odb *sql.DB, sessionID string) ([]store.Message, string, error) {
	rows, err := odb.QueryContext(ctx, `
		SELECT m.id, m.time_created, m.data, p.data
		FROM message m
		LEFT JOIN part p ON p.message_id = m.id
		WHERE m.session_id = ?
		ORDER BY m.time_created, m.id, p.id`, sessionID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var msgs []store.Message
	var summary string
	var curID string
	var cur v1Message
	var summaryBuf strings.Builder
	flushSummary := func() {
		if s := strings.TrimSpace(summaryBuf.String()); s != "" {
			summary = s
		}
		summaryBuf.Reset()
	}
	add := func(role string, ts int64, content string) {
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
		msgs = append(msgs, store.Message{Ordinal: len(msgs), Role: role, TS: ts, Content: content})
	}

	for rows.Next() {
		var id string
		var created int64
		var mdata string
		var pdata sql.NullString
		if err := rows.Scan(&id, &created, &mdata, &pdata); err != nil {
			return nil, "", err
		}
		if id != curID {
			flushSummary()
			curID = id
			cur = v1Message{}
			_ = json.Unmarshal([]byte(mdata), &cur)
		}
		if !pdata.Valid {
			continue
		}
		var p v1Part
		if json.Unmarshal([]byte(pdata.String), &p) != nil {
			continue
		}
		ts := created / 1000
		switch cur.Role {
		case "user":
			// Synthetic text is context opencode injected (file
			// contents, reminders); ignored text was dropped by the user.
			if p.Type == "text" && !p.Synthetic && !p.Ignored {
				add("user", ts, p.Text)
			}
		case "assistant":
			if cur.Summary {
				if p.Type == "text" {
					summaryBuf.WriteString(p.Text)
					summaryBuf.WriteString("\n\n")
				}
				continue
			}
			switch p.Type {
			case "text":
				add("assistant", ts, p.Text)
			case "reasoning":
				add("thinking", ts, p.Text)
			case "tool":
				add("tool", ts, toolCall(p.Tool, p.State.Input))
				switch p.State.Status {
				case "completed":
					add("tool", ts, toolResult(p.Tool, p.State.Output, false))
				case "error":
					add("tool", ts, toolResult(p.Tool, p.State.Error, true))
				}
			}
			// step-start, step-finish, snapshot, patch, file, agent,
			// subtask, retry, compaction parts: skipped.
		}
	}
	flushSummary()
	return msgs, summary, rows.Err()
}

// -------------------------------------------------------------------
// V2: session_message table.
// -------------------------------------------------------------------

// v2Data is the subset of session_message.data we read. The id and type
// live in their own columns.
type v2Data struct {
	Text    string `json:"text"`    // user
	Command string `json:"command"` // shell
	Output  *struct {
		Output string `json:"output"`
	} `json:"output"` // shell
	Content []v2Content `json:"content"` // assistant
	Status  string      `json:"status"`  // compaction
	Summary string      `json:"summary"` // compaction
}

// v2Content is one assistant content item: text, reasoning, or tool.
type v2Content struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Name  string `json:"name"`
	State struct {
		Status  string          `json:"status"`
		Input   json.RawMessage `json:"input"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"state"`
}

// readSessionV2 returns the session's messages in seq order, plus the
// latest completed compaction summary.
func readSessionV2(ctx context.Context, odb *sql.DB, sessionID string) ([]store.Message, string, error) {
	rows, err := odb.QueryContext(ctx, `
		SELECT type, time_created, data
		FROM session_message
		WHERE session_id = ?
		ORDER BY seq`, sessionID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var msgs []store.Message
	var summary string
	add := func(role string, ts int64, content string) {
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
		msgs = append(msgs, store.Message{Ordinal: len(msgs), Role: role, TS: ts, Content: content})
	}
	for rows.Next() {
		var typ, data string
		var created int64
		if err := rows.Scan(&typ, &created, &data); err != nil {
			return nil, "", err
		}
		var d v2Data
		if json.Unmarshal([]byte(data), &d) != nil {
			continue
		}
		ts := created / 1000
		switch typ {
		case "user":
			add("user", ts, d.Text)
		case "shell":
			// A shell command the user ran directly, not an LLM tool.
			if c := strings.TrimSpace(d.Command); c != "" {
				content := "[bash] " + c
				if d.Output != nil && strings.TrimSpace(d.Output.Output) != "" {
					content += "\n" + truncateRunes(strings.TrimSpace(d.Output.Output), maxToolResult)
				}
				add("tool", ts, content)
			}
		case "assistant":
			for _, c := range d.Content {
				switch c.Type {
				case "text":
					add("assistant", ts, c.Text)
				case "reasoning":
					add("thinking", ts, c.Text)
				case "tool":
					add("tool", ts, toolCall(c.Name, c.State.Input))
					var out strings.Builder
					for _, item := range c.State.Content {
						if item.Type == "text" && item.Text != "" {
							if out.Len() > 0 {
								out.WriteString("\n")
							}
							out.WriteString(item.Text)
						}
					}
					switch c.State.Status {
					case "completed":
						add("tool", ts, toolResult(c.Name, out.String(), false))
					case "error":
						msg := out.String()
						if c.State.Error != nil && c.State.Error.Message != "" {
							msg = c.State.Error.Message
						}
						add("tool", ts, toolResult(c.Name, msg, true))
					}
				}
			}
		case "compaction":
			if d.Status == "completed" && strings.TrimSpace(d.Summary) != "" {
				summary = strings.TrimSpace(d.Summary)
			}
		}
		// synthetic, system, skill, agent-switched, model-switched,
		// location-switched, idle: skipped.
	}
	return msgs, summary, rows.Err()
}

// -------------------------------------------------------------------
// Helpers.
// -------------------------------------------------------------------

// SessionUID returns the session_uid aii stores for an opencode session
// id. Every opencode id starts with "ses_", so the prefix is dropped to
// keep 8-character short ids (and cite tokens) distinct between sessions.
func SessionUID(id string) string { return strings.TrimPrefix(id, "ses_") }

func toolCall(name string, input json.RawMessage) string {
	return fmt.Sprintf("[tool_call name=%s] %s", name, truncateRunes(compactJSON(input), maxToolInput))
}

func toolResult(name, body string, isErr bool) string {
	tag := "[tool_result"
	if isErr {
		tag += " error"
	}
	if name != "" {
		tag += " name=" + name
	}
	return tag + "] " + truncateRunes(strings.TrimSpace(body), maxToolResult)
}

// openRO opens an opencode database read-only. busy_timeout covers the
// brief locks opencode takes while it checkpoints the WAL.
func openRO(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", readOnlyURI(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxIdleTime(time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// sessionSourcePath is the per-session key for index state and the
// session's source_path. It carries the database path, so sessions from
// two databases (for example the stable and a dev channel) keep separate
// state.
func sessionSourcePath(dbPath, id string) string {
	if abs, err := filepath.Abs(dbPath); err == nil {
		dbPath = abs
	}
	return "opencode:" + dbPath + "#session:" + id
}

// readOnlyURI builds the SQLite URI that opens path read-only. A Windows
// path such as C:\Users\me\opencode.db becomes file:///C:/Users/me/...;
// without the slash conversion and the leading "/", SQLite reads the
// drive as a URI authority and refuses to open the file.
func readOnlyURI(path string) string {
	p := filepath.ToSlash(path)
	if len(p) >= 2 && p[1] == ':' && (p[0] >= 'A' && p[0] <= 'Z' || p[0] >= 'a' && p[0] <= 'z') {
		p = "/" + strings.ReplaceAll(p, `\`, "/")
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	return u.String()
}

// compactJSON re-serializes a RawMessage with no superfluous whitespace.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// truncateRunes shortens s to at most n runes, appending an ellipsis if
// it was trimmed.
func truncateRunes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// firstUserText returns the first line of the first user message.
func firstUserText(msgs []store.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			t := strings.TrimSpace(m.Content)
			if i := strings.IndexByte(t, '\n'); i >= 0 {
				t = t[:i]
			}
			return t
		}
	}
	return ""
}
