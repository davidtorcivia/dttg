-- Full-text search over item text. External-content FTS5 table mirroring items,
-- kept in sync by triggers; SearchItems queries it (with a LIKE fallback).
CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(
    title, note, link_title, link_description, link_site_name, file_name,
    content='items', content_rowid='id', tokenize='unicode61'
);

-- Backfill existing rows.
INSERT INTO items_fts(rowid, title, note, link_title, link_description, link_site_name, file_name)
    SELECT id, title, note, link_title, link_description, link_site_name, file_name FROM items;

-- Keep the index in sync with items.
CREATE TRIGGER IF NOT EXISTS items_fts_ai AFTER INSERT ON items BEGIN
    INSERT INTO items_fts(rowid, title, note, link_title, link_description, link_site_name, file_name)
        VALUES (new.id, new.title, new.note, new.link_title, new.link_description, new.link_site_name, new.file_name);
END;

CREATE TRIGGER IF NOT EXISTS items_fts_ad AFTER DELETE ON items BEGIN
    INSERT INTO items_fts(items_fts, rowid, title, note, link_title, link_description, link_site_name, file_name)
        VALUES ('delete', old.id, old.title, old.note, old.link_title, old.link_description, old.link_site_name, old.file_name);
END;

CREATE TRIGGER IF NOT EXISTS items_fts_au AFTER UPDATE ON items BEGIN
    INSERT INTO items_fts(items_fts, rowid, title, note, link_title, link_description, link_site_name, file_name)
        VALUES ('delete', old.id, old.title, old.note, old.link_title, old.link_description, old.link_site_name, old.file_name);
    INSERT INTO items_fts(rowid, title, note, link_title, link_description, link_site_name, file_name)
        VALUES (new.id, new.title, new.note, new.link_title, new.link_description, new.link_site_name, new.file_name);
END;
