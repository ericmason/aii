package store

import (
	"database/sql"
	"fmt"
	"slices"
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
	//
	// The hit set carries only ids and bm25 scores. Snippets are built
	// afterward, only for the excerpts on the returned page (see
	// pageSnippets): a common term can match hundreds of thousands of
	// messages, and snippet() over all of them dominated search time.
	// Each hit is joined to its session through idx_messages_id_session
	// when it exists, which avoids reading every matching message row from
	// disk. Without it, the join falls back to the rowid lookup.
	const triPenalty = 0.7 // bm25 returns negative scores; scale shrinks magnitude
	var args []any

	// One read transaction so the snippet pass sees the same snapshot as
	// the ranking pass, even while the indexer writes.
	tx, err := d.Begin()
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer tx.Rollback()

	msgHint := ""
	if !d.hasSearchIndex.Load() {
		ok, err := indexExists(tx, searchIndexName)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		d.hasSearchIndex.Store(ok)
	}
	if d.hasSearchIndex.Load() {
		msgHint = "INDEXED BY " + searchIndexName
	}

	// src records which index produced the hit (srcPorter or srcTrigram) so
	// the excerpt's snippet comes from the same index that scored it.
	rawHits := fmt.Sprintf(`
            SELECT m.id AS mid, m.session_id, m.ordinal, m.role, m.ts,
                   %d AS src, bm25(messages_fts) AS score
            FROM messages_fts
            JOIN messages m %s ON m.id = messages_fts.rowid
            JOIN sessions s ON s.id = m.session_id
            WHERE messages_fts MATCH ? %s`, srcPorter, msgHint, filterSQL)
	args = append(args, matchPorter)
	args = append(args, fargs...)

	if matchTri != "" {
		rawHits += fmt.Sprintf(`
            UNION ALL
            SELECT m.id, m.session_id, m.ordinal, m.role, m.ts,
                   %d, bm25(messages_fts_tri) * %f
            FROM messages_fts_tri
            JOIN messages m %s ON m.id = messages_fts_tri.rowid
            JOIN sessions s ON s.id = m.session_id
            WHERE messages_fts_tri MATCH ? %s`, srcTrigram, triPenalty, msgHint, filterSQL)
		args = append(args, matchTri)
		args = append(args, fargs...)
	}

	// A message that matches both indexes appears twice in raw_hits. The
	// session's MIN(score) is the same either way and COUNT(DISTINCT mid)
	// counts it once, so the per-message dedupe only has to run over the
	// hits of the sessions on this page.
	query := fmt.Sprintf(`
        WITH raw_hits AS MATERIALIZED (%s
        ),
        session_scores AS (
            SELECT session_id, MIN(score) AS best_score, COUNT(DISTINCT mid) AS match_count
            FROM raw_hits GROUP BY session_id
        ),
        top_sessions AS MATERIALIZED (
            SELECT session_id, best_score, match_count
            FROM session_scores ORDER BY best_score LIMIT ? OFFSET ?
        ),
        page_hits AS (
            SELECT * FROM (
                SELECT *, ROW_NUMBER() OVER (PARTITION BY mid ORDER BY score, src) AS drn
                FROM raw_hits
                WHERE session_id IN (SELECT session_id FROM top_sessions)
            ) WHERE drn = 1
        ),
        ranked AS (
            SELECT h.*,
                   ROW_NUMBER() OVER (PARTITION BY h.session_id ORDER BY h.score) AS rn
            FROM page_hits h
        )
        SELECT s.id, s.session_uid, s.agent,
               COALESCE(s.workspace, ''), COALESCE(s.title, ''), COALESCE(s.summary, ''),
               COALESCE(s.started_at, 0),
               ts_info.best_score, ts_info.match_count,
               r.ordinal, r.role, COALESCE(r.ts, 0), r.score, r.mid, r.src
        FROM ranked r
        JOIN sessions s ON s.id = r.session_id
        JOIN top_sessions ts_info ON ts_info.session_id = r.session_id
        WHERE r.rn <= %d
        ORDER BY ts_info.best_score, s.id, r.rn`,
		rawHits, maxExcerptsPerSession)
	args = append(args, q.Limit, q.Offset)

	out, refs, err := scanSearchRows(tx, query, args)
	if err != nil {
		return nil, err
	}

	// Fill snippets per index, one pass each.
	for _, idx := range []struct {
		src   int
		table string
		match string
	}{
		{srcPorter, "messages_fts", matchPorter},
		{srcTrigram, "messages_fts_tri", matchTri},
	} {
		var mids []int64
		for _, ref := range refs {
			if ref.src == idx.src {
				mids = append(mids, ref.mid)
			}
		}
		if len(mids) == 0 {
			continue
		}
		snips, err := pageSnippets(tx, idx.table, idx.match, mids)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if ref.src == idx.src {
				out[ref.result].Excerpts[ref.excerpt].Snippet = snips[ref.mid]
			}
		}
	}
	return out, nil
}

// Values of the src column in Search's hit set: which FTS index scored
// the hit.
const (
	srcPorter  = 0
	srcTrigram = 1
)

// excerptRef points from an excerpt in Search's output back to the message
// and index it came from, so its snippet can be filled in afterward.
type excerptRef struct {
	result, excerpt int
	mid             int64
	src             int
}

// scanSearchRows runs Search's ranking query and groups its rows into one
// Result per session, leaving snippets empty.
func scanSearchRows(tx *sql.Tx, query string, args []any) ([]Result, []excerptRef, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var (
		out  []Result
		refs []excerptRef
	)
	byID := map[int64]int{} // session_id -> index into out
	for rows.Next() {
		var (
			r   Result
			ex  Excerpt
			ref excerptRef
		)
		if err := rows.Scan(&r.SessionID, &r.SessionUID, &r.Agent, &r.Workspace, &r.Title, &r.Summary,
			&r.StartedAt,
			&r.BestScore, &r.MatchCount,
			&ex.Ordinal, &ex.Role, &ex.TS, &ex.Score, &ref.mid, &ref.src); err != nil {
			return nil, nil, err
		}
		idx, ok := byID[r.SessionID]
		if !ok {
			idx = len(out)
			byID[r.SessionID] = idx
			out = append(out, r)
		}
		ref.result = idx
		ref.excerpt = len(out[idx].Excerpts)
		out[idx].Excerpts = append(out[idx].Excerpts, ex)
		refs = append(refs, ref)
	}
	return out, refs, rows.Err()
}

// pageSnippets returns snippet() text for the given message ids from one
// FTS table, keyed by message id.
//
// It evaluates the MATCH once and builds a snippet only for the requested
// rows. Asking per row with "MATCH ? AND rowid = ?" re-expands the query
// every time, which costs about 100 ms per row for a short prefix term
// such as app* on a large index.
//
// The ids are sorted and sent in batches of at most snippetBatchSize, which
// keeps each query under SQLite's bound-parameter limit when a large
// --limit puts many thousands of excerpts on the page.
func pageSnippets(tx *sql.Tx, table, match string, mids []int64) (map[int64]string, error) {
	sorted := slices.Clone(mids)
	slices.Sort(sorted)
	out := make(map[int64]string, len(mids))
	for chunk := range slices.Chunk(sorted, snippetBatchSize) {
		if err := snippetBatch(tx, table, match, chunk, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// snippetBatchSize caps the message ids per snippet query. A variable so
// tests can force several batches.
var snippetBatchSize = 500

// snippetBatch adds snippets for one sorted batch of ids to out.
func snippetBatch(tx *sql.Tx, table, match string, mids []int64, out map[int64]string) error {
	args := make([]any, 0, len(mids)+3)
	marks := make([]string, len(mids))
	for i, id := range mids {
		marks[i] = "?"
		args = append(args, id)
	}
	args = append(args, match, mids[0], mids[len(mids)-1])
	query := fmt.Sprintf(`
        WITH snips AS MATERIALIZED (
            SELECT rowid AS id,
                   CASE WHEN rowid IN (%[2]s)
                        THEN snippet(%[1]s, 0, '«', '»', '…', 12) END AS snip
            FROM %[1]s
            WHERE %[1]s MATCH ? AND rowid BETWEEN ? AND ?
        )
        SELECT id, snip FROM snips WHERE snip IS NOT NULL`,
		table, strings.Join(marks, ","))
	rows, err := tx.Query(query, args...)
	if err != nil {
		return fmt.Errorf("search snippets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id   int64
			snip string
		)
		if err := rows.Scan(&id, &snip); err != nil {
			return err
		}
		out[id] = snip
	}
	return rows.Err()
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
// FTS5 syntax (parens/colons/stars) from detonating on user input.
// Uppercase AND, OR, and NOT between two terms pass through as boolean
// operators; anywhere else they're dropped, except that a NOT with no
// term before it also drops the term after it, so "NOT x" matches
// nothing rather than everything containing x.
func normalizeMatch(q string) string {
	out, _ := joinMatch(tokenizeMatch(q), func(it matchItem) string {
		switch {
		case it.text == "":
			return ""
		case it.phrase:
			return `"` + it.text + `"`
		case ftsKeywords[it.text]:
			// A bare NEAR* or OR* is a syntax error; quoting makes it a term.
			return `"` + it.text + `"*`
		default:
			return it.text + "*"
		}
	})
	if out == "" {
		return `""`
	}
	return out
}

// normalizeMatchTrigram builds a MATCH string for the trigram FTS table.
// Trigram tokenization requires ≥3-char substrings and doesn't support
// prefix stars. Each bare token and each quoted phrase becomes a quoted
// phrase; tokens shorter than 3 chars are dropped. Boolean operators
// follow the same rules as normalizeMatch. Returns "" when no usable
// tokens remain — callers then skip the trigram branch entirely. Also
// returns "" when the query has operators and a term was dropped: the
// operators would then group differently than in the porter query, and
// since the two branches are UNIONed, the trigram side would return rows
// the porter side excludes (for example "download NOT ab").
func normalizeMatchTrigram(q string) string {
	items := tokenizeMatch(q)
	out, dropped := joinMatch(items, func(it matchItem) string {
		if len([]rune(it.text)) < 3 {
			return ""
		}
		return `"` + it.text + `"`
	})
	if dropped {
		for _, it := range items {
			if it.op {
				return ""
			}
		}
	}
	return out
}

// matchItem is one piece of a user query: a bare word, a quoted phrase,
// or a boolean operator (AND, OR, NOT).
type matchItem struct {
	text   string
	phrase bool
	op     bool
}

// ftsKeywords are the bare words FTS5 parses as syntax rather than terms.
var ftsKeywords = map[string]bool{"AND": true, "OR": true, "NOT": true, "NEAR": true}

// tokenizeMatch splits a query into sanitized words, quoted phrases, and
// operators. An unterminated quote runs to the end of the query.
func tokenizeMatch(q string) []matchItem {
	var items []matchItem
	in := []rune(strings.TrimSpace(q))
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c == '"' {
			j := i + 1
			for j < len(in) && in[j] != '"' {
				j++
			}
			items = append(items, matchItem{text: sanitizePhrase(string(in[i+1 : j])), phrase: true})
			i = j
			continue
		}
		if isSpace(c) {
			continue
		}
		j := i
		for j < len(in) && !isSpace(in[j]) {
			j++
		}
		raw := string(in[i:j])
		switch raw {
		case "AND", "OR", "NOT":
			items = append(items, matchItem{text: raw, op: true})
		default:
			items = append(items, matchItem{text: sanitizeToken(raw)})
		}
		i = j - 1
	}
	return items
}

// joinMatch renders items into a MATCH expression. render returns "" to
// drop a term. An operator is kept only when a kept term sits on each
// side of it; of several operators in a row, the last one wins, and an
// operator whose right-hand term is dropped goes with it. A NOT with no
// kept term before it drops the next term too. dropped reports whether
// any term was dropped.
func joinMatch(items []matchItem, render func(matchItem) string) (expr string, dropped bool) {
	var out []string
	pendingOp := ""
	skipNext := false
	for _, it := range items {
		if it.op {
			if len(out) > 0 {
				pendingOp = it.text
			} else if it.text == "NOT" {
				skipNext = true
			}
			continue
		}
		term := render(it)
		if skipNext || term == "" {
			skipNext = false
			pendingOp = ""
			dropped = true
			continue
		}
		if pendingOp != "" {
			out = append(out, pendingOp)
			pendingOp = ""
		}
		out = append(out, term)
	}
	return strings.Join(out, " "), dropped
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
