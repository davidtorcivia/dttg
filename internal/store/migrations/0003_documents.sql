-- Add the 'document' kind (PDFs and other files) and file_* columns.
-- SQLite can't alter a CHECK constraint, so rebuild items. The migration runner
-- disables FK enforcement around this, so DROP TABLE won't cascade-delete media.
CREATE TABLE items_new (
    id               INTEGER PRIMARY KEY,
    kind             TEXT NOT NULL CHECK (kind IN ('image','link','text','embed','document')),
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
    published_at     INTEGER,
    thumb_key        TEXT NOT NULL DEFAULT '',
    placeholder      TEXT NOT NULL DEFAULT '',
    file_key         TEXT NOT NULL DEFAULT '',
    file_name        TEXT NOT NULL DEFAULT '',
    file_mime        TEXT NOT NULL DEFAULT '',
    file_size        INTEGER NOT NULL DEFAULT 0
);

INSERT INTO items_new
    (id,kind,title,note,source_url,visibility,category_id,
     link_title,link_description,link_site_name,embed_provider,embed_html,
     cover_remote_url,cover_key,dominant_color,width,height,
     created_at,updated_at,published_at,thumb_key,placeholder)
SELECT
     id,kind,title,note,source_url,visibility,category_id,
     link_title,link_description,link_site_name,embed_provider,embed_html,
     cover_remote_url,cover_key,dominant_color,width,height,
     created_at,updated_at,published_at,thumb_key,placeholder
FROM items;

DROP TABLE items;
ALTER TABLE items_new RENAME TO items;

CREATE INDEX IF NOT EXISTS idx_items_vis_created ON items(visibility, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_items_category    ON items(category_id);
