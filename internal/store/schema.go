package store

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id              INTEGER PRIMARY KEY,
    agent           TEXT NOT NULL,
    session_uid     TEXT NOT NULL,
    workspace       TEXT,
    title           TEXT,
    summary         TEXT,
    started_at      INTEGER,
    ended_at        INTEGER,
    source_path     TEXT NOT NULL,
    source_mtime_ns INTEGER NOT NULL DEFAULT 0,
    source_size     INTEGER NOT NULL DEFAULT 0,
    UNIQUE(agent, session_uid)
);

CREATE TABLE IF NOT EXISTS messages (
    id            INTEGER PRIMARY KEY,
    session_id    INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal       INTEGER NOT NULL,
    role          TEXT NOT NULL,
    ts            INTEGER,
    content       TEXT NOT NULL,
    source_offset INTEGER,
    UNIQUE(session_id, ordinal)
);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    role UNINDEXED,
    content='messages',
    content_rowid='id',
    tokenize='porter unicode61 remove_diacritics 2'
);

-- Secondary FTS tuned for code identifiers (CamelCase, snake_case, digits).
-- Trigram tokenizer matches any 3+ char substring, so "UserID" is found by
-- "userid" — which porter/unicode61 splits on case boundaries and misses.
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts_tri USING fts5(
    content,
    content='messages',
    content_rowid='id',
    tokenize='trigram case_sensitive 0'
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content, role) VALUES (new.id, new.content, new.role);
    INSERT INTO messages_fts_tri(rowid, content) VALUES (new.id, new.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content, role) VALUES ('delete', old.id, old.content, old.role);
    INSERT INTO messages_fts_tri(messages_fts_tri, rowid, content) VALUES ('delete', old.id, old.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content, role) VALUES ('delete', old.id, old.content, old.role);
    INSERT INTO messages_fts(rowid, content, role) VALUES (new.id, new.content, new.role);
    INSERT INTO messages_fts_tri(messages_fts_tri, rowid, content) VALUES ('delete', old.id, old.content);
    INSERT INTO messages_fts_tri(rowid, content) VALUES (new.id, new.content);
END;

CREATE TABLE IF NOT EXISTS index_state (
    source_path TEXT PRIMARY KEY,
    mtime_ns    INTEGER NOT NULL,
    size        INTEGER NOT NULL,
    last_offset INTEGER NOT NULL DEFAULT 0,
    fingerprint TEXT
);

CREATE INDEX IF NOT EXISTS idx_sessions_agent     ON sessions(agent);
CREATE INDEX IF NOT EXISTS idx_sessions_workspace ON sessions(workspace);
CREATE INDEX IF NOT EXISTS idx_sessions_started   ON sessions(started_at);
CREATE INDEX IF NOT EXISTS idx_messages_session   ON messages(session_id, ordinal);
`

// searchIndexName is a covering index for search: it maps an FTS hit's
// rowid to its session and metadata without reading the message row,
// whose large content spreads rows across many pages. Search names it
// with INDEXED BY because the planner otherwise prefers the rowid lookup.
const searchIndexName = "idx_messages_id_session"

// searchIndexSQL is kept out of schema: building it on a large existing
// database takes minutes and holds the write lock, so Open attempts it
// separately and tolerates another process holding the lock.
const searchIndexSQL = `CREATE INDEX IF NOT EXISTS ` + searchIndexName + ` ON messages(id, session_id, ordinal, role, ts)`
