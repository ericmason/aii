// Package aider parses Aider's per-project .aider.chat.history.md files
// into normalized Batches for indexing.
//
// Aider has no central session store. Each run appends to
// <git root>/.aider.chat.history.md (aider/args.py, --chat-history-file)
// through InputOutput.append_chat_history (aider/io.py), which writes:
//
//   - "# aider chat started at YYYY-MM-DD HH:MM:SS" (local time) at the
//     start of every run. Each header starts one session here.
//   - "#### <line>" for every line the user typed (user_input).
//   - "> <line>" for Aider's own tool output, warnings, errors, and
//     confirmation prompts (tool_output, tool_error, confirm_ask).
//   - Unprefixed Markdown for model replies (ai_output).
//
// Discovery: the file lives in each project, so the parser looks in
// three places and never crawls the home directory:
//
//  1. Every history file it has indexed before (index_state rows).
//  2. Every workspace other sources have recorded in the sessions table,
//     plus that workspace's enclosing git root.
//  3. The directories listed in AII_AIDER_ROOTS (os.PathListSeparator
//     separated), each searched up to maxRootDepth levels deep.
//
// Incremental indexing: the file is append-only, so the index_state row
// for the file remembers the byte offset of the last session header.
// Later runs re-read only from that offset. Every session gets its own
// source_path ("<file>#<header offset>") so the still-growing last
// session can be replaced on its own, without touching earlier ones.
package aider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

const (
	agentName = "aider"

	// HistoryFileName is Aider's default --chat-history-file name.
	HistoryFileName = ".aider.chat.history.md"

	// RootsEnv lists extra directories to search for history files.
	RootsEnv = "AII_AIDER_ROOTS"

	headerPrefix = "# aider chat started at "
	headerLayout = "2006-01-02 15:04:05"

	maxTitleRunes   = 200
	maxToolRunes    = 4096
	maxRootDepth    = 4
	maxGitRootClimb = 8
)

// skipDirs are never descended into while searching AII_AIDER_ROOTS.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "target": true, "dist": true,
	"build": true, "__pycache__": true, "venv": true, ".venv": true,
}

// homeSkipDirs are large system folders skipped when a root is the home
// directory itself.
var homeSkipDirs = map[string]bool{
	"Library": true, "Applications": true, "Movies": true, "Music": true,
	"Pictures": true, "AppData": true, "snap": true,
}

// Source implements source.Source for Aider.
type Source struct {
	// Roots overrides AII_AIDER_ROOTS (useful for tests).
	Roots []string
}

// New returns a default Aider Source.
func New() *Source { return &Source{} }

// Name returns the agent identifier used for sessions.agent.
func (s *Source) Name() string { return agentName }

// Scan finds history files and emits a Batch per new or changed session.
func (s *Source) Scan(ctx context.Context, db *store.DB, full bool, out chan<- source.Batch) error {
	files := s.discover(ctx, db)
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := scanFile(ctx, db, full, out, path); err != nil {
			if err == context.Canceled || err == context.DeadlineExceeded {
				return err
			}
			log.Printf("aider: scan %s: %v", path, err)
		}
	}
	return nil
}

// -------------------------------------------------------------------
// Discovery
// -------------------------------------------------------------------

// discover returns the absolute paths of every history file found, with
// duplicates removed, in a stable order.
func (s *Source) discover(ctx context.Context, db *store.DB) []string {
	seen := map[string]bool{}
	var infos []os.FileInfo
	var files []string
	add := func(path string) {
		abs, err := filepath.Abs(path)
		if err != nil || seen[abs] {
			return
		}
		seen[abs] = true
		// A symlinked or differently cased path can reach a file already
		// found; resolve it and compare the files themselves.
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return
		}
		fi, err := os.Stat(resolved)
		if err != nil || !fi.Mode().IsRegular() {
			return
		}
		for _, other := range infos {
			if os.SameFile(fi, other) {
				return
			}
		}
		infos = append(infos, fi)
		files = append(files, abs)
	}

	// 1. Files indexed on an earlier run.
	if rows, err := db.QueryContext(ctx,
		`SELECT source_path FROM index_state WHERE source_path LIKE ?`,
		"%"+HistoryFileName); err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				add(p)
			}
		}
		rows.Close()
	}

	// 2. Workspaces any source has recorded.
	if rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT workspace FROM sessions WHERE workspace IS NOT NULL AND workspace != ''`); err == nil {
		var workspaces []string
		for rows.Next() {
			var ws string
			if rows.Scan(&ws) == nil {
				workspaces = append(workspaces, ws)
			}
		}
		rows.Close()
		for _, ws := range workspaces {
			if !filepath.IsAbs(ws) {
				continue
			}
			add(filepath.Join(ws, HistoryFileName))
			if root := gitRoot(ws); root != "" {
				add(filepath.Join(root, HistoryFileName))
			}
		}
	}

	// 3. Configured roots.
	for _, root := range s.roots() {
		walkRoot(ctx, root, add)
	}
	return files
}

func (s *Source) roots() []string {
	if s.Roots != nil {
		return s.Roots
	}
	var out []string
	for _, r := range filepath.SplitList(os.Getenv(RootsEnv)) {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if r == "~" || strings.HasPrefix(r, "~/") || strings.HasPrefix(r, "~"+string(filepath.Separator)) {
			if home, err := os.UserHomeDir(); err == nil {
				r = filepath.Join(home, r[1:])
			}
		}
		out = append(out, r)
	}
	return out
}

// gitRoot walks up from dir to the nearest directory holding .git, the
// default location Aider writes its history file to.
func gitRoot(dir string) string {
	d := filepath.Clean(dir)
	for i := 0; i < maxGitRootClimb; i++ {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
	return ""
}

// walkRoot looks for history files in root and up to maxRootDepth levels
// of subdirectories, skipping hidden and dependency directories.
func walkRoot(ctx context.Context, root string, add func(string)) {
	root = filepath.Clean(root)
	baseDepth := strings.Count(root, string(filepath.Separator))
	isHome := false
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		isHome = filepath.Clean(home) == root
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !d.IsDir() {
			if d.Name() == HistoryFileName {
				add(path)
			}
			return nil
		}
		if path != root {
			name := d.Name()
			if strings.HasPrefix(name, ".") || skipDirs[name] {
				return filepath.SkipDir
			}
			if isHome && homeSkipDirs[name] && filepath.Dir(path) == root {
				return filepath.SkipDir
			}
			if strings.Count(path, string(filepath.Separator))-baseDepth > maxRootDepth {
				return filepath.SkipDir
			}
		}
		return nil
	})
}

// -------------------------------------------------------------------
// Per-file scan
// -------------------------------------------------------------------

// scanFile reads the new part of one history file and emits its sessions.
func scanFile(ctx context.Context, db *store.DB, full bool, out chan<- source.Batch, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	size := fi.Size()
	mtimeNs := fi.ModTime().UnixNano()

	prev, err := db.GetState(path)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}

	start := int64(0)
	reset := full
	if prev != nil {
		switch {
		case size < prev.Size:
			reset = true // rewritten or truncated
		case !full && size == prev.Size && mtimeNs == prev.MtimeNs:
			return nil
		case !full:
			start = prev.LastOffset
		}
	}
	if start > size {
		start, reset = 0, true
	}
	// A saved offset always points at a session header. If it doesn't
	// any more, the file was replaced (say, deleted and recreated
	// larger); re-read all of it.
	if start > 0 && !atHeader(path, start) {
		start, reset = 0, true
	}

	// On a reset, drop every session this file produced before so a
	// shorter file can't leave stale sessions behind.
	if reset && prev != nil {
		keys, err := sessionKeys(db, path)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := send(ctx, out, source.Batch{
				State:    store.IndexState{SourcePath: k},
				Truncate: true,
			}); err != nil {
				return err
			}
		}
	}

	data, err := readRange(path, start, size)
	if err != nil {
		return err
	}

	// The saved offsets are always the start of a header line, so they
	// never land inside an unterminated last line: a partial line is
	// re-read, and its session replaced, on the next run.
	sessions := splitSessions(data, start)
	workspace := filepath.Dir(path)
	lastOffset := start
	for i, ps := range sessions {
		lastOffset = ps.Offset
		key := sessionKey(path, ps.Offset)
		ended := ps.StartedAt
		if i == len(sessions)-1 {
			// Only the last session can still be growing; the file's
			// mtime is the best end time we have for it.
			ended = fi.ModTime().Unix()
		}
		msgs := ps.Messages
		for j := range msgs {
			msgs[j].Ordinal = j
			// Aider records no per-message times; use the session start.
			msgs[j].TS = ps.StartedAt
		}
		b := source.Batch{
			Session: store.Session{
				Agent:         agentName,
				UID:           sessionUID(path, ps.Offset),
				Workspace:     workspace,
				Title:         title(msgs),
				StartedAt:     ps.StartedAt,
				EndedAt:       ended,
				SourcePath:    key,
				SourceMtimeNs: mtimeNs,
				SourceSize:    size,
			},
			Messages: msgs,
			State:    store.IndexState{SourcePath: key, MtimeNs: mtimeNs, Size: size, LastOffset: ps.Offset},
			// Replace the session wholesale: it's small, and the tail
			// session's last message may have grown since the last run.
			Truncate: true,
		}
		if len(msgs) == 0 {
			// A header with nothing after it yet. Remember nothing but
			// the offset, so the session is picked up once it has text.
			b = source.Batch{State: b.State, Truncate: true}
		}
		if err := send(ctx, out, b); err != nil {
			return err
		}
	}

	return send(ctx, out, source.Batch{State: store.IndexState{
		SourcePath: path,
		MtimeNs:    mtimeNs,
		Size:       size,
		LastOffset: lastOffset,
	}})
}

// atHeader reports whether the bytes at offset begin a session header.
func atHeader(path string, offset int64) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, len(headerPrefix))
	if _, err := f.ReadAt(buf, offset); err != nil {
		return false
	}
	return string(buf) == headerPrefix
}

// readRange reads path from start up to size, the length from the stat
// the saved state records. Text Aider appends during the read is left for
// the next run, so the stored size always matches what was indexed.
func readRange(path string, start, size int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, size-start))
}

func send(ctx context.Context, out chan<- source.Batch, b source.Batch) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case out <- b:
		return nil
	}
}

// sessionKey is the source_path recorded for one session of a file.
func sessionKey(path string, offset int64) string {
	return path + "#" + strconv.FormatInt(offset, 10)
}

// sessionUID derives a stable hex id from the file path and the header's
// byte offset. Hex keeps it valid in cite tokens.
func sessionUID(path string, offset int64) string {
	sum := sha256.Sum256([]byte(sessionKey(path, offset)))
	return hex.EncodeToString(sum[:16])
}

// sessionKeys lists the per-session source paths already stored for path.
func sessionKeys(db *store.DB, path string) ([]string, error) {
	rows, err := db.Query(
		`SELECT source_path FROM sessions WHERE agent = ? AND substr(source_path, 1, ?) = ?`,
		agentName, len(path)+1, path+"#")
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// -------------------------------------------------------------------
// Markdown parsing
// -------------------------------------------------------------------

// parsedSession is one "# aider chat started at" section of a file.
type parsedSession struct {
	Offset    int64 // absolute byte offset of the header line
	StartedAt int64 // unix seconds, from the header (local time)
	Messages  []store.Message
}

type lineKind int

const (
	kindNone lineKind = iota
	kindUser
	kindTool
	kindAssistant
)

// splitSessions parses data (which begins at byte base of the file) into
// sessions. Text before the first header, if any, becomes a session
// anchored at base with no start time.
func splitSessions(data []byte, base int64) []parsedSession {
	var (
		sessions []parsedSession
		cur      *parsedSession
		kind     = kindNone
		buf      []string
		bufStart int64
		// afterBlank is true at the start of the data, after a header,
		// and after a blank line.
		afterBlank = true
	)

	flush := func() {
		if cur != nil && kind != kindNone {
			if m, ok := buildMessage(kind, buf, bufStart); ok {
				cur.Messages = append(cur.Messages, m)
			}
		}
		kind, buf = kindNone, nil
	}

	offset := base
	for len(data) > 0 {
		lineLen := len(data)
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			lineLen = i + 1
		}
		raw := string(data[:lineLen])
		data = data[lineLen:]
		lineStart := offset
		offset += int64(lineLen)

		line := strings.TrimRight(raw, "\r\n")
		if ts, ok := parseHeader(line); ok {
			flush()
			sessions = append(sessions, parsedSession{Offset: lineStart, StartedAt: ts})
			cur = &sessions[len(sessions)-1]
			afterBlank = true
			continue
		}
		if cur == nil {
			if strings.TrimSpace(line) == "" {
				continue
			}
			sessions = append(sessions, parsedSession{Offset: lineStart})
			cur = &sessions[len(sessions)-1]
		}

		k, text := classify(line)
		if k == kindNone {
			// Blank line: part of an assistant reply, a separator
			// everywhere else.
			if kind == kindAssistant {
				buf = append(buf, "")
			} else {
				flush()
			}
			afterBlank = true
			continue
		}
		// Aider writes each user line as "#### text  " after a blank
		// line or another user line. Anything else starting "#### " is a
		// heading in the model's reply.
		if k == kindUser && !(strings.HasSuffix(line, "  ") && (afterBlank || kind == kindUser)) {
			k, text = kindAssistant, line
		}
		// Aider prefixes only the first line of a multi-line tool
		// message with "> "; the rest follow it with no blank line.
		if k == kindAssistant && kind == kindTool && !afterBlank {
			k, text = kindTool, strings.TrimSuffix(line, "  ")
		}
		afterBlank = false
		if k != kind {
			flush()
			kind, bufStart = k, lineStart
		}
		buf = append(buf, text)
	}
	flush()
	return sessions
}

// parseHeader recognizes a session header and returns its time.
func parseHeader(line string) (int64, bool) {
	rest, ok := strings.CutPrefix(line, headerPrefix)
	if !ok {
		return 0, false
	}
	t, err := time.ParseInLocation(headerLayout, strings.TrimSpace(rest), time.Local)
	if err != nil {
		return 0, true
	}
	return t.Unix(), true
}

// classify returns a line's kind and its text with Aider's prefix and
// Markdown hard-break ("  ") removed.
func classify(line string) (lineKind, string) {
	if strings.TrimSpace(line) == "" {
		return kindNone, ""
	}
	// Aider always writes the prefix followed by a space. Requiring it
	// keeps edit-block markers like ">>>>>>> REPLACE" in the reply.
	if rest, ok := strings.CutPrefix(line, "#### "); ok {
		return kindUser, strings.TrimSuffix(rest, "  ")
	}
	if rest, ok := strings.CutPrefix(line, "> "); ok {
		return kindTool, strings.TrimSuffix(rest, "  ")
	}
	return kindAssistant, line
}

func buildMessage(k lineKind, lines []string, offset int64) (store.Message, bool) {
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if text == "" || (k == kindUser && text == "<blank>") {
		return store.Message{}, false
	}
	m := store.Message{SourceOffset: offset}
	switch k {
	case kindUser:
		m.Role = "user"
	case kindTool:
		m.Role = "tool"
		text = "[tool_result] " + truncateRunes(text, maxToolRunes)
	default:
		m.Role = "assistant"
	}
	m.Content = text
	return m, true
}

// title prefers the first user message that isn't a slash command, and
// keeps only its first line.
func title(msgs []store.Message) string {
	first := ""
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		if first == "" {
			first = m.Content
		}
		if !strings.HasPrefix(m.Content, "/") {
			return truncateRunes(firstLine(m.Content), maxTitleRunes)
		}
	}
	return truncateRunes(firstLine(first), maxTitleRunes)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// truncateRunes shortens s to at most n runes.
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
