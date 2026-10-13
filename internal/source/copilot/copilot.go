// Package copilot parses GitHub Copilot Chat sessions that VS Code stores
// under its user data directory and emits a Batch per session file.
//
// VS Code keeps one file per chat session:
//
//   - <User>/workspaceStorage/<hash>/chatSessions/<sessionId>.jsonl for
//     chats opened in a folder or workspace. The sibling workspace.json
//     names the folder.
//   - <User>/globalStorage/emptyWindowChatSessions/<sessionId>.jsonl for
//     chats opened in a window with no folder.
//
// VS Code 1.109 and later write an append-only operation log (.jsonl):
// the first line is {"kind":0,"v":<full session>}, and later lines set
// (kind 1), push into (kind 2), or delete (kind 3) a value at a key path
// "k". Older releases wrote the full session as one JSON document
// (.json). Either way the session is an ISerializableChatData object with
// a `requests` array; each request carries the user's message and the
// response parts.
//
// Because later log lines can rewrite earlier requests, the parser
// replays the whole file whenever its size or mtime changes and replaces
// what the index held for it.
//
// Format reference: src/vs/workbench/contrib/chat/common/model/
// objectMutationLog.ts, chatSessionOperationLog.ts, chatSessionStore.ts,
// and chatModel.ts in github.com/microsoft/vscode.
package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

const (
	agentName = "copilot"

	maxToolResult = 4 * 1024
	maxToolInput  = 1 * 1024
	maxTitleRunes = 200
)

// Source implements source.Source for GitHub Copilot Chat in VS Code.
type Source struct {
	// UserDirs overrides the VS Code "User" directories to scan. Empty
	// means DefaultUserDirs().
	UserDirs []string
}

// New returns a default Source.
func New() *Source { return &Source{} }

// Name returns the agent identifier used for sessions.agent.
func (s *Source) Name() string { return agentName }

// products are the VS Code builds whose user data directories are scanned.
var products = []string{"Code", "Code - Insiders", "VSCodium"}

// DefaultUserDirs returns the "User" directories of VS Code, VS Code
// Insiders, and VSCodium for this OS. $VSCODE_PORTABLE, when set, points
// at a portable install whose data lives in <dir>/user-data/User.
func DefaultUserDirs() []string {
	if p := os.Getenv("VSCODE_PORTABLE"); p != "" {
		return []string{filepath.Join(p, "user-data", "User")}
	}
	var base string
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		base = filepath.Join(home, "Library", "Application Support")
	case "windows":
		base = os.Getenv("APPDATA")
	default:
		base = os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil
			}
			base = filepath.Join(home, ".config")
		}
	}
	if base == "" {
		return nil
	}
	out := make([]string, 0, len(products))
	for _, p := range products {
		out = append(out, filepath.Join(base, p, "User"))
	}
	return out
}

// Scan walks every chat session file and emits a Batch for each one that
// is new or changed (or every file when full=true).
func (s *Source) Scan(ctx context.Context, db *store.DB, full bool, out chan<- source.Batch) error {
	dirs := s.UserDirs
	if len(dirs) == 0 {
		dirs = DefaultUserDirs()
	}
	for _, dir := range dirs {
		for _, pat := range []string{
			filepath.Join(dir, "workspaceStorage", "*", "chatSessions", "*.jsonl"),
			filepath.Join(dir, "workspaceStorage", "*", "chatSessions", "*.json"),
			filepath.Join(dir, "globalStorage", "emptyWindowChatSessions", "*.jsonl"),
			filepath.Join(dir, "globalStorage", "emptyWindowChatSessions", "*.json"),
		} {
			files, err := filepath.Glob(pat)
			if err != nil {
				return fmt.Errorf("glob %s: %w", pat, err)
			}
			for _, f := range files {
				if err := ctx.Err(); err != nil {
					return err
				}
				// VS Code reads <id>.jsonl in preference to <id>.json.
				if strings.HasSuffix(f, ".json") {
					if _, err := os.Stat(f + "l"); err == nil {
						continue
					}
				}
				if err := scanFile(ctx, db, f, full, out); err != nil {
					log.Printf("copilot: %s: %v", f, err)
				}
			}
		}
	}
	return nil
}

func scanFile(ctx context.Context, db *store.DB, file string, full bool, out chan<- source.Batch) error {
	abs, err := filepath.Abs(file)
	if err != nil {
		abs = file
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
	var raw any
	if strings.HasSuffix(abs, ".jsonl") {
		raw, err = replayLog(data)
	} else {
		err = json.Unmarshal(data, &raw)
	}
	state := store.IndexState{SourcePath: abs, MtimeNs: mtimeNs, Size: size, LastOffset: size}
	truncate := prev != nil || full
	if err != nil {
		// Keep what is indexed and leave the state alone, so the file is
		// retried once VS Code finishes writing it.
		log.Printf("copilot: %s: %v", abs, err)
		return nil
	}

	var sess chatSession
	if b, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(b, &sess)
	}
	if sess.SessionID == "" {
		// Not a chat session; remember it so we skip it until it changes.
		out <- source.Batch{State: state, Truncate: truncate}
		return nil
	}

	if !truncate {
		// VS Code rewrites <id>.json as <id>.jsonl when it migrates a
		// session. If the session was indexed from another file, replace
		// those rows instead of adding to them.
		if existing, err := db.SessionByUID(agentName, sess.SessionID); err == nil && existing != nil && existing.SourcePath != abs {
			b := source.Batch{State: store.IndexState{SourcePath: existing.SourcePath}, Truncate: true}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case out <- b:
			}
		}
	}

	msgs := sessionMessages(sess)
	title := strings.TrimSpace(sess.CustomTitle)
	if title == "" {
		title = strings.TrimSpace(sess.ComputedTitle)
	}
	if title == "" {
		title = firstUserText(msgs)
	}
	ended := sess.LastMessageDate
	for _, r := range sess.Requests {
		if r.Timestamp > ended {
			ended = r.Timestamp
		}
	}
	workspace := sess.WorkingDirectory
	if workspace != "" {
		workspace = uriPath(workspace)
	}
	if workspace == "" {
		workspace = workspaceFolder(abs)
	}

	b := source.Batch{
		Session: store.Session{
			Agent:         agentName,
			UID:           sess.SessionID,
			Workspace:     workspace,
			Title:         truncateRunes(title, maxTitleRunes),
			StartedAt:     sess.CreationDate / 1000,
			EndedAt:       ended / 1000,
			SourcePath:    abs,
			SourceMtimeNs: mtimeNs,
			SourceSize:    size,
		},
		Messages: msgs,
		State:    state,
		Truncate: truncate,
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case out <- b:
	}
	return nil
}

// workspaceFolder reads the workspace.json next to a workspaceStorage
// chatSessions directory and returns the folder (or .code-workspace file)
// it names. Empty-window sessions have no workspace.
func workspaceFolder(file string) string {
	wsDir := filepath.Dir(filepath.Dir(file))
	b, err := os.ReadFile(filepath.Join(wsDir, "workspace.json"))
	if err != nil {
		return ""
	}
	var ws struct {
		Folder    string `json:"folder"`
		Workspace string `json:"workspace"`
	}
	if json.Unmarshal(b, &ws) != nil {
		return ""
	}
	if ws.Folder != "" {
		return uriPath(ws.Folder)
	}
	return uriPath(ws.Workspace)
}

// uriPath converts a file:// URI to a local path. Other schemes (remote
// SSH, containers, ...) are returned unchanged.
func uriPath(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "file" {
		return s
	}
	p := u.Path
	// file:///c%3A/Users/... on Windows.
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

// -------------------------------------------------------------------
// Operation log replay
// -------------------------------------------------------------------

// logEntry is one line of a .jsonl session log.
type logEntry struct {
	Kind int             `json:"kind"`
	K    []any           `json:"k"`
	V    json.RawMessage `json:"v"`
	I    *int            `json:"i"`
}

// replayLog rebuilds the session object from an operation log, applying
// entries the same way VS Code's ObjectMutationLog.read does. A trailing
// line that fails to parse (a write in progress) is ignored. The file is
// already in memory, so lines of any length are replayed.
func replayLog(data []byte) (any, error) {
	var state any
	for rest := data; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var e logEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		var v any
		if len(e.V) > 0 {
			if err := json.Unmarshal(e.V, &v); err != nil {
				continue
			}
		}
		switch e.Kind {
		case 0:
			state = v
		case 1:
			if state != nil {
				state = setPath(state, e.K, v, false)
			}
		case 2:
			if state != nil {
				state = pushPath(state, e.K, v, e.I)
			}
		case 3:
			if state != nil {
				state = setPath(state, e.K, nil, true)
			}
		}
	}
	if state == nil {
		return nil, errors.New("no initial entry")
	}
	return state, nil
}

// setPath sets (or deletes) the value at key path k inside root and
// returns the updated root. Missing intermediate containers are left
// alone, as VS Code would throw on them.
func setPath(root any, k []any, v any, del bool) any {
	if len(k) == 0 {
		return root
	}
	switch c := root.(type) {
	case map[string]any:
		key, ok := k[0].(string)
		if !ok {
			return root
		}
		if len(k) == 1 {
			if del {
				delete(c, key)
			} else {
				c[key] = v
			}
			return c
		}
		if child, ok := c[key]; ok {
			c[key] = setPath(child, k[1:], v, del)
		}
		return c
	case []any:
		idx, ok := index(k[0])
		if !ok || idx < 0 {
			return root
		}
		if len(k) == 1 {
			for idx >= len(c) {
				c = append(c, nil)
			}
			if del {
				c[idx] = nil
			} else {
				c[idx] = v
			}
			return c
		}
		if idx < len(c) {
			c[idx] = setPath(c[idx], k[1:], v, del)
		}
		return c
	}
	return root
}

// pushPath truncates the array at key path k to i items (when i is set)
// and appends the items in v.
func pushPath(root any, k []any, v any, i *int) any {
	if len(k) == 0 {
		return root
	}
	var arr []any
	switch c := root.(type) {
	case map[string]any:
		key, ok := k[0].(string)
		if !ok {
			return root
		}
		if len(k) > 1 {
			if child, ok := c[key]; ok {
				c[key] = pushPath(child, k[1:], v, i)
			}
			return c
		}
		arr, _ = c[key].([]any)
		c[key] = appendItems(arr, v, i)
		return c
	case []any:
		idx, ok := index(k[0])
		if !ok || idx < 0 || idx >= len(c) {
			return root
		}
		if len(k) > 1 {
			c[idx] = pushPath(c[idx], k[1:], v, i)
			return c
		}
		arr, _ = c[idx].([]any)
		c[idx] = appendItems(arr, v, i)
		return c
	}
	return root
}

func appendItems(arr []any, v any, i *int) []any {
	if i != nil && *i >= 0 && *i < len(arr) {
		arr = arr[:*i]
	}
	if items, ok := v.([]any); ok {
		arr = append(arr, items...)
	}
	if arr == nil {
		arr = []any{}
	}
	return arr
}

func index(k any) (int, bool) {
	f, ok := k.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// -------------------------------------------------------------------
// Session mapping
// -------------------------------------------------------------------

// chatSession is the subset of ISerializableChatData aii reads.
type chatSession struct {
	Version          int           `json:"version"`
	SessionID        string        `json:"sessionId"`
	CreationDate     int64         `json:"creationDate"`
	LastMessageDate  int64         `json:"lastMessageDate"`
	CustomTitle      string        `json:"customTitle"`
	ComputedTitle    string        `json:"computedTitle"`
	WorkingDirectory string        `json:"workingDirectory"`
	Requests         []chatRequest `json:"requests"`
}

type chatRequest struct {
	Timestamp            int64             `json:"timestamp"`
	Message              json.RawMessage   `json:"message"` // string (old) or {text, parts}
	Response             []json.RawMessage `json:"response"`
	IsHidden             bool              `json:"isHidden"`
	HiddenFromTranscript bool              `json:"hiddenFromTranscript"`
	// Hides only the user's message; the response stays visible.
	RequestHiddenFromTranscript bool `json:"requestHiddenFromTranscript"`
}

// responsePart covers the persisted response part shapes aii reads. A
// part with no kind is a markdown string ({value: "..."}).
type responsePart struct {
	Kind    string          `json:"kind"`
	Value   json.RawMessage `json:"value"`
	Content *struct {
		Value string `json:"value"`
	} `json:"content"`
	// inlineReference
	Name            string `json:"name"`
	InlineReference *struct {
		Path string `json:"path"`
		URI  *struct {
			Path string `json:"path"`
		} `json:"uri"`
	} `json:"inlineReference"`
	// toolInvocationSerialized
	ToolID            string          `json:"toolId"`
	InvocationMessage json.RawMessage `json:"invocationMessage"`
	PastTenseMessage  json.RawMessage `json:"pastTenseMessage"`
	ResultDetails     json.RawMessage `json:"resultDetails"`
	ResultError       json.RawMessage `json:"resultError"`
}

// sessionMessages maps each request to a user row followed by the
// response: thinking rows, assistant text, and a call plus result row per
// tool invocation.
func sessionMessages(s chatSession) []store.Message {
	var msgs []store.Message
	add := func(role string, ts int64, content string) {
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
		msgs = append(msgs, store.Message{Ordinal: len(msgs), Role: role, TS: ts, Content: content})
	}
	for _, r := range s.Requests {
		if r.IsHidden || r.HiddenFromTranscript {
			continue
		}
		ts := r.Timestamp / 1000
		if ts == 0 {
			ts = s.CreationDate / 1000
		}
		if !r.RequestHiddenFromTranscript {
			add("user", ts, messageText(r.Message))
		}

		// Markdown arrives in streamed chunks; join adjacent chunks into
		// one assistant row and flush it before any other part.
		var text strings.Builder
		flush := func() {
			add("assistant", ts, text.String())
			text.Reset()
		}
		for _, raw := range r.Response {
			var p responsePart
			if json.Unmarshal(raw, &p) != nil {
				continue
			}
			switch p.Kind {
			case "", "markdownContent":
				if p.Content != nil {
					text.WriteString(p.Content.Value)
				} else {
					text.WriteString(stringValue(p.Value))
				}
			case "markdownVuln":
				if p.Content != nil {
					text.WriteString(p.Content.Value)
				}
			case "inlineReference":
				text.WriteString("`" + inlineName(p) + "`")
			case "thinking":
				flush()
				add("thinking", ts, thinkingText(p.Value))
			case "toolInvocationSerialized":
				flush()
				input, output, isErr := toolDetails(p.ResultDetails)
				if input == "" {
					input = markdownText(p.InvocationMessage)
				}
				add("tool", ts, fmt.Sprintf("[tool_call name=%s] %s", p.ToolID, truncateRunes(input, maxToolInput)))
				if len(p.ResultError) > 0 && string(p.ResultError) != "null" {
					isErr = true
					if output == "" {
						output = markdownText(p.ResultError)
					}
				}
				if output == "" {
					output = markdownText(p.PastTenseMessage)
				}
				if output != "" {
					tag := "[tool_result"
					if isErr {
						tag += " error"
					}
					add("tool", ts, tag+" name="+p.ToolID+"] "+truncateRunes(strings.TrimSpace(output), maxToolResult))
				}
			}
			// progressMessage, codeblockUri, textEditGroup, undoStop,
			// confirmation, warning, and other UI parts: skipped.
		}
		flush()
	}
	return msgs
}

// messageText returns the text of a request message, which is a plain
// string in very old sessions and {text, parts} otherwise.
func messageText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &m) == nil {
		return m.Text
	}
	return ""
}

// markdownText reads a string | IMarkdownString value.
func markdownText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m struct {
		Value   string `json:"value"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &m) == nil {
		if m.Value != "" {
			return m.Value
		}
		return m.Message
	}
	return ""
}

func stringValue(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// thinkingText reads a thinking part's value: a string or a list of them.
func thinkingText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(list, "")
	}
	return ""
}

func inlineName(p responsePart) string {
	if p.Name != "" {
		return p.Name
	}
	if r := p.InlineReference; r != nil {
		if r.Path != "" {
			return path.Base(r.Path)
		}
		if r.URI != nil && r.URI.Path != "" {
			return path.Base(r.URI.Path)
		}
	}
	return ""
}

// toolDetails reads IToolResultInputOutputDetails: the tool input and the
// text of its embedded outputs.
func toolDetails(raw json.RawMessage) (input, output string, isErr bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return "", "", false
	}
	var d struct {
		Input   string          `json:"input"`
		Output  json.RawMessage `json:"output"`
		IsError bool            `json:"isError"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return "", "", false
	}
	input = compactJSON(d.Input)
	var outs []struct {
		Type     string `json:"type"`
		Value    any    `json:"value"`
		IsText   bool   `json:"isText"`
		MimeType string `json:"mimeType"`
	}
	var b strings.Builder
	if json.Unmarshal(d.Output, &outs) == nil {
		for _, o := range outs {
			s, ok := o.Value.(string)
			if !ok || o.Type != "embed" || !(o.IsText || o.MimeType == "" || strings.HasPrefix(o.MimeType, "text/")) {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(s)
		}
	} else {
		var s string
		if json.Unmarshal(d.Output, &s) == nil {
			b.WriteString(s)
		}
	}
	return input, b.String(), d.IsError
}

// compactJSON compacts s when it holds JSON and returns it unchanged
// otherwise.
func compactJSON(s string) string {
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(s)) == nil {
		return buf.String()
	}
	return strings.TrimSpace(s)
}

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
