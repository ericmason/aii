// Package claudecode parses Claude Code JSONL session transcripts from
// ~/.claude/projects/*/*.jsonl and emits per-file Batch deltas.
//
// Each file corresponds to exactly one session (identified by the
// `sessionId` field inside the records, not the filename). The parser
// tracks incremental progress via store.IndexState so repeated scans
// only re-read new bytes appended to the file since the last run.
package claudecode

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
	agentName = "claude_code"

	// Truncation budgets for noisy content.
	maxToolResult = 4 * 1024
	maxToolInput  = 1 * 1024
	maxTitleRunes = 200

	// Scanner buffer sizes. Claude Code lines can be multi-megabyte
	// (large Read/Bash tool results), so we bump way past the 64 KB
	// default.
	scannerInitial = 1 << 20  // 1 MiB
	scannerMax     = 16 << 20 // 16 MiB
)

// Source implements source.Source for Claude Code.
type Source struct {
	// Root overrides the default scan root. Empty means ~/.claude/projects.
	Root string
}

// New returns a default Source rooted at ~/.claude/projects.
func New() *Source { return &Source{} }

// Name returns the agent identifier used for sessions.agent.
func (s *Source) Name() string { return agentName }

// resolveRoot returns the directory containing per-project session folders.
func (s *Source) resolveRoot() (string, error) {
	if s.Root != "" {
		return s.Root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// Scan walks the Claude Code projects tree and emits one Batch per JSONL
// file that has new content (or all files when full=true).
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
			log.Printf("claudecode: %s: %v", path, err)
			continue
		}
	}
	return nil
}

// scanFile processes a single JSONL file, emitting at most one Batch.
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
	case prev != nil && prev.Size > size:
		// File shrank: rotation or manual edit. Re-index from scratch.
		truncate = true
		startOffset = 0
	case !full && prev != nil && prev.Size == size && prev.MtimeNs == mtimeNs:
		return nil
	case prev != nil && !truncate:
		startOffset = prev.LastOffset
	}
	if full {
		truncate = true
		startOffset = 0
	}

	f, err := os.Open(abs)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	if startOffset > 0 {
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			return fmt.Errorf("seek: %w", err)
		}
	}

	sess, msgs, newOffset, err := parseDelta(ctx, f, startOffset, size)
	if err != nil {
		return err
	}
	if sess.UID == "" && len(msgs) == 0 {
		// Nothing useful — still persist state so we don't re-read next time.
		out <- source.Batch{
			Session:  store.Session{Agent: agentName, SourcePath: abs, SourceMtimeNs: mtimeNs, SourceSize: size},
			Messages: nil,
			State: store.IndexState{
				SourcePath: abs,
				MtimeNs:    mtimeNs,
				Size:       size,
				LastOffset: newOffset,
			},
			Truncate: truncate,
		}
		return nil
	}

	// Apply baseOrdinal so these messages continue after whatever the DB
	// already has for this session.
	baseOrdinal := 0
	if !truncate && sess.UID != "" {
		baseOrdinal, err = nextOrdinalForUID(db, sess.UID)
		if err != nil {
			return fmt.Errorf("base ordinal: %w", err)
		}
	}
	for i := range msgs {
		msgs[i].Ordinal += baseOrdinal
	}

	sess.Agent = agentName
	sess.SourcePath = abs
	sess.SourceMtimeNs = mtimeNs
	sess.SourceSize = size

	select {
	case <-ctx.Done():
		return ctx.Err()
	case out <- source.Batch{
		Session:  sess,
		Messages: msgs,
		State: store.IndexState{
			SourcePath: abs,
			MtimeNs:    mtimeNs,
			Size:       size,
			LastOffset: newOffset,
		},
		Truncate: truncate,
	}:
	}
	return nil
}

// nextOrdinalForUID returns the next ordinal for an existing claude_code
// session identified by session_uid, or 0 if no prior session exists.
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

// -------------------------------------------------------------------
// JSONL record parsing
// -------------------------------------------------------------------

// rawLine is the outer envelope common to all Claude Code records.
// We only decode the fields we care about; unknown fields are ignored.
type rawLine struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	CWD       string          `json:"cwd"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// innerMessage is what's inside the "message" field for user/assistant lines.
type innerMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentPart covers all nested content shapes (text, thinking, tool_use,
// tool_result). Not every field is populated for every type.
type contentPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

// parseDelta reads the file from its current position through EOF, parsing
// one JSON record per line. It returns the collected session metadata,
// message list (ordinals starting at 0), and the byte offset immediately
// after the last complete line consumed.
func parseDelta(ctx context.Context, r io.Reader, startOffset, size int64) (store.Session, []store.Message, int64, error) {
	var sess store.Session
	var msgs []store.Message

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, scannerInitial), scannerMax)

	offset := startOffset       // byte position of the NEXT unread line
	lastCompleteOffset := offset // where to resume on next run

	ordinal := 0
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return sess, msgs, lastCompleteOffset, ctx.Err()
		default:
		}
		lineStart := offset
		line := scanner.Bytes()
		// +1 for the \n scanner consumed. This isn't strictly correct if
		// the final line had no newline, but Scan only returns lines that
		// ended in a newline OR EOF; we reconcile EOF below.
		offset += int64(len(line)) + 1
		lastCompleteOffset = offset

		trimmed := trimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if m, ok := handleLine(trimmed, lineStart, &sess, ordinal); ok {
			for _, em := range m {
				em.Ordinal = ordinal
				ordinal++
				msgs = append(msgs, em)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// Skip forward past the broken line so we don't wedge forever,
		// but log it.
		log.Printf("claudecode: scanner error at offset %d: %v", lastCompleteOffset, err)
	}

	// If the file's last line has no trailing newline, Scan still returned
	// it but we have no way to know from here. We conservatively cap the
	// offset at `size` so SaveState never records beyond EOF.
	if lastCompleteOffset > size {
		lastCompleteOffset = size
	}

	// Finalize session bookkeeping.
	if len(msgs) > 0 {
		if sess.StartedAt == 0 {
			sess.StartedAt = msgs[0].TS
		}
		sess.EndedAt = msgs[len(msgs)-1].TS
		if sess.Title == "" {
			sess.Title = firstUserText(msgs)
		}
	}
	return sess, msgs, lastCompleteOffset, nil
}

// handleLine decodes a single JSONL record and returns 0, 1, or 2 messages.
// It also updates per-session metadata on `sess` in place. The bool is true
// if the line was parseable (even if it produced no messages).
//
// `lineStart` is the byte offset where this line began in the file, used
// to set Message.SourceOffset.
func handleLine(line []byte, lineStart int64, sess *store.Session, _ int) ([]store.Message, bool) {
	var rec rawLine
	if err := json.Unmarshal(line, &rec); err != nil {
		log.Printf("claudecode: bad json at offset %d: %v", lineStart, err)
		return nil, false
	}

	// Set session-level metadata opportunistically.
	if sess.UID == "" && rec.SessionID != "" {
		sess.UID = rec.SessionID
	}
	if sess.Workspace == "" && rec.CWD != "" {
		sess.Workspace = rec.CWD
	}

	switch rec.Type {
	case "user":
		return userMessage(rec, lineStart), true
	case "assistant":
		return assistantMessage(rec, lineStart), true
	case "progress", "hook_progress", "file-history-snapshot",
		"queue-operation", "system", "last-prompt":
		return nil, true
	default:
		// Unknown type: ignore silently, forward-compatible.
		return nil, true
	}
}

// userMessage extracts zero-or-one user message from a "type":"user" line.
// Content may be a bare string or an array of content parts.
func userMessage(rec rawLine, lineStart int64) []store.Message {
	var inner innerMessage
	if len(rec.Message) == 0 || json.Unmarshal(rec.Message, &inner) != nil {
		return nil
	}
	text := extractUserContent(inner.Content)
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return []store.Message{{
		Role:         "user",
		TS:           parseTS(rec.Timestamp),
		Content:      text,
		SourceOffset: lineStart,
	}}
}

// extractUserContent handles the three shapes a user content field can take.
func extractUserContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Case 1: plain string.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	// Case 2: array of parts.
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			if s := strings.TrimSpace(p.Text); s != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(s)
			}
		case "tool_result":
			body := extractToolResultContent(p.Content)
			body = truncateRunes(body, maxToolResult)
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			tag := "[tool_result"
			if p.IsError {
				tag += " error"
			}
			if p.ToolUseID != "" {
				tag += " id=" + shortID(p.ToolUseID)
			}
			tag += "] "
			b.WriteString(tag)
			b.WriteString(body)
		}
	}
	return b.String()
}

// extractToolResultContent handles the two shapes a tool_result content
// field can take: a plain string or an array of {type:text,text} parts.
func extractToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// assistantMessage extracts up to two messages per assistant line: a main
// assistant message (text + tool_use) and an optional thinking message.
func assistantMessage(rec rawLine, lineStart int64) []store.Message {
	var inner innerMessage
	if len(rec.Message) == 0 || json.Unmarshal(rec.Message, &inner) != nil {
		return nil
	}
	var parts []contentPart
	if err := json.Unmarshal(inner.Content, &parts); err != nil {
		return nil
	}

	ts := parseTS(rec.Timestamp)

	var mainBuf, thinkBuf strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			if s := strings.TrimSpace(p.Text); s != "" {
				if mainBuf.Len() > 0 {
					mainBuf.WriteString("\n")
				}
				mainBuf.WriteString(s)
			}
		case "thinking":
			if s := strings.TrimSpace(p.Thinking); s != "" {
				if thinkBuf.Len() > 0 {
					thinkBuf.WriteString("\n")
				}
				thinkBuf.WriteString(s)
			}
		case "tool_use":
			compact := compactJSON(p.Input)
			compact = truncateRunes(compact, maxToolInput)
			if mainBuf.Len() > 0 {
				mainBuf.WriteString("\n")
			}
			fmt.Fprintf(&mainBuf, "[tool_use name=%s] %s", p.Name, compact)
		}
	}

	var out []store.Message
	if s := strings.TrimSpace(mainBuf.String()); s != "" {
		out = append(out, store.Message{
			Role:         "assistant",
			TS:           ts,
			Content:      s,
			SourceOffset: lineStart,
		})
	}
	if s := strings.TrimSpace(thinkBuf.String()); s != "" {
		out = append(out, store.Message{
			Role:         "thinking",
			TS:           ts,
			Content:      s,
			SourceOffset: lineStart,
		})
	}
	return out
}

// -------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------

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
// Falls back to the original bytes if re-encoding fails.
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
// it was trimmed. Safe on invalid UTF-8.
func truncateRunes(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// firstUserText returns the first user message's content, truncated for title use.
func firstUserText(msgs []store.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			t := strings.TrimSpace(m.Content)
			// Single-line: titles shouldn't contain newlines.
			if i := strings.IndexByte(t, '\n'); i >= 0 {
				t = t[:i]
			}
			return truncateRunes(t, maxTitleRunes)
		}
	}
	return ""
}

// shortID trims Claude's tool_use_... ids to something more eyeball-friendly.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// trimSpace is a byte-slice wrapper around strings.TrimSpace to avoid an
// allocation when the line is already clean. Returns the same slice when
// no trimming is needed.
func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end {
		c := b[start]
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			break
		}
		start++
	}
	for end > start {
		c := b[end-1]
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			break
		}
		end--
	}
	return b[start:end]
}
