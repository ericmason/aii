// Package pi parses pi coding agent session transcripts from
// ~/.pi/agent/sessions/--<cwd>--/<timestamp>_<id>.jsonl and emits
// per-file Batch deltas.
//
// Each file holds exactly one session. The first line is a `session`
// header carrying the session id and cwd; every later line is a tree
// entry (`message`, `model_change`, `compaction`, `session_info`, ...)
// linked by id/parentId. Branches share the file, and pi only ever
// appends, so the parser reads entries in file order and resumes from
// the last byte offset on the next run, like the Claude Code source.
//
// Format reference: packages/coding-agent/docs/session-format.md and
// message-types.md in github.com/badlogic/pi-mono (session version 3).
package pi

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

const (
	agentName = "pi"

	// Truncation budgets for noisy content, matching the other sources.
	maxToolResult  = 4 * 1024
	maxToolInput   = 1 * 1024
	maxTitleRunes  = 200
	maxSummaryRune = 4096

	scannerInitial = 1 << 20  // 1 MiB
	scannerMax     = 16 << 20 // 16 MiB
)

// Source implements source.Source for the pi coding agent.
type Source struct {
	// Root overrides the sessions directory. Empty means
	// $PI_CODING_AGENT_SESSION_DIR, then $PI_CODING_AGENT_DIR/sessions,
	// then ~/.pi/agent/sessions (the same order pi itself uses).
	Root string
}

// New returns a default Source.
func New() *Source { return &Source{} }

// Name returns the agent identifier used for sessions.agent.
func (s *Source) Name() string { return agentName }

// DefaultRoot returns the sessions directory pi writes to by default.
func DefaultRoot() string {
	if d := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		return filepath.Join(d, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}

func (s *Source) resolveRoot() (string, error) {
	if s.Root != "" {
		return s.Root, nil
	}
	if r := DefaultRoot(); r != "" {
		return r, nil
	}
	return "", errors.New("pi: cannot resolve home directory")
}

// Scan walks the sessions tree and emits one Batch per JSONL file that
// has new content (or every file when full=true).
func (s *Source) Scan(ctx context.Context, db *store.DB, full bool, out chan<- source.Batch) error {
	root, err := s.resolveRoot()
	if err != nil {
		return err
	}
	entries, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		return fmt.Errorf("glob %s: %w", root, err)
	}
	for _, path := range entries {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := s.scanFile(ctx, db, path, full, out); err != nil {
			log.Printf("pi: %s: %v", path, err)
		}
	}
	return nil
}

// scanFile processes one session file, emitting at most one Batch.
func (s *Source) scanFile(ctx context.Context, db *store.DB, path string, full bool, out chan<- source.Batch) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	size := info.Size()
	mtimeNs := info.ModTime().UnixNano()

	prev, err := db.GetState(abs)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}

	var startOffset int64
	truncate := false
	switch {
	case full:
		truncate = true
	case prev != nil && prev.Size > size:
		// File shrank: pi never does this, so it was edited by hand.
		truncate = true
	case prev != nil && prev.Size == size && prev.MtimeNs == mtimeNs:
		return nil
	case prev != nil:
		startOffset = prev.LastOffset
	}

	f, err := os.Open(abs)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	// The header is the first line; read it on every pass so incremental
	// runs still know the session id, cwd, and start time.
	hdr, err := readHeader(f)
	if err != nil {
		return err
	}
	if startOffset > 0 {
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			return fmt.Errorf("seek: %w", err)
		}
	} else if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	sess, msgs, newOffset, fallbackTitle := parseDelta(ctx, f, startOffset, size)
	if err := ctx.Err(); err != nil {
		return err
	}
	uid := SessionUID(hdr.ID)
	if sess.Title == "" && fallbackTitle != "" {
		// The first user message titles the session only when nothing
		// earlier did: a delta's first message is a later turn.
		if startOffset == 0 || truncate {
			sess.Title = fallbackTitle
		} else if existing, err := db.SessionByUID(agentName, uid); err == nil && (existing == nil || existing.Title == "") {
			sess.Title = fallbackTitle
		}
	}

	state := store.IndexState{SourcePath: abs, MtimeNs: mtimeNs, Size: size, LastOffset: newOffset}
	if hdr.ID == "" {
		// Not a pi session file; remember it so we skip it until it changes.
		out <- source.Batch{State: state, Truncate: truncate}
		return nil
	}

	baseOrdinal := 0
	if !truncate {
		baseOrdinal, err = nextOrdinalForUID(db, uid)
		if err != nil {
			return fmt.Errorf("base ordinal: %w", err)
		}
	}
	for i := range msgs {
		msgs[i].Ordinal += baseOrdinal
	}

	sess.Agent = agentName
	sess.UID = uid
	sess.Workspace = hdr.CWD
	sess.StartedAt = parseTS(hdr.Timestamp)
	sess.SourcePath = abs
	sess.SourceMtimeNs = mtimeNs
	sess.SourceSize = size

	select {
	case <-ctx.Done():
		return ctx.Err()
	case out <- source.Batch{Session: sess, Messages: msgs, State: state, Truncate: truncate}:
	}
	return nil
}

// header is the `session` line at the top of every file.
type header struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CWD       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
}

// readHeader decodes the first line of r. A file whose first line is
// not a session header yields a zero header and no error.
func readHeader(r io.Reader) (header, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	line, err := br.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return header{}, fmt.Errorf("read header: %w", err)
	}
	var h header
	if json.Unmarshal(line, &h) != nil || h.Type != "session" {
		return header{}, nil
	}
	return h, nil
}

// -------------------------------------------------------------------
// JSONL entry parsing
// -------------------------------------------------------------------

// entry is the envelope shared by every line. Only the fields we read
// are decoded.
type entry struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
	Name      string          `json:"name"`    // session_info
	Summary   string          `json:"summary"` // compaction, branch_summary
}

// agentMessage covers every AgentMessage role pi persists.
type agentMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolName   string          `json:"toolName"`
	ToolCallID string          `json:"toolCallId"`
	IsError    bool            `json:"isError"`
	Command    string          `json:"command"` // bashExecution
	Output     string          `json:"output"`  // bashExecution
	ExitCode   *int            `json:"exitCode"`
}

// contentBlock covers text, thinking, image, and toolCall blocks.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// readLine reads one newline-terminated line from br. It returns the
// line without its newline, the number of bytes consumed (including the
// newline), whether a newline ended it, and whether it exceeded max
// bytes. An oversized line is consumed in full but its content is
// dropped. err is non-nil only for read errors other than EOF.
func readLine(br *bufio.Reader, max int) (line []byte, n int64, complete, tooLong bool, err error) {
	for {
		chunk, rerr := br.ReadSlice('\n')
		n += int64(len(chunk))
		if !tooLong {
			if len(line)+len(chunk) > max+1 {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case rerr == nil:
			if !tooLong {
				line = line[:len(line)-1]
			}
			return line, n, true, tooLong, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case errors.Is(rerr, io.EOF):
			return line, n, false, tooLong, nil
		default:
			return line, n, false, tooLong, rerr
		}
	}
}

// parseDelta reads entries from r (positioned at startOffset) through
// EOF. It returns session metadata gathered from the delta (the title
// only when a session_info entry sets one), messages with ordinals
// starting at 0, the offset just past the last complete line, and the
// first user message's text as a fallback title.
func parseDelta(ctx context.Context, r io.Reader, startOffset, size int64) (store.Session, []store.Message, int64, string) {
	return parseDeltaMax(ctx, r, startOffset, size, scannerMax)
}

func parseDeltaMax(ctx context.Context, r io.Reader, startOffset, size int64, maxLine int) (store.Session, []store.Message, int64, string) {
	var sess store.Session
	var msgs []store.Message

	// Read only up to the size stat'd before the read. Bytes pi appends
	// meanwhile belong to the next run, which resumes from the offset
	// returned here, so nothing is emitted twice.
	if size >= startOffset {
		r = io.LimitReader(r, size-startOffset)
	}
	br := bufio.NewReaderSize(r, scannerInitial)
	offset := startOffset
	for ctx.Err() == nil {
		lineStart := offset
		line, n, complete, tooLong, err := readLine(br, maxLine)
		if err != nil {
			log.Printf("pi: read error at offset %d: %v", lineStart, err)
			break
		}
		if n == 0 || !complete {
			// pi writes every entry with a trailing newline, so a last
			// line without one is still being written, whatever its size.
			// Leave it for the next run.
			break
		}
		if tooLong {
			log.Printf("pi: skipping %d-byte line at offset %d (over %d bytes)", n, lineStart, maxLine)
			offset += n
			continue
		}

		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			offset += n
			continue
		}
		var e entry
		if err := json.Unmarshal([]byte(trimmed), &e); err != nil {
			log.Printf("pi: bad json at offset %d: %v", lineStart, err)
			offset += n
			continue
		}
		offset += n
		ts := parseTS(e.Timestamp)
		switch e.Type {
		case "message":
			for _, m := range messageToRows(e.Message) {
				m.Ordinal = len(msgs)
				m.TS = ts
				m.SourceOffset = lineStart
				msgs = append(msgs, m)
			}
		case "session_info":
			// The latest /name wins; the store keeps the old title when
			// this is empty.
			if n := strings.TrimSpace(e.Name); n != "" {
				sess.Title = truncateRunes(n, maxTitleRunes)
			}
		case "compaction":
			if s := strings.TrimSpace(e.Summary); s != "" {
				sess.Summary = truncateRunes(s, maxSummaryRune)
			}
		}
		// session, model_change, thinking_level_change, usage, label,
		// custom, custom_message, context_edit, branch_summary: skipped.
	}

	var fallback string
	if len(msgs) > 0 {
		sess.EndedAt = msgs[len(msgs)-1].TS
		fallback = firstUserText(msgs)
	}
	return sess, msgs, offset, fallback
}

// messageToRows maps one AgentMessage to zero or more store rows.
// Assistant turns split into thinking, text, and one tool row per call,
// mirroring how the Codex source stores function calls.
func messageToRows(raw json.RawMessage) []store.Message {
	var m agentMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return nil
	}
	switch m.Role {
	case "user":
		if t := strings.TrimSpace(contentText(m.Content)); t != "" {
			return []store.Message{{Role: "user", Content: t}}
		}
	case "assistant":
		return assistantRows(m.Content)
	case "toolResult":
		body := truncateRunes(strings.TrimSpace(contentText(m.Content)), maxToolResult)
		tag := "[tool_result"
		if m.IsError {
			tag += " error"
		}
		if m.ToolName != "" {
			tag += " name=" + m.ToolName
		}
		return []store.Message{{Role: "tool", Content: tag + "] " + body}}
	case "bashExecution":
		// A shell command the user ran directly with `!`, not an LLM tool.
		cmd := strings.TrimSpace(m.Command)
		if cmd == "" {
			return nil
		}
		content := "[bash] " + cmd
		if out := strings.TrimSpace(m.Output); out != "" {
			content += "\n" + truncateRunes(out, maxToolResult)
		}
		return []store.Message{{Role: "tool", Content: content}}
	}
	// system (prompt and tool loadout), custom, branchSummary,
	// compactionSummary, and unknown roles are skipped.
	return nil
}

// assistantRows renders an assistant content array.
func assistantRows(raw json.RawMessage) []store.Message {
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var text, think strings.Builder
	var tools []store.Message
	for _, b := range blocks {
		switch b.Type {
		case "text":
			appendLine(&text, b.Text)
		case "thinking":
			appendLine(&think, b.Thinking)
		case "toolCall":
			args := truncateRunes(compactJSON(b.Arguments), maxToolInput)
			tools = append(tools, store.Message{
				Role:    "tool",
				Content: fmt.Sprintf("[tool_call name=%s] %s", b.Name, args),
			})
		}
	}
	var out []store.Message
	if s := strings.TrimSpace(think.String()); s != "" {
		out = append(out, store.Message{Role: "thinking", Content: s})
	}
	if s := strings.TrimSpace(text.String()); s != "" {
		out = append(out, store.Message{Role: "assistant", Content: s})
	}
	return append(out, tools...)
}

// contentText flattens a string-or-blocks content field to its text.
// Image blocks are dropped.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range blocks {
		if c.Type == "text" {
			appendLine(&b, c.Text)
		}
	}
	return b.String()
}

// -------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------

func appendLine(b *strings.Builder, s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(s)
}

// nextOrdinalForUID returns the next ordinal for an existing pi session,
// or 0 if no prior session exists.
func nextOrdinalForUID(db *store.DB, uid string) (int, error) {
	const q = `SELECT COALESCE(MAX(ordinal), -1) + 1
	           FROM messages
	           WHERE session_id = (SELECT id FROM sessions WHERE agent = ? AND session_uid = ?)`
	var n sql.NullInt64
	if err := db.DB.QueryRow(q, agentName, uid).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	if !n.Valid {
		return 0, nil
	}
	return int(n.Int64), nil
}

// parseTS converts an RFC3339(Nano) timestamp to unix seconds; 0 on failure.
func parseTS(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UTC().Unix()
}

// compactJSON re-serializes a RawMessage with no superfluous whitespace.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
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
			return truncateRunes(t, maxTitleRunes)
		}
	}
	return ""
}

// SessionUID maps a pi session id to the uid aii stores. pi ids are
// UUIDv7, whose first 8 hex digits are a timestamp prefix that only
// changes about once a minute, so two sessions started close together
// would share a short id. The stored uid moves the random last group to
// the front ("rrrrrrrrrrrr-tttttttt-tttt-7xxx-yxxx") so its first 8
// characters tell sessions apart. Ids that aren't UUIDv7 are kept as is.
func SessionUID(id string) string {
	if !isUUIDv7(id) {
		return id
	}
	return id[24:] + "-" + id[:23]
}

func isUUIDv7(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' || s[14] != '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// LookupSession resolves a session ref the way SessionByUIDAny does,
// and also accepts a full pi session id as pi prints it.
func LookupSession(db *store.DB, ref string) (*store.Session, error) {
	s, err := db.SessionByUIDAny(ref)
	if err != nil || s != nil {
		return s, err
	}
	if uid := SessionUID(ref); uid != ref {
		return db.SessionByUID(agentName, uid)
	}
	return nil, nil
}
