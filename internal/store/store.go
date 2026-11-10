package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
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

	// NeedsReindex marks a file whose last batch never reached the
	// database, so its stored state describes less than the file holds.
	// Sources treat it like --full for that file: reparse from the
	// start and replace what is stored. SaveState clears it, so a
	// successful batch retires the mark.
	NeedsReindex bool
}

type DB struct {
	*sql.DB

	// hasSearchIndex caches a positive check for searchIndexName. While it
	// is false, Search checks again on each call, so it picks up an index
	// that another process finishes building.
	hasSearchIndex atomic.Bool
}

// BusyTimeout is how long a statement waits for another connection to
// release the write lock before it fails with SQLITE_BUSY. Set it
// before Open; tests lower it to reach the retry paths quickly.
var BusyTimeout = 5 * time.Second

func DefaultPath() string {
	if p := os.Getenv("AII_DB"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "aii", "aii.db")
}

// Open opens the database for querying. It applies the cheap, bounded
// schema migrations and nothing else: the one-off rebuilds that scan
// every message — the trigram index rebuild and the search covering
// index — belong to OpenForIndex.
//
// That split exists because those rebuilds take minutes on a large
// database and hold SQLite's single write lock while they run. A query
// command that did them would stall the indexer, whose batches fail
// with SQLITE_BUSY once busy_timeout expires.
func Open(path string) (*DB, error) { return open(path, false) }

// OpenForIndex opens the database for an index run and completes the
// one-off rebuilds Open skips. Call it only while holding the index
// lock, so the process doing the rebuild is also the only writer.
// Each rebuild is skipped, not waited on, when another process holds
// the write lock.
func OpenForIndex(path string) (*DB, error) { return open(path, true) }

func open(path string, rebuild bool) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(%d)",
		path, BusyTimeout.Milliseconds())
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
		`ALTER TABLE token_usage ADD COLUMN seq INTEGER`,
		`ALTER TABLE index_state ADD COLUMN needs_reindex INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate %q: %w", alter, err)
		}
	}

	// Trigram FTS migration: CREATE TRIGGER IF NOT EXISTS won't replace the
	// old two-table triggers with the three-table version, and an existing
	// messages_fts_tri will be empty for legacy DBs. Detect and fix both.
	if err := migrateTrigram(db, rebuild); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate trigram: %w", err)
	}

	if rebuild {
		if err := ensureSearchIndex(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("create search index: %w", err)
		}
	}

	return &DB{DB: db}, nil
}

// ensureSearchIndex creates the covering index Search uses, if it is
// missing. On a large existing database the first build takes minutes
// and holds the write lock. When another process holds that lock (for
// example, because it is building the index), OpenForIndex does not
// wait: it skips the build and Search runs without the index until a
// later index run builds it.
func ensureSearchIndex(db *sql.DB) error {
	exists, err := indexExists(db, searchIndexName)
	if err != nil || exists {
		return err
	}
	// Open caps the pool at one connection, so these pragmas apply to the
	// connection that runs the CREATE INDEX.
	if _, err := db.Exec(`PRAGMA busy_timeout = 0`); err != nil {
		return err
	}
	_, err = db.Exec(searchIndexSQL)
	if _, rerr := db.Exec(fmt.Sprintf(`PRAGMA busy_timeout = %d`, BusyTimeout.Milliseconds())); rerr != nil && err == nil {
		err = rerr
	}
	if isBusy(err) {
		return nil
	}
	return err
}

// indexExists reports whether the named index is in the schema.
func indexExists(db interface {
	QueryRow(string, ...any) *sql.Row
}, name string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&n)
	return n > 0, err
}

// IsBusy reports whether err means another connection holds the lock
// the statement needed — SQLITE_BUSY or SQLITE_LOCKED. Callers outside
// the store use it to decide whether a write is worth retrying.
func IsBusy(err error) bool { return isBusy(err) }

// isBusy reports whether err is SQLITE_BUSY or SQLITE_LOCKED, including
// their extended codes.
func isBusy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

// migrateTrigram brings old DBs up to the two-FTS-table schema: it
// replaces the message triggers so they also feed messages_fts_tri,
// and, with rebuild set, rebuilds the trigram index when it holds
// fewer documents than the messages table.
//
// Rewiring the triggers is DDL and costs nothing. The rebuild reads
// every message and holds the write lock for minutes on a large
// database, so only an index run asks for it.
func migrateTrigram(db *sql.DB, rebuild bool) error {
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

	if !rebuild {
		return nil
	}
	stale, err := trigramIndexStale(db)
	if err != nil {
		return err
	}
	if stale {
		if _, err := db.Exec(`INSERT INTO messages_fts_tri(messages_fts_tri) VALUES('rebuild')`); err != nil {
			return fmt.Errorf("rebuild trigram index: %w", err)
		}
	}
	return nil
}

// trigramIndexStale reports whether messages_fts_tri holds fewer
// documents than the messages table.
//
// An external-content FTS5 table delegates COUNT(*) to the content
// table, so it can't say whether the index is populated. The shadow
// `_docsize` table gains a row only as a document is indexed, so it is
// the real source of truth.
//
// Both counts come from one transaction, which SQLite serves from a
// single read snapshot. Counted in separate statements, a writer that
// inserts messages in between makes a current index look stale, and
// the rebuild that follows reads every message while holding the write
// lock — long enough for the indexer's own batches to fail with
// SQLITE_BUSY.
func trigramIndexStale(db *sql.DB) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var indexedDocs, msgCount int64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM messages_fts_tri_docsize`).Scan(&indexedDocs); err != nil {
		return false, err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount); err != nil {
		return false, err
	}
	return indexedDocs < msgCount, nil
}

// UpsertSession returns the session row id. It runs in its own
// transaction; a caller that writes a whole batch atomically uses
// UpsertSessionTx instead.
func (d *DB) UpsertSession(s *Session) (int64, error) {
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id, err := d.UpsertSessionTx(tx, s)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// UpsertSessionTx is UpsertSession inside the caller's transaction.
func (d *DB) UpsertSessionTx(tx *sql.Tx, s *Session) (int64, error) {
	_, err := tx.Exec(`
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
	if err := tx.QueryRow(`SELECT id FROM sessions WHERE agent = ? AND session_uid = ?`, s.Agent, s.UID).Scan(&id); err != nil {
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
	row := d.QueryRow(`SELECT source_path, mtime_ns, size, last_offset, COALESCE(fingerprint, ''),
                              COALESCE(needs_reindex, 0)
                       FROM index_state WHERE source_path = ?`, path)
	s := &IndexState{}
	err := row.Scan(&s.SourcePath, &s.MtimeNs, &s.Size, &s.LastOffset, &s.Fingerprint, &s.NeedsReindex)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// SaveState records what has been indexed for one file and clears any
// reindex mark, since the state now matches what the database holds.
func (d *DB) SaveState(s IndexState) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := d.SaveStateTx(tx, s); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveStateTx is SaveState inside the caller's transaction.
func (d *DB) SaveStateTx(tx *sql.Tx, s IndexState) error {
	_, err := tx.Exec(`
        INSERT INTO index_state (source_path, mtime_ns, size, last_offset, fingerprint, needs_reindex)
        VALUES (?, ?, ?, ?, ?, 0)
        ON CONFLICT(source_path) DO UPDATE SET
            mtime_ns      = excluded.mtime_ns,
            size          = excluded.size,
            last_offset   = excluded.last_offset,
            fingerprint   = excluded.fingerprint,
            needs_reindex = 0`,
		s.SourcePath, s.MtimeNs, s.Size, s.LastOffset, s.Fingerprint)
	return err
}

// MarkNeedsReindex flags a file whose batch never reached the database,
// so the next run reparses it from the start instead of trusting the
// stored mtime and size and skipping it. A file with no state row needs
// no mark: the next run reads it from scratch anyway.
func (d *DB) MarkNeedsReindex(path string) error {
	_, err := d.Exec(`UPDATE index_state SET needs_reindex = 1 WHERE source_path = ?`, path)
	return err
}

// DeleteBySourcePath removes sessions & state for a given file (used on
// truncation). It leaves token_usage alone: usage rows are keyed by session
// uid, not row id, and must outlive the transcript they came from.
func (d *DB) DeleteBySourcePath(path string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := d.DeleteBySourcePathTx(tx, path); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteBySourcePathTx is DeleteBySourcePath inside the caller's
// transaction.
func (d *DB) DeleteBySourcePathTx(tx *sql.Tx, path string) error {
	if _, err := tx.Exec(`DELETE FROM sessions WHERE source_path = ?`, path); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM index_state WHERE source_path = ?`, path)
	return err
}

// DeleteSessionAtPath deletes one session (and, by cascade, its messages)
// if it is stored for the given source file. Token usage is left alone.
func (d *DB) DeleteSessionAtPath(agent, sessionUID, path string) error {
	_, err := d.Exec(`DELETE FROM sessions WHERE agent = ? AND session_uid = ? AND source_path = ?`,
		agent, sessionUID, path)
	return err
}

// DeleteSessionAtPathTx is DeleteSessionAtPath inside the caller's
// transaction.
func (d *DB) DeleteSessionAtPathTx(tx *sql.Tx, agent, sessionUID, path string) error {
	_, err := tx.Exec(`DELETE FROM sessions WHERE agent = ? AND session_uid = ? AND source_path = ?`,
		agent, sessionUID, path)
	return err
}

// HasSessionAtPath reports whether a session is stored for the given
// source file.
func (d *DB) HasSessionAtPath(agent, sessionUID, path string) (bool, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM sessions WHERE agent = ? AND session_uid = ? AND source_path = ?`,
		agent, sessionUID, path).Scan(&n)
	return n > 0, err
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

// LatestSessionsByWorkspace returns up to limit most recent sessions in
// the given workspace, newest first. Callers that want to skip
// still-active transcripts can stat each session's SourcePath and pick
// the first one whose live mtime is older than their threshold — we
// can't filter on that here because the stored source_mtime_ns is only
// refreshed when the indexer runs, which may be minutes stale.
func (d *DB) LatestSessionsByWorkspace(ws string, limit int) ([]*Session, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := d.Query(`SELECT id, agent, session_uid, COALESCE(workspace,''), COALESCE(title,''),
                                 COALESCE(summary,''),
                                 COALESCE(started_at,0), COALESCE(ended_at,0), source_path,
                                 source_mtime_ns, source_size
                          FROM sessions WHERE workspace = ?
                          ORDER BY COALESCE(ended_at, started_at) DESC LIMIT ?`, ws, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		s := &Session{}
		if err := rows.Scan(&s.ID, &s.Agent, &s.UID, &s.Workspace, &s.Title, &s.Summary,
			&s.StartedAt, &s.EndedAt,
			&s.SourcePath, &s.SourceMtimeNs, &s.SourceSize); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
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
	// EndedMidTask filters to sessions whose final message is a user or
	// tool message — i.e. the assistant never got a chance to respond,
	// suggesting the session was interrupted or crashed.
	EndedMidTask bool
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
	if f.EndedMidTask {
		clauses = append(clauses, endedMidTaskSQL)
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
	// CountSessions's table alias is `sessions` (no `s.`), so swap the
	// EXISTS subquery to reference the bare table.
	if f.EndedMidTask {
		clauses = append(clauses, strings.ReplaceAll(endedMidTaskSQL, "s.id", "sessions.id"))
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	var n int64
	err := d.QueryRow("SELECT COUNT(*) FROM sessions "+where, args...).Scan(&n)
	return n, err
}

// endedMidTaskSQL flags sessions whose final message was authored by
// the user or a tool — meaning the assistant never got the last word.
// Implies the session was interrupted (user quit), the agent crashed
// mid-response, or a tool result is sitting unprocessed.
const endedMidTaskSQL = `EXISTS (
    SELECT 1 FROM messages m
    WHERE m.session_id = s.id
      AND m.role IN ('user','tool')
      AND m.ordinal = (SELECT MAX(ordinal) FROM messages WHERE session_id = s.id)
)`

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
