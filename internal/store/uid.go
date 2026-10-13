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

// foldUID lowercases ASCII letters only, matching SQLite's built-in
// lower(). Session-id matching is ASCII case-insensitive.
func foldUID(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
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
// of agent. See SessionByRef.
func (d *DB) SessionByUIDAny(uid string) (*Session, error) {
	return d.SessionByRef("", uid)
}

// SessionByRef finds a session by full uid or uid prefix. It tries each
// match rule in order of preference (exact uid, exact uid ignoring
// ASCII case, uid prefix ignoring ASCII case) and stops at the first
// rule that matches anything. When agent is set, it first looks only at
// that agent's sessions, then falls back to all agents if that finds
// nothing. It returns (nil, nil) when nothing matches and an
// *AmbiguousUIDError when the deciding rule matches more than one
// session.
func (d *DB) SessionByRef(agent, ref string) (*Session, error) {
	if agent != "" {
		s, err := d.sessionByRef(agent, ref)
		if err != nil || s != nil {
			return s, err
		}
	}
	return d.sessionByRef("", ref)
}

func (d *DB) sessionByRef(agent, ref string) (*Session, error) {
	fold := foldUID(ref)
	tiers := []struct {
		where string
		args  []any
	}{
		{`session_uid = ?`, []any{ref}},
		{`lower(session_uid) = ?`, []any{fold}},
		{`lower(session_uid) >= ? AND lower(session_uid) < ?`, []any{fold, prefixUpperBound(fold)}},
	}
	var matches []Session
	for _, t := range tiers {
		where, args := t.where, t.args
		if agent != "" {
			where += ` AND agent = ?`
			args = append(args, agent)
		}
		rows, err := d.Query(`SELECT `+sessionCols+` FROM sessions WHERE `+where+`
		                      ORDER BY started_at DESC LIMIT ?`,
			append(args, maxAmbiguousCandidates+1)...)
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
	e := &AmbiguousUIDError{Ref: ref, Candidates: matches}
	if len(matches) > maxAmbiguousCandidates {
		e.Candidates, e.More = matches[:maxAmbiguousCandidates], true
	}
	for _, s := range e.Candidates {
		e.CiteUIDs = append(e.CiteUIDs, d.CiteUID(s.UID))
	}
	return nil, e
}

// CiteUID returns the shortest prefix of uid, at least MinCiteUIDLen
// characters, that no other indexed session id starts with, ignoring
// ASCII case. Cite tokens use it so SessionByRef resolves them to
// exactly one session. Unique ids keep their 8-char prefix, which
// matches tokens printed by earlier versions. A nil DB falls back to
// the 8-char prefix. An id that another session shares (under a
// different agent, or differing only in case) gets the full id, which
// SessionByRef resolves through its exact-match rule or the token's
// agent code.
func (d *DB) CiteUID(uid string) string {
	n := MinCiteUIDLen
	if d != nil {
		fold := foldUID(uid)
		var dup int
		if err := d.QueryRow(`SELECT COUNT(*) FROM sessions WHERE lower(session_uid) = ?`, fold).Scan(&dup); err == nil && dup > 1 {
			return uid
		}
		for _, q := range []string{
			`SELECT lower(session_uid) FROM sessions WHERE lower(session_uid) < ? ORDER BY lower(session_uid) DESC LIMIT 1`,
			`SELECT lower(session_uid) FROM sessions WHERE lower(session_uid) > ? ORDER BY lower(session_uid) ASC LIMIT 1`,
		} {
			var nb string
			if err := d.QueryRow(q, fold).Scan(&nb); err != nil {
				continue
			}
			if l := commonPrefixLen(fold, nb) + 1; l > n {
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
