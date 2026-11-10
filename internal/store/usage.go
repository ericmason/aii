package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// Usage is the token usage of one model API request, or for agents that
// log running totals (Codex), one running-total snapshot.
//
// Key identifies the request within the agent. Two records with the same
// agent and key are the same request seen twice (Claude Code writes one
// transcript line per content block, all carrying the same usage), so the
// store keeps one row per key.
type Usage struct {
	Agent         string
	SessionUID    string
	Key           string
	TS            int64
	Model         string
	Input         int64 // input tokens not read from or written to the prompt cache
	Output        int64
	CacheCreation int64
	CacheRead     int64
	// Cumulative marks a running total for the whole session up to TS,
	// rather than the usage of one request. Reports turn consecutive
	// running totals into per-interval amounts, so a session's total is
	// its last running total.
	Cumulative bool
}

// Total returns the sum of all four token counts.
func (u Usage) Total() int64 { return u.Input + u.Output + u.CacheCreation + u.CacheRead }

// InsertUsage records usage rows in tx. When a key is already stored, the
// row with the larger token total wins: Claude Code can write an early
// copy of a response's usage before the final output count is known.
func (d *DB) InsertUsage(tx *sql.Tx, rows []Usage) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`
        INSERT INTO token_usage (agent, session_uid, request_key, ts, model,
                                 input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, cumulative)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(agent, request_key) DO UPDATE SET
            ts                    = excluded.ts,
            model                 = COALESCE(NULLIF(excluded.model, ''), token_usage.model),
            input_tokens          = excluded.input_tokens,
            output_tokens         = excluded.output_tokens,
            cache_creation_tokens = excluded.cache_creation_tokens,
            cache_read_tokens     = excluded.cache_read_tokens
        WHERE excluded.input_tokens + excluded.output_tokens + excluded.cache_creation_tokens + excluded.cache_read_tokens
            > token_usage.input_tokens + token_usage.output_tokens + token_usage.cache_creation_tokens + token_usage.cache_read_tokens`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, u := range rows {
		cum := 0
		if u.Cumulative {
			cum = 1
		}
		if _, err := stmt.Exec(u.Agent, u.SessionUID, u.Key, nullIfZero(u.TS), nullIfEmpty(u.Model),
			u.Input, u.Output, u.CacheCreation, u.CacheRead, cum); err != nil {
			return err
		}
	}
	return nil
}

// LastUsageModel returns the model of the most recent usage row recorded
// for a session, or "" if there is none. Parsers that read a transcript
// incrementally use it when the new bytes don't name the model again.
func (d *DB) LastUsageModel(agent, sessionUID string) (string, error) {
	var m sql.NullString
	err := d.QueryRow(`SELECT model FROM token_usage
                       WHERE agent = ? AND session_uid = ? AND model IS NOT NULL
                       ORDER BY ts DESC, id DESC LIMIT 1`, agent, sessionUID).Scan(&m)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return m.String, err
}

// UsageFilter selects usage for a report. Zero fields are ignored.
type UsageFilter struct {
	Agent     string
	Workspace string
	SinceUnix int64 // inclusive
	UntilUnix int64 // exclusive
	// By lists the grouping keys, in order: day, month, model, agent,
	// workspace, session. Empty means one total row.
	By    []string
	Limit int
}

// UsageGroupKeys are the values UsageFilter.By accepts.
var UsageGroupKeys = []string{"day", "month", "model", "agent", "workspace", "session"}

// UsageRow is one group of a usage report.
type UsageRow struct {
	Day           string `json:"day,omitempty"`
	Month         string `json:"month,omitempty"`
	Model         string `json:"model,omitempty"`
	Agent         string `json:"agent,omitempty"`
	Workspace     string `json:"workspace,omitempty"`
	Cite          string `json:"cite,omitempty"` // session token for `aii show`, filled in by the CLI
	SessionUID    string `json:"session_uid,omitempty"`
	Title         string `json:"title,omitempty"`
	Requests      int64  `json:"requests"`
	Input         int64  `json:"input_tokens"`
	Output        int64  `json:"output_tokens"`
	CacheCreation int64  `json:"cache_creation_tokens"`
	CacheRead     int64  `json:"cache_read_tokens"`
	Total         int64  `json:"total_tokens"`
}

// usageEventsSQL yields one row per request. Rows stored as running totals
// become the difference from the session's previous running total, ordered
// by the total itself, which only grows.
const usageEventsSQL = `
    SELECT agent, session_uid, ts, model,
           input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens
    FROM token_usage WHERE cumulative = 0
    UNION ALL
    SELECT agent, session_uid, ts, model,
           MAX(input_tokens - prev_input, 0), MAX(output_tokens - prev_output, 0),
           MAX(cache_creation_tokens - prev_cc, 0), MAX(cache_read_tokens - prev_cr, 0)
    FROM (
        SELECT agent, session_uid, ts, model,
               input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
               COALESCE(LAG(input_tokens)          OVER w, 0) AS prev_input,
               COALESCE(LAG(output_tokens)         OVER w, 0) AS prev_output,
               COALESCE(LAG(cache_creation_tokens) OVER w, 0) AS prev_cc,
               COALESCE(LAG(cache_read_tokens)     OVER w, 0) AS prev_cr
        FROM token_usage
        WHERE cumulative = 1
        WINDOW w AS (PARTITION BY agent, session_uid
                     ORDER BY input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens, ts, id)
    )`

// UsageReport sums token usage by the filter's grouping keys. Days and
// months are in the local time zone.
func (d *DB) UsageReport(f UsageFilter) ([]UsageRow, error) {
	var (
		cols    []string
		groups  []string
		clauses []string
		args    []any
	)
	for _, k := range f.By {
		switch k {
		case "day":
			cols = append(cols, "date(e.ts, 'unixepoch', 'localtime')")
		case "month":
			cols = append(cols, "strftime('%Y-%m', e.ts, 'unixepoch', 'localtime')")
		case "model":
			cols = append(cols, "COALESCE(e.model, '')")
		case "agent":
			cols = append(cols, "e.agent")
		case "workspace":
			cols = append(cols, "COALESCE(s.workspace, '')")
		case "session":
			cols = append(cols, "e.agent", "e.session_uid", "COALESCE(MAX(s.title), '')")
			groups = append(groups, "e.agent", "e.session_uid")
			continue
		default:
			return nil, fmt.Errorf("unknown usage grouping %q (want one of %s)", k, strings.Join(UsageGroupKeys, ", "))
		}
		groups = append(groups, cols[len(cols)-1])
	}
	if f.Agent != "" {
		clauses = append(clauses, "e.agent = ?")
		args = append(args, f.Agent)
	}
	if f.Workspace != "" {
		clauses = append(clauses, "s.workspace = ?")
		args = append(args, f.Workspace)
	}
	if f.SinceUnix > 0 {
		clauses = append(clauses, "e.ts >= ?")
		args = append(args, f.SinceUnix)
	}
	if f.UntilUnix > 0 {
		clauses = append(clauses, "e.ts < ?")
		args = append(args, f.UntilUnix)
	}
	where, groupBy, order := "", "", ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	if len(groups) > 0 {
		groupBy = "GROUP BY " + strings.Join(groups, ", ")
		order = "ORDER BY " + usageOrder(f.By)
	}
	selectCols := strings.Join(append(cols, ""), ", ")
	q := fmt.Sprintf(`
        WITH e AS (%s)
        SELECT %s COUNT(*), COALESCE(SUM(e.input_tokens), 0) AS t_in, COALESCE(SUM(e.output_tokens), 0) AS t_out,
               COALESCE(SUM(e.cache_creation_tokens), 0) AS t_cc, COALESCE(SUM(e.cache_read_tokens), 0) AS t_cr
        FROM e LEFT JOIN sessions s ON s.agent = e.agent AND s.session_uid = e.session_uid
        %s %s %s`, usageEventsSQL, selectCols, where, groupBy, order)
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage report: %w", err)
	}
	defer rows.Close()

	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		var dests []any
		for _, k := range f.By {
			switch k {
			case "day":
				dests = append(dests, &r.Day)
			case "month":
				dests = append(dests, &r.Month)
			case "model":
				dests = append(dests, &r.Model)
			case "agent":
				dests = append(dests, &r.Agent)
			case "workspace":
				dests = append(dests, &r.Workspace)
			case "session":
				dests = append(dests, &r.Agent, &r.SessionUID, &r.Title)
			}
		}
		dests = append(dests, &r.Requests, &r.Input, &r.Output, &r.CacheCreation, &r.CacheRead)
		if err := rows.Scan(dests...); err != nil {
			return nil, err
		}
		r.Total = r.Input + r.Output + r.CacheCreation + r.CacheRead
		if r.Requests == 0 {
			continue // the ungrouped total of an empty selection
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// usageOrder sorts time buckets oldest first; any other grouping sorts the
// largest total first.
func usageOrder(by []string) string {
	var parts []string
	pos := 1
	for _, k := range by {
		if k == "day" || k == "month" {
			parts = append(parts, fmt.Sprintf("%d", pos))
		}
		if k == "session" {
			pos += 3
		} else {
			pos++
		}
	}
	return strings.Join(append(parts, "(t_in + t_out + t_cc + t_cr) DESC"), ", ")
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
