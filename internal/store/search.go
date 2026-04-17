package store

import (
	"database/sql"
	"fmt"
	"strings"
)

type Query struct {
	Text      string
	Agent     string
	Workspace string
	Role      string // optional: restrict to messages from this role (user|assistant|…)
	SinceUnix int64
	UntilUnix int64
	Limit     int
	Offset    int // skip this many sessions before Limit — for pagination
}

// Excerpt is one matching message within a session.
type Excerpt struct {
	Ordinal int     `json:"ordinal"`
	Role    string  `json:"role"`
	TS      int64   `json:"ts"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"`
}

// Result is a session with its best-ranked excerpts — one per session.
type Result struct {
	SessionID  int64     `json:"-"`
	SessionUID string    `json:"session_uid"`
	Agent      string    `json:"agent"`
	Workspace  string    `json:"workspace"`
	Title      string    `json:"title"`
	Summary    string    `json:"summary,omitempty"`
	StartedAt  int64     `json:"started_at"`
	BestScore  float64   `json:"score"`
	MatchCount int       `json:"match_count"`
	Excerpts   []Excerpt `json:"excerpts"`
}

// maxExcerptsPerSession is how many snippets we surface per result.
const maxExcerptsPerSession = 3

// Search returns one Result per matching session, ranked by that session's
// best bm25 score, and attaches the top few excerpts for context.
//
// The hit set is the UNION of two FTS indexes:
//   - messages_fts     (porter/unicode61) — natural-language matches
//   - messages_fts_tri (trigram)          — identifier substrings like
//     UserID, snake_case, webhookRetry
//
// A message that matches in both is deduped to its best-scoring row.
// Trigram scores are scaled down so a porter hit wins a tie.
func (d *DB) Search(q Query) ([]Result, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	matchPorter := normalizeMatch(q.Text)
	matchTri := normalizeMatchTrigram(q.Text)

	// Shared filter clause: applied identically inside each FTS branch.
	var (
		filters []string
		fargs   []any
	)
	if q.Agent != "" {
		filters = append(filters, "s.agent = ?")
		fargs = append(fargs, q.Agent)
	}
	if q.Workspace != "" {
		filters = append(filters, "s.workspace = ?")
		fargs = append(fargs, q.Workspace)
	}
	if q.SinceUnix > 0 {
		filters = append(filters, "COALESCE(m.ts, s.started_at, 0) >= ?")
		fargs = append(fargs, q.SinceUnix)
	}
	if q.UntilUnix > 0 {
		filters = append(filters, "COALESCE(m.ts, s.started_at, 0) <= ?")
		fargs = append(fargs, q.UntilUnix)
	}
	if q.Role != "" {
		filters = append(filters, "m.role = ?")
		fargs = append(fargs, q.Role)
	}
	filterSQL := ""
	if len(filters) > 0 {
		filterSQL = "AND " + strings.Join(filters, " AND ")
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	// Trigram branch is optional: when the query has no token ≥3 chars
	// there's nothing it could match, and asking would be a no-op.
	const triPenalty = 0.7 // bm25 returns negative scores; scale shrinks magnitude
	var (
		query string
		args  []any
	)
	if matchTri != "" {
		query = fmt.Sprintf(`
        WITH raw_hits AS MATERIALIZED (
            SELECT m.id AS mid, m.session_id, m.ordinal, m.role, m.ts,
                   snippet(messages_fts, 0, '«', '»', '…', 12) AS snip,
                   bm25(messages_fts) AS score
            FROM messages_fts
            JOIN messages m ON m.id = messages_fts.rowid
            JOIN sessions s ON s.id = m.session_id
            WHERE messages_fts MATCH ? %[1]s
            UNION ALL
            SELECT m.id, m.session_id, m.ordinal, m.role, m.ts,
                   snippet(messages_fts_tri, 0, '«', '»', '…', 12),
                   bm25(messages_fts_tri) * %[3]f
            FROM messages_fts_tri
            JOIN messages m ON m.id = messages_fts_tri.rowid
            JOIN sessions s ON s.id = m.session_id
            WHERE messages_fts_tri MATCH ? %[1]s
        ),
        hits AS MATERIALIZED (
            SELECT mid, session_id, ordinal, role, ts, snip, score FROM (
                SELECT *, ROW_NUMBER() OVER (PARTITION BY mid ORDER BY score) AS drn
                FROM raw_hits
            ) WHERE drn = 1
        ),
        session_scores AS (
            SELECT session_id, MIN(score) AS best_score, COUNT(*) AS match_count
            FROM hits GROUP BY session_id
        ),
        top_sessions AS (
            SELECT session_id, best_score, match_count
            FROM session_scores ORDER BY best_score LIMIT ? OFFSET ?
        ),
        ranked AS (
            SELECT h.*,
                   ROW_NUMBER() OVER (PARTITION BY h.session_id ORDER BY h.score) AS rn
            FROM hits h
            WHERE h.session_id IN (SELECT session_id FROM top_sessions)
        )
        SELECT s.id, s.session_uid, s.agent,
               COALESCE(s.workspace, ''), COALESCE(s.title, ''), COALESCE(s.summary, ''),
               COALESCE(s.started_at, 0),
               ts_info.best_score, ts_info.match_count,
               r.ordinal, r.role, COALESCE(r.ts, 0), r.snip, r.score
        FROM ranked r
        JOIN sessions s ON s.id = r.session_id
        JOIN top_sessions ts_info ON ts_info.session_id = r.session_id
        WHERE r.rn <= %[2]d
        ORDER BY ts_info.best_score, s.id, r.rn`,
			filterSQL, maxExcerptsPerSession, triPenalty)

		args = append(args, matchPorter)
		args = append(args, fargs...)
		args = append(args, matchTri)
		args = append(args, fargs...)
		args = append(args, q.Limit, q.Offset)
	} else {
		query = fmt.Sprintf(`
        WITH hits AS MATERIALIZED (
            SELECT m.id AS mid, m.session_id, m.ordinal, m.role, m.ts,
                   snippet(messages_fts, 0, '«', '»', '…', 12) AS snip,
                   bm25(messages_fts) AS score
            FROM messages_fts
            JOIN messages m ON m.id = messages_fts.rowid
            JOIN sessions s ON s.id = m.session_id
            WHERE messages_fts MATCH ? %s
        ),
        session_scores AS (
            SELECT session_id, MIN(score) AS best_score, COUNT(*) AS match_count
            FROM hits GROUP BY session_id
        ),
        top_sessions AS (
            SELECT session_id, best_score, match_count
            FROM session_scores ORDER BY best_score LIMIT ? OFFSET ?
        ),
        ranked AS (
            SELECT h.*,
                   ROW_NUMBER() OVER (PARTITION BY h.session_id ORDER BY h.score) AS rn
            FROM hits h
            WHERE h.session_id IN (SELECT session_id FROM top_sessions)
        )
        SELECT s.id, s.session_uid, s.agent,
               COALESCE(s.workspace, ''), COALESCE(s.title, ''), COALESCE(s.summary, ''),
               COALESCE(s.started_at, 0),
               ts_info.best_score, ts_info.match_count,
               r.ordinal, r.role, COALESCE(r.ts, 0), r.snip, r.score
        FROM ranked r
        JOIN sessions s ON s.id = r.session_id
        JOIN top_sessions ts_info ON ts_info.session_id = r.session_id
        WHERE r.rn <= %d
        ORDER BY ts_info.best_score, s.id, r.rn`,
			filterSQL, maxExcerptsPerSession)

		args = append(args, matchPorter)
		args = append(args, fargs...)
		args = append(args, q.Limit, q.Offset)
	}

	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var out []Result
	byID := map[int64]int{} // session_id -> index into out
	for rows.Next() {
		var (
			r  Result
			ex Excerpt
		)
		if err := rows.Scan(&r.SessionID, &r.SessionUID, &r.Agent, &r.Workspace, &r.Title, &r.Summary,
			&r.StartedAt,
			&r.BestScore, &r.MatchCount,
			&ex.Ordinal, &ex.Role, &ex.TS, &ex.Snippet, &ex.Score); err != nil {
			return nil, err
		}
		if idx, ok := byID[r.SessionID]; ok {
			out[idx].Excerpts = append(out[idx].Excerpts, ex)
			continue
		}
		r.Excerpts = []Excerpt{ex}
		byID[r.SessionID] = len(out)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ContextRows returns messages around a given ordinal in a session.
func (d *DB) ContextRows(sessionID int64, ordinal, span int) ([]Row, error) {
	return d.RangeRows(sessionID, ordinal-span, ordinal+span)
}

// RangeRows returns every message in [fromOrdinal, toOrdinal] (inclusive)
// for the given session, in ordinal order.
func (d *DB) RangeRows(sessionID int64, fromOrdinal, toOrdinal int) ([]Row, error) {
	rows, err := d.Query(`SELECT ordinal, role, COALESCE(ts,0), content
                          FROM messages
                          WHERE session_id = ? AND ordinal BETWEEN ? AND ?
                          ORDER BY ordinal`,
		sessionID, fromOrdinal, toOrdinal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Ordinal, &r.Role, &r.TS, &r.Content); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// normalizeMatch turns a free-form user query into an FTS5 MATCH string.
// Quoted phrases survive as phrases; bare words become prefix terms so
// typing "auth" also matches "authorize", "authentication", etc. Non-
// alphanumeric runes in bare words are stripped — this keeps special
// FTS5 syntax (AND/OR/NEAR/parens/colons) from detonating on user input.
func normalizeMatch(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return `""`
	}

	var out []string
	in := []rune(q)
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c == '"' {
			j := i + 1
			for j < len(in) && in[j] != '"' {
				j++
			}
			phrase := sanitizePhrase(string(in[i+1 : j]))
			if phrase != "" {
				out = append(out, `"`+phrase+`"`)
			}
			if j < len(in) {
				i = j
			} else {
				i = len(in)
			}
			continue
		}
		if c == ' ' || c == '\t' {
			continue
		}
		j := i
		for j < len(in) && !isSpace(in[j]) {
			j++
		}
		tok := sanitizeToken(string(in[i:j]))
		if tok != "" {
			out = append(out, tok+"*")
		}
		i = j - 1
	}
	if len(out) == 0 {
		return `""`
	}
	return strings.Join(out, " ")
}

// normalizeMatchTrigram builds a MATCH string for the trigram FTS table.
// Trigram tokenization requires ≥3-char substrings and doesn't support
// prefix stars. Each bare token and each quoted phrase becomes a quoted
// phrase; tokens shorter than 3 chars are dropped. Returns "" when no
// usable tokens remain — callers then skip the trigram branch entirely.
func normalizeMatchTrigram(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	var out []string
	in := []rune(q)
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c == '"' {
			j := i + 1
			for j < len(in) && in[j] != '"' {
				j++
			}
			phrase := sanitizePhrase(string(in[i+1 : j]))
			if len([]rune(phrase)) >= 3 {
				out = append(out, `"`+phrase+`"`)
			}
			if j < len(in) {
				i = j
			} else {
				i = len(in)
			}
			continue
		}
		if isSpace(c) {
			continue
		}
		j := i
		for j < len(in) && !isSpace(in[j]) {
			j++
		}
		tok := sanitizeToken(string(in[i:j]))
		if len([]rune(tok)) >= 3 {
			out = append(out, `"`+tok+`"`)
		}
		i = j - 1
	}
	return strings.Join(out, " ")
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

func sanitizeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sanitizePhrase(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '"' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// ErrNoRows is exposed for callers that want to detect empty lookups.
var ErrNoRows = sql.ErrNoRows
