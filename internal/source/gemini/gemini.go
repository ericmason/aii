// Package gemini parses Gemini CLI chat sessions and emits a Batch per
// session file.
//
// Gemini CLI records every interactive session under its runtime
// directory (~/.gemini, or $GEMINI_CLI_HOME/.gemini):
//
//	tmp/<project>/chats/session-<time>-<id>.jsonl         main sessions
//	tmp/<project>/chats/<parent-session-id>/<id>.jsonl   subagent sessions
//	tmp/<project>/chats/session-<time>-<id>.json         older releases
//	tmp/<project>/.project_root                          project path
//
// <project> is a short slug (older releases used a SHA-256 of the
// project path). The .jsonl file is a log of records that together
// describe the conversation: a metadata record, full message records
// (re-appended whole when a message changes), `$patch` records that edit
// or reorder messages, `$set` metadata updates, and `$rewindTo` records
// that drop every message from a given id onward. Older releases wrote
// one ConversationRecord JSON document instead.
//
// Because later records can rewrite earlier messages, the parser replays
// the whole file whenever its size or mtime changes and replaces what
// the index held for it.
//
// Format reference: packages/core/src/services/chatRecordingService.ts
// and chatRecordingTypes.ts in github.com/google-gemini/gemini-cli.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

const (
	agentName = "gemini"

	maxToolResult = 4 * 1024
	maxToolInput  = 1 * 1024
	maxTitleRunes = 200
)

// Source implements source.Source for Gemini CLI.
type Source struct {
	// Roots overrides the directories holding per-project folders (the
	// `tmp` directories). Empty means DefaultRoots().
	Roots []string
}

// New returns a default Source.
func New() *Source { return &Source{} }

// Name returns the agent identifier used for sessions.agent.
func (s *Source) Name() string { return agentName }

// DefaultRoots returns the tmp directories Gemini CLI writes sessions
// to: <home>/.gemini/tmp, where <home> is $GEMINI_CLI_HOME or the user's
// home directory, plus <home>/.cache/.gemini/tmp, which Gemini CLI uses
// when it runs under the macOS Seatbelt sandbox.
func DefaultRoots() []string {
	home := os.Getenv("GEMINI_CLI_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = h
	}
	return []string{
		filepath.Join(home, ".gemini", "tmp"),
		filepath.Join(home, ".cache", ".gemini", "tmp"),
	}
}

// Scan walks every chats directory and emits a Batch for each session
// file that is new or changed (or every file when full=true).
func (s *Source) Scan(ctx context.Context, db *store.DB, full bool, out chan<- source.Batch) error {
	roots := s.Roots
	if len(roots) == 0 {
		roots = DefaultRoots()
	}
	for _, root := range roots {
		var files []string
		for _, pat := range []string{
			filepath.Join(root, "*", "chats", "*.jsonl"),
			filepath.Join(root, "*", "chats", "*.json"),
			filepath.Join(root, "*", "chats", "*", "*.jsonl"),
		} {
			m, err := filepath.Glob(pat)
			if err != nil {
				return fmt.Errorf("glob %s: %w", pat, err)
			}
			files = append(files, m...)
		}
		for _, path := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			// A resumed legacy session is rewritten next to the
			// original as <name>.jsonl; index only the newer file.
			if strings.HasSuffix(path, ".json") {
				if _, err := os.Stat(path + "l"); err == nil {
					continue
				}
			}
			if err := scanFile(ctx, db, root, path, full, out); err != nil {
				log.Printf("gemini: %s: %v", path, err)
			}
		}
	}
	return nil
}

// scanFile replays one session file and emits at most one Batch.
func scanFile(ctx context.Context, db *store.DB, root, path string, full bool, out chan<- source.Batch) error {
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
	if !full && prev != nil && prev.Size == size && prev.MtimeNs == mtimeNs {
		return nil
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	conv := parseConversation(data)

	state := store.IndexState{SourcePath: abs, MtimeNs: mtimeNs, Size: size, LastOffset: size}
	truncate := prev != nil || full
	if conv.SessionID == "" {
		// Not a session file (or an empty one); remember it so we skip
		// it until it changes.
		out <- source.Batch{State: state, Truncate: truncate}
		return nil
	}

	if !truncate {
		// Gemini CLI rewrites an older .json session as .jsonl. If the
		// session was indexed from another file, replace those rows
		// instead of adding to them.
		if existing, err := db.SessionByUID(agentName, conv.SessionID); err == nil && existing != nil && existing.SourcePath != abs {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case out <- source.Batch{State: store.IndexState{SourcePath: existing.SourcePath}, Truncate: true}:
			}
		}
	}

	msgs := conversationMessages(conv.Messages)
	title := strings.TrimSpace(conv.Summary)
	if i := strings.IndexByte(title, '\n'); i >= 0 {
		title = title[:i]
	}
	if title == "" {
		title = firstUserText(msgs)
	}
	workspace := projectRoot(root, abs)
	if workspace == "" && len(conv.Directories) > 0 {
		workspace = conv.Directories[0]
	}

	sess := store.Session{
		Agent:         agentName,
		UID:           conv.SessionID,
		Workspace:     workspace,
		Title:         truncateRunes(title, maxTitleRunes),
		StartedAt:     parseTS(conv.StartTime),
		EndedAt:       parseTS(conv.LastUpdated),
		SourcePath:    abs,
		SourceMtimeNs: mtimeNs,
		SourceSize:    size,
	}
	if sess.EndedAt == 0 && len(msgs) > 0 {
		sess.EndedAt = msgs[len(msgs)-1].TS
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case out <- source.Batch{Session: sess, Messages: msgs, State: state, Truncate: truncate}:
	}
	return nil
}

// projectRoot returns the project path for a session file, read from
// the .project_root marker in its project folder, or from the
// projects.json registry that maps paths to folder names. Returns "" for
// folders named by the older path hash with no marker.
func projectRoot(root, file string) string {
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return ""
	}
	slug := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	if b, err := os.ReadFile(filepath.Join(root, slug, ".project_root")); err == nil {
		if p := strings.TrimSpace(string(b)); p != "" {
			return p
		}
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(root), "projects.json"))
	if err != nil {
		return ""
	}
	var reg struct {
		Projects map[string]string `json:"projects"`
	}
	if json.Unmarshal(b, &reg) != nil {
		return ""
	}
	for path, id := range reg.Projects {
		if id == slug {
			return path
		}
	}
	return ""
}

// -------------------------------------------------------------------
// Conversation replay
// -------------------------------------------------------------------

// conversation is the replayed state of one session file.
type conversation struct {
	SessionID   string
	StartTime   string
	LastUpdated string
	Summary     string
	Directories []string
	Messages    []*messageRecord
}

// messageRecord is one MessageRecord. Content fields are PartListUnion
// values (a string, a Part, or an array of either), kept raw.
type messageRecord struct {
	ID             string           `json:"id"`
	Timestamp      string           `json:"timestamp"`
	Type           string           `json:"type"`
	Content        json.RawMessage  `json:"content"`
	DisplayContent json.RawMessage  `json:"displayContent"`
	ToolCalls      []toolCallRecord `json:"toolCalls"`
	Thoughts       []struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
	} `json:"thoughts"`
}

type toolCallRecord struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Args          json.RawMessage `json:"args"`
	Result        json.RawMessage `json:"result"`
	ResultDisplay json.RawMessage `json:"resultDisplay"`
	Status        string          `json:"status"`
}

// metadata holds the session-level fields a metadata record can carry.
type metadata struct {
	SessionID   *string          `json:"sessionId"`
	ProjectHash *string          `json:"projectHash"`
	StartTime   *string          `json:"startTime"`
	LastUpdated *string          `json:"lastUpdated"`
	Summary     *string          `json:"summary"`
	Directories []string         `json:"directories"`
	Messages    []*messageRecord `json:"messages"`
}

// messagePatch is one entry of a `$patch` record.
type messagePatch struct {
	ID        *string         `json:"id"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
	} `json:"toolCalls"`
}

// replay applies records in file order, mirroring Gemini CLI's own
// loader: messages keep their first position when re-appended.
type replay struct {
	conv  conversation
	order []string
	byID  map[string]*messageRecord
}

func (r *replay) put(m *messageRecord) {
	if _, ok := r.byID[m.ID]; !ok {
		r.order = append(r.order, m.ID)
	}
	r.byID[m.ID] = m
}

func (r *replay) remove(ids map[string]bool) {
	kept := r.order[:0]
	for _, id := range r.order {
		if ids[id] {
			delete(r.byID, id)
			continue
		}
		kept = append(kept, id)
	}
	r.order = kept
}

func (r *replay) setMeta(m metadata) {
	if m.SessionID != nil {
		r.conv.SessionID = *m.SessionID
	}
	if m.StartTime != nil {
		r.conv.StartTime = *m.StartTime
	}
	if m.LastUpdated != nil {
		r.conv.LastUpdated = *m.LastUpdated
	}
	if m.Summary != nil {
		r.conv.Summary = *m.Summary
	}
	if m.Directories != nil {
		r.conv.Directories = m.Directories
	}
}

func (r *replay) applyPatch(p messagePatch) {
	if p.ID == nil {
		return
	}
	m, ok := r.byID[*p.ID]
	if !ok {
		return
	}
	if len(p.Content) > 0 {
		m.Content = p.Content
	}
	for _, tp := range p.ToolCalls {
		for i := range m.ToolCalls {
			if m.ToolCalls[i].ID == tp.ID && tp.Result != nil {
				m.ToolCalls[i].Result = tp.Result
			}
		}
	}
}

// line applies one JSONL record. Record kinds are told apart the same
// way Gemini CLI does it: $rewindTo, then $patch, then a message (has an
// id), then $set, then a bare metadata record.
func (r *replay) line(b []byte) {
	var probe map[string]json.RawMessage
	if json.Unmarshal(b, &probe) != nil {
		return
	}
	if raw, ok := probe["$rewindTo"]; ok {
		var id string
		if json.Unmarshal(raw, &id) == nil {
			drop := map[string]bool{}
			found := false
			for _, oid := range r.order {
				if oid == id {
					found = true
				}
				if found {
					drop[oid] = true
				}
			}
			if !found {
				// Rewind to an unknown id clears the conversation.
				for _, oid := range r.order {
					drop[oid] = true
				}
			}
			r.remove(drop)
			return
		}
	}
	if raw, ok := probe["$patch"]; ok && isObject(raw) {
		var p struct {
			messagePatch
			Updates   []messagePatch `json:"updates"`
			RemoveIDs []string       `json:"removeIds"`
			OrderIDs  []string       `json:"orderIds"`
		}
		if json.Unmarshal(raw, &p) != nil {
			return
		}
		r.applyPatch(p.messagePatch)
		for _, u := range p.Updates {
			r.applyPatch(u)
		}
		if len(p.RemoveIDs) > 0 {
			drop := map[string]bool{}
			for _, id := range p.RemoveIDs {
				drop[id] = true
			}
			r.remove(drop)
		}
		if len(p.OrderIDs) > 0 {
			// Listed ids move to the end in the given order; the rest
			// keep their relative order in front.
			listed := map[string]bool{}
			var tail []string
			for _, id := range p.OrderIDs {
				if _, ok := r.byID[id]; ok && !listed[id] {
					listed[id] = true
					tail = append(tail, id)
				}
			}
			var head []string
			for _, id := range r.order {
				if !listed[id] {
					head = append(head, id)
				}
			}
			r.order = append(head, tail...)
		}
		return
	}
	if raw, ok := probe["id"]; ok && isString(raw) {
		var m messageRecord
		if json.Unmarshal(b, &m) == nil {
			r.put(&m)
		}
		return
	}
	if raw, ok := probe["$set"]; ok && isObject(raw) {
		var m metadata
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		if m.Messages != nil {
			r.order, r.byID = nil, map[string]*messageRecord{}
			for _, msg := range m.Messages {
				if msg != nil && msg.ID != "" {
					r.put(msg)
				}
			}
		}
		r.setMeta(m)
		return
	}
	if isString(probe["sessionId"]) && isString(probe["projectHash"]) {
		var m metadata
		if json.Unmarshal(b, &m) != nil {
			return
		}
		for _, msg := range m.Messages {
			if msg != nil && msg.ID != "" {
				r.put(msg)
			}
		}
		r.setMeta(m)
	}
}

// parseConversation replays a session file. It falls back to the older
// single-document JSON format when the JSONL replay finds no session id.
func parseConversation(data []byte) conversation {
	// The file is already in memory, so every line is replayed whatever
	// its length; a line-length cap would silently drop the records after
	// a large tool output.
	r := &replay{byID: map[string]*messageRecord{}}
	for rest := data; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		if line = bytes.TrimSpace(line); len(line) > 0 {
			r.line(line)
		}
	}
	if r.conv.SessionID != "" {
		for _, id := range r.order {
			r.conv.Messages = append(r.conv.Messages, r.byID[id])
		}
		return r.conv
	}

	var legacy struct {
		SessionID   string           `json:"sessionId"`
		StartTime   string           `json:"startTime"`
		LastUpdated string           `json:"lastUpdated"`
		Summary     string           `json:"summary"`
		Directories []string         `json:"directories"`
		Messages    []*messageRecord `json:"messages"`
	}
	if json.Unmarshal(data, &legacy) != nil {
		return conversation{}
	}
	var msgs []*messageRecord
	for _, m := range legacy.Messages {
		if m != nil {
			msgs = append(msgs, m)
		}
	}
	return conversation{
		SessionID:   legacy.SessionID,
		StartTime:   legacy.StartTime,
		LastUpdated: legacy.LastUpdated,
		Summary:     legacy.Summary,
		Directories: legacy.Directories,
		Messages:    msgs,
	}
}

// -------------------------------------------------------------------
// Message mapping
// -------------------------------------------------------------------

// conversationMessages maps replayed records to store rows. A gemini
// record splits into thinking, text, and a call plus result row per
// tool call. info, warning, and error records are skipped.
func conversationMessages(recs []*messageRecord) []store.Message {
	var msgs []store.Message
	add := func(role string, ts int64, content string) {
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
		msgs = append(msgs, store.Message{Ordinal: len(msgs), Role: role, TS: ts, Content: content})
	}
	for _, m := range recs {
		ts := parseTS(m.Timestamp)
		switch m.Type {
		case "user":
			// displayContent is what the user typed; content can hold
			// expanded @file references.
			text := partsText(m.DisplayContent)
			if strings.TrimSpace(text) == "" {
				text = partsText(m.Content)
			}
			add("user", ts, text)
		case "gemini":
			var think strings.Builder
			for _, t := range m.Thoughts {
				line := strings.TrimSpace(t.Subject)
				if d := strings.TrimSpace(t.Description); d != "" {
					if line != "" {
						line += ": "
					}
					line += d
				}
				appendLine(&think, line)
			}
			add("thinking", ts, think.String())
			add("assistant", ts, partsText(m.Content))
			for _, tc := range m.ToolCalls {
				add("tool", ts, fmt.Sprintf("[tool_call name=%s] %s", tc.Name, truncateRunes(compactJSON(tc.Args), maxToolInput)))
				body := toolResultText(tc)
				if body == "" && tc.Status != "error" {
					continue
				}
				tag := "[tool_result"
				if tc.Status == "error" {
					tag += " error"
				}
				tag += " name=" + tc.Name + "] "
				add("tool", ts, tag+truncateRunes(body, maxToolResult))
			}
		}
	}
	return msgs
}

// part is the subset of a @google/genai Part we read.
type part struct {
	Text             *string `json:"text"`
	Thought          bool    `json:"thought"`
	FunctionResponse *struct {
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	} `json:"functionResponse"`
}

// partsText flattens a PartListUnion (string, Part, or array of either)
// to its visible text. Thought parts and non-text parts are dropped.
func partsText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var b strings.Builder
	var walk func(json.RawMessage)
	walk = func(r json.RawMessage) {
		r = bytes.TrimSpace(r)
		if len(r) == 0 {
			return
		}
		switch r[0] {
		case '"':
			var s string
			if json.Unmarshal(r, &s) == nil {
				appendLine(&b, s)
			}
		case '[':
			var items []json.RawMessage
			if json.Unmarshal(r, &items) == nil {
				for _, it := range items {
					walk(it)
				}
			}
		case '{':
			var p part
			if json.Unmarshal(r, &p) == nil && p.Text != nil && !p.Thought {
				appendLine(&b, *p.Text)
			}
		}
	}
	walk(raw)
	return b.String()
}

// toolResultText renders a tool call's result: text parts plus the
// output (or error) of any functionResponse, falling back to the
// display string Gemini CLI showed the user.
func toolResultText(tc toolCallRecord) string {
	var b strings.Builder
	var items []json.RawMessage
	r := bytes.TrimSpace(tc.Result)
	if len(r) > 0 && r[0] == '[' {
		_ = json.Unmarshal(r, &items)
	} else if len(r) > 0 && string(r) != "null" {
		items = []json.RawMessage{r}
	}
	for _, it := range items {
		it = bytes.TrimSpace(it)
		if len(it) > 0 && it[0] == '"' {
			var s string
			if json.Unmarshal(it, &s) == nil {
				appendLine(&b, s)
			}
			continue
		}
		var p part
		if json.Unmarshal(it, &p) != nil {
			continue
		}
		if p.Text != nil {
			appendLine(&b, *p.Text)
		}
		if p.FunctionResponse != nil {
			var resp struct {
				Output *string `json:"output"`
				Error  *string `json:"error"`
			}
			if json.Unmarshal(p.FunctionResponse.Response, &resp) == nil && (resp.Output != nil || resp.Error != nil) {
				if resp.Output != nil {
					appendLine(&b, *resp.Output)
				}
				if resp.Error != nil {
					appendLine(&b, *resp.Error)
				}
			} else if len(p.FunctionResponse.Response) > 0 {
				appendLine(&b, compactJSON(p.FunctionResponse.Response))
			}
		}
	}
	if b.Len() == 0 {
		var s string
		if json.Unmarshal(tc.ResultDisplay, &s) == nil {
			appendLine(&b, s)
		}
	}
	return b.String()
}

// -------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------

func isObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

func isString(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '"'
}

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
