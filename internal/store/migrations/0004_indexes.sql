-- Speed up tag-based lookups: the tag filter in ListItems and the GetRelated
-- subquery both scan item_tags by item_id, which previously had no index
-- (only tag_id was indexed).
CREATE INDEX IF NOT EXISTS idx_item_tags_item ON item_tags(item_id);

-- Helps the periodic expired-session purge (range scan on expiry).
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);
