-- Multi-image posts: each item may hold an ordered list of processed images.
-- Position 0 mirrors items.cover_key/thumb_key/small_key/placeholder/dominant_color/width/height
-- so board cards, search, OG cards, and existing queries stay fast and backwards-compatible.
ALTER TABLE items ADD COLUMN image_count INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS item_images (
    id             INTEGER PRIMARY KEY,
    item_id        INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    position       INTEGER NOT NULL DEFAULT 0,
    cover_key      TEXT NOT NULL DEFAULT '',
    thumb_key      TEXT NOT NULL DEFAULT '',
    small_key      TEXT NOT NULL DEFAULT '',
    placeholder    TEXT NOT NULL DEFAULT '',
    dominant_color TEXT NOT NULL DEFAULT '',
    width          INTEGER NOT NULL DEFAULT 0,
    height         INTEGER NOT NULL DEFAULT 0,
    alt            TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_item_images_item_pos ON item_images(item_id, position, id);

-- Backfill existing items that already have a processed cover image.
INSERT INTO item_images (item_id, position, cover_key, thumb_key, small_key, placeholder, dominant_color, width, height, created_at)
SELECT id, 0, cover_key, thumb_key, small_key, placeholder, dominant_color, width, height, created_at
FROM items
WHERE cover_key != '';

UPDATE items SET image_count = CASE
    WHEN cover_key != '' THEN 1
    WHEN cover_remote_url != '' THEN 1
    ELSE 0
END;
