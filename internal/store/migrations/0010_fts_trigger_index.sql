-- Re-index FTS only when an indexed column changes, not on every media/flag
-- update (ReplaceItemMedia, SetItemSmallKey, published_at stamps).
DROP TRIGGER IF EXISTS items_fts_au;
CREATE TRIGGER items_fts_au AFTER UPDATE OF title, note, link_title, link_description, link_site_name, file_name ON items BEGIN
    INSERT INTO items_fts(items_fts, rowid, title, note, link_title, link_description, link_site_name, file_name)
        VALUES ('delete', old.id, old.title, old.note, old.link_title, old.link_description, old.link_site_name, old.file_name);
    INSERT INTO items_fts(rowid, title, note, link_title, link_description, link_site_name, file_name)
        VALUES (new.id, new.title, new.note, new.link_title, new.link_description, new.link_site_name, new.file_name);
END;

-- The admin board lists every item (no visibility predicate), which can't use
-- idx_items_vis_created's leading column.
CREATE INDEX IF NOT EXISTS idx_items_created ON items(created_at DESC, id DESC);
