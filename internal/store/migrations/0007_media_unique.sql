-- Enforce uniqueness on media rows. Rebuild because older SQLite cannot ADD
-- UNIQUE constraints in place.
CREATE TABLE media_new (
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
    created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE(item_id, variant),
    UNIQUE(storage_key)
);
-- Keep the lowest id per (item_id, variant) and per storage_key.
INSERT INTO media_new (id,item_id,variant,storage_key,content_type,width,height,bytes,on_local,on_r2,created_at)
SELECT m.id,m.item_id,m.variant,m.storage_key,m.content_type,m.width,m.height,m.bytes,m.on_local,m.on_r2,m.created_at
FROM media m
WHERE m.id IN (
  SELECT MIN(id) FROM media GROUP BY item_id, variant
)
AND m.id IN (
  SELECT MIN(id) FROM media GROUP BY storage_key
);
DROP TABLE media;
ALTER TABLE media_new RENAME TO media;
