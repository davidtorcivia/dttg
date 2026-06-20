-- Single-user archive schema. Timestamps are unix seconds (INTEGER) for
-- unambiguous scanning. Media variants live in their own table; an item caches
-- a resolved cover (remote URL now, processed storage key after the M2 pipeline).

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS api_tokens (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    last_used_at INTEGER
);

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS categories (
    id          INTEGER PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    position    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS items (
    id               INTEGER PRIMARY KEY,
    kind             TEXT NOT NULL CHECK (kind IN ('image','link','text','embed')),
    title            TEXT NOT NULL DEFAULT '',
    note             TEXT NOT NULL DEFAULT '',
    source_url       TEXT NOT NULL DEFAULT '',
    visibility       TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','private')),
    category_id      INTEGER REFERENCES categories(id) ON DELETE SET NULL,
    link_title       TEXT NOT NULL DEFAULT '',
    link_description TEXT NOT NULL DEFAULT '',
    link_site_name   TEXT NOT NULL DEFAULT '',
    embed_provider   TEXT NOT NULL DEFAULT '',
    embed_html       TEXT NOT NULL DEFAULT '',
    cover_remote_url TEXT NOT NULL DEFAULT '',
    cover_key        TEXT NOT NULL DEFAULT '',
    dominant_color   TEXT NOT NULL DEFAULT '',
    width            INTEGER NOT NULL DEFAULT 0,
    height           INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at       INTEGER NOT NULL DEFAULT (unixepoch()),
    published_at     INTEGER
);

CREATE TABLE IF NOT EXISTS media (
    id           INTEGER PRIMARY KEY,
    item_id      INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    variant      TEXT NOT NULL,
    storage_key  TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    width        INTEGER NOT NULL DEFAULT 0,
    height       INTEGER NOT NULL DEFAULT 0,
    bytes        INTEGER NOT NULL DEFAULT 0,
    on_local     INTEGER NOT NULL DEFAULT 1,
    on_r2        INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS tags (
    id   INTEGER PRIMARY KEY,
    slug TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS item_tags (
    item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    tag_id  INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (item_id, tag_id)
);

CREATE INDEX IF NOT EXISTS idx_items_vis_created ON items(visibility, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_items_category    ON items(category_id);
CREATE INDEX IF NOT EXISTS idx_media_item        ON media(item_id);
CREATE INDEX IF NOT EXISTS idx_item_tags_tag     ON item_tags(tag_id);
