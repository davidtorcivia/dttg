CREATE TABLE IF NOT EXISTS pending_shares (
    id         TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    expires_at INTEGER NOT NULL,
    title      TEXT NOT NULL DEFAULT '',
    text       TEXT NOT NULL DEFAULT '',
    url        TEXT NOT NULL DEFAULT '',
    file_key   TEXT NOT NULL DEFAULT '',
    file_name  TEXT NOT NULL DEFAULT '',
    file_mime  TEXT NOT NULL DEFAULT '',
    file_size  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_pending_shares_expires ON pending_shares(expires_at);
