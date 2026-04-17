// Package cursor parses Cursor (VSCode-fork) agent composer conversations
// out of its SQLite state stores and emits a Batch per composer.
//
// Cursor stores agent state in two places on macOS:
//
//   - Per-workspace DBs:
//       ~/Library/Application Support/Cursor/User/workspaceStorage/<hash>/state.vscdb
//     Each has an ItemTable row `composer.composerData` listing the
//     composer IDs that belong to that workspace. We use this to build a
//     composerId -> workspacePath map (via the sibling workspace.json).
//
//   - Global DB:
//       ~/Library/Application Support/Cursor/User/globalStorage/state.vscdb
//     The `cursorDiskKV` table holds the real conversation data:
//       composerData:<composerId> -> JSON with an ordered list of bubbles
//                                    in `fullConversationHeadersOnly`
//       bubbleId:<composerId>:<bubbleId> -> JSON for each bubble
//
// The parser walks workspace DBs first (cheap, small), then streams the
// global composerData rows and, for composers that have grown since the
// last scan (tracked via store.IndexState fingerprints), fetches the new
// bubbles and emits them as source.Batch values.
package cursor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"

	_ "modernc.org/sqlite"
)

const (
	agentName = "cursor"

	maxTitleRunes   = 200
	maxToolArgRunes = 512

	busyRetries    = 5
	busyBackoffDur = 100 * time.Millisecond
)

// Source implements source.Source for Cursor.
type Source struct {
	// Root overrides the default Cursor User directory (useful for tests).
	// If empty, resolves to ~/Library/Application Support/Cursor/User.
	Root string
}

// New returns a default Cursor Source.
func New() *Source { return &Source{} }

// Name returns the agent identifier used for sessions.agent.
func (s *Source) Name() string { return agentName }

// resolveRoot returns the Cursor User directory containing workspaceStorage
// and globalStorage subdirectories.
func (s *Source) resolveRoot() (string, error) {
	if s.Root != "" {
		return s.Root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "Cursor", "User"), nil
}

// Scan walks Cursor storage, resolves the composerId -> workspace map
// from per-workspace DBs, then streams composer rows out of the global
// DB and emits a Batch per composer that is new or has grown.
func (s *Source) Scan(ctx context.Context, db *store.DB, full bool, out chan<- source.Batch) error {
	root, err := s.resolveRoot()
	if err != nil {
		return err
	}

	wsMap, wsCount := s.buildWorkspaceMap(ctx, root)
	log.Printf("cursor: found %d workspaces, %d composer->workspace mappings", wsCount, len(wsMap))

	globalDB := filepath.Join(root, "globalStorage", "state.vscdb")
	if _, err := os.Stat(globalDB); err != nil {
		if os.IsNotExist(err) {
			log.Printf("cursor: global DB not present at %s, nothing to scan", globalDB)
			return nil
		}
		return fmt.Errorf("stat global DB: %w", err)
	}

	gdb, err := openRO(globalDB)
	if err != nil {
		return fmt.Errorf("open global DB: %w", err)
	}
	defer gdb.Close()

	// Prepared per-bubble lookup.
	bubbleStmt, err := gdb.PrepareContext(ctx, `SELECT value FROM cursorDiskKV WHERE key = ?`)
	if err != nil {
		return fmt.Errorf("prepare bubble query: %w", err)
	}
	defer bubbleStmt.Close()

	rows, err := gdb.QueryContext(ctx, `SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%'`)
	if err != nil {
		return fmt.Errorf("query composerData: %w", err)
	}
	defer rows.Close()

	var composersSeen, composersEmitted, composersSkipped int
	for rows.Next() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		var key string
		var val []byte
		if err := rows.Scan(&key, &val); err != nil {
			log.Printf("cursor: composerData scan: %v", err)
			continue
		}
		composersSeen++

		composerID := strings.TrimPrefix(key, "composerData:")
		if composerID == "" || composerID == key {
			continue
		}

		cd, err := parseComposerData(val)
		if err != nil {
			log.Printf("cursor: composer %s: parse composerData: %v", composerID, err)
			continue
		}
		if len(cd.Headers) == 0 {
			composersSkipped++
			continue
		}

		if emitted, err := s.scanComposer(ctx, db, bubbleStmt, composerID, cd, wsMap, full, out); err != nil {
			log.Printf("cursor: composer %s: %v", composerID, err)
			continue
		} else if emitted {
			composersEmitted++
		} else {
			composersSkipped++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate composerData: %w", err)
	}
	log.Printf("cursor: composers seen=%d emitted=%d skipped=%d", composersSeen, composersEmitted, composersSkipped)
	return nil
}

// -------------------------------------------------------------------
// Workspace-storage layer: build composerId -> workspacePath map.
// -------------------------------------------------------------------

// buildWorkspaceMap scans ~/Library/.../Cursor/User/workspaceStorage/*/state.vscdb
// and returns composerId -> workspace path. workspaceCount is how many DBs
// we were able to open.
func (s *Source) buildWorkspaceMap(ctx context.Context, root string) (map[string]string, int) {
	m := map[string]string{}
	wsRoot := filepath.Join(root, "workspaceStorage")
	entries, err := filepath.Glob(filepath.Join(wsRoot, "*", "state.vscdb"))
	if err != nil {
		log.Printf("cursor: glob workspace DBs: %v", err)
		return m, 0
	}
	opened := 0
	for _, dbPath := range entries {
		select {
		case <-ctx.Done():
			return m, opened
		default:
		}
		hashDir := filepath.Dir(dbPath)
		wsPath := readWorkspaceJSON(filepath.Join(hashDir, "workspace.json"))
		ids, err := readComposerIDsFromWorkspaceDB(ctx, dbPath)
		if err != nil {
			log.Printf("cursor: workspace DB %s: %v", dbPath, err)
			continue
		}
		opened++
		for _, id := range ids {
			if id == "" {
				continue
			}
			// First one wins; workspaces rarely share composer IDs.
			if _, ok := m[id]; !ok {
				m[id] = wsPath
			}
		}
	}
	return m, opened
}

// readWorkspaceJSON parses workspace.json and returns the folder/configuration
// URI converted to a local filesystem path, or "" if unreadable.
func readWorkspaceJSON(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var ws struct {
		Folder        string `json:"folder"`
		Configuration string `json:"configuration"`
	}
	if err := json.Unmarshal(data, &ws); err != nil {
		return ""
	}
	uri := ws.Folder
	if uri == "" {
		uri = ws.Configuration
	}
	if uri == "" {
		return ""
	}
	return fileURIToPath(uri)
}

// fileURIToPath converts `file:///...` to a filesystem path. Non-file URIs
// are returned as-is (they may point into vscode-remote or similar).
func fileURIToPath(uri string) string {
	if strings.HasPrefix(uri, "file://") {
		u, err := url.Parse(uri)
		if err != nil {
			// Fallback: naive strip.
			p := strings.TrimPrefix(uri, "file://")
			if dec, derr := url.PathUnescape(p); derr == nil {
				return dec
			}
			return p
		}
		return u.Path
	}
	return uri
}

// readComposerIDsFromWorkspaceDB opens a workspace state.vscdb read-only and
// returns the list of composer IDs referenced by its composer.composerData row.
func readComposerIDsFromWorkspaceDB(ctx context.Context, path string) ([]string, error) {
	db, err := openRO(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	var val []byte
	err = db.QueryRowContext(ctx, `SELECT value FROM ItemTable WHERE key = 'composer.composerData'`).Scan(&val)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query ItemTable: %w", err)
	}
	var payload struct {
		AllComposers []struct {
			ComposerID string `json:"composerId"`
		} `json:"allComposers"`
	}
	if err := json.Unmarshal(val, &payload); err != nil {
		return nil, fmt.Errorf("parse composer.composerData: %w", err)
	}
	out := make([]string, 0, len(payload.AllComposers))
	for _, c := range payload.AllComposers {
		if c.ComposerID != "" {
			out = append(out, c.ComposerID)
		}
	}
	return out, nil
}

// -------------------------------------------------------------------
// Global-storage layer: composerData + bubble decoding.
// -------------------------------------------------------------------

// composerHeader is one entry in composerData.fullConversationHeadersOnly.
type composerHeader struct {
	BubbleID       string `json:"bubbleId"`
	Type           int    `json:"type"`
	ServerBubbleID string `json:"serverBubbleId"`
}

// composerDataDoc is the subset of composerData:<id> JSON we care about.
type composerDataDoc struct {
	Headers   []composerHeader          `json:"fullConversationHeadersOnly"`
	CreatedAt int64                     `json:"createdAt"` // ms
	UpdatedAt int64                     `json:"lastUpdatedAt"`
	Title     string                    `json:"name"`
	Summary   latestConversationSummary `json:"latestConversationSummary"`
}

// latestConversationSummary mirrors the rolling summary Cursor maintains
// for long conversations. The useful human text lives a few levels deep.
type latestConversationSummary struct {
	Summary struct {
		Summary string `json:"summary"`
	} `json:"summary"`
}

// humanSummary turns Cursor's rolling latestConversationSummary into a
// compact plain-text digest: boilerplate preamble stripped, tool-call
// noise dropped, attached-file context removed, and the actual
// user/assistant prose kept with role labels.
func (cd composerDataDoc) humanSummary() string {
	raw := strings.TrimSpace(cd.Summary.Summary.Summary)
	if raw == "" {
		return ""
	}

	// 1. Drop everything up to the first content block — the preamble is
	//    always the same boilerplate paragraph.
	s := raw
	for _, start := range []string{"<previous_user_message>", "<previous_assistant_message>"} {
		if i := strings.Index(s, start); i >= 0 {
			s = s[i:]
			break
		}
	}

	// 2. Nuke blocks we don't want to see in the digest (content included).
	for _, tag := range []string{
		"previous_tool_call", "additional_data",
		"attached_files", "file_contents", "last_terminal_cwd",
	} {
		s = stripXMLBlocks(s, tag)
	}

	// 3. Extract readable role-labeled prose from remaining blocks.
	var parts []string
	for _, block := range []struct{ tag, label string }{
		{"previous_user_message", "User"},
		{"previous_assistant_message", "Assistant"},
	} {
		for _, inner := range innerXMLBlocks(s, block.tag) {
			inner = stripUserQueryTags(inner)
			inner = strings.TrimSpace(inner)
			if inner == "" {
				continue
			}
			parts = append(parts, block.label+": "+inner)
		}
	}

	out := strings.Join(parts, "\n\n")
	out = strings.ReplaceAll(out, "<omitted />", "…")
	// Collapse runs of blank lines the XML strip may have introduced.
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(out)
}

// stripXMLBlocks removes <tag>...</tag> spans (non-recursive, case-sensitive)
// from s.
func stripXMLBlocks(s, tag string) string {
	open := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return s
		}
		rest := s[i+len(open):]
		j := strings.Index(rest, closeTag)
		if j < 0 {
			// Unterminated — drop from the opener onward.
			return strings.TrimRight(s[:i], " \n\t")
		}
		s = s[:i] + rest[j+len(closeTag):]
	}
}

// innerXMLBlocks returns the text inside every <tag>...</tag> span in s
// (non-recursive).
func innerXMLBlocks(s, tag string) []string {
	open := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	var out []string
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return out
		}
		rest := s[i+len(open):]
		j := strings.Index(rest, closeTag)
		if j < 0 {
			out = append(out, rest)
			return out
		}
		out = append(out, rest[:j])
		s = rest[j+len(closeTag):]
	}
}

// stripUserQueryTags keeps the inner text of <user_query>…</user_query>
// (and a couple of related wrappers) while dropping the tags themselves.
func stripUserQueryTags(s string) string {
	for _, tag := range []string{"user_query", "user_message", "assistant_message"} {
		open := "<" + tag + ">"
		closeTag := "</" + tag + ">"
		s = strings.ReplaceAll(s, open, "")
		s = strings.ReplaceAll(s, closeTag, "")
	}
	return s
}

// parseComposerData decodes the JSON blob stored under composerData:<id>.
func parseComposerData(raw []byte) (composerDataDoc, error) {
	var cd composerDataDoc
	if len(raw) == 0 {
		return cd, errors.New("empty value")
	}
	if err := json.Unmarshal(raw, &cd); err != nil {
		return cd, err
	}
	return cd, nil
}

// bubbleDoc is the subset of a bubble payload we extract content from. Bubble
// JSON is wide and varies across Cursor versions — we only peek at the fields
// we need, and we use json.RawMessage for the structured ones to be lenient.
type bubbleDoc struct {
	Text           string          `json:"text"`
	RichText       string          `json:"richText"`
	Content        string          `json:"content"`
	Message        string          `json:"message"`
	ToolFormerData json.RawMessage `json:"toolFormerData"`
	ToolCalls      json.RawMessage `json:"tool_calls"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	ToolCallID     string          `json:"toolCallId"`
}

// extractBubbleText returns the best-effort human-readable text of a bubble.
// Returns "" if the bubble has no useful content.
func extractBubbleText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var b bubbleDoc
	if err := json.Unmarshal(raw, &b); err != nil {
		return ""
	}
	// 1. Prefer direct plain text fields.
	for _, s := range []string{b.Text, b.Content, b.Message} {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	// 2. Tool interactions: toolFormerData and tool_calls.
	if summary := summarizeToolFormer(b.ToolFormerData); summary != "" {
		return summary
	}
	if summary := summarizeToolCalls(b.ToolCalls); summary != "" {
		return summary
	}
	// 3. Loose-field tool shape.
	if b.Name != "" {
		args := truncateRunes(compactJSON(b.Arguments), maxToolArgRunes)
		if args == "" || args == "null" {
			return fmt.Sprintf("[tool name=%s]", b.Name)
		}
		return fmt.Sprintf("[tool name=%s args=%s]", b.Name, args)
	}
	// 4. richText: last resort — don't try to decode Lexical, just skip.
	_ = b.RichText
	return ""
}

// summarizeToolFormer looks for {name, args|arguments|rawArgs} inside a
// toolFormerData blob and renders a one-line summary.
func summarizeToolFormer(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var t struct {
		Name      string          `json:"name"`
		Tool      string          `json:"tool"`
		Args      json.RawMessage `json:"args"`
		Arguments json.RawMessage `json:"arguments"`
		RawArgs   string          `json:"rawArgs"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return ""
	}
	name := t.Name
	if name == "" {
		name = t.Tool
	}
	if name == "" {
		return ""
	}
	args := t.RawArgs
	if args == "" && len(t.Args) > 0 {
		args = compactJSON(t.Args)
	}
	if args == "" && len(t.Arguments) > 0 {
		args = compactJSON(t.Arguments)
	}
	args = truncateRunes(args, maxToolArgRunes)
	if args == "" {
		return fmt.Sprintf("[tool name=%s]", name)
	}
	return fmt.Sprintf("[tool name=%s args=%s]", name, args)
}

// summarizeToolCalls renders the first tool_calls entry, if any.
func summarizeToolCalls(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var calls []struct {
		Name      string          `json:"name"`
		Function  json.RawMessage `json:"function"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil || len(calls) == 0 {
		return ""
	}
	c := calls[0]
	name := c.Name
	if name == "" && len(c.Function) > 0 {
		var fn struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(c.Function, &fn); err == nil {
			name = fn.Name
			if len(c.Arguments) == 0 {
				c.Arguments = fn.Arguments
			}
		}
	}
	if name == "" {
		return ""
	}
	args := truncateRunes(compactJSON(c.Arguments), maxToolArgRunes)
	if args == "" || args == "null" {
		return fmt.Sprintf("[tool name=%s]", name)
	}
	return fmt.Sprintf("[tool name=%s args=%s]", name, args)
}

// -------------------------------------------------------------------
// Per-composer scan + Batch emission.
// -------------------------------------------------------------------

// scanComposer decides whether this composer needs (re-)indexing and, if so,
// fetches its new bubbles and sends a Batch. Returns true if a Batch was
// emitted.
func (s *Source) scanComposer(
	ctx context.Context,
	db *store.DB,
	bubbleStmt *sql.Stmt,
	composerID string,
	cd composerDataDoc,
	wsMap map[string]string,
	full bool,
	out chan<- source.Batch,
) (bool, error) {
	sourcePath := "cursor:composer:" + composerID
	fp := "count=" + strconv.Itoa(len(cd.Headers))

	prev, err := db.GetState(sourcePath)
	if err != nil {
		return false, fmt.Errorf("get state: %w", err)
	}

	if !full && prev != nil && prev.Fingerprint == fp {
		return false, nil
	}

	// Decide where to start reading bubbles and whether to truncate.
	truncate := false
	baseIdx := 0
	prevCount := 0
	if prev != nil {
		prevCount = parseCountFingerprint(prev.Fingerprint)
		if prevCount < 0 {
			prevCount = int(prev.LastOffset)
		}
	}
	switch {
	case full:
		truncate = true
		baseIdx = 0
	case prev == nil:
		baseIdx = 0
	case prevCount > len(cd.Headers):
		// Shrank — re-index from scratch.
		truncate = true
		baseIdx = 0
	default:
		baseIdx = prevCount
		if baseIdx > len(cd.Headers) {
			baseIdx = len(cd.Headers)
		}
	}

	// baseOrdinal mirrors the DB's existing message count so new bubbles
	// continue from the right spot. On truncate we start at 0.
	baseOrdinal := 0
	if !truncate {
		baseOrdinal, err = nextOrdinalForUID(db, composerID)
		if err != nil {
			return false, fmt.Errorf("base ordinal: %w", err)
		}
	}

	var msgs []store.Message
	for i := baseIdx; i < len(cd.Headers); i++ {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}
		h := cd.Headers[i]
		if h.BubbleID == "" {
			continue
		}
		raw, err := fetchBubble(ctx, bubbleStmt, composerID, h.BubbleID)
		if err != nil {
			log.Printf("cursor: composer %s bubble %s: %v", composerID, h.BubbleID, err)
			continue
		}
		if len(raw) == 0 {
			continue
		}
		text := strings.TrimSpace(extractBubbleText(raw))
		if text == "" {
			continue
		}
		role := "assistant"
		if h.Type == 1 {
			role = "user"
		}
		msgs = append(msgs, store.Message{
			Ordinal: baseOrdinal + len(msgs),
			Role:    role,
			TS:      0,
			Content: text,
		})
	}

	// Compose session metadata. started_at comes from the first header
	// with a createdAt on the composerData doc (preferred), or from the
	// workspace DB's allComposers entry if we had stored it — we only
	// parse createdAt off composerData here for simplicity.
	startedAt := int64(0)
	if cd.CreatedAt > 0 {
		startedAt = cd.CreatedAt / 1000
	}

	title := strings.TrimSpace(cd.Title)
	if title == "" {
		title = firstUserTitle(msgs)
	}
	title = truncateRunes(title, maxTitleRunes)

	workspace := wsMap[composerID]

	endedAt := int64(0)
	if cd.UpdatedAt > 0 {
		endedAt = cd.UpdatedAt / 1000
	}

	sess := store.Session{
		Agent:      agentName,
		UID:        composerID,
		Workspace:  workspace,
		Title:      title,
		Summary:    truncateRunes(cd.humanSummary(), 4096),
		StartedAt:  startedAt,
		EndedAt:    endedAt,
		SourcePath: sourcePath,
	}

	state := store.IndexState{
		SourcePath:  sourcePath,
		MtimeNs:     0,
		Size:        0,
		LastOffset:  int64(len(cd.Headers)),
		Fingerprint: fp,
	}

	// Even with no new readable messages we persist the new fingerprint
	// so we don't keep probing this composer every scan.
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case out <- source.Batch{
		Session:  sess,
		Messages: msgs,
		State:    state,
		Truncate: truncate,
	}:
	}
	return true, nil
}

// fetchBubble looks up one bubble row. Returns (nil, nil) if not found.
func fetchBubble(ctx context.Context, stmt *sql.Stmt, composerID, bubbleID string) ([]byte, error) {
	key := "bubbleId:" + composerID + ":" + bubbleID
	var val []byte
	err := stmt.QueryRowContext(ctx, key).Scan(&val)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return val, nil
}

// -------------------------------------------------------------------
// Helpers.
// -------------------------------------------------------------------

// openRO opens a Cursor SQLite DB read-only with immutable=1 to avoid
// locking problems when Cursor itself is running. Retries on SQLITE_BUSY.
func openRO(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?mode=ro&immutable=1&_pragma=busy_timeout(5000)"
	var lastErr error
	for attempt := 0; attempt < busyRetries; attempt++ {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			lastErr = err
			time.Sleep(busyBackoffDur)
			continue
		}
		// Read-only + immutable, so multiple connections are safe. We need
		// at least 2: the main composerData iterator holds one, and the
		// per-bubble lookups use another. With only 1 the bubble query
		// blocks forever waiting for the still-open rows iterator.
		db.SetMaxOpenConns(4)
		if err := db.Ping(); err != nil {
			db.Close()
			lastErr = err
			if isBusy(err) {
				time.Sleep(busyBackoffDur)
				continue
			}
			return nil, err
		}
		return db, nil
	}
	return nil, fmt.Errorf("open %s after %d tries: %w", path, busyRetries, lastErr)
}

// isBusy reports whether err looks like a SQLITE_BUSY / locked condition.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "busy") || strings.Contains(msg, "locked")
}

// nextOrdinalForUID returns the next ordinal for an existing cursor session
// identified by session_uid, or 0 if no prior session exists.
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

// parseCountFingerprint returns the N from a "count=N" fingerprint, or -1
// if the string doesn't match that format.
func parseCountFingerprint(fp string) int {
	rest, ok := strings.CutPrefix(fp, "count=")
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		return -1
	}
	return n
}

// compactJSON re-serializes a RawMessage with no superfluous whitespace.
// Falls back to the original bytes if re-encoding fails. Returns "" for
// empty input.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
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

// firstUserTitle returns a title-friendly first user message extract.
func firstUserTitle(msgs []store.Message) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		t := strings.TrimSpace(m.Content)
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			t = t[:i]
		}
		return t
	}
	return ""
}
