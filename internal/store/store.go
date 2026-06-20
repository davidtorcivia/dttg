// Package store is the SQLite data layer (pure-Go modernc driver). All
// timestamps are unix seconds. Queries COALESCE nullable columns so models use
// plain Go types and templates stay simple.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct{ db *sql.DB }

// Open connects to the SQLite database, applies pragmas, and runs migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Single-user app: serialize access to dodge writer-lock contention entirely.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// BackupTo writes a consistent snapshot of the database to path via VACUUM INTO.
// path is server-controlled (single quotes are escaped defensively).
func (s *Store) BackupTo(ctx context.Context, path string) error {
	esc := strings.ReplaceAll(path, "'", "''")
	_, err := s.db.ExecContext(ctx, "VACUUM INTO '"+esc+"'")
	return err
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL DEFAULT (unixepoch()))`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	// Disable FK enforcement during migrations so safe table rebuilds (DROP +
	// recreate) don't fire cascade deletes on child tables. Restore afterwards.
	if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() { _, _ = s.db.Exec(`PRAGMA foreign_keys=ON`) }()

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name=?`, e.Name()).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		b, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(string(b)); err != nil {
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if _, err := s.db.Exec(`INSERT INTO schema_migrations(name) VALUES(?)`, e.Name()); err != nil {
			return err
		}
	}
	return nil
}

// ---------- models ----------

type Category struct {
	ID          int64
	Slug        string
	Name        string
	Description string
	Position    int
	Count       int
}

type Tag struct {
	ID   int64
	Slug string
	Name string
}

type Item struct {
	ID              int64
	Kind            string // image | link | text | embed
	Title           string
	Note            string
	SourceURL       string
	Visibility      string // public | private
	CategoryID      int64
	CategorySlug    string
	CategoryName    string
	LinkTitle       string
	LinkDescription string
	LinkSiteName    string
	EmbedProvider   string
	EmbedHTML       string
	CoverRemoteURL  string
	CoverKey        string
	ThumbKey        string
	Placeholder     string
	FileKey         string // document/file blob key
	FileName        string
	FileMime        string
	FileSize        int64
	DominantColor   string
	Width           int
	Height          int
	CreatedAt       time.Time
	UpdatedAt       time.Time
	PublishedAt     *time.Time
	Tags            []Tag
}

// ---------- settings ----------

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, val string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, val)
	return err
}

// ---------- sessions ----------

func (s *Store) CreateSession(ctx context.Context, id string, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(id, expires_at) VALUES(?, unixepoch()+?)`, id, int64(ttl.Seconds()))
	return err
}

func (s *Store) SessionValid(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE id=? AND expires_at > unixepoch()`, id).Scan(&n)
	return n > 0, err
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
	return err
}

// ---------- api tokens ----------

func (s *Store) CreateToken(ctx context.Context, name, hash string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens(name, token_hash) VALUES(?,?)`, name, hash)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// TokenValid reports whether the hash matches a stored token and stamps last use.
func (s *Store) TokenValid(ctx context.Context, hash string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at=unixepoch() WHERE token_hash=?`, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---------- categories ----------

func (s *Store) ListCategories(ctx context.Context, includePrivate bool) ([]Category, error) {
	all := 0
	if includePrivate {
		all = 1
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.slug, c.name, c.description, c.position,
		       (SELECT COUNT(*) FROM items i
		        WHERE i.category_id=c.id AND (? = 1 OR i.visibility='public')) AS cnt
		FROM categories c
		ORDER BY c.position, c.name`, all)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.Description, &c.Position, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetOrCreateCategory(ctx context.Context, name string) (int64, error) {
	slug := Slugify(name)
	// Idempotent + concurrency-safe (parallel ingest may request the same slug).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO categories(slug,name) VALUES(?,?) ON CONFLICT(slug) DO NOTHING`, slug, name); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM categories WHERE slug=?`, slug).Scan(&id)
	return id, err
}

// ---------- tags ----------

func (s *Store) GetOrCreateTag(ctx context.Context, name string) (int64, error) {
	slug := Slugify(name)
	if slug == "" {
		return 0, errors.New("empty tag")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tags(slug,name) VALUES(?,?) ON CONFLICT(slug) DO NOTHING`, slug, name); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM tags WHERE slug=?`, slug).Scan(&id)
	return id, err
}

func (s *Store) AttachTag(ctx context.Context, itemID, tagID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO item_tags(item_id, tag_id) VALUES(?,?)`, itemID, tagID)
	return err
}

// ---------- items ----------

const itemColumns = `
	i.id, i.kind, i.title, i.note, i.source_url, i.visibility,
	COALESCE(i.category_id,0), COALESCE(c.slug,''), COALESCE(c.name,''),
	i.link_title, i.link_description, i.link_site_name, i.embed_provider, i.embed_html,
	i.cover_remote_url, i.cover_key, i.thumb_key, i.placeholder, i.dominant_color, i.width, i.height,
	i.created_at, i.updated_at, i.published_at,
	i.file_key, i.file_name, i.file_mime, i.file_size`

type rowScanner interface{ Scan(dest ...any) error }

func scanItem(sc rowScanner) (Item, error) {
	var it Item
	var createdAt, updatedAt int64
	var published sql.NullInt64
	err := sc.Scan(&it.ID, &it.Kind, &it.Title, &it.Note, &it.SourceURL, &it.Visibility,
		&it.CategoryID, &it.CategorySlug, &it.CategoryName,
		&it.LinkTitle, &it.LinkDescription, &it.LinkSiteName, &it.EmbedProvider, &it.EmbedHTML,
		&it.CoverRemoteURL, &it.CoverKey, &it.ThumbKey, &it.Placeholder, &it.DominantColor, &it.Width, &it.Height,
		&createdAt, &updatedAt, &published,
		&it.FileKey, &it.FileName, &it.FileMime, &it.FileSize)
	if err != nil {
		return it, err
	}
	it.CreatedAt = time.Unix(createdAt, 0).UTC()
	it.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	if published.Valid {
		t := time.Unix(published.Int64, 0).UTC()
		it.PublishedAt = &t
	}
	return it, nil
}

type ItemFilter struct {
	IncludePrivate bool
	CategorySlug   string
	TagSlug        string
	Limit          int
	Offset         int
}

func (s *Store) ListItems(ctx context.Context, f ItemFilter) ([]Item, error) {
	q := `SELECT` + itemColumns + `
		FROM items i
		LEFT JOIN categories c ON c.id = i.category_id`
	var where []string
	var args []any
	if !f.IncludePrivate {
		where = append(where, "i.visibility='public'")
	}
	if f.CategorySlug != "" {
		where = append(where, "c.slug = ?")
		args = append(args, f.CategorySlug)
	}
	if f.TagSlug != "" {
		where = append(where, `i.id IN (
			SELECT it.item_id FROM item_tags it
			JOIN tags t ON t.id = it.tag_id WHERE t.slug = ?)`)
		args = append(args, f.TagSlug)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY i.created_at DESC, i.id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
		if f.Offset > 0 {
			q += " OFFSET ?"
			args = append(args, f.Offset)
		}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SearchItems does a simple case-insensitive LIKE search across an item's text
// fields, category name, and tag names.
func (s *Store) SearchItems(ctx context.Context, query string, includePrivate bool) ([]Item, error) {
	like := "%" + strings.ToLower(strings.TrimSpace(query)) + "%"
	q := `SELECT` + itemColumns + `
		FROM items i
		LEFT JOIN categories c ON c.id = i.category_id
		WHERE `
	if !includePrivate {
		q += "i.visibility='public' AND "
	}
	q += `(
		lower(i.title) LIKE ? OR lower(i.note) LIKE ? OR
		lower(i.link_title) LIKE ? OR lower(i.link_description) LIKE ? OR
		lower(i.link_site_name) LIKE ? OR lower(i.file_name) LIKE ? OR
		lower(c.name) LIKE ? OR
		i.id IN (SELECT it.item_id FROM item_tags it JOIN tags t ON t.id=it.tag_id WHERE lower(t.name) LIKE ?)
	)
	ORDER BY i.created_at DESC, i.id DESC
	LIMIT 200`
	args := make([]any, 8)
	for i := range args {
		args[i] = like
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetItem returns a single item with its tags. Returns (nil, nil) when not found
// or when the item is private and includePrivate is false.
func (s *Store) GetItem(ctx context.Context, id int64, includePrivate bool) (*Item, error) {
	q := `SELECT` + itemColumns + `
		FROM items i
		LEFT JOIN categories c ON c.id = i.category_id
		WHERE i.id = ?`
	if !includePrivate {
		q += " AND i.visibility='public'"
	}
	it, err := scanItem(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tags, err := s.itemTags(ctx, it.ID)
	if err != nil {
		return nil, err
	}
	it.Tags = tags
	return &it, nil
}

func (s *Store) itemTags(ctx context.Context, itemID int64) ([]Tag, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.slug, t.name FROM tags t
		JOIN item_tags it ON it.tag_id = t.id
		WHERE it.item_id = ? ORDER BY t.name`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Slug, &t.Name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateItem inserts an item. Zero CreatedAt means "now"; public items get a
// published_at stamp automatically.
func (s *Store) CreateItem(ctx context.Context, it Item) (int64, error) {
	now := time.Now().Unix()
	created := now
	if !it.CreatedAt.IsZero() {
		created = it.CreatedAt.Unix()
	}
	if it.Visibility == "" {
		it.Visibility = "public"
	}
	var categoryID any
	if it.CategoryID != 0 {
		categoryID = it.CategoryID
	}
	var published any
	switch {
	case it.PublishedAt != nil:
		published = it.PublishedAt.Unix()
	case it.Visibility == "public":
		published = created
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO items
		(kind,title,note,source_url,visibility,category_id,
		 link_title,link_description,link_site_name,embed_provider,embed_html,
		 cover_remote_url,cover_key,thumb_key,placeholder,dominant_color,width,height,
		 file_key,file_name,file_mime,file_size,
		 created_at,updated_at,published_at)
		VALUES (?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?,?, ?,?,?)`,
		it.Kind, it.Title, it.Note, it.SourceURL, it.Visibility, categoryID,
		it.LinkTitle, it.LinkDescription, it.LinkSiteName, it.EmbedProvider, it.EmbedHTML,
		it.CoverRemoteURL, it.CoverKey, it.ThumbKey, it.Placeholder, it.DominantColor, it.Width, it.Height,
		it.FileKey, it.FileName, it.FileMime, it.FileSize,
		created, now, published)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ResetContent deletes all archive content (items, media, tags, categories)
// while keeping settings and API tokens. IDs restart from 1.
func (s *Store) ResetContent(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM item_tags`,
		`DELETE FROM media`,
		`DELETE FROM items`,
		`DELETE FROM tags`,
		`DELETE FROM categories`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CountItems(ctx context.Context, includePrivate bool) (int, error) {
	q := `SELECT COUNT(*) FROM items`
	if !includePrivate {
		q += ` WHERE visibility='public'`
	}
	var n int
	err := s.db.QueryRowContext(ctx, q).Scan(&n)
	return n, err
}

// UpdateItem updates the editable fields of an item (admin edit).
func (s *Store) UpdateItem(ctx context.Context, id int64, title, note string, categoryID int64, visibility string) error {
	if visibility != "private" {
		visibility = "public"
	}
	var cat any
	if categoryID != 0 {
		cat = categoryID
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE items
		SET title=?, note=?, category_id=?, visibility=?, updated_at=unixepoch(),
		    published_at=CASE WHEN ?='public' AND published_at IS NULL THEN unixepoch() ELSE published_at END
		WHERE id=?`,
		strings.TrimSpace(title), strings.TrimSpace(note), cat, visibility, visibility, id)
	return err
}

// SetItemTags replaces an item's tags with the given names.
func (s *Store) SetItemTags(ctx context.Context, itemID int64, names []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM item_tags WHERE item_id=?`, itemID); err != nil {
		return err
	}
	for _, name := range names {
		slug := Slugify(name)
		if slug == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tags(slug,name) VALUES(?,?) ON CONFLICT(slug) DO NOTHING`, slug, name); err != nil {
			return err
		}
		var tagID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE slug=?`, slug).Scan(&tagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO item_tags(item_id,tag_id) VALUES(?,?)`, itemID, tagID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteItem removes an item; media rows and tag links cascade.
func (s *Store) DeleteItem(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM items WHERE id=?`, id)
	return err
}

// ---------- media variants ----------

type Media struct {
	ID          int64
	ItemID      int64
	Variant     string // original | full | thumb
	StorageKey  string
	ContentType string
	Width       int
	Height      int
	Bytes       int64
	OnLocal     bool
	OnR2        bool
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) AddMedia(ctx context.Context, m Media) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO media
		(item_id,variant,storage_key,content_type,width,height,bytes,on_local,on_r2)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		m.ItemID, m.Variant, m.StorageKey, m.ContentType, m.Width, m.Height, m.Bytes,
		b2i(m.OnLocal), b2i(m.OnR2))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListUnmirroredMedia returns refined variants present locally but not yet on R2
// (used by the reconcile/backfill job once R2 is configured).
func (s *Store) ListUnmirroredMedia(ctx context.Context) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id,item_id,variant,storage_key,content_type,width,height,bytes,on_local,on_r2
		FROM media
		WHERE on_r2=0 AND on_local=1 AND variant IN ('full','thumb')
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Media
	for rows.Next() {
		var m Media
		var onLocal, onR2 int
		if err := rows.Scan(&m.ID, &m.ItemID, &m.Variant, &m.StorageKey, &m.ContentType,
			&m.Width, &m.Height, &m.Bytes, &onLocal, &onR2); err != nil {
			return nil, err
		}
		m.OnLocal, m.OnR2 = onLocal == 1, onR2 == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) MarkMediaMirrored(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media SET on_r2=1 WHERE id=?`, id)
	return err
}

// ListMediaForItem returns all media rows for an item (used to delete blobs).
func (s *Store) ListMediaForItem(ctx context.Context, itemID int64) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id,item_id,variant,storage_key,content_type,width,height,bytes,on_local,on_r2
		FROM media WHERE item_id=?`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Media
	for rows.Next() {
		var m Media
		var onLocal, onR2 int
		if err := rows.Scan(&m.ID, &m.ItemID, &m.Variant, &m.StorageKey, &m.ContentType,
			&m.Width, &m.Height, &m.Bytes, &onLocal, &onR2); err != nil {
			return nil, err
		}
		m.OnLocal, m.OnR2 = onLocal == 1, onR2 == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetAdjacent returns the ids of the newer (prev) and older (next) items
// relative to it within the board ordering (created_at DESC, id DESC). Returns 0
// for either end. Respects visibility unless includePrivate.
func (s *Store) GetAdjacent(ctx context.Context, it Item, includePrivate bool) (prevID, nextID int64, err error) {
	vis := ""
	if !includePrivate {
		vis = " AND visibility='public'"
	}
	created := it.CreatedAt.Unix()

	// prev = newer (appears before in DESC order)
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM items WHERE (created_at > ? OR (created_at = ? AND id > ?))`+vis+
			` ORDER BY created_at ASC, id ASC LIMIT 1`, created, created, it.ID).Scan(&prevID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, err
	}

	// next = older (appears after in DESC order)
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM items WHERE (created_at < ? OR (created_at = ? AND id < ?))`+vis+
			` ORDER BY created_at DESC, id DESC LIMIT 1`, created, created, it.ID).Scan(&nextID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return prevID, 0, err
	}
	return prevID, nextID, nil
}

// GetRelated returns items that share a category or any tag with it (excluding
// it), newest first.
func (s *Store) GetRelated(ctx context.Context, it Item, limit int, includePrivate bool) ([]Item, error) {
	vis := ""
	if !includePrivate {
		vis = " AND i.visibility='public'"
	}
	q := `SELECT DISTINCT` + itemColumns + `
		FROM items i
		LEFT JOIN categories c ON c.id = i.category_id
		WHERE i.id != ?` + vis + ` AND (
			(? != 0 AND i.category_id = ?)
			OR i.id IN (SELECT item_id FROM item_tags WHERE tag_id IN
				(SELECT tag_id FROM item_tags WHERE item_id = ?))
		)
		ORDER BY i.created_at DESC, i.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, it.ID, it.CategoryID, it.CategoryID, it.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		v, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Stats summarizes the public archive (for the colophon / easter eggs).
type Stats struct {
	Count  int
	Oldest time.Time
	Newest time.Time
}

func (s *Store) PublicStats(ctx context.Context) (Stats, error) {
	var st Stats
	var oldest, newest sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(created_at), MAX(created_at) FROM items WHERE visibility='public'`).
		Scan(&st.Count, &oldest, &newest)
	if oldest.Valid {
		st.Oldest = time.Unix(oldest.Int64, 0).UTC()
	}
	if newest.Valid {
		st.Newest = time.Unix(newest.Int64, 0).UTC()
	}
	return st, err
}

// Slugify lowercases and hyphenates a string for use in URLs.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '-' || r == '_' || r == '/' || r == '.':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
