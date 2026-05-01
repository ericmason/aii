// Package mcpserver exposes aii's search and retrieval surface over the
// Model Context Protocol (MCP) so agents running inside Claude Code,
// Cursor, Codex, and other MCP-speaking runtimes can call it as a
// first-class tool rather than shelling out and parsing stdout.
//
// The tool contract is intentionally small — search, get_session,
// related, stats — with descriptions that point the caller back at the
// cite-token format so they can chain calls ("search → get_session
// around cite.ordinal") without guessing.
package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ericmason/aii/internal/store"
)

// Serve boots a stdio MCP server that stays alive until the transport
// closes (or ctx is cancelled, which bubbles through the underlying
// reader).
func Serve(ctx context.Context, db *store.DB, version string) error {
	s := server.NewMCPServer("aii", version,
		server.WithToolCapabilities(false),
	)
	registerTools(s, db)
	return server.ServeStdio(s)
}

func registerTools(s *server.MCPServer, db *store.DB) {
	s.AddTool(
		mcp.NewTool("search",
			mcp.WithDescription(
				"Hybrid full-text search over every AI coding-agent transcript "+
					"indexed locally (Claude Code, Codex, Cursor). Returns "+
					"sessions ranked by bm25 with up to 3 top excerpts each. "+
					"Every excerpt carries a stable cite token of the form "+
					"'<agent_code>/<short_uid>:<ordinal>' (e.g. cc/a1b2c3d4:42). "+
					"Pass that cite (or just the short_uid) to get_session to "+
					"pull the exact surrounding messages. Agent codes: "+
					"cc=claude_code, cdx=codex, cur=cursor."),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Free-form query. Bare words become prefix terms; quote phrases for exact match.")),
			mcp.WithString("agent",
				mcp.Description("Filter by agent: cc, codex, or cursor. Empty = all.")),
			mcp.WithString("workspace",
				mcp.Description("Exact workspace path to restrict to.")),
			mcp.WithString("role",
				mcp.Description("Restrict to messages from one speaker: user | assistant | tool | thinking.")),
			mcp.WithString("since",
				mcp.Description("Relative duration (7d, 24h, 2w) or YYYY-MM-DD / RFC3339.")),
			mcp.WithNumber("limit",
				mcp.DefaultNumber(20),
				mcp.Description("Max sessions to return (1–200).")),
			mcp.WithNumber("offset",
				mcp.DefaultNumber(0),
				mcp.Description("Skip N sessions before returning — use with limit for pagination.")),
		),
		makeSearchHandler(db),
	)

	s.AddTool(
		mcp.NewTool("get_session",
			mcp.WithDescription(
				"Fetch messages from one session. Accepts a full or short uid, "+
					"OR a cite token like cc/a1b2c3d4:42 (the agent prefix is "+
					"ignored; the ordinal is only used if you also ask for a "+
					"slice via around/span). By default returns the entire "+
					"session — often long. Prefer slicing: set around to a "+
					"specific ordinal with span for ±N context, or use from/to "+
					"for an ordinal range. Use role to filter, and "+
					"max_msg_chars to bound each message's size."),
			mcp.WithString("session",
				mcp.Required(),
				mcp.Description("Session uid (full or 8-char short) OR cite token 'agent/uid:ordinal'.")),
			mcp.WithNumber("around",
				mcp.Description("Anchor ordinal. If set, returns messages within ±span of this ordinal.")),
			mcp.WithNumber("span",
				mcp.DefaultNumber(3),
				mcp.Description("Half-window size used with `around`. Total messages ≈ 2*span+1.")),
			mcp.WithNumber("from",
				mcp.Description("First ordinal to include (inclusive). Mutually exclusive with around.")),
			mcp.WithNumber("to",
				mcp.Description("Last ordinal to include (inclusive). Mutually exclusive with around.")),
			mcp.WithString("role",
				mcp.Description("Keep only messages from this role: user | assistant | tool | thinking.")),
			mcp.WithNumber("max_msg_chars",
				mcp.DefaultNumber(0),
				mcp.Description("Truncate each message's content to this many chars (0 = no cap).")),
		),
		makeGetSessionHandler(db),
	)

	s.AddTool(
		mcp.NewTool("related",
			mcp.WithDescription(
				"Find sessions covering similar ground to a given session. "+
					"Uses the source session's title (or first substantive user "+
					"message) as a query seed and runs hybrid search with the "+
					"source uid excluded. Good for 'I remember we discussed "+
					"this in a few threads, find them all'."),
			mcp.WithString("session",
				mcp.Required(),
				mcp.Description("Session uid (full or short) OR cite token.")),
			mcp.WithNumber("limit",
				mcp.DefaultNumber(10),
				mcp.Description("Max related sessions to return.")),
			mcp.WithString("agent",
				mcp.Description("Optional agent filter for the related search.")),
			mcp.WithString("since",
				mcp.Description("Optional recency filter (7d, 2026-01-01, etc.).")),
		),
		makeRelatedHandler(db),
	)

	s.AddTool(
		mcp.NewTool("list_sessions",
			mcp.WithDescription(
				"Browse sessions by metadata — no full-text query needed. "+
					"Use this when the user asks 'what was I working on last "+
					"week?', 'show me all sessions in this workspace', or any "+
					"time-window question. Returns sessions ordered by "+
					"started_at (newest first by default) with title, "+
					"workspace, and message count. Pair with get_session to "+
					"open any specific thread."),
			mcp.WithString("agent",
				mcp.Description("Filter by agent: cc, codex, or cursor. Empty = all.")),
			mcp.WithString("workspace",
				mcp.Description("Exact workspace path to restrict to.")),
			mcp.WithString("since",
				mcp.Description("Lower bound on session time. 7d, 24h, 2w, YYYY-MM-DD, or RFC3339.")),
			mcp.WithString("until",
				mcp.Description("Upper bound on session time. Same formats as `since`.")),
			mcp.WithNumber("limit",
				mcp.DefaultNumber(50),
				mcp.Description("Max sessions to return (1–500).")),
			mcp.WithNumber("offset",
				mcp.DefaultNumber(0),
				mcp.Description("Skip N sessions before returning — use with limit for pagination.")),
			mcp.WithString("order",
				mcp.DefaultString("desc"),
				mcp.Description("Sort direction by session time: asc (oldest first) or desc (newest first).")),
			mcp.WithBoolean("ended_mid_task",
				mcp.Description("Only return sessions whose final message was from user or tool — i.e. the assistant never responded. Use to recover interrupted threads.")),
		),
		makeListSessionsHandler(db),
	)

	s.AddTool(
		mcp.NewTool("stats",
			mcp.WithDescription(
				"Return per-agent row counts and the latest session timestamp. "+
					"Useful for sanity-checking that the index is populated "+
					"before issuing a search."),
		),
		makeStatsHandler(db),
	)
}

// --- search ------------------------------------------------------------

type searchArgs struct {
	Query     string  `json:"query"`
	Agent     string  `json:"agent"`
	Workspace string  `json:"workspace"`
	Role      string  `json:"role"`
	Since     string  `json:"since"`
	Limit     float64 `json:"limit"`
	Offset    float64 `json:"offset"`
}

type searchHit struct {
	Cite         string  `json:"cite"`
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

func makeSearchHandler(db *store.DB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a searchArgs
		if err := req.BindArguments(&a); err != nil {
			return mcp.NewToolResultErrorFromErr("bad arguments", err), nil
		}
		if strings.TrimSpace(a.Query) == "" {
			return mcp.NewToolResultError("query is required"), nil
		}
		since, err := parseSince(a.Since)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("bad since", err), nil
		}
		limit := int(a.Limit)
		if limit <= 0 {
			limit = 20
		}
		if limit > 200 {
			limit = 200
		}
		results, err := db.Search(store.Query{
			Text:      a.Query,
			Agent:     NormalizeAgent(a.Agent),
			Workspace: a.Workspace,
			Role:      NormalizeRole(a.Role),
			SinceUnix: since,
			Limit:     limit,
			Offset:    int(a.Offset),
		})
		if err != nil {
			return mcp.NewToolResultErrorFromErr("search failed", err), nil
		}

		hits := make([]searchHit, 0, len(results))
		for i, r := range results {
			rank := i + 1 + int(a.Offset)
			for _, e := range r.Excerpts {
				hits = append(hits, searchHit{
					Cite:         fmt.Sprintf("%s/%s:%d", ShortAgent(r.Agent), ShortUID(r.SessionUID), e.Ordinal),
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
				})
			}
		}
		payload := map[string]any{
			"query":   a.Query,
			"count":   len(results),
			"hits":    hits,
		}
		return mcp.NewToolResultStructured(payload, renderFallbackSearch(a.Query, hits)), nil
	}
}

func renderFallbackSearch(query string, hits []searchHit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "search %q → %d excerpts\n", query, len(hits))
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s  score=%.2f  [%s]  %s\n", h.Cite, h.Score, h.Role, oneLine(h.Snippet))
	}
	return b.String()
}

// --- get_session -------------------------------------------------------

type sessionArgs struct {
	Session     string  `json:"session"`
	Around      float64 `json:"around"`
	AroundSet   bool    `json:"-"`
	Span        float64 `json:"span"`
	From        float64 `json:"from"`
	FromSet     bool    `json:"-"`
	To          float64 `json:"to"`
	ToSet       bool    `json:"-"`
	Role        string  `json:"role"`
	MaxMsgChars float64 `json:"max_msg_chars"`
}

type sessionRow struct {
	Cite    string `json:"cite"`
	Ordinal int    `json:"ordinal"`
	Role    string `json:"role"`
	TS      int64  `json:"ts,omitempty"`
	Content string `json:"content"`
}

func makeGetSessionHandler(db *store.DB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// We need to know which numeric args were actually provided
		// (around=0 is a legitimate request), so parse from the raw map
		// instead of relying on zero values.
		raw := req.GetArguments()
		sessionArg, _ := raw["session"].(string)
		if strings.TrimSpace(sessionArg) == "" {
			return mcp.NewToolResultError("session is required"), nil
		}
		uid, ordFromCite := parseSessionRef(sessionArg)

		span := intArg(raw, "span", 3)
		roleFilter, _ := raw["role"].(string)
		maxMsgChars := intArg(raw, "max_msg_chars", 0)

		sess, err := db.SessionByUIDAny(uid)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lookup failed", err), nil
		}
		if sess == nil {
			return mcp.NewToolResultErrorf("session not found: %s", uid), nil
		}

		aroundRaw, aroundSet := raw["around"]
		fromRaw, fromSet := raw["from"]
		toRaw, toSet := raw["to"]

		var rows []store.Row
		switch {
		case aroundSet:
			rows, err = db.ContextRows(sess.ID, floatToInt(aroundRaw), span)
		case fromSet || toSet:
			from := floatToInt(fromRaw)
			to := floatToInt(toRaw)
			if !toSet {
				to = 1 << 30
			}
			if !fromSet {
				from = 0
			}
			rows, err = db.RangeRows(sess.ID, from, to)
		case ordFromCite >= 0:
			// No slice arg given but the cite carried an ordinal — use it as anchor.
			rows, err = db.ContextRows(sess.ID, ordFromCite, span)
		default:
			rows, err = db.SessionMessages(sess.ID)
		}
		if err != nil {
			return mcp.NewToolResultErrorFromErr("fetch failed", err), nil
		}

		role := NormalizeRole(roleFilter)
		out := make([]sessionRow, 0, len(rows))
		for _, r := range rows {
			if role != "" && r.Role != role {
				continue
			}
			content := r.Content
			if maxMsgChars > 0 && len(content) > maxMsgChars {
				content = content[:maxMsgChars] + "…[truncated]"
			}
			out = append(out, sessionRow{
				Cite:    fmt.Sprintf("%s/%s:%d", ShortAgent(sess.Agent), ShortUID(sess.UID), r.Ordinal),
				Ordinal: r.Ordinal,
				Role:    r.Role,
				TS:      r.TS,
				Content: content,
			})
		}
		payload := map[string]any{
			"session": map[string]any{
				"uid":        sess.UID,
				"agent":      sess.Agent,
				"title":      sess.Title,
				"workspace":  sess.Workspace,
				"started_at": sess.StartedAt,
				"ended_at":   sess.EndedAt,
			},
			"count":    len(out),
			"messages": out,
		}
		return mcp.NewToolResultStructured(payload, renderFallbackSession(sess, out)), nil
	}
}

func renderFallbackSession(s *store.Session, rows []sessionRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s — %s (%d messages)\n", ShortAgent(s.Agent), ShortUID(s.UID), s.Title, len(rows))
	for _, r := range rows {
		fmt.Fprintf(&b, "- %s [%s] %s\n", r.Cite, r.Role, oneLine(r.Content))
	}
	return b.String()
}

// --- related -----------------------------------------------------------

type relatedArgs struct {
	Session string  `json:"session"`
	Limit   float64 `json:"limit"`
	Agent   string  `json:"agent"`
	Since   string  `json:"since"`
}

func makeRelatedHandler(db *store.DB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a relatedArgs
		if err := req.BindArguments(&a); err != nil {
			return mcp.NewToolResultErrorFromErr("bad arguments", err), nil
		}
		uid, _ := parseSessionRef(a.Session)
		if uid == "" {
			return mcp.NewToolResultError("session is required"), nil
		}
		src, err := db.SessionByUIDAny(uid)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lookup failed", err), nil
		}
		if src == nil {
			return mcp.NewToolResultErrorf("session not found: %s", uid), nil
		}

		seed, err := relatedSeed(db, src)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("seed build failed", err), nil
		}
		if strings.TrimSpace(seed) == "" {
			return mcp.NewToolResultError("no title, summary, or user message to pivot on"), nil
		}

		since, err := parseSince(a.Since)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("bad since", err), nil
		}
		limit := int(a.Limit)
		if limit <= 0 {
			limit = 10
		}

		results, err := db.Search(store.Query{
			Text:      seed,
			Agent:     NormalizeAgent(a.Agent),
			SinceUnix: since,
			Limit:     limit + 1,
		})
		if err != nil {
			return mcp.NewToolResultErrorFromErr("search failed", err), nil
		}
		filtered := make([]store.Result, 0, len(results))
		for _, r := range results {
			if r.SessionUID == src.UID {
				continue
			}
			filtered = append(filtered, r)
		}
		if len(filtered) > limit {
			filtered = filtered[:limit]
		}

		hits := make([]searchHit, 0, len(filtered))
		for i, r := range filtered {
			rank := i + 1
			for _, e := range r.Excerpts {
				hits = append(hits, searchHit{
					Cite:         fmt.Sprintf("%s/%s:%d", ShortAgent(r.Agent), ShortUID(r.SessionUID), e.Ordinal),
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
				})
			}
		}
		payload := map[string]any{
			"source_uid": src.UID,
			"seed":       truncate(seed, 120),
			"count":      len(filtered),
			"hits":       hits,
		}
		return mcp.NewToolResultStructured(payload,
			fmt.Sprintf("related to %s (seed: %s) → %d sessions", ShortUID(src.UID), truncate(seed, 60), len(filtered))), nil
	}
}

func relatedSeed(db *store.DB, s *store.Session) (string, error) {
	for _, c := range []string{s.Title, s.Summary} {
		if c = strings.TrimSpace(c); c != "" {
			return c, nil
		}
	}
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

// --- list_sessions -----------------------------------------------------

type listSessionsArgs struct {
	Agent        string  `json:"agent"`
	Workspace    string  `json:"workspace"`
	Since        string  `json:"since"`
	Until        string  `json:"until"`
	Limit        float64 `json:"limit"`
	Offset       float64 `json:"offset"`
	Order        string  `json:"order"`
	EndedMidTask bool    `json:"ended_mid_task"`
}

type sessionItem struct {
	Cite         string `json:"cite"`
	Agent        string `json:"agent"`
	UID          string `json:"uid"`
	Title        string `json:"title,omitempty"`
	Summary      string `json:"summary,omitempty"`
	Workspace    string `json:"workspace,omitempty"`
	StartedAt    int64  `json:"started_at,omitempty"`
	EndedAt      int64  `json:"ended_at,omitempty"`
	MessageCount int64  `json:"message_count"`
}

func makeListSessionsHandler(db *store.DB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a listSessionsArgs
		if err := req.BindArguments(&a); err != nil {
			return mcp.NewToolResultErrorFromErr("bad arguments", err), nil
		}
		since, err := parseSince(a.Since)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("bad since", err), nil
		}
		until, err := parseSince(a.Until)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("bad until", err), nil
		}
		limit := int(a.Limit)
		if limit <= 0 {
			limit = 50
		}
		if limit > 500 {
			limit = 500
		}
		items, err := db.ListSessions(store.SessionFilter{
			Agent:        NormalizeAgent(a.Agent),
			Workspace:    a.Workspace,
			SinceUnix:    since,
			UntilUnix:    until,
			Limit:        limit,
			Offset:       int(a.Offset),
			Order:        a.Order,
			EndedMidTask: a.EndedMidTask,
		})
		if err != nil {
			return mcp.NewToolResultErrorFromErr("list_sessions failed", err), nil
		}
		out := make([]sessionItem, 0, len(items))
		for _, s := range items {
			out = append(out, sessionItem{
				Cite:         fmt.Sprintf("%s/%s", ShortAgent(s.Agent), ShortUID(s.UID)),
				Agent:        s.Agent,
				UID:          s.UID,
				Title:        s.Title,
				Summary:      s.Summary,
				Workspace:    s.Workspace,
				StartedAt:    s.StartedAt,
				EndedAt:      s.EndedAt,
				MessageCount: s.MessageCount,
			})
		}
		payload := map[string]any{
			"count":    len(out),
			"sessions": out,
		}
		var fb strings.Builder
		fmt.Fprintf(&fb, "%d sessions\n", len(out))
		for _, s := range out {
			title := s.Title
			if title == "" {
				title = s.UID
			}
			fmt.Fprintf(&fb, "- %s  %s  (%d msg)  %s\n",
				s.Cite, store.FormatTime(s.StartedAt), s.MessageCount, oneLine(title))
		}
		return mcp.NewToolResultStructured(payload, fb.String()), nil
	}
}

// --- stats -------------------------------------------------------------

func makeStatsHandler(db *store.DB) server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ss, err := db.Stats()
		if err != nil {
			return mcp.NewToolResultErrorFromErr("stats failed", err), nil
		}
		agents := make([]map[string]any, 0, len(ss))
		for _, s := range ss {
			agents = append(agents, map[string]any{
				"agent":    s.Agent,
				"sessions": s.Sessions,
				"messages": s.Messages,
				"latest":   s.Latest,
			})
		}
		payload := map[string]any{
			"agents":  agents,
			"db_path": store.DefaultPath(),
		}
		var fb strings.Builder
		for _, a := range agents {
			fmt.Fprintf(&fb, "%s: %d sessions, %d messages\n", a["agent"], a["sessions"], a["messages"])
		}
		return mcp.NewToolResultStructured(payload, fb.String()), nil
	}
}
