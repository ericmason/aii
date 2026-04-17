// Command aii indexes and searches local AI coding-agent transcripts
// (Claude Code, Codex, Cursor) using a SQLite FTS5 database.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"log"
	"os/exec"
	"runtime"

	"github.com/ericmason/aii/internal/indexer"
	"github.com/ericmason/aii/internal/mcpserver"
	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/source/claudecode"
	"github.com/ericmason/aii/internal/source/codex"
	"github.com/ericmason/aii/internal/source/cursor"
	"github.com/ericmason/aii/internal/store"
	"github.com/ericmason/aii/internal/tui"
	"github.com/ericmason/aii/internal/web"
)

const aiiVersion = "0.2.0"

const usageText = `aii — search every AI chat you've had locally

Usage:
  aii index   [--source all|cc|codex|cursor] [--full] [--verbose]
  aii search  <query> [--agent cc|codex|cursor] [--workspace DIR]
                       [--since 7d|2026-01-01] [--limit 20] [--json]
  aii show    <session-uid> | --last [--format md|plain]
  aii ask     <question> [--k 6] [--context 2] [--agent ..] [--since ..]
                         [--cmd "claude -p"] [--dry-run] [--show-sources]
  aii related <uid> [--limit 10] [--agent ..]  # find similar sessions
  aii mcp                                       # stdio MCP server for agent use
  aii stats
  aii serve   [--addr 127.0.0.1:8723]
  aii ui      [--addr 127.0.0.1:8723]       # serve + open browser (alias: web)
  aii tui
  aii doctor
  aii cron    install|uninstall|status|run  # schedule background indexing

The database lives at $AII_DB or ~/.local/share/aii/aii.db.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usageText)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var err error
	switch cmd {
	case "index":
		err = cmdIndex(ctx, args)
	case "search":
		err = cmdSearch(args)
	case "ask":
		err = cmdAsk(ctx, args)
	case "show":
		err = cmdShow(args)
	case "stats":
		err = cmdStats(args)
	case "serve":
		err = cmdServe(ctx, args)
	case "ui", "web":
		err = cmdUI(ctx, args)
	case "tui":
		err = cmdTUI(ctx, args)
	case "doctor":
		err = cmdDoctor()
	case "related":
		err = cmdRelated(args)
	case "mcp":
		err = cmdMCP(ctx, args)
	case "cron":
		err = cmdCron(args)
	case "help", "-h", "--help":
		if helpAsJSON(args) {
			err = cmdHelpJSON()
		} else {
			fmt.Print(usageText)
			return
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usageText)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// --- index -------------------------------------------------------------

func cmdIndex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	which := fs.String("source", "all", "all|cc|codex|cursor")
	full := fs.Bool("full", false, "reindex from scratch")
	verbose := fs.Bool("verbose", false, "log each session indexed")
	quiet := fs.Bool("quiet", false, "suppress the summary line (used by background spawns)")
	fs.Parse(reorderFlags(args))

	// One indexer at a time. Cron + manual + background-spawned runs
	// would otherwise contend on the single SQLite writer.
	release, ok, otherPID := acquireIndexLock()
	if !ok {
		if !*quiet {
			fmt.Printf("another indexer is running (pid %d); skipping\n", otherPID)
		}
		return nil
	}
	defer release()

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	sources, err := selectSources(*which)
	if err != nil {
		return err
	}

	stats, err := indexer.Run(ctx, db, sources, indexer.Options{Full: *full, Verbose: *verbose})
	if err != nil {
		return err
	}
	markIndexed()
	if !*quiet {
		fmt.Printf("indexed %d sessions, %d messages from %d sources in %s (%d errors)\n",
			stats.SessionsTouch, stats.MessagesAdded, stats.Sources, stats.Duration.Truncate(time.Millisecond), stats.Errors)
	}

	if *verbose {
		t := time.Now()
		if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			log.Printf("checkpoint: %v", err)
		} else {
			log.Printf("checkpoint: %s", time.Since(t).Truncate(time.Millisecond))
		}
		t = time.Now()
		if _, err := db.Exec(`ANALYZE`); err != nil {
			log.Printf("analyze: %v", err)
		} else {
			log.Printf("analyze: %s", time.Since(t).Truncate(time.Millisecond))
		}
	} else {
		db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	return nil
}

func selectSources(which string) ([]source.Source, error) {
	var out []source.Source
	add := func(s source.Source) { out = append(out, s) }
	switch which {
	case "all":
		add(claudecode.New()); add(codex.New()); add(cursor.New())
	case "cc", "claude_code", "claude":
		add(claudecode.New())
	case "codex":
		add(codex.New())
	case "cursor":
		add(cursor.New())
	default:
		return nil, fmt.Errorf("unknown --source %q", which)
	}
	return out, nil
}

// --- search ------------------------------------------------------------

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	agent := fs.String("agent", "", "cc|codex|cursor")
	workspace := fs.String("workspace", "", "exact workspace path")
	role := fs.String("role", "", "user|assistant|tool|thinking")
	since := fs.String("since", "", "7d | 2026-01-01")
	limit := fs.Int("limit", 20, "max results")
	offset := fs.Int("offset", 0, "skip this many results before returning (pagination)")
	jsonOut := fs.Bool("json", false, "emit a single JSON array (all results)")
	ndjsonOut := fs.Bool("ndjson", false, "emit one JSON line per excerpt (agent-friendly, streamable)")
	agentFlag := fs.Bool("agent-mode", false, "force agent-oriented output (default when stdout is piped)")
	pretty := fs.Bool("pretty", false, "force human-readable output even when piped")
	maxBytes := fs.Int("max-bytes", 0, "soft cap on total output bytes (default: unlimited)")
	fs.Parse(reorderFlags(args))
	if fs.NArg() == 0 {
		return errors.New("search requires a query")
	}
	q := strings.Join(fs.Args(), " ")

	kickBackgroundIndex()

	sinceUnix, err := parseSince(*since)
	if err != nil {
		return err
	}

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	results, err := db.Search(store.Query{
		Text:      q,
		Agent:     normalizeAgent(*agent),
		Workspace: *workspace,
		Role:      normalizeRole(*role),
		SinceUnix: sinceUnix,
		Limit:     *limit,
		Offset:    *offset,
	})
	if err != nil {
		return err
	}

	switch pickSearchFormat(*jsonOut, *ndjsonOut, *pretty, *agentFlag) {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	case "ndjson":
		return writeSearchNDJSON(os.Stdout, results, *offset, *maxBytes)
	default:
		return writeSearchPretty(os.Stdout, results, *offset, *maxBytes)
	}
}

// pickSearchFormat resolves the four output-mode flags into one of
// "json" | "ndjson" | "pretty". Explicit flags take precedence over
// TTY detection. When stdout isn't a TTY and nothing else is set, we
// default to ndjson so piped consumers (agents, shell scripts) get
// structured output by default.
func pickSearchFormat(jsonOut, ndjsonOut, pretty, agentFlag bool) string {
	if jsonOut {
		return "json"
	}
	if ndjsonOut {
		return "ndjson"
	}
	if pretty {
		return "pretty"
	}
	if agentFlag || isAgentEnv() || !stdoutIsTTY {
		return "ndjson"
	}
	return "pretty"
}

func isAgentEnv() bool { return os.Getenv("AII_AGENT") != "" }

// writeSearchPretty is the human-readable table we've always had.
func writeSearchPretty(w *os.File, results []store.Result, offset, maxBytes int) error {
	if len(results) == 0 {
		fmt.Fprintln(w, cMuted("no results"))
		return nil
	}
	var b strings.Builder
	fmt.Fprintln(&b, cMuted(fmt.Sprintf("— %d session%s, sorted by relevance —", len(results), pluralS(len(results)))))
	b.WriteString("\n")
	for i, r := range results {
		date := store.FormatTime(r.StartedAt)
		if date == "" {
			date = "—"
		}
		ws := r.Workspace
		if ws == "" {
			ws = "—"
		}
		title := r.Title
		if title == "" {
			title = r.SessionUID
		}
		rank := cMuted(fmt.Sprintf("%2d.", i+1+offset))
		badge := cAgent(r.Agent)
		cite := cMuted(fmt.Sprintf("%s/%s", shortAgent(r.Agent), shortUID(r.SessionUID)))
		dateCol := cMuted(date)
		matches := ""
		if r.MatchCount > len(r.Excerpts) {
			matches = cMuted(fmt.Sprintf(" (+%d more hit%s)", r.MatchCount-len(r.Excerpts), pluralS(r.MatchCount-len(r.Excerpts))))
		}
		fmt.Fprintf(&b, "%s %s  %s  %s  %s %s%s\n",
			rank, badge, dateCol, cite, cMuted("·"), cHead(truncate(title, 80)), matches)
		fmt.Fprintf(&b, "     %s %s\n", cMuted("↳"), cAccent(truncate(ws, 100)))
		if s := strings.TrimSpace(r.Summary); s != "" {
			fmt.Fprintf(&b, "     %s %s\n", cMuted("▌"), cDim(truncate(s, 180)))
		}
		for j, e := range r.Excerpts {
			glyph := "├─"
			if j == len(r.Excerpts)-1 {
				glyph = "└─"
			}
			line := strings.ReplaceAll(strings.ReplaceAll(e.Snippet, "\n", " "), "\r", " ")
			line = highlightHit(line)
			cite := cMuted(fmt.Sprintf("%s:%d", shortUID(r.SessionUID), e.Ordinal))
			fmt.Fprintf(&b, "     %s %s %s  %s\n", cMuted(glyph), cRole(e.Role), cite, line)
		}
		b.WriteString("\n")
	}
	return writeCapped(w, b.String(), maxBytes)
}

// ndjsonHit is the flat, self-contained row we emit per excerpt. Agents
// can parse it as a stream and each line is complete on its own — no
// need to track prior session context across lines.
type ndjsonHit struct {
	Cite         string  `json:"cite"` // cc/abc12345:42 — agent-uid:ordinal
	Agent        string  `json:"agent"`
	UID          string  `json:"uid"`
	Ordinal      int     `json:"ordinal"`
	Role         string  `json:"role"`
	TS           int64   `json:"ts,omitempty"`
	Score        float64 `json:"score"`
	SessionScore float64 `json:"session_score"`
	MatchCount   int     `json:"match_count"`
	Rank         int     `json:"rank"`
	Title        string  `json:"title,omitempty"`
	Workspace    string  `json:"workspace,omitempty"`
	StartedAt    int64   `json:"started_at,omitempty"`
	Snippet      string  `json:"snippet"`
}

// writeSearchNDJSON streams one compact JSON object per excerpt. If a
// byte budget is set, we stop emitting and add a final {"truncated":
// true, ...} marker — never split a JSON line.
func writeSearchNDJSON(w *os.File, results []store.Result, offset, maxBytes int) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	bytesWritten := 0
	skipped := 0
	for i, r := range results {
		rank := i + 1 + offset
		for _, e := range r.Excerpts {
			hit := ndjsonHit{
				Cite:         fmt.Sprintf("%s/%s:%d", shortAgent(r.Agent), shortUID(r.SessionUID), e.Ordinal),
				Agent:        r.Agent,
				UID:          r.SessionUID,
				Ordinal:      e.Ordinal,
				Role:         e.Role,
				TS:           e.TS,
				Score:        e.Score,
				SessionScore: r.BestScore,
				MatchCount:   r.MatchCount,
				Rank:         rank,
				Title:        r.Title,
				Workspace:    r.Workspace,
				StartedAt:    r.StartedAt,
				Snippet:      e.Snippet,
			}
			// Pre-serialize to measure bytes before committing to stdout.
			buf, err := json.Marshal(hit)
			if err != nil {
				return err
			}
			line := append(buf, '\n')
			if maxBytes > 0 && bytesWritten+len(line) > maxBytes {
				skipped++
				continue
			}
			if _, err := w.Write(line); err != nil {
				return err
			}
			bytesWritten += len(line)
		}
	}
	_ = enc // reserved for future streaming extensions
	if skipped > 0 {
		fmt.Fprintf(w, "{\"truncated\":true,\"skipped_hits\":%d}\n", skipped)
	}
	return nil
}

// writeCapped honors --max-bytes for pretty output by slicing on rune
// boundaries and appending an ellipsis marker.
func writeCapped(w *os.File, s string, maxBytes int) error {
	if maxBytes > 0 && len(s) > maxBytes {
		// Back off to the last newline within budget so we don't cut a
		// line in half — much easier on the eyes.
		cut := maxBytes
		if nl := strings.LastIndexByte(s[:cut], '\n'); nl > 0 {
			cut = nl
		}
		s = s[:cut] + "\n…[truncated]\n"
	}
	_, err := io.WriteString(w, s)
	return err
}

func normalizeRole(r string) string {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "", "all":
		return ""
	case "user", "u", "human":
		return "user"
	case "assistant", "a", "ai", "asst":
		return "assistant"
	case "thinking", "t", "thnk", "think":
		return "thinking"
	case "tool":
		return "tool"
	}
	return r
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// --- ask ---------------------------------------------------------------

// cmdAsk turns the aii corpus into a personal RAG: pulls the top-k
// sessions matching the question, expands each best hit with ±N
// surrounding messages, assembles a prompt, and pipes it to a
// configured LLM CLI. With --dry-run it prints the prompt and exits,
// which is how you compose with any other tool (`aii ask ... --dry-run
// | llama`, etc.).
func cmdAsk(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	k := fs.Int("k", 6, "number of sessions to retrieve")
	contextN := fs.Int("context", 2, "messages of context to include around each hit (±N)")
	agent := fs.String("agent", "", "cc|codex|cursor")
	workspace := fs.String("workspace", "", "exact workspace path")
	since := fs.String("since", "", "7d | 2026-01-01")
	cmdStr := fs.String("cmd", "", "LLM command to pipe the prompt into (default: $AII_ASK_CMD or `claude -p`)")
	dryRun := fs.Bool("dry-run", false, "print the assembled prompt instead of running the LLM")
	showSources := fs.Bool("show-sources", false, "after the answer, print the session UIDs used")
	maxChars := fs.Int("max-msg-chars", 4000, "truncate any single message to this many chars")
	fs.Parse(reorderFlags(args))
	if fs.NArg() == 0 {
		return errors.New("ask requires a question")
	}
	question := strings.Join(fs.Args(), " ")

	kickBackgroundIndex()

	sinceUnix, err := parseSince(*since)
	if err != nil {
		return err
	}

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	results, err := db.Search(store.Query{
		Text:      question,
		Agent:     normalizeAgent(*agent),
		Workspace: *workspace,
		SinceUnix: sinceUnix,
		Limit:     *k,
	})
	if err != nil {
		return err
	}
	if len(results) == 0 {
		return errors.New("no relevant transcripts found")
	}

	prompt, usedUIDs, err := buildAskPrompt(db, question, results, *contextN, *maxChars)
	if err != nil {
		return err
	}

	if *dryRun {
		fmt.Print(prompt)
		if *showSources {
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "sources:")
			for _, u := range usedUIDs {
				fmt.Fprintln(os.Stderr, " ", u)
			}
		}
		return nil
	}

	shellCmd := resolveAskCmd(*cmdStr)
	if shellCmd == "" {
		return errors.New("no LLM command available: install `claude`, set $AII_ASK_CMD, or pass --cmd (or use --dry-run)")
	}

	// Stream the prompt into `sh -c $cmd` and the response back out.
	fmt.Fprintln(os.Stderr, cMuted(fmt.Sprintf("aii ask: %d sources via `%s`", len(usedUIDs), shellCmd)))
	runner := exec.CommandContext(ctx, "sh", "-c", shellCmd)
	runner.Stdin = strings.NewReader(prompt)
	runner.Stdout = os.Stdout
	runner.Stderr = os.Stderr
	if err := runner.Run(); err != nil {
		return fmt.Errorf("ask command failed: %w", err)
	}
	if *showSources {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "sources:")
		for _, u := range usedUIDs {
			fmt.Fprintln(os.Stderr, " ", u)
		}
	}
	return nil
}

// buildAskPrompt assembles the full prompt sent to the LLM. For each
// result we pull ±contextN messages around the best-scoring excerpt so
// the model sees both question and answer, not just the matching line.
func buildAskPrompt(db *store.DB, question string, results []store.Result, contextN, maxChars int) (string, []string, error) {
	var b strings.Builder
	b.WriteString("You are helping the user recall details from their own past conversations with AI coding agents (Claude Code, Codex, Cursor).\n")
	b.WriteString("Answer the user's question using ONLY the transcript excerpts below. If the excerpts don't contain the answer, say so plainly — do not invent.\n")
	b.WriteString("Cite sources inline as [uid:ordinal] (e.g. [7a3f2b1c:42]). Prefer concrete details (commands run, file paths, exact error text) over paraphrase.\n\n")
	b.WriteString("--- transcript excerpts ---\n\n")

	var uids []string
	for i, r := range results {
		uids = append(uids, shortUID(r.SessionUID))

		// Pick the best-scoring excerpt's ordinal as the anchor.
		anchor := 0
		if len(r.Excerpts) > 0 {
			anchor = r.Excerpts[0].Ordinal
			for _, e := range r.Excerpts[1:] {
				if e.Score < r.Excerpts[0].Score {
					anchor = e.Ordinal
				}
			}
		}
		ctxRows, err := db.ContextRows(r.SessionID, anchor, contextN)
		if err != nil {
			return "", nil, err
		}

		fmt.Fprintf(&b, "## [%s] session %s (%s)\n", r.Agent, shortUID(r.SessionUID), store.FormatTime(r.StartedAt))
		if r.Workspace != "" {
			fmt.Fprintf(&b, "_workspace: %s_\n", r.Workspace)
		}
		if r.Title != "" {
			fmt.Fprintf(&b, "_title: %s_\n", r.Title)
		}
		b.WriteString("\n")
		for _, row := range ctxRows {
			content := row.Content
			if len(content) > maxChars {
				content = content[:maxChars] + "…[truncated]"
			}
			fmt.Fprintf(&b, "### %s:%d [%s]\n%s\n\n",
				shortUID(r.SessionUID), row.Ordinal, row.Role, content)
		}
		if i < len(results)-1 {
			b.WriteString("\n")
		}
	}

	b.WriteString("\n--- question ---\n")
	b.WriteString(question)
	b.WriteString("\n")
	return b.String(), uids, nil
}

// resolveAskCmd picks the LLM shell command to pipe the prompt into.
// Priority: explicit flag > $AII_ASK_CMD > `claude -p` if on PATH >
// `codex exec` if on PATH > "" (caller errors).
func resolveAskCmd(flag string) string {
	if strings.TrimSpace(flag) != "" {
		return flag
	}
	if env := strings.TrimSpace(os.Getenv("AII_ASK_CMD")); env != "" {
		return env
	}
	if _, err := exec.LookPath("claude"); err == nil {
		return "claude -p"
	}
	if _, err := exec.LookPath("codex"); err == nil {
		return "codex exec"
	}
	return ""
}

// --- show --------------------------------------------------------------

func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	last := fs.Bool("last", false, "show most recent session")
	format := fs.String("format", "md", "md|plain|ndjson")
	around := fs.Int("around", -1, "anchor on this ordinal (pairs with --span)")
	span := fs.Int("span", 3, "messages of context to show on each side of --around")
	from := fs.Int("from", -1, "first ordinal to include (inclusive)")
	to := fs.Int("to", -1, "last ordinal to include (inclusive)")
	role := fs.String("role", "", "user|assistant|tool|thinking — keep only messages with this role")
	maxMsgChars := fs.Int("max-msg-chars", 0, "truncate each message's content to this many chars (0 = no cap)")
	maxBytes := fs.Int("max-bytes", 0, "soft cap on total output bytes (0 = no cap)")
	fs.Parse(reorderFlags(args))

	kickBackgroundIndex()

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	var session *store.Session
	if *last {
		session, err = db.LatestSession()
	} else {
		if fs.NArg() == 0 {
			return errors.New("show requires a session UID or --last")
		}
		session, err = db.SessionByUIDAny(fs.Arg(0))
	}
	if err != nil {
		return err
	}
	if session == nil {
		return errors.New("session not found")
	}

	rows, err := fetchSlice(db, session.ID, *around, *span, *from, *to)
	if err != nil {
		return err
	}
	rows = filterRows(rows, normalizeRole(*role), *maxMsgChars)

	return renderSession(os.Stdout, *session, rows, *format, *maxBytes)
}

// fetchSlice returns the subset of a session's messages selected by
// --around/--span or --from/--to. When neither is set it returns the
// whole session, preserving the legacy `aii show <uid>` behavior.
func fetchSlice(db *store.DB, sessionID int64, around, span, from, to int) ([]store.Row, error) {
	switch {
	case around >= 0:
		return db.ContextRows(sessionID, around, span)
	case from >= 0 || to >= 0:
		if from < 0 {
			from = 0
		}
		if to < 0 {
			to = 1 << 30
		}
		// ContextRows takes (anchor, span); we abuse it by centering on
		// (from+to)/2 with half-width span. Simpler: use a direct query.
		return db.RangeRows(sessionID, from, to)
	default:
		return db.SessionMessages(sessionID)
	}
}

// filterRows drops rows whose role doesn't match (if a role filter is
// set) and truncates overly long content when maxMsgChars > 0.
func filterRows(rows []store.Row, role string, maxMsgChars int) []store.Row {
	out := rows[:0]
	for _, r := range rows {
		if role != "" && r.Role != role {
			continue
		}
		if maxMsgChars > 0 && len(r.Content) > maxMsgChars {
			r.Content = r.Content[:maxMsgChars] + "…[truncated]"
		}
		out = append(out, r)
	}
	return out
}

// renderSession emits the session in the requested format, honoring the
// --max-bytes cap. md/plain render everything into a buffer so we can
// cap precisely; ndjson streams, re-checking the budget between rows.
func renderSession(w *os.File, s store.Session, rows []store.Row, format string, maxBytes int) error {
	switch format {
	case "ndjson":
		return writeSessionNDJSON(w, s, rows, maxBytes)
	case "plain":
		var b strings.Builder
		fmt.Fprintf(&b, "%s  %s  %s  %s\n\n", s.Agent, s.UID, store.FormatTime(s.StartedAt), s.Workspace)
		for _, r := range rows {
			fmt.Fprintf(&b, "[%s] %s  %s\n%s\n\n",
				r.Role, store.FormatTime(r.TS),
				fmt.Sprintf("%s/%s:%d", shortAgent(s.Agent), shortUID(s.UID), r.Ordinal),
				r.Content)
		}
		return writeCapped(w, b.String(), maxBytes)
	default: // md
		var b strings.Builder
		fmt.Fprintf(&b, "# %s  %s\n\n", s.Agent, s.UID)
		if s.Workspace != "" {
			fmt.Fprintf(&b, "_%s_\n\n", s.Workspace)
		}
		if s.Title != "" {
			fmt.Fprintf(&b, "> %s\n\n", s.Title)
		}
		for _, r := range rows {
			cite := fmt.Sprintf("%s/%s:%d", shortAgent(s.Agent), shortUID(s.UID), r.Ordinal)
			fmt.Fprintf(&b, "## %s  `%s`  _%s_\n\n%s\n\n", r.Role, cite, store.FormatTime(r.TS), r.Content)
		}
		return writeCapped(w, b.String(), maxBytes)
	}
}

type ndjsonRow struct {
	Cite    string `json:"cite"`
	Agent   string `json:"agent"`
	UID     string `json:"uid"`
	Ordinal int    `json:"ordinal"`
	Role    string `json:"role"`
	TS      int64  `json:"ts,omitempty"`
	Content string `json:"content"`
}

func writeSessionNDJSON(w *os.File, s store.Session, rows []store.Row, maxBytes int) error {
	bytesWritten := 0
	skipped := 0
	for _, r := range rows {
		line, err := json.Marshal(ndjsonRow{
			Cite:    fmt.Sprintf("%s/%s:%d", shortAgent(s.Agent), shortUID(s.UID), r.Ordinal),
			Agent:   s.Agent,
			UID:     s.UID,
			Ordinal: r.Ordinal,
			Role:    r.Role,
			TS:      r.TS,
			Content: r.Content,
		})
		if err != nil {
			return err
		}
		line = append(line, '\n')
		if maxBytes > 0 && bytesWritten+len(line) > maxBytes {
			skipped++
			continue
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		bytesWritten += len(line)
	}
	if skipped > 0 {
		fmt.Fprintf(w, "{\"truncated\":true,\"skipped_rows\":%d}\n", skipped)
	}
	return nil
}

// --- stats -------------------------------------------------------------

func cmdStats(_ []string) error {
	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	ss, err := db.Stats()
	if err != nil {
		return err
	}
	if len(ss) == 0 {
		fmt.Println(cMuted("no data yet — run `aii index`"))
		return nil
	}
	fmt.Println(cHead("sessions indexed"))
	fmt.Println(cMuted(strings.Repeat("─", 54)))
	fmt.Printf("%s  %s  %s  %s\n",
		cMuted(padRight("agent", 12)),
		cMuted(padLeft("sessions", 10)),
		cMuted(padLeft("messages", 10)),
		cMuted("latest"))
	var totalS, totalM int64
	for _, s := range ss {
		fmt.Printf("%s  %s  %s  %s\n",
			cAgent(s.Agent)+strings.Repeat(" ", max(0, 12-5)),
			padLeft(fmt.Sprintf("%d", s.Sessions), 10),
			padLeft(fmt.Sprintf("%d", s.Messages), 10),
			cMuted(store.FormatTime(s.Latest)))
		totalS += s.Sessions
		totalM += s.Messages
	}
	fmt.Println(cMuted(strings.Repeat("─", 54)))
	fmt.Printf("%s  %s  %s\n",
		cMuted(padRight("total", 12)),
		cBold(padLeft(fmt.Sprintf("%d", totalS), 10)),
		cBold(padLeft(fmt.Sprintf("%d", totalM), 10)))
	return nil
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func padLeft(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return strings.Repeat(" ", w-len(s)) + s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- related -----------------------------------------------------------

// cmdRelated pivots from one session to others covering similar ground.
// We derive a query seed from the session's own metadata — title, then
// summary, then the first substantive user message — and run normal
// hybrid search with the source session filtered out.
func cmdRelated(args []string) error {
	fs := flag.NewFlagSet("related", flag.ExitOnError)
	agent := fs.String("agent", "", "cc|codex|cursor")
	since := fs.String("since", "", "7d | 2026-01-01")
	limit := fs.Int("limit", 10, "max results")
	jsonOut := fs.Bool("json", false, "emit a single JSON array")
	ndjsonOut := fs.Bool("ndjson", false, "emit one JSON line per excerpt")
	agentFlag := fs.Bool("agent-mode", false, "force agent-oriented output")
	pretty := fs.Bool("pretty", false, "force human-readable output")
	maxBytes := fs.Int("max-bytes", 0, "soft cap on output bytes")
	fs.Parse(reorderFlags(args))
	if fs.NArg() == 0 {
		return errors.New("related requires a session UID")
	}

	kickBackgroundIndex()

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	src, err := db.SessionByUIDAny(fs.Arg(0))
	if err != nil {
		return err
	}
	if src == nil {
		return errors.New("session not found")
	}

	seed, err := buildRelatedSeed(db, src)
	if err != nil {
		return err
	}
	if strings.TrimSpace(seed) == "" {
		return errors.New("no title, summary, or user message to pivot on")
	}

	sinceUnix, err := parseSince(*since)
	if err != nil {
		return err
	}

	// Pull extra so we can drop the source session and still hit --limit.
	results, err := db.Search(store.Query{
		Text:      seed,
		Agent:     normalizeAgent(*agent),
		SinceUnix: sinceUnix,
		Limit:     *limit + 1,
	})
	if err != nil {
		return err
	}
	results = excludeSession(results, src.UID)
	if len(results) > *limit {
		results = results[:*limit]
	}

	switch pickSearchFormat(*jsonOut, *ndjsonOut, *pretty, *agentFlag) {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	case "ndjson":
		return writeSearchNDJSON(os.Stdout, results, 0, *maxBytes)
	default:
		fmt.Fprintln(os.Stderr, cMuted(fmt.Sprintf(
			"pivoting from %s/%s via seed: %s",
			shortAgent(src.Agent), shortUID(src.UID), truncate(seed, 70))))
		return writeSearchPretty(os.Stdout, results, 0, *maxBytes)
	}
}

// buildRelatedSeed picks the best available handle for "what is this
// session about?" — title beats summary beats first user message. We
// strip to alphanumerics so the seed survives the FTS5 normalizer.
func buildRelatedSeed(db *store.DB, s *store.Session) (string, error) {
	candidates := []string{s.Title, s.Summary}
	for _, c := range candidates {
		if c = strings.TrimSpace(c); c != "" {
			return c, nil
		}
	}
	// Fall back to the first non-trivial user message.
	rows, err := db.SessionMessages(s.ID)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Role != "user" {
			continue
		}
		c := strings.TrimSpace(r.Content)
		if len(c) < 20 {
			continue
		}
		if len(c) > 300 {
			c = c[:300]
		}
		return c, nil
	}
	return "", nil
}

func excludeSession(rs []store.Result, uid string) []store.Result {
	out := rs[:0]
	for _, r := range rs {
		if r.SessionUID == uid {
			continue
		}
		out = append(out, r)
	}
	return out
}

// --- help --json -------------------------------------------------------

// helpAsJSON returns true if `--json` appears in the help args — kept
// separate so `aii help` still prints prose by default.
func helpAsJSON(args []string) bool {
	for _, a := range args {
		if a == "--json" || a == "-json" {
			return true
		}
	}
	return false
}

// cmdHelpJSON emits a machine-readable schema of aii's surface area so
// an agent (or MCP tool-description generator) can discover commands,
// flags, and the grammar of cite tokens without parsing the prose help.
func cmdHelpJSON() error {
	type flagInfo struct {
		Name    string `json:"name"`
		Default string `json:"default,omitempty"`
		Desc    string `json:"desc"`
	}
	type cmdInfo struct {
		Name    string     `json:"name"`
		Summary string     `json:"summary"`
		Usage   string     `json:"usage"`
		Flags   []flagInfo `json:"flags,omitempty"`
	}
	schema := struct {
		Binary          string    `json:"binary"`
		DBPath          string    `json:"db_path"`
		CiteTokenFormat string    `json:"cite_token_format"`
		CiteTokenExpr   string    `json:"cite_token_regex"`
		AgentCodes      []string  `json:"agent_codes"`
		Roles           []string  `json:"roles"`
		SinceFormats    []string  `json:"since_formats"`
		OutputModes     []string  `json:"output_modes"`
		Env             []string  `json:"environment_vars"`
		Commands        []cmdInfo `json:"commands"`
	}{
		Binary:          "aii",
		DBPath:          store.DefaultPath(),
		CiteTokenFormat: "<agent_code>/<short_uid>:<ordinal>",
		CiteTokenExpr:   `^(cc|cdx|cur)/[0-9a-f]{8}:\d+$`,
		AgentCodes:      []string{"cc", "cdx", "cur"},
		Roles:           []string{"user", "assistant", "thinking", "tool"},
		SinceFormats:    []string{"7d", "24h", "30m", "1w", "2026-01-01", "RFC3339"},
		OutputModes:     []string{"pretty", "json", "ndjson"},
		Env:             []string{"AII_DB", "AII_AGENT", "AII_ASK_CMD", "NO_COLOR", "FORCE_COLOR"},
		Commands: []cmdInfo{
			{"index", "Scan agent directories and update the FTS index", "aii index [--source all|cc|codex|cursor] [--full] [--verbose]", nil},
			{"search", "Hybrid (porter + trigram) full-text search over all transcripts", "aii search <query> [--agent ..] [--workspace ..] [--role ..] [--since ..] [--limit 20] [--offset 0] [--json|--ndjson|--pretty] [--max-bytes N]", []flagInfo{
				{"--role", "", "filter messages by speaker role (user|assistant|tool|thinking)"},
				{"--offset", "0", "skip N results — use with --limit for pagination"},
				{"--ndjson", "", "stream one JSON object per excerpt — default when stdout is piped"},
				{"--max-bytes", "0", "stop emitting after this many bytes and add a truncation marker"},
			}},
			{"show", "Print a single session — full, sliced, or as ndjson", "aii show <uid>|--last [--around N --span M] [--from N --to M] [--role ..] [--format md|plain|ndjson] [--max-msg-chars N] [--max-bytes N]", []flagInfo{
				{"--around", "", "anchor on an ordinal and include ±span around it"},
				{"--from/--to", "", "inclusive ordinal range"},
				{"--format", "md", "md for humans, ndjson for agents (one message per line)"},
			}},
			{"ask", "Retrieve top-k transcripts and pipe a grounded prompt into an LLM CLI", "aii ask <question> [--k 6] [--context 2] [--cmd \"claude -p\"] [--dry-run] [--show-sources]", nil},
			{"related", "Find sessions covering similar ground to the given uid", "aii related <uid> [--limit 10] [--agent ..] [--since ..]", nil},
			{"stats", "Row counts by agent + latest session timestamp", "aii stats", nil},
			{"doctor", "Print DB path, source paths, and per-agent counts", "aii doctor", nil},
			{"serve", "HTTP UI bound to localhost", "aii serve [--addr 127.0.0.1:8723]", nil},
			{"ui", "Serve + open browser (alias: web)", "aii ui [--addr 127.0.0.1:8723]", nil},
			{"tui", "Two-pane bubbletea search UI", "aii tui", nil},
			{"mcp", "Run as an MCP server over stdio — callable as a tool from coding agents", "aii mcp", nil},
		},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(schema)
}

// --- serve -------------------------------------------------------------

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8723", "listen address")
	fs.Parse(reorderFlags(args))

	kickBackgroundIndex()

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	return web.Serve(ctx, db, *addr)
}

// --- mcp ---------------------------------------------------------------

// cmdMCP starts a stdio MCP server that exposes aii's search surface as
// tools for coding agents. It never writes to stdout — stdout is the
// MCP transport — so log output goes to stderr.
func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	fs.Parse(reorderFlags(args))

	// Long-lived process — kick once at startup, then keep refreshing
	// in the background so search results stay current for whatever
	// agent is using us. The kick is non-blocking; the ticker just
	// re-checks periodically.
	kickBackgroundIndex()
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				kickBackgroundIndex()
			}
		}
	}()

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	log.SetOutput(os.Stderr)
	log.Printf("aii mcp: stdio server up, db=%s", store.DefaultPath())
	return mcpserver.Serve(ctx, db, aiiVersion)
}

// --- ui ----------------------------------------------------------------

func cmdUI(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8723", "listen address")
	fs.Parse(reorderFlags(args))

	kickBackgroundIndex()

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	// Open the browser a beat after the server comes up — the handler
	// fires first so we hit a responding page on the first request.
	url := "http://" + *addr
	go func() {
		time.Sleep(250 * time.Millisecond)
		if err := openBrowser(url); err != nil {
			log.Printf("open browser: %v (navigate to %s manually)", err, url)
		}
	}()

	fmt.Printf("aii: serving %s — press ctrl-c to stop\n", url)
	return web.Serve(ctx, db, *addr)
}

// openBrowser dispatches to the platform's "open a URL" command.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("no browser opener for %s", runtime.GOOS)
	}
	return cmd.Start()
}

// --- tui ---------------------------------------------------------------

func cmdTUI(_ context.Context, _ []string) error {
	kickBackgroundIndex()

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()
	return tui.Run(db)
}

// --- doctor ------------------------------------------------------------

func cmdDoctor() error {
	home, _ := os.UserHomeDir()
	dbPath := store.DefaultPath()
	fmt.Println("DB:", dbPath)

	info, err := os.Stat(dbPath)
	if err == nil {
		fmt.Printf("  size: %d bytes, modified %s\n", info.Size(), info.ModTime().Format(time.RFC3339))
	} else {
		fmt.Println("  (not yet created — run `aii index`)")
	}

	fmt.Println("Source paths:")
	fmt.Printf("  claude_code  %s/.claude/projects\n", home)
	fmt.Printf("  codex        %s/.codex/sessions (+ history.jsonl)\n", home)
	fmt.Printf("  cursor       %s/Library/Application Support/Cursor/User/{workspaceStorage,globalStorage}/\n", home)

	if err == nil {
		db, err := store.Open(dbPath)
		if err != nil {
			return err
		}
		defer db.Close()
		ss, err := db.Stats()
		if err != nil {
			return err
		}
		fmt.Println("Row counts:")
		for _, s := range ss {
			fmt.Printf("  %-12s  %d sessions, %d messages\n", s.Agent, s.Sessions, s.Messages)
		}
	}
	return nil
}

// --- helpers -----------------------------------------------------------

func normalizeAgent(a string) string {
	switch strings.ToLower(a) {
	case "", "all":
		return ""
	case "cc", "claude", "claude_code":
		return "claude_code"
	case "codex":
		return "codex"
	case "cursor":
		return "cursor"
	default:
		return a
	}
}

func shortAgent(a string) string {
	switch a {
	case "claude_code":
		return "cc"
	case "codex":
		return "cdx"
	case "cursor":
		return "cur"
	}
	return a
}

func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// reorderFlags sorts args so all flags come before positionals, letting
// users write `aii search foo --json` or `aii search --json foo` — Go's
// stdlib flag parser would otherwise stop at the first positional.
// Bool flags (stored in boolFlags) are allowed to stand alone; all other
// known flags consume the next arg. Unknown flags are passed through.
func reorderFlags(in []string) []string {
	boolFlags := map[string]bool{"--json": true, "-json": true, "--full": true, "-full": true,
		"--verbose": true, "-verbose": true, "--last": true, "-last": true,
		"--dry-run": true, "-dry-run": true, "--show-sources": true, "-show-sources": true}
	var flags, rest []string
	for i := 0; i < len(in); i++ {
		a := in[i]
		if a == "--" {
			rest = append(rest, in[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			// --key=value pattern keeps value; --key value consumes next
			if !strings.Contains(a, "=") && !boolFlags[a] && i+1 < len(in) && !strings.HasPrefix(in[i+1], "-") {
				flags = append(flags, in[i+1])
				i++
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

func parseSince(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	// 7d, 24h, 30m
	if len(s) > 1 {
		unit := s[len(s)-1]
		if unit == 'd' || unit == 'h' || unit == 'm' || unit == 'w' {
			var n int
			if _, err := fmt.Sscanf(s[:len(s)-1], "%d", &n); err == nil && n > 0 {
				var dur time.Duration
				switch unit {
				case 'm':
					dur = time.Duration(n) * time.Minute
				case 'h':
					dur = time.Duration(n) * time.Hour
				case 'd':
					dur = time.Duration(n) * 24 * time.Hour
				case 'w':
					dur = time.Duration(n) * 7 * 24 * time.Hour
				}
				return time.Now().Add(-dur).Unix(), nil
			}
		}
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Unix(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("invalid --since %q (expected 7d, 2h, or 2026-01-01)", s)
}
