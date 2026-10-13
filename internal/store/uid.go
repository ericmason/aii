package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// MinCiteUIDLen is the shortest session-id prefix printed in cite
// tokens. Tokens written before ambiguity handling were exactly this
// long, so unique 8-char prefixes keep resolving.
const MinCiteUIDLen = 8

// maxAmbiguousCandidates caps how many sessions an ambiguity error
// lists.
const maxAmbiguousCandidates = 10

// AmbiguousUIDError reports a session reference that matches more than
// one indexed session. Callers must not pick one of the candidates.
type AmbiguousUIDError struct {
	Ref        string
	Candidates []Session
	// CiteUIDs holds the unambiguous id prefix for each candidate.
	CiteUIDs []string
	// More is true when additional matches exist beyond Candidates.
	More bool
}

func (e *AmbiguousUIDError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "session id %q is ambiguous; it matches more than one session. Use a longer id:", e.Ref)
	for i, s := range e.Candidates {
		title := strings.Join(strings.Fields(s.Title), " ")
		if r := []rune(title); len(r) > 60 {
			title = string(r[:59]) + "…"
		}
		fmt.Fprintf(&b, "\n  %s/%s  %s  %s", s.Agent, e.CiteUIDs[i], FormatTime(s.StartedAt), title)
	}
	if e.More {
		b.WriteString("\n  …and more")
	}
	return b.String()
}

// prefixUpperBound returns a string that sorts after every string that
// starts with p, so `uid >= p AND uid < bound` is an index-friendly
// prefix match without LIKE wildcards.
func prefixUpperBound(p string) string { return p + "\U0010FFFF" }

const sessionCols = `id, agent, session_uid, COALESCE(workspace,''), COALESCE(title,''),
                     COALESCE(summary,''),
                     COALESCE(started_at,0), COALESCE(ended_at,0), source_path,
                     source_mtime_ns, source_size`

func scanSessions(rows *sql.Rows) ([]Session, error) {
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.Agent, &s.UID, &s.Workspace, &s.Title, &s.Summary,
			&s.StartedAt, &s.EndedAt,
			&s.SourcePath, &s.SourceMtimeNs, &s.SourceSize); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SessionByUIDAny finds a session by full uid or uid prefix, regardless
// of agent. It tries each match rule in order of preference (exact uid,
// then uid prefix) and stops at the first rule that matches anything.
// It returns (nil, nil) when nothing matches and an *AmbiguousUIDError
// when that rule matches more than one session.
func (d *DB) SessionByUIDAny(uid string) (*Session, error) {
	tiers := []struct {
		where string
		args  []any
	}{
		{`session_uid = ?`, []any{uid}},
		{`session_uid >= ? AND session_uid < ?`, []any{uid, prefixUpperBound(uid)}},
	}
	var matches []Session
	for _, t := range tiers {
		rows, err := d.Query(`SELECT `+sessionCols+` FROM sessions WHERE `+t.where+`
		                      ORDER BY started_at DESC LIMIT ?`,
			append(t.args, maxAmbiguousCandidates+1)...)
		if err != nil {
			return nil, err
		}
		if matches, err = scanSessions(rows); err != nil {
			return nil, err
		}
		if len(matches) > 0 {
			break
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	}
	e := &AmbiguousUIDError{Ref: uid, Candidates: matches}
	if len(matches) > maxAmbiguousCandidates {
		e.Candidates, e.More = matches[:maxAmbiguousCandidates], true
	}
	for _, s := range e.Candidates {
		e.CiteUIDs = append(e.CiteUIDs, d.CiteUID(s.UID))
	}
	return nil, e
}

// CiteUID returns the shortest prefix of uid, at least MinCiteUIDLen
// characters, that no other indexed session id starts with. Cite
// tokens use it so SessionByUIDAny resolves them to exactly one
// session. Unique ids keep their 8-char prefix, which matches tokens
// printed by earlier versions. A nil DB, or an id shared by sessions
// from different agents, falls back to the 8-char prefix or full id.
func (d *DB) CiteUID(uid string) string {
	n := MinCiteUIDLen
	if d != nil {
		var dup int
		if err := d.QueryRow(`SELECT COUNT(*) FROM sessions WHERE session_uid = ?`, uid).Scan(&dup); err == nil && dup > 1 {
			return uid
		}
		for _, q := range []string{
			`SELECT session_uid FROM sessions WHERE session_uid < ? ORDER BY session_uid DESC LIMIT 1`,
			`SELECT session_uid FROM sessions WHERE session_uid > ? ORDER BY session_uid ASC LIMIT 1`,
		} {
			var nb string
			if err := d.QueryRow(q, uid).Scan(&nb); err != nil {
				continue
			}
			if l := commonPrefixLen(uid, nb) + 1; l > n {
				n = l
			}
		}
		// Don't end a token on a separator: "0199a1b2-" reads as truncated.
		for n < len(uid) && uid[n-1] == '-' {
			n++
		}
	}
	if n >= len(uid) {
		return uid
	}
	return uid[:n]
}

func commonPrefixLen(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}
