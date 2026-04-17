// Package codex parses Codex CLI rollout JSONL files and the shared
// ~/.codex/history.jsonl prompt log into normalized Batches for indexing.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	srcpkg "github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

// Source implements source.Source for the Codex CLI.
type Source struct{}

// New constructs a new Codex source.
func New() *Source { return &Source{} }

// Name returns the agent name used for DB rows and state lookups.
func (*Source) Name() string { return "codex" }

// Scan walks the Codex session tree and history file, emitting one Batch
// per session that has new data since the last run (or every session when
// full is true).
func (s *Source) Scan(ctx context.Context, db *store.DB, full bool, out chan<- srcpkg.Batch) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}

	sessionsRoot := filepath.Join(home, ".codex", "sessions")
	if err := s.scanRollouts(ctx, db, full, out, sessionsRoot); err != nil {
		return err
	}

	historyPath := filepath.Join(home, ".codex", "history.jsonl")
	if err := s.scanHistory(ctx, db, full, out, historyPath); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rollout scan
// ---------------------------------------------------------------------------

// Top-level envelope for every rollout line.
type rolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// session_meta payload.
type sessionMetaPayload struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
}

// response_item payload has a nested type discriminator.
type responseItemHead struct {
	Type string `json:"type"`
	Role string `json:"role"`
}

// Content element shared across message + reasoning payloads.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messagePayload struct {
	Type    string        `json:"type"`
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type reasoningPayload struct {
	Type             string        `json:"type"`
	Summary          []contentPart `json:"summary"`
	Content          []contentPart `json:"content"`
	EncryptedContent string        `json:"encrypted_content"`
}

type functionCallPayload struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionCallOutputPayload struct {
	Type   string `json:"type"`
	Output string `json:"output"`
}

func (s *Source) scanRollouts(ctx context.Context, db *store.DB, full bool, out chan<- srcpkg.Batch, root string) error {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			fmt.Fprintf(os.Stderr, "codex: walk %s: %v\n", path, werr)
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if err := s.scanRolloutFile(ctx, db, full, out, path); err != nil {
			if err == context.Canceled || err == context.DeadlineExceeded {
				return err
			}
			fmt.Fprintf(os.Stderr, "codex: scan %s: %v\n", path, err)
		}
		return nil
	})
}

func (s *Source) scanRolloutFile(ctx context.Context, db *store.DB, full bool, out chan<- srcpkg.Batch, path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}

	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	size := fi.Size()
	mtimeNs := fi.ModTime().UnixNano()

	prev, err := db.GetState(absPath)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}

	truncate := false
	startOffset := int64(0)
	if prev != nil {
		if size < prev.Size {
			// File rotated/shrunk.
			truncate = true
			startOffset = 0
		} else {
			if !full && size == prev.Size && mtimeNs == prev.MtimeNs {
				return nil
			}
			startOffset = prev.LastOffset
		}
	}
	if full {
		truncate = true
		startOffset = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if startOffset > 0 {
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			return err
		}
	}

	reader := bufio.NewReaderSize(f, 1<<20)

	var (
		sessionUID    string
		workspace     string
		startedAt     int64
		title         string
		messages      []store.Message
		lastTS        int64
		readOffset    = startOffset
		lastLineStart = startOffset
		completeEOF   = false
	)

	// Determine ordinal base from DB. We don't know sessionUID yet when the
	// file is resumed from mid-way, but session_meta appears on line 1 only.
	// If startOffset > 0 we must look up via the existing session row before
	// emitting messages. We do that lazily once we discover the UID.
	ordinalBase := 0
	ordinalKnown := false

	lookupOrdinal := func(uid string) (int, error) {
		if uid == "" {
			return 0, nil
		}
		var n int64
		err := db.DB.QueryRow(`
			SELECT COALESCE(MAX(ordinal),-1)+1 FROM messages
			WHERE session_id = (SELECT id FROM sessions WHERE agent='codex' AND session_uid=?)`, uid).Scan(&n)
		if err != nil {
			// No row or other error — default to 0.
			return 0, nil
		}
		return int(n), nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		lineBytes, err := reader.ReadBytes('\n')
		hasNewline := err == nil
		if len(lineBytes) > 0 {
			if hasNewline {
				// Full line consumed, advance lastLineStart past it.
				lastLineStart = readOffset + int64(len(lineBytes))
			}
			readOffset += int64(len(lineBytes))
		}
		if err == io.EOF {
			if len(lineBytes) == 0 {
				completeEOF = true
				break
			}
			// Incomplete trailing line — don't parse or advance lastLineStart.
			completeEOF = true
			break
		}
		if err != nil && err != io.EOF {
			return fmt.Errorf("read rollout: %w", err)
		}
		if !hasNewline {
			// Shouldn't happen except on EOF handled above.
			break
		}
		trimmed := strings.TrimRight(string(lineBytes), "\r\n")
		if trimmed == "" {
			continue
		}

		var line rolloutLine
		if err := json.Unmarshal([]byte(trimmed), &line); err != nil {
			fmt.Fprintf(os.Stderr, "codex: malformed line in %s: %v\n", path, err)
			continue
		}

		ts := parseTimestamp(line.Timestamp)

		switch line.Type {
		case "session_meta":
			var meta sessionMetaPayload
			if err := json.Unmarshal(line.Payload, &meta); err != nil {
				fmt.Fprintf(os.Stderr, "codex: bad session_meta in %s: %v\n", path, err)
				continue
			}
			if meta.ID != "" {
				sessionUID = meta.ID
			}
			if meta.Cwd != "" {
				workspace = meta.Cwd
			}
			if meta.Timestamp != "" {
				if sec := parseTimestamp(meta.Timestamp); sec > 0 {
					startedAt = sec
				}
			}
			if startedAt == 0 {
				startedAt = ts
			}
		case "response_item":
			var head responseItemHead
			if err := json.Unmarshal(line.Payload, &head); err != nil {
				fmt.Fprintf(os.Stderr, "codex: bad response_item in %s: %v\n", path, err)
				continue
			}
			msg, ok := buildMessage(head, line.Payload, ts)
			if !ok {
				continue
			}
			if !ordinalKnown {
				if truncate {
					ordinalBase = 0
				} else {
					ordinalBase, _ = lookupOrdinal(sessionUID)
				}
				ordinalKnown = true
			}
			msg.Ordinal = ordinalBase + len(messages)
			msg.SourceOffset = lastLineStart - int64(len(lineBytes))
			messages = append(messages, msg)
			if ts > lastTS {
				lastTS = ts
			}
			if title == "" && msg.Role == "user" {
				title = truncateRunes(msg.Content, 200)
			}
		default:
			// Skip all other line types (event_msg, function_call, etc.
			// that appear at the top level; we only care about
			// response_item's nested payloads).
		}
	}

	// Determine new LastOffset: start of the incomplete tail if any, else
	// current readOffset (EOF).
	newLastOffset := lastLineStart
	if completeEOF {
		newLastOffset = readOffset
	}

	if sessionUID == "" {
		// Can happen when resuming a file whose session_meta was in the
		// already-consumed prefix. Try to recover via SessionByUIDAny isn't
		// possible without UID — fall back to path-based lookup.
		if prev != nil {
			uid, werr := sessionUIDFromPath(db, absPath)
			if werr == nil && uid != "" {
				sessionUID = uid
			}
		}
	}

	if sessionUID == "" {
		// Nothing we can anchor messages to. Still record state so we
		// don't spin on the same file forever.
		state := store.IndexState{
			SourcePath: absPath,
			MtimeNs:    mtimeNs,
			Size:       size,
			LastOffset: newLastOffset,
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- srcpkg.Batch{State: state, Truncate: truncate}:
		}
		return nil
	}

	endedAt := lastTS
	if endedAt == 0 && prev != nil {
		// keep whatever was there before — leave zero so store keeps prior.
	}

	session := store.Session{
		Agent:         "codex",
		UID:           sessionUID,
		Workspace:     workspace,
		Title:         title,
		StartedAt:     startedAt,
		EndedAt:       endedAt,
		SourcePath:    absPath,
		SourceMtimeNs: mtimeNs,
		SourceSize:    size,
	}

	state := store.IndexState{
		SourcePath: absPath,
		MtimeNs:    mtimeNs,
		Size:       size,
		LastOffset: newLastOffset,
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case out <- srcpkg.Batch{
		Session:  session,
		Messages: messages,
		State:    state,
		Truncate: truncate,
	}:
	}
	return nil
}

// buildMessage turns a response_item payload into a store.Message, or
// returns ok=false to skip. It embeds role-specific parsing.
func buildMessage(head responseItemHead, payload json.RawMessage, ts int64) (store.Message, bool) {
	switch head.Type {
	case "message":
		var m messagePayload
		if err := json.Unmarshal(payload, &m); err != nil {
			return store.Message{}, false
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "user" && role != "assistant" {
			return store.Message{}, false
		}
		var parts []string
		for _, p := range m.Content {
			if p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
		text := strings.Join(parts, "\n")
		text = strings.TrimSpace(text)
		if text == "" {
			return store.Message{}, false
		}
		if role == "user" && strings.HasPrefix(text, "<turn_aborted>") {
			return store.Message{}, false
		}
		return store.Message{Role: role, TS: ts, Content: text}, true

	case "reasoning":
		var r reasoningPayload
		if err := json.Unmarshal(payload, &r); err != nil {
			return store.Message{}, false
		}
		var parts []string
		for _, p := range r.Summary {
			if p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
		for _, p := range r.Content {
			if p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
		text := strings.TrimSpace(strings.Join(parts, "\n"))
		if text == "" {
			// encrypted_content alone — skip.
			return store.Message{}, false
		}
		return store.Message{Role: "thinking", TS: ts, Content: text}, true

	case "function_call":
		var fc functionCallPayload
		if err := json.Unmarshal(payload, &fc); err != nil {
			return store.Message{}, false
		}
		args := truncateBytes(fc.Arguments, 1024)
		content := "[tool_call name=" + fc.Name + "] " + args
		return store.Message{Role: "tool", TS: ts, Content: content}, true

	case "function_call_output":
		var fo functionCallOutputPayload
		if err := json.Unmarshal(payload, &fo); err != nil {
			return store.Message{}, false
		}
		out := truncateBytes(fo.Output, 4096)
		return store.Message{Role: "tool", TS: ts, Content: "[tool_result] " + out}, true
	}
	return store.Message{}, false
}

func sessionUIDFromPath(db *store.DB, path string) (string, error) {
	var uid string
	err := db.DB.QueryRow(`SELECT session_uid FROM sessions WHERE source_path = ? AND agent='codex' LIMIT 1`, path).Scan(&uid)
	if err != nil {
		return "", err
	}
	return uid, nil
}

// ---------------------------------------------------------------------------
// history.jsonl scan (fallback)
// ---------------------------------------------------------------------------

type historyLine struct {
	SessionID string `json:"session_id"`
	TS        int64  `json:"ts"`
	Text      string `json:"text"`
}

func (s *Source) scanHistory(ctx context.Context, db *store.DB, full bool, out chan<- srcpkg.Batch, path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	size := fi.Size()
	mtimeNs := fi.ModTime().UnixNano()

	prev, err := db.GetState(absPath)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}

	truncate := false
	startOffset := int64(0)
	if prev != nil {
		if size < prev.Size {
			truncate = true
			startOffset = 0
		} else {
			if !full && size == prev.Size && mtimeNs == prev.MtimeNs {
				return nil
			}
			startOffset = prev.LastOffset
		}
	}
	if full {
		truncate = true
		startOffset = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if startOffset > 0 {
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			return err
		}
	}

	reader := bufio.NewReaderSize(f, 1<<20)

	readOffset := startOffset
	lastLineStart := startOffset
	completeEOF := false

	sessionExists := func(uid string) (bool, error) {
		var n int
		err := db.DB.QueryRow(`SELECT 1 FROM sessions WHERE session_uid = ? LIMIT 1`, uid).Scan(&n)
		if err != nil {
			return false, nil //nolint:nilerr
		}
		return true, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		lineBytes, err := reader.ReadBytes('\n')
		hasNewline := err == nil
		lineStart := readOffset
		if len(lineBytes) > 0 {
			if hasNewline {
				lastLineStart = readOffset + int64(len(lineBytes))
			}
			readOffset += int64(len(lineBytes))
		}
		if err == io.EOF {
			if len(lineBytes) == 0 {
				completeEOF = true
				break
			}
			completeEOF = true
			break
		}
		if err != nil && err != io.EOF {
			return fmt.Errorf("read history: %w", err)
		}
		if !hasNewline {
			break
		}
		trimmed := strings.TrimRight(string(lineBytes), "\r\n")
		if trimmed == "" {
			continue
		}

		var h historyLine
		if err := json.Unmarshal([]byte(trimmed), &h); err != nil {
			fmt.Fprintf(os.Stderr, "codex: malformed history line: %v\n", err)
			continue
		}
		if h.SessionID == "" || strings.TrimSpace(h.Text) == "" {
			continue
		}
		if strings.HasPrefix(h.Text, "<permissions instructions>") ||
			strings.HasPrefix(h.Text, "<turn_aborted>") {
			continue
		}
		exists, _ := sessionExists(h.SessionID)
		if exists {
			continue
		}

		session := store.Session{
			Agent:         "codex",
			UID:           h.SessionID,
			Workspace:     "",
			Title:         truncateRunes(h.Text, 200),
			StartedAt:     h.TS,
			EndedAt:       h.TS,
			SourcePath:    absPath,
			SourceMtimeNs: mtimeNs,
			SourceSize:    size,
		}
		msg := store.Message{
			Ordinal:      0,
			Role:         "user",
			TS:           h.TS,
			Content:      h.Text,
			SourceOffset: lineStart,
		}

		// Per-session state row: set LastOffset progressively as we go so
		// that a mid-stream failure on the next line still checkpoints. We
		// emit the file-level state at the end; the per-session batch uses
		// a state whose path matches the real file so the indexer saves it
		// monotonically. The final batch overwrites with the final offset.
		batchState := store.IndexState{
			SourcePath: absPath,
			MtimeNs:    mtimeNs,
			Size:       size,
			LastOffset: readOffset,
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- srcpkg.Batch{
			Session:  session,
			Messages: []store.Message{msg},
			State:    batchState,
			Truncate: truncate && lineStart == startOffset,
		}:
		}
		// Only truncate once, on the first emitted batch.
		truncate = false
	}

	finalOffset := lastLineStart
	if completeEOF {
		finalOffset = readOffset
	}

	// Emit a trailing state-only batch so the file-level offset is always
	// persisted even when no new sessions were discovered.
	state := store.IndexState{
		SourcePath: absPath,
		MtimeNs:    mtimeNs,
		Size:       size,
		LastOffset: finalOffset,
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case out <- srcpkg.Batch{State: state, Truncate: truncate}:
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseTimestamp(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339, s); err2 == nil {
			return t2.Unix()
		}
		return 0
	}
	return t.Unix()
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func truncateBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	// Snap to a rune boundary to avoid splitting a multibyte sequence.
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}
