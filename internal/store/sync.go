package store

import (
	"database/sql"
	"strings"
)

// Cloud-sync accessors. The engine lives in internal/cloudsync; this
// file is only the SQLite surface it needs.

// SyncSourcePrefix marks sessions that arrived via `aii sync pull`
// rather than a local transcript. A synthetic source_path can never
// match a scanned filesystem path, so the indexer's truncation
// handling (DeleteBySourcePath) can never delete synced sessions.
const SyncSourcePrefix = "aii-sync://"

// SyntheticSourcePath builds the source_path for a pulled session.
func SyntheticSourcePath(agent, uid string) string {
	return SyncSourcePrefix + agent + "/" + uid
}

// IsSyntheticSourcePath reports whether a session was pull-imported
// and has no local transcript backing it.
func IsSyntheticSourcePath(p string) bool { return strings.HasPrefix(p, SyncSourcePrefix) }

// UpsertSessionFromSync merges a pulled session's metadata. Unlike the
// indexer's UpsertSession, local values win (remote only fills
// blanks), and the DO UPDATE clause omits source_path/mtime/size
// entirely — a pull can never clobber a real local transcript path
// with the synthetic one. The reverse direction is intentionally
// asymmetric: when the real transcript later appears locally, the
// indexer's UpsertSession overwrites the synthetic path with the real
// one, which re-attaches truncation handling.
func (d *DB) UpsertSessionFromSync(s *Session) (int64, error) {
	_, err := d.Exec(`
        INSERT INTO sessions (agent, session_uid, workspace, title, summary, started_at, ended_at, source_path, source_mtime_ns, source_size)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0)
        ON CONFLICT(agent, session_uid) DO UPDATE SET
            workspace  = COALESCE(NULLIF(sessions.workspace, ''), excluded.workspace),
            title      = COALESCE(NULLIF(sessions.title, ''), excluded.title),
            summary    = COALESCE(NULLIF(sessions.summary, ''), excluded.summary),
            started_at = MIN(COALESCE(sessions.started_at, excluded.started_at),
                             COALESCE(excluded.started_at, sessions.started_at)),
            ended_at   = MAX(COALESCE(sessions.ended_at, 0), COALESCE(excluded.ended_at, 0))`,
		s.Agent, s.UID, s.Workspace, s.Title, s.Summary,
		nullIfZero(s.StartedAt), nullIfZero(s.EndedAt),
		SyntheticSourcePath(s.Agent, s.UID))
	if err != nil {
		return 0, err
	}
	var id int64
	if err := d.QueryRow(`SELECT id FROM sessions WHERE agent = ? AND session_uid = ?`, s.Agent, s.UID).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// ReplaceSessionMessages wipes and rewrites a session's messages
// inside the caller's transaction — the epoch-supersede path. The
// delete fires the FTS delete triggers, the inserts the insert
// triggers, so both FTS tables stay correct.
func (d *DB) ReplaceSessionMessages(tx *sql.Tx, sessionID int64, msgs []Message) error {
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	return d.InsertMessages(tx, sessionID, msgs)
}

// DeleteSessionByUID removes one session and (via FK cascade, which
// fires the FTS delete triggers) its messages. Used by `aii sync
// purge --local`.
func (d *DB) DeleteSessionByUID(agent, uid string) error {
	_, err := d.Exec(`DELETE FROM sessions WHERE agent = ? AND session_uid = ?`, agent, uid)
	return err
}

type SyncState struct {
	Agent      string
	UID        string
	Epoch      int64
	ChainCount int
	ChainHash  []byte
	Excluded   bool
}

func (d *DB) GetSyncState(agent, uid string) (*SyncState, error) {
	row := d.QueryRow(`SELECT agent, session_uid, epoch, chain_count, chain_hash, excluded
                       FROM sync_state WHERE agent = ? AND session_uid = ?`, agent, uid)
	s := &SyncState{}
	var excluded int
	err := row.Scan(&s.Agent, &s.UID, &s.Epoch, &s.ChainCount, &s.ChainHash, &excluded)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Excluded = excluded != 0
	return s, nil
}

func (d *DB) SaveSyncState(s SyncState) error {
	excluded := 0
	if s.Excluded {
		excluded = 1
	}
	if s.ChainHash == nil {
		s.ChainHash = []byte{}
	}
	_, err := d.Exec(`
        INSERT INTO sync_state (agent, session_uid, epoch, chain_count, chain_hash, excluded)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(agent, session_uid) DO UPDATE SET
            epoch = excluded.epoch, chain_count = excluded.chain_count,
            chain_hash = excluded.chain_hash, excluded = excluded.excluded`,
		s.Agent, s.UID, s.Epoch, s.ChainCount, s.ChainHash, excluded)
	return err
}

func (d *DB) ListSyncStates() ([]SyncState, error) {
	rows, err := d.Query(`SELECT agent, session_uid, epoch, chain_count, chain_hash, excluded FROM sync_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncState
	for rows.Next() {
		var s SyncState
		var excluded int
		if err := rows.Scan(&s.Agent, &s.UID, &s.Epoch, &s.ChainCount, &s.ChainHash, &excluded); err != nil {
			return nil, err
		}
		s.Excluded = excluded != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// SyncMetaGet returns "" for an absent key.
func (d *DB) SyncMetaGet(key string) (string, error) {
	var v string
	err := d.QueryRow(`SELECT value FROM sync_meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (d *DB) SyncMetaSet(key, value string) error {
	_, err := d.Exec(`INSERT INTO sync_meta (key, value) VALUES (?, ?)
                      ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
