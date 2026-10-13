// Package cline parses Cline and Roo Code task histories into normalized
// Batches. Roo Code is a fork of Cline and kept Cline's per-task storage,
// so one parser serves both; New and NewRooCode differ only in the agent
// name and the directories they read.
//
// Layouts read (paths relative to a storage root):
//
//   - tasks/<task-id>/api_conversation_history.json — a JSON array of
//     Anthropic-style messages ({role, content, ts?}). Written by the
//     Cline extension before its SDK rewrite, and by every Roo Code
//     version. Workspace and title come from Cline's
//     state/taskHistory.json or Roo Code's tasks/<id>/history_item.json.
//   - sessions/<id>/<id>.messages.json — Cline SDK sessions
//     ({version, sessionId, messages: [...]}), written by the current
//     Cline extension and CLI under ~/.cline/data. The sibling <id>.json
//     manifest holds cwd, prompt, and started_at.
//
// Storage roots:
//
//   - Cline: VS Code globalStorage/saoudrizwan.claude-dev (legacy tasks)
//     and ~/.cline/data (CLINE_DATA_DIR, or CLINE_DIR + /data).
//   - Roo Code: VS Code globalStorage/rooveterinaryinc.roo-cline, and
//     ~/.vscode-mock/global-storage for the Roo Code CLI.
//
// VS Code globalStorage is looked up for VS Code, VS Code Insiders,
// VSCodium, Cursor, and VS Code Server, on macOS, Linux, and Windows.
//
// The files are rewritten whole on every change, so each task's state
// row stores a fingerprint of the messages already indexed. When the new
// file still starts with those messages only the tail is emitted;
// otherwise (the user edited or deleted an earlier message) the task is
// re-indexed from scratch.
package cline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

const (
	clineAgent   = "cline"
	rooCodeAgent = "roo_code"

	clineExtensionID   = "saoudrizwan.claude-dev"
	rooCodeExtensionID = "rooveterinaryinc.roo-cline"

	apiHistoryFile  = "api_conversation_history.json"
	uiMessagesFile  = "ui_messages.json"
	historyItemFile = "history_item.json"

	maxTitleRunes   = 200
	maxToolArgBytes = 1024
	maxToolOutBytes = 4096
)

// Source implements source.Source for Cline or Roo Code.
type Source struct {
	agent string

	// Roots overrides the default storage roots (useful for tests). Each
	// root may hold tasks/, state/taskHistory.json, and sessions/.
	Roots []string
}

// New returns a Source for the Cline extension and CLI.
func New() *Source { return &Source{agent: clineAgent} }

// NewRooCode returns a Source for the Roo Code extension and CLI.
func NewRooCode() *Source { return &Source{agent: rooCodeAgent} }

// Name returns the agent identifier used for sessions.agent.
func (s *Source) Name() string { return s.agent }

// Scan reads every storage root and emits a Batch per task that is new
// or changed since the last run.
func (s *Source) Scan(ctx context.Context, db *store.DB, full bool, out chan<- source.Batch) error {
	seen := map[string]bool{} // task id -> already indexed from an earlier root
	for _, root := range s.roots() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			continue
		}
		if err := s.scanRoot(ctx, db, full, out, root, seen); err != nil {
			if err == context.Canceled || err == context.DeadlineExceeded {
				return err
			}
			log.Printf("%s: scan %s: %v", s.agent, root, err)
		}
	}
	return nil
}

// roots lists storage roots in priority order: when the same task id
// appears under two roots (Cline copies legacy tasks into ~/.cline/data),
// the first one wins.
func (s *Source) roots() []string {
	if s.Roots != nil {
		return s.Roots
	}
	home, _ := os.UserHomeDir()
	var roots []string
	ext := clineExtensionID
	if s.agent == clineAgent {
		roots = append(roots, clineDataDir(home))
	} else {
		ext = rooCodeExtensionID
		roots = append(roots, filepath.Join(home, ".vscode-mock", "global-storage"))
	}
	for _, gs := range globalStorageDirs(home) {
		roots = append(roots, filepath.Join(gs, ext))
	}
	return roots
}

// DataDir returns the Cline CLI data directory aii reads, honoring
// CLINE_DATA_DIR and CLINE_DIR.
func DataDir() string {
	home, _ := os.UserHomeDir()
	return clineDataDir(home)
}

// clineDataDir mirrors Cline's resolveClineDataDir: CLINE_DATA_DIR, then
// CLINE_DIR/data, then ~/.cline/data.
func clineDataDir(home string) string {
	if d := strings.TrimSpace(os.Getenv("CLINE_DATA_DIR")); d != "" {
		return d
	}
	if d := strings.TrimSpace(os.Getenv("CLINE_DIR")); d != "" {
		return filepath.Join(d, "data")
	}
	return filepath.Join(home, ".cline", "data")
}

// globalStorageDirs returns VS Code-family User/globalStorage directories
// for the current OS.
func globalStorageDirs(home string) []string {
	hosts := []string{"Code", "Code - Insiders", "VSCodium", "Cursor"}
	var base string
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support")
	case "windows":
		base = os.Getenv("APPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Roaming")
		}
	default:
		base = os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
	}
	var dirs []string
	for _, h := range hosts {
		dirs = append(dirs, filepath.Join(base, h, "User", "globalStorage"))
	}
	// VS Code Remote (SSH, WSL, containers) keeps extension data here.
	dirs = append(dirs, filepath.Join(home, ".vscode-server", "data", "User", "globalStorage"))
	return dirs
}

// -------------------------------------------------------------------
// Root scan
// -------------------------------------------------------------------

// taskFile is one conversation file found under a root, plus the
// metadata that lives next to it.
type taskFile struct {
	id        string
	path      string // message file
	sdk       bool   // sessions/<id>/<id>.messages.json layout
	workspace string
	title     string
	startedAt int64 // unix seconds
}

func (s *Source) scanRoot(ctx context.Context, db *store.DB, full bool, out chan<- source.Batch, root string, seen map[string]bool) error {
	var files []taskFile

	// Cline SDK sessions.
	sessDirs, _ := os.ReadDir(filepath.Join(root, "sessions"))
	for _, d := range sessDirs {
		if !d.IsDir() {
			continue
		}
		id := d.Name()
		dir := filepath.Join(root, "sessions", id)
		p := filepath.Join(dir, id+".messages.json")
		if _, err := os.Stat(p); err != nil {
			continue
		}
		tf := taskFile{id: id, path: p, sdk: true}
		readManifest(filepath.Join(dir, id+".json"), &tf)
		files = append(files, tf)
	}

	// Legacy task directories.
	history := readTaskHistory(filepath.Join(root, "state", "taskHistory.json"))
	taskDirs, _ := os.ReadDir(filepath.Join(root, "tasks"))
	for _, d := range taskDirs {
		if !d.IsDir() {
			continue
		}
		id := d.Name()
		dir := filepath.Join(root, "tasks", id)
		p := filepath.Join(dir, apiHistoryFile)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		tf := taskFile{id: id, path: p}
		if h, ok := history[id]; ok {
			tf.applyHistory(h)
		}
		if h, ok := readHistoryItem(filepath.Join(dir, historyItemFile)); ok {
			tf.applyHistory(h)
		}
		files = append(files, tf)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	for _, tf := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen[tf.id] {
			continue
		}
		seen[tf.id] = true
		if err := s.scanTask(ctx, db, full, out, tf); err != nil {
			if err == context.Canceled || err == context.DeadlineExceeded {
				return err
			}
			log.Printf("%s: task %s: %v", s.agent, tf.id, err)
		}
	}
	return nil
}

// historyItem covers the HistoryItem fields of both Cline
// (cwdOnTaskInitialization) and Roo Code (workspace).
type historyItem struct {
	ID        string `json:"id"`
	TS        int64  `json:"ts"` // ms
	Task      string `json:"task"`
	Cwd       string `json:"cwdOnTaskInitialization"`
	Workspace string `json:"workspace"`
}

func (tf *taskFile) applyHistory(h historyItem) {
	if ws := firstNonEmpty(h.Workspace, h.Cwd); ws != "" {
		tf.workspace = ws
	}
	if t := strings.TrimSpace(h.Task); t != "" {
		tf.title = t
	}
	if h.TS > 0 {
		tf.startedAt = h.TS / 1000
	}
}

// readTaskHistory reads state/taskHistory.json into a map by task id.
func readTaskHistory(path string) map[string]historyItem {
	m := map[string]historyItem{}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	var items []historyItem
	if err := json.Unmarshal(data, &items); err != nil {
		log.Printf("cline: parse %s: %v", path, err)
		return m
	}
	for _, h := range items {
		if h.ID != "" {
			m[h.ID] = h
		}
	}
	return m
}

func readHistoryItem(path string) (historyItem, bool) {
	var h historyItem
	data, err := os.ReadFile(path)
	if err != nil {
		return h, false
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return h, false
	}
	return h, true
}

// readManifest fills workspace, title, and start time from a Cline SDK
// session manifest (<id>.json).
func readManifest(path string, tf *taskFile) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var m struct {
		StartedAt     string `json:"started_at"`
		Cwd           string `json:"cwd"`
		WorkspaceRoot string `json:"workspace_root"`
		Prompt        string `json:"prompt"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	tf.workspace = firstNonEmpty(m.WorkspaceRoot, m.Cwd)
	if p := cleanUserText(m.Prompt); p != "" {
		tf.title = p
	}
	if t, err := time.Parse(time.RFC3339Nano, m.StartedAt); err == nil {
		tf.startedAt = t.Unix()
	}
}

// -------------------------------------------------------------------
// Per-task scan
// -------------------------------------------------------------------

func (s *Source) scanTask(ctx context.Context, db *store.DB, full bool, out chan<- source.Batch, tf taskFile) error {
	path, err := filepath.Abs(tf.path)
	if err != nil {
		path = tf.path
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	size := fi.Size()
	mtimeNs := fi.ModTime().UnixNano()
	uiPath := filepath.Join(filepath.Dir(path), uiMessagesFile)
	if !tf.sdk {
		// The final answer can reach ui_messages.json without the API
		// history changing (see withUICompletions), so a change to either
		// file triggers a rescan.
		if ui, err := os.Stat(uiPath); err == nil {
			size += ui.Size()
			mtimeNs = max(mtimeNs, ui.ModTime().UnixNano())
		}
	}

	prev, err := db.GetState(path)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}
	if !full && prev != nil && prev.Size == size && prev.MtimeNs == mtimeNs {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	raw, err := decodeMessages(data, tf.sdk)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	rendered := raw
	if !tf.sdk {
		rendered = withUICompletions(uiPath, raw)
	}
	msgs, minTS, maxTS := renderMessages(rendered)

	// Decide whether the indexed messages are still a prefix of the file.
	base := 0
	truncate := full
	if prev != nil && !full {
		n, h, ok := parseFingerprint(prev.Fingerprint)
		if ok && n <= len(msgs) && hashMessages(msgs[:n]) == h {
			base = n
		} else {
			truncate = true
		}
	}

	workspace := tf.workspace
	if workspace == "" {
		workspace = workspaceFromEnvironment(raw)
	}
	title := tf.title
	if title == "" {
		title = firstUserText(msgs)
	}
	startedAt := tf.startedAt
	if minTS > 0 && (startedAt == 0 || minTS < startedAt) {
		startedAt = minTS
	}
	endedAt := maxTS
	if startedAt == 0 || endedAt == 0 {
		// Older Cline and Roo Code versions keep timestamps only in
		// ui_messages.json.
		uiMin, uiMax := uiMessageRange(filepath.Join(filepath.Dir(path), uiMessagesFile))
		if startedAt == 0 {
			startedAt = uiMin
		}
		if endedAt == 0 {
			endedAt = uiMax
		}
	}
	if startedAt == 0 {
		startedAt = idTime(tf.id)
	}
	if endedAt == 0 {
		endedAt = fi.ModTime().Unix()
	}

	for i := range msgs {
		msgs[i].Ordinal = i
	}

	b := source.Batch{
		Session: store.Session{
			Agent:         s.agent,
			UID:           tf.id,
			Workspace:     workspace,
			Title:         truncateRunes(firstLine(title), maxTitleRunes),
			StartedAt:     startedAt,
			EndedAt:       endedAt,
			SourcePath:    path,
			SourceMtimeNs: mtimeNs,
			SourceSize:    size,
		},
		Messages: msgs[base:],
		State: store.IndexState{
			SourcePath:  path,
			MtimeNs:     mtimeNs,
			Size:        size,
			LastOffset:  int64(len(msgs)),
			Fingerprint: fingerprint(msgs),
		},
		Truncate: truncate,
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case out <- b:
	}
	return nil
}

// -------------------------------------------------------------------
// Message decoding
// -------------------------------------------------------------------

// apiMessage is one stored message in either layout. Roo Code also
// stores reasoning items as {type: "reasoning", summary, text}.
type apiMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	TS               int64           `json:"ts"` // ms
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Summary          []summaryPart   `json:"summary"`
	ReasoningContent string          `json:"reasoning_content"`
}

type summaryPart struct {
	Text string `json:"text"`
}

// contentBlock covers the Anthropic block shapes Cline and Roo Code store.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Content  json.RawMessage `json:"content"`
	IsError  bool            `json:"is_error"`
}

// decodeMessages reads either a bare message array (legacy) or the SDK
// wrapper object.
func decodeMessages(data []byte, sdk bool) ([]apiMessage, error) {
	var msgs []apiMessage
	if sdk {
		var wrapper struct {
			Messages []apiMessage `json:"messages"`
		}
		if err := json.Unmarshal(data, &wrapper); err != nil {
			return nil, err
		}
		return wrapper.Messages, nil
	}
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// renderMessages flattens stored messages into indexable rows, one per
// text, thinking, tool-call, or tool-result block. It also returns the
// earliest and latest message timestamps in unix seconds.
func renderMessages(raw []apiMessage) ([]store.Message, int64, int64) {
	var (
		out          []store.Message
		minTS, maxTS int64
	)
	for _, m := range raw {
		ts := int64(0)
		if m.TS > 0 {
			ts = m.TS / 1000
			if minTS == 0 || ts < minTS {
				minTS = ts
			}
			if ts > maxTS {
				maxTS = ts
			}
		}
		add := func(role, text string) {
			text = strings.TrimSpace(text)
			if text != "" {
				out = append(out, store.Message{Role: role, TS: ts, Content: text})
			}
		}

		if m.Type == "reasoning" {
			var parts []string
			for _, p := range m.Summary {
				parts = append(parts, p.Text)
			}
			add("thinking", firstNonEmpty(strings.Join(parts, "\n"), m.Text))
			continue
		}
		role := strings.ToLower(m.Role)
		if role != "user" && role != "assistant" {
			continue
		}
		add("thinking", m.ReasoningContent)

		var str string
		if json.Unmarshal(m.Content, &str) == nil {
			addText(add, role, str)
			continue
		}
		var blocks []contentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		// Older versions sent a text-format tool result as a marker block
		// ("[read_file for 'x'] Result:") followed by the output in its
		// own text blocks. Collect those into one tool row.
		var legacy []string
		flushLegacy := func() {
			if legacy != nil {
				res := cleanUserText(strings.Join(legacy, "\n"))
				add("tool", "[tool_result] "+truncateBytes(res, maxToolOutBytes))
				legacy = nil
			}
		}
		for _, b := range blocks {
			if b.Type == "text" && role == "user" {
				t := strings.TrimSpace(b.Text)
				switch {
				case legacyToolResult.MatchString(t):
					flushLegacy()
					legacy = []string{t}
					continue
				case legacy != nil && cleanUserText(t) != "":
					legacy = append(legacy, t)
					continue
				}
			} else {
				flushLegacy()
			}
			switch b.Type {
			case "text":
				addText(add, role, b.Text)
			case "thinking":
				add("thinking", b.Thinking)
			case "tool_use":
				args := truncateBytes(compactJSON(b.Input), maxToolArgBytes)
				add("tool", "[tool_call name="+b.Name+"] "+args)
			case "tool_result":
				res := toolResultText(b.Content)
				if b.IsError {
					res = "[error] " + res
				}
				add("tool", "[tool_result] "+truncateBytes(res, maxToolOutBytes))
			}
		}
		flushLegacy()
	}
	return out, minTS, maxTS
}

// legacyToolResult matches tool results that older Cline and Roo Code
// versions sent back as plain user text, such as
// "[read_file for 'main.go'] Result:".
var legacyToolResult = regexp.MustCompile(`^\[[a-z_]+(?: for [^\]]*)?\] Result:`)

// systemReminderPrefix starts the reminders the Cline SDK agent runtime
// adds as user turns, such as "[SYSTEM] This run is not complete until
// you call one of these terminal completion tools" (cline/cline
// sdk/packages/agents/src/agent-runtime.ts).
const systemReminderPrefix = "[SYSTEM] "

// addText routes a text block: user text is cleaned of the context Cline
// appends, and user text that is really a tool result goes to "tool".
func addText(add func(role, text string), role, text string) {
	if role != "user" {
		add(role, text)
		return
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, systemReminderPrefix) {
		return // injected by the agent runtime, not typed by the user
	}
	if legacyToolResult.MatchString(trimmed) {
		add("tool", "[tool_result] "+truncateBytes(cleanUserText(trimmed), maxToolOutBytes))
		return
	}
	add("user", cleanUserText(text))
}

// toolResultText flattens a tool_result content field: a string, an
// array of text and image blocks, or (Cline SDK tools such as read_files
// and run_commands) an array of {query, result, error, success} objects.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return strings.TrimSpace(str)
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var parts []string
	for _, it := range items {
		var b struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Query   string          `json:"query"`
			Result  json.RawMessage `json:"result"`
			Error   string          `json:"error"`
			Success *bool           `json:"success"`
		}
		if json.Unmarshal(it, &b) != nil {
			continue
		}
		switch {
		case b.Type == "text":
			parts = append(parts, strings.TrimSpace(b.Text))
		case b.Type != "":
			// Images and other binary blocks carry no searchable text.
		case b.Query != "" || len(b.Result) > 0 || b.Error != "":
			var lines []string
			if b.Query != "" {
				lines = append(lines, b.Query)
			}
			var res string
			if json.Unmarshal(b.Result, &res) != nil {
				res = compactJSON(b.Result)
			}
			if strings.TrimSpace(res) != "" {
				lines = append(lines, strings.TrimSpace(res))
			}
			if b.Error != "" {
				lines = append(lines, "[error] "+strings.TrimSpace(b.Error))
			}
			parts = append(parts, strings.Join(lines, "\n"))
		default:
			parts = append(parts, compactJSON(it))
		}
	}
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

var (
	// Context blocks the extension appends to user turns; never typed by
	// the user.
	dropBlocks = regexp.MustCompile(`(?s)<(environment_details|mode_notice)>.*?</(environment_details|mode_notice)>`)
	// Wrappers around user-typed text; the tags go, the text stays.
	wrapperTags = regexp.MustCompile(`</?(task|feedback|user_message|user_input|user_command|answer)(\s[^>]*)?>`)
)

// cleanUserText strips extension-added context and wrapper tags from a
// user turn, leaving what the user typed.
func cleanUserText(s string) string {
	s = dropBlocks.ReplaceAllString(s, "")
	s = wrapperTags.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// envWorkspace matches the working-directory heading in the
// environment_details block Cline ("Current Working Directory") and Roo
// Code ("Current Workspace Directory") append to user turns.
var envWorkspace = regexp.MustCompile(`# Current (?:Working|Workspace) Directory \(([^)\n]+)\)`)

// workspaceFromEnvironment recovers the workspace from the first user
// turn when no metadata file recorded it.
func workspaceFromEnvironment(raw []apiMessage) string {
	for _, m := range raw {
		if m.Role != "user" {
			continue
		}
		if sm := envWorkspace.FindSubmatch(m.Content); sm != nil {
			// The match sits inside a JSON string; decode its escapes.
			var ws string
			if json.Unmarshal([]byte(`"`+string(sm[1])+`"`), &ws) == nil {
				return ws
			}
			return string(sm[1])
		}
	}
	return ""
}

// withUICompletions returns raw with each task answer that is missing
// from the API history added as an assistant turn at its place in time.
// The Roo CLI in --print mode exits before saving the last assistant turn
// to api_conversation_history.json, so its attempt_completion result
// exists only as a "completion_result" entry in ui_messages.json, and a
// resumed task keeps that gap in the middle of its history.
//
// A completion counts as missing only when the API turns on both sides
// of it are user turns (or it comes after the last turn, which is a user
// turn). An assistant turn on either side holds the answer: Roo can show
// completion_result just before it saves that turn. Histories without
// timestamps (older Cline and Roo Code versions) are returned unchanged,
// because a completion can't be placed in them.
func withUICompletions(path string, raw []apiMessage) []apiMessage {
	var turns []int // indexes of user and assistant turns in raw
	for i, m := range raw {
		role := strings.ToLower(m.Role)
		if m.Type == "reasoning" || (role != "user" && role != "assistant") {
			continue
		}
		if m.TS <= 0 {
			return raw
		}
		turns = append(turns, i)
	}
	if len(turns) == 0 {
		return raw
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return raw
	}
	var ui []struct {
		TS      int64  `json:"ts"`
		Type    string `json:"type"`
		Say     string `json:"say"`
		Text    string `json:"text"`
		Partial bool   `json:"partial"`
	}
	if json.Unmarshal(data, &ui) != nil {
		return raw
	}
	isUser := func(i int) bool { return strings.EqualFold(raw[i].Role, "user") }
	insertAt := map[int][]apiMessage{} // raw index -> completions placed before it
	for _, u := range ui {
		text := strings.TrimSpace(u.Text)
		if u.Type != "say" || u.Say != "completion_result" || u.Partial || u.TS <= 0 || text == "" {
			continue
		}
		k := 0 // turns at or before the completion
		for k < len(turns) && raw[turns[k]].TS <= u.TS {
			k++
		}
		if k == 0 || !isUser(turns[k-1]) || (k < len(turns) && !isUser(turns[k])) {
			continue
		}
		at := len(raw)
		if k < len(turns) {
			at = turns[k]
		}
		content, _ := json.Marshal(text)
		insertAt[at] = append(insertAt[at], apiMessage{Role: "assistant", Content: content, TS: u.TS})
	}
	if len(insertAt) == 0 {
		return raw
	}
	out := make([]apiMessage, 0, len(raw)+len(insertAt))
	for i := 0; i <= len(raw); i++ {
		out = append(out, insertAt[i]...)
		if i < len(raw) {
			out = append(out, raw[i])
		}
	}
	return out
}

// uiMessageRange returns the earliest and latest ts in ui_messages.json.
func uiMessageRange(path string) (int64, int64) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	var msgs []struct {
		TS int64 `json:"ts"`
	}
	if json.Unmarshal(data, &msgs) != nil {
		return 0, 0
	}
	var lo, hi int64
	for _, m := range msgs {
		if m.TS <= 0 {
			continue
		}
		t := m.TS / 1000
		if lo == 0 || t < lo {
			lo = t
		}
		if t > hi {
			hi = t
		}
	}
	return lo, hi
}

// -------------------------------------------------------------------
// Fingerprints
// -------------------------------------------------------------------

// fingerprint records how many rendered messages were indexed and a hash
// of them, so the next scan can tell an append from a rewrite.
func fingerprint(msgs []store.Message) string {
	return "count=" + strconv.Itoa(len(msgs)) + " sha256=" + hashMessages(msgs)
}

func parseFingerprint(fp string) (int, string, bool) {
	countPart, hashPart, ok := strings.Cut(fp, " ")
	if !ok {
		return 0, "", false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(countPart, "count="))
	if err != nil || n < 0 {
		return 0, "", false
	}
	return n, strings.TrimPrefix(hashPart, "sha256="), true
}

func hashMessages(msgs []store.Message) string {
	h := sha256.New()
	for _, m := range msgs {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Content))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// -------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------

func firstUserText(msgs []store.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

// firstLine keeps titles on one line.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// idTime returns the creation time encoded in a task ID, in unix seconds,
// or 0. Cline task IDs start with Date.now() in milliseconds (SDK session
// IDs add "_<suffix>"); Roo Code task IDs are UUIDv7, whose first 48 bits
// are the creation time in milliseconds.
func idTime(id string) int64 {
	const minMs, maxMs = 1_500_000_000_000, 4_000_000_000_000
	digits := id
	if i := strings.IndexByte(digits, '_'); i >= 0 {
		digits = digits[:i]
	}
	if ms, err := strconv.ParseInt(digits, 10, 64); err == nil && ms >= minMs && ms < maxMs {
		return ms / 1000
	}
	hex := strings.ReplaceAll(id, "-", "")
	if len(hex) == 32 && hex[12] == '7' {
		if ms, err := strconv.ParseInt(hex[:12], 16, 64); err == nil && ms >= minMs && ms < maxMs {
			return ms / 1000
		}
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// compactJSON strips insignificant whitespace from raw JSON, leaving
// strings (and their escapes) as stored.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func truncateRunes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// truncateBytes cuts s to at most n bytes on a rune boundary.
func truncateBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}
