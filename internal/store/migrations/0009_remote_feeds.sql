CREATE TABLE IF NOT EXISTS remote_feeds (
    id              INTEGER PRIMARY KEY,
    feed_url        TEXT NOT NULL UNIQUE,
    site_url        TEXT NOT NULL DEFAULT '',
    title           TEXT NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    icon_url        TEXT NOT NULL DEFAULT '',
    etag            TEXT NOT NULL DEFAULT '',
    last_modified   TEXT NOT NULL DEFAULT '',
    last_fetched_at INTEGER NOT NULL DEFAULT 0,
    last_success_at INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    active          INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1)),
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at      INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS remote_feed_items (
    id             INTEGER PRIMARY KEY,
    feed_id        INTEGER NOT NULL REFERENCES remote_feeds(id) ON DELETE CASCADE,
    remote_id      TEXT NOT NULL,
    url            TEXT NOT NULL DEFAULT '',
    external_url   TEXT NOT NULL DEFAULT '',
    title          TEXT NOT NULL DEFAULT '',
    content_text   TEXT NOT NULL DEFAULT '',
    image_url      TEXT NOT NULL DEFAULT '',
    attachment_url TEXT NOT NULL DEFAULT '',
    attachment_mime TEXT NOT NULL DEFAULT '',
    author_name    TEXT NOT NULL DEFAULT '',
    author_url     TEXT NOT NULL DEFAULT '',
    published_at   INTEGER NOT NULL,
    fetched_at     INTEGER NOT NULL DEFAULT (unixepoch()),
    raw_json        TEXT NOT NULL DEFAULT '',
    UNIQUE(feed_id, remote_id)
);

CREATE INDEX IF NOT EXISTS idx_remote_feeds_active ON remote_feeds(active, title);
CREATE INDEX IF NOT EXISTS idx_remote_feed_items_feed ON remote_feed_items(feed_id, published_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_remote_feed_items_published ON remote_feed_items(published_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS reposts (
    id                  INTEGER PRIMARY KEY,
    remote_feed_item_id  INTEGER NOT NULL UNIQUE REFERENCES remote_feed_items(id) ON DELETE CASCADE,
    local_item_id        INTEGER UNIQUE REFERENCES items(id) ON DELETE SET NULL,
    created_at           INTEGER NOT NULL DEFAULT (unixepoch())
);
