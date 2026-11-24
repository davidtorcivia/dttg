-- Content hash (SHA-256 of uploaded file bytes) and indexes for duplicate
-- detection and fast filtering by kind/visibility/color.
ALTER TABLE items ADD COLUMN content_hash TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_items_source_url   ON items(source_url) WHERE source_url != '';
CREATE INDEX IF NOT EXISTS idx_items_content_hash ON items(content_hash) WHERE content_hash != '';
CREATE INDEX IF NOT EXISTS idx_items_kind         ON items(kind);
