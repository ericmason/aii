package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Session struct {
	ID            int64
	Agent         string
	UID           string
	Workspace     string
	Title         string
	Summary       string
	StartedAt     int64
	EndedAt       int64
	SourcePath    string
	SourceMtimeNs int64
	SourceSize    int64
}

type Message struct {
	SessionID    int64
	Ordinal      int
	Role         string
	TS           int64
	Content      string
	SourceOffset int64
}

type IndexState struct {
	SourcePath  string
	MtimeNs     int64
	Size        int64
	LastOffset  int64
	Fingerprint string
}

type DB struct{ *sql.DB }

func DefaultPath() string {
	if p := os.Getenv("AII_DB"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "aii", "aii.db")
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	// Idempotent additive migrations. Older DBs won't have these columns.
	for _, alter := range []string{
		`ALTER TABLE sessions ADD COLUMN summary TEXT`,
	} {
		if _, err := db.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate %q: %w", alter, err)
		}
	}

	// Trigram FTS migration: CREATE TRIGGER IF NOT EXISTS won't replace the
	// old two-table triggers with the three-table version, and an existing
	// messages_fts_tri will be empty for legacy DBs. Detect and fix both.
	if err := migrateTrigram(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate trigram: %w", err)
	}

	return &DB{db}, nil
}

// migrateTrigram brings old DBs up to the two-FTS-table schema: replaces
// the message triggers to also feed messages_fts_tri, and rebuilds the
// trigram index if it's empty but messages is not.
func migrateTrigram(db *sql.DB) error {
	var triggerSQL sql.NullString
	row := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='messages_ai'`)
	if err := row.Scan(&triggerSQL); err != nil && err != sql.ErrNoRows {
		return err
	}
	needRewire := triggerSQL.Valid && !strings.Contains(triggerSQL.String, "messages_fts_tri")
	if needRewire {
		for _, stmt := range []string{
			`DROP TRIGGER IF EXISTS messages_ai`,
			`DROP TRIGGER IF EXISTS messages_ad`,
			`DROP TRIGGER IF EXISTS messages_au`,
			`CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
			    INSERT INTO messages_fts(rowid, content, role) VALUES (new.id, new.content, new.role);
			    INSERT INTO messages_fts_tri(rowid, content) VALUES (new.id, new.content);
			END`,
			`CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
			    INSERT INTO messages_fts(messages_fts, rowid, content, role) VALUES ('delete', old.id, old.content, old.role);
			    INSERT INTO messages_fts_tri(messages_fts_tri, rowid, content) VALUES ('delete', old.id, old.content);
			END`,
			`CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
			    INSERT INTO messages_fts(messages_fts, rowid, content, role) VALUES ('delete', old.id, old.content, old.role);
			    INSERT INTO messages_fts(rowid, content, role) VALUES (new.id, new.content, new.role);
			    INSERT INTO messages_fts_tri(messages_fts_tri, rowid, content) VALUES ('delete', old.id, old.content);
			    INSERT INTO messages_fts_tri(rowid, content) VALUES (new.id, new.content);
			END`,
		} {
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("rewire trigger: %w", err)
			}
		}
	}

	// An external-content FTS5 table delegates COUNT(*) to the content
	// table, so it can't tell us whether the index is populated. The
	// shadow `_docsize` table is only written to as documents are
	// actually indexed — that's the real source of truth.
	var indexedDocs, msgCount int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts_tri_docsize`).Scan(&indexedDocs); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount); err != nil {
		return err
	}
	if indexedDocs < msgCount {
		if _, err := db.Exec(`INSERT INTO messages_fts_tri(messages_fts_tri) VALUES('rebuild')`); err != nil {
			return fmt.Errorf("rebuild trigram index: %w", err)
		}
	}
	return nil
}

// UpsertSession returns the session row id.
func (d *DB) UpsertSession(s *Session) (int64, error) {
	_, err := d.Exec(`
        INSERT INTO sessions (agent, session_uid, workspace, title, summary, started_at, ended_at, source_path, source_mtime_ns, source_size)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(agent, session_uid) DO UPDATE SET
            workspace       = COALESCE(NULLIF(excluded.workspace, ''), sessions.workspace),
            title           = COALESCE(NULLIF(excluded.title, ''), sessions.title),
            summary         = COALESCE(NULLIF(excluded.summary, ''), sessions.summary),
            started_at      = COALESCE(sessions.started_at, excluded.started_at),
            ended_at        = MAX(COALESCE(sessions.ended_at, 0), COALESCE(excluded.ended_at, 0)),
            source_path     = excluded.source_path,
            source_mtime_ns = excluded.source_mtime_ns,
            source_size     = excluded.source_size`,
		s.Agent, s.UID, s.Workspace, s.Title, s.Summary,
		nullIfZero(s.StartedAt), nullIfZero(s.EndedAt),
		s.SourcePath, s.SourceMtimeNs, s.SourceSize)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := d.QueryRow(`SELECT id FROM sessions WHERE agent = ? AND session_uid = ?`, s.Agent, s.UID).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (d *DB) InsertMessages(tx *sql.Tx, sessionID int64, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`
        INSERT INTO messages (session_id, ordinal, role, ts, content, source_offset)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(session_id, ordinal) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range msgs {
		if _, err := stmt.Exec(sessionID, m.Ordinal, m.Role, nullIfZero(m.TS), m.Content, nullIfZero(m.SourceOffset)); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) NextOrdinal(sessionID int64) (int, error) {
	var n sql.NullInt64
	if err := d.QueryRow(`SELECT MAX(ordinal) FROM messages WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		return 0, err
	}
	if !n.Valid {
		return 0, nil
	}
	return int(n.Int64) + 1, nil
}

func (d *DB) GetState(path string) (*IndexState, error) {
	row := d.QueryRow(`SELECT source_path, mtime_ns, size, last_offset, COALESCE(fingerprint, '')
                       FROM index_state WHERE source_path = ?`, path)
	s := &IndexState{}
	err := row.Scan(&s.SourcePath, &s.MtimeNs, &s.Size, &s.LastOffset, &s.Fingerprint)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (d *DB) SaveState(s IndexState) error {
	_, err := d.Exec(`
        INSERT INTO index_state (source_path, mtime_ns, size, last_offset, fingerprint)
        VALUES (?, ?, ?, ?, ?)
        ON CONFLICT(source_path) DO UPDATE SET
            mtime_ns    = excluded.mtime_ns,
            size        = excluded.size,
            last_offset = excluded.last_offset,
            fingerprint = excluded.fingerprint`,
		s.SourcePath, s.MtimeNs, s.Size, s.LastOffset, s.Fingerprint)
	return err
}

// DeleteBySourcePath removes sessions & state for a given file (used on truncation).
func (d *DB) DeleteBySourcePath(path string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM sessions WHERE source_path = ?`, path); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM index_state WHERE source_path = ?`, path); err != nil {
		return err
	}
	return tx.Commit()
}

type Stats struct {
	Agent    string
	Sessions int64
	Messages int64
	Latest   int64
}

func (d *DB) Stats() ([]Stats, error) {
	rows, err := d.Query(`
        SELECT s.agent,
               COUNT(DISTINCT s.id),
               COUNT(m.id),
               COALESCE(MAX(s.ended_at), MAX(s.started_at), 0)
        FROM sessions s LEFT JOIN messages m ON m.session_id = s.id
        GROUP BY s.agent ORDER BY s.agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stats
	for rows.Next() {
		var s Stats
		if err := rows.Scan(&s.Agent, &s.Sessions, &s.Messages, &s.Latest); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) SessionByUID(agent, uid string) (*Session, error) {
	row := d.QueryRow(`SELECT id, agent, session_uid, COALESCE(workspace,''), COALESCE(title,''),
                              COALESCE(summary,''),
                              COALESCE(started_at,0), COALESCE(ended_at,0), source_path,
                              source_mtime_ns, source_size
                       FROM sessions WHERE agent = ? AND session_uid = ?`, agent, uid)
	s := &Session{}
	if err := row.Scan(&s.ID, &s.Agent, &s.UID, &s.Workspace, &s.Title, &s.Summary,
		&s.StartedAt, &s.EndedAt,
		&s.SourcePath, &s.SourceMtimeNs, &s.SourceSize); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return s, nil
}

// SessionByUIDAny finds a session by uid regardless of agent (for convenience).
func (d *DB) SessionByUIDAny(uid string) (*Session, error) {
	row := d.QueryRow(`SELECT id, agent, session_uid, COALESCE(workspace,''), COALESCE(title,''),
                              COALESCE(summary,''),
                              COALESCE(started_at,0), COALESCE(ended_at,0), source_path,
                              source_mtime_ns, source_size
                       FROM sessions WHERE session_uid = ? OR session_uid LIKE ? LIMIT 1`, uid, uid+"%")
	s := &Session{}
	if err := row.Scan(&s.ID, &s.Agent, &s.UID, &s.Workspace, &s.Title, &s.Summary,
		&s.StartedAt, &s.EndedAt,
		&s.SourcePath, &s.SourceMtimeNs, &s.SourceSize); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return s, nil
}

func (d *DB) LatestSession() (*Session, error) {
	row := d.QueryRow(`SELECT id, agent, session_uid, COALESCE(workspace,''), COALESCE(title,''),
                              COALESCE(summary,''),
                              COALESCE(started_at,0), COALESCE(ended_at,0), source_path,
                              source_mtime_ns, source_size
                       FROM sessions ORDER BY COALESCE(ended_at, started_at) DESC LIMIT 1`)
	s := &Session{}
	if err := row.Scan(&s.ID, &s.Agent, &s.UID, &s.Workspace, &s.Title, &s.Summary,
		&s.StartedAt, &s.EndedAt,
		&s.SourcePath, &s.SourceMtimeNs, &s.SourceSize); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return s, nil
}

type Row struct {
	Ordinal int
	Role    string
	TS      int64
	Content string
}

// SessionFilter selects sessions by metadata; no full-text query.
// Empty fields are ignored. Order is "desc" (newest first) by default.
type SessionFilter struct {
	Agent     string
	Workspace string
	SinceUnix int64
	UntilUnix int64
	Limit     int
	Offset    int
	Order     string // "asc" | "desc" — by started_at (defaults desc)
}

// SessionListItem is a Session augmented with its message count, used by
// list/browse views where we want the size of each thread without
// pulling its messages.
type SessionListItem struct {
	Session
	MessageCount int64
}

// ListSessions returns sessions matching the filter, ordered by
// started_at and paginated. Unlike Search this never touches FTS — it's
// the "show me everything from last week" entry point.
func (d *DB) ListSessions(f SessionFilter) ([]SessionListItem, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	order := "DESC"
	if strings.EqualFold(f.Order, "asc") {
		order = "ASC"
	}

	var (
		clauses []string
		args    []any
	)
	if f.Agent != "" {
		clauses = append(clauses, "s.agent = ?")
		args = append(args, f.Agent)
	}
	if f.Workspace != "" {
		clauses = append(clauses, "s.workspace = ?")
		args = append(args, f.Workspace)
	}
	// Filter on the session's effective timestamp (ended_at if present,
	// else started_at). Sessions without timestamps (legacy rows) are
	// dropped from time-filtered queries — they'd sort to the top
	// otherwise and confuse the "last 7 days" view.
	if f.SinceUnix > 0 {
		clauses = append(clauses, "COALESCE(s.ended_at, s.started_at, 0) >= ?")
		args = append(args, f.SinceUnix)
	}
	if f.UntilUnix > 0 {
		clauses = append(clauses, "COALESCE(s.ended_at, s.started_at, 0) <= ?")
		args = append(args, f.UntilUnix)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}

	q := fmt.Sprintf(`
        SELECT s.id, s.agent, s.session_uid,
               COALESCE(s.workspace, ''), COALESCE(s.title, ''), COALESCE(s.summary, ''),
               COALESCE(s.started_at, 0), COALESCE(s.ended_at, 0),
               s.source_path, s.source_mtime_ns, s.source_size,
               (SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id) AS msg_count
        FROM sessions s
        %s
        ORDER BY COALESCE(s.ended_at, s.started_at, 0) %s, s.id %s
        LIMIT ? OFFSET ?`, where, order, order)
	args = append(args, f.Limit, f.Offset)

	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []SessionListItem
	for rows.Next() {
		var it SessionListItem
		if err := rows.Scan(&it.ID, &it.Agent, &it.UID, &it.Workspace, &it.Title, &it.Summary,
			&it.StartedAt, &it.EndedAt,
			&it.SourcePath, &it.SourceMtimeNs, &it.SourceSize,
			&it.MessageCount); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CountSessions returns the total session count for a filter — useful
// for paginated views that want to show "showing N of M".
func (d *DB) CountSessions(f SessionFilter) (int64, error) {
	var (
		clauses []string
		args    []any
	)
	if f.Agent != "" {
		clauses = append(clauses, "agent = ?")
		args = append(args, f.Agent)
	}
	if f.Workspace != "" {
		clauses = append(clauses, "workspace = ?")
		args = append(args, f.Workspace)
	}
	if f.SinceUnix > 0 {
		clauses = append(clauses, "COALESCE(ended_at, started_at, 0) >= ?")
		args = append(args, f.SinceUnix)
	}
	if f.UntilUnix > 0 {
		clauses = append(clauses, "COALESCE(ended_at, started_at, 0) <= ?")
		args = append(args, f.UntilUnix)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	var n int64
	err := d.QueryRow("SELECT COUNT(*) FROM sessions "+where, args...).Scan(&n)
	return n, err
}

func (d *DB) SessionMessages(sessionID int64) ([]Row, error) {
	rows, err := d.Query(`SELECT ordinal, role, COALESCE(ts,0), content
                          FROM messages WHERE session_id = ? ORDER BY ordinal`, sessionID)
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

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// FormatTime renders a unix seconds value as YYYY-MM-DD HH:MM.
func FormatTime(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).Local().Format("2006-01-02 15:04")
}
