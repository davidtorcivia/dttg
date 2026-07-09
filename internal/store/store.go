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
	"strconv"
	"strings"
	"time"
	"unicode"

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
		// SQLite ignores PRAGMA foreign_keys changes while a transaction is open,
		// so disable on the connection before Begin.
		if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			_, _ = s.db.Exec(`PRAGMA foreign_keys=ON`)
			return err
		}
		if _, err := tx.Exec(string(b)); err != nil {
			_ = tx.Rollback()
			_, _ = s.db.Exec(`PRAGMA foreign_keys=ON`)
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(name) VALUES(?)`, e.Name()); err != nil {
			_ = tx.Rollback()
			_, _ = s.db.Exec(`PRAGMA foreign_keys=ON`)
			return err
		}
		if err := tx.Commit(); err != nil {
			_, _ = s.db.Exec(`PRAGMA foreign_keys=ON`)
			return err
		}
		if _, err := s.db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
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
	SmallKey        string
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

// ---------- remote feeds / reposts ----------

type RemoteFeed struct {
	ID            int64
	FeedURL       string
	SiteURL       string
	Title         string
	Description   string
	IconURL       string
	ETag          string
	LastModified  string
	LastFetchedAt time.Time
	LastSuccessAt time.Time
	LastError     string
	Active        bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type RemoteFeedItem struct {
	ID             int64
	FeedID         int64
	FeedTitle      string
	FeedURL        string
	RemoteID       string
	URL            string
	ExternalURL    string
	Title          string
	ContentText    string
	ImageURL       string
	AttachmentURL  string
	AttachmentMime string
	AuthorName     string
	AuthorURL      string
	PublishedAt    time.Time
	FetchedAt      time.Time
	RawJSON        string
	RepostedItemID int64
}

type RemoteFeedItemFilter struct {
	Limit           int
	BeforePublished int64
	BeforeID        int64
	ActiveOnly      bool
}

type RemoteFeedUpdate struct {
	Title         string
	SiteURL       string
	Description   string
	IconURL       string
	ETag          string
	LastModified  string
	LastFetchedAt time.Time
	LastSuccessAt time.Time
	LastError     string
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
// Session rows store the SHA-256 hex of the cookie value (see web.HashSession).
// Callers must pass the hash, never the raw cookie id.

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

// PurgeExpiredSessions removes sessions past their expiry so the table doesn't
// grow without bound (run periodically).
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= unixepoch()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Ping verifies database connectivity (used by the readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

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

// APIToken is a non-secret view of a stored API token (never includes the hash).
type APIToken struct {
	ID         int64
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// ListTokens returns all API tokens ordered by creation time (newest first).
func (s *Store) ListTokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, created_at, last_used_at FROM api_tokens
		ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var created int64
		var last sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &created, &last); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0).UTC()
		if last.Valid {
			lu := time.Unix(last.Int64, 0).UTC()
			t.LastUsedAt = &lu
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken deletes a token by numeric id or exact name. Returns sql.ErrNoRows
// when nothing matched.
func (s *Store) RevokeToken(ctx context.Context, idOrName string) error {
	var res sql.Result
	var err error
	if id, perr := strconv.ParseInt(idOrName, 10, 64); perr == nil {
		res, err = s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id=?`, id)
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE name=?`, idOrName)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
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
	if slug == "" {
		return 0, errors.New("empty category slug")
	}
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

// ListTags returns every tag, most-used first (then alphabetical) so callers can
// surface the relevant ones at the top. Used by the extension's autocomplete.
func (s *Store) ListTags(ctx context.Context) ([]Tag, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.slug, t.name
		FROM tags t
		LEFT JOIN item_tags it ON it.tag_id = t.id
		GROUP BY t.id
		ORDER BY COUNT(it.item_id) DESC, t.name`)
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

// ---------- items ----------

const itemColumns = `
	i.id, i.kind, i.title, i.note, i.source_url, i.visibility,
	COALESCE(i.category_id,0), COALESCE(c.slug,''), COALESCE(c.name,''),
	i.link_title, i.link_description, i.link_site_name, i.embed_provider, i.embed_html,
	i.cover_remote_url, i.cover_key, i.thumb_key, i.small_key, i.placeholder, i.dominant_color, i.width, i.height,
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
		&it.CoverRemoteURL, &it.CoverKey, &it.ThumbKey, &it.SmallKey, &it.Placeholder, &it.DominantColor, &it.Width, &it.Height,
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
	// Keyset cursor: when both set, return items strictly older than (BeforeCreated, BeforeID).
	BeforeCreated int64
	BeforeID      int64
	// Offset is retained only for sitemap chunking; prefer keyset for board scroll.
	Offset int
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
	if f.BeforeCreated > 0 && f.BeforeID > 0 {
		where = append(where, `(i.created_at < ? OR (i.created_at = ? AND i.id < ?))`)
		args = append(args, f.BeforeCreated, f.BeforeCreated, f.BeforeID)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY i.created_at DESC, i.id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
		if f.Offset > 0 && f.BeforeCreated == 0 {
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

// itemListColumns is the board/search card projection — omits detail-only fields
// (embed_html, link_description) that inflate HTML for image-heavy boards.
const itemListColumns = `
	i.id, i.kind, i.title, i.note, i.source_url, i.visibility,
	COALESCE(i.category_id,0), COALESCE(c.slug,''), COALESCE(c.name,''),
	'' AS link_title, '' AS link_description, i.link_site_name, i.embed_provider, '' AS embed_html,
	i.cover_remote_url, i.cover_key, i.thumb_key, i.small_key, i.placeholder, i.dominant_color, i.width, i.height,
	i.created_at, i.updated_at, i.published_at,
	i.file_key, i.file_name, i.file_mime, i.file_size`

// ListItemCards is ListItems with the card projection (no embed HTML / long link text).
func (s *Store) ListItemCards(ctx context.Context, f ItemFilter) ([]Item, error) {
	q := `SELECT` + itemListColumns + `
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
	if f.BeforeCreated > 0 && f.BeforeID > 0 {
		where = append(where, `(i.created_at < ? OR (i.created_at = ? AND i.id < ?))`)
		args = append(args, f.BeforeCreated, f.BeforeCreated, f.BeforeID)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY i.created_at DESC, i.id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
		if f.Offset > 0 && f.BeforeCreated == 0 {
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

// ItemMediaKeys is a minimal projection for broken-item detection.
type ItemMediaKeys struct {
	ID       int64
	CoverKey string
	FileKey  string
}

// ListItemMediaKeys returns id/cover/file keys for all items (optionally public-only).
func (s *Store) ListItemMediaKeys(ctx context.Context, includePrivate bool) ([]ItemMediaKeys, error) {
	q := `SELECT id, cover_key, file_key FROM items`
	if !includePrivate {
		q += ` WHERE visibility='public'`
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemMediaKeys
	for rows.Next() {
		var k ItemMediaKeys
		if err := rows.Scan(&k.ID, &k.CoverKey, &k.FileKey); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ftsQuery turns a user query into a safe FTS5 MATCH expression: alphanumeric
// terms with a trailing * for prefix matching. Returns "" if there are no usable
// terms (caller falls back to LIKE).
func ftsQuery(query string) string {
	var terms []string
	for _, f := range strings.Fields(query) {
		clean := strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				return unicode.ToLower(r)
			}
			return -1
		}, f)
		if clean != "" {
			terms = append(terms, clean+"*")
		}
	}
	return strings.Join(terms, " ")
}

// SearchFilter configures SearchItems.
type SearchFilter struct {
	Query          string
	IncludePrivate bool
	Limit          int
}

// SearchItems uses the FTS5 index for item text (fast + prefix matching) and a
// LIKE for category/tag names, falling back to a full LIKE scan if FTS is
// unavailable or the query has no indexable terms.
func (s *Store) SearchItems(ctx context.Context, f SearchFilter) ([]Item, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	match := ftsQuery(f.Query)
	if match == "" {
		return s.searchItemsLike(ctx, f.Query, f.IncludePrivate, limit)
	}
	like := "%" + strings.ToLower(strings.TrimSpace(f.Query)) + "%"
	q := `SELECT` + itemColumns + `
		FROM items i
		LEFT JOIN categories c ON c.id = i.category_id
		WHERE `
	if !f.IncludePrivate {
		q += "i.visibility='public' AND "
	}
	q += `(
		i.id IN (SELECT rowid FROM items_fts WHERE items_fts MATCH ?) OR
		lower(c.name) LIKE ? OR
		i.id IN (SELECT it.item_id FROM item_tags it JOIN tags t ON t.id=it.tag_id WHERE lower(t.name) LIKE ?)
	)
	ORDER BY i.created_at DESC, i.id DESC
	LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, match, like, like, limit)
	if err != nil {
		return s.searchItemsLike(ctx, f.Query, f.IncludePrivate, limit) // FTS unavailable/malformed
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
	if err := rows.Err(); err != nil {
		return s.searchItemsLike(ctx, f.Query, f.IncludePrivate, limit)
	}
	return out, nil
}

// searchItemsLike is the case-insensitive LIKE scan across an item's text fields,
// category name, and tag names (the fallback for SearchItems).
func (s *Store) searchItemsLike(ctx context.Context, query string, includePrivate bool, limit int) ([]Item, error) {
	if limit <= 0 {
		limit = 200
	}
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
	LIMIT ?`
	args := make([]any, 8)
	for i := range args {
		args[i] = like
	}
	args = append(args, limit)
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
		 cover_remote_url,cover_key,thumb_key,small_key,placeholder,dominant_color,width,height,
		 file_key,file_name,file_mime,file_size,
		 created_at,updated_at,published_at)
		VALUES (?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?,?,?, ?,?,?,?, ?,?,?)`,
		it.Kind, it.Title, it.Note, it.SourceURL, it.Visibility, categoryID,
		it.LinkTitle, it.LinkDescription, it.LinkSiteName, it.EmbedProvider, it.EmbedHTML,
		it.CoverRemoteURL, it.CoverKey, it.ThumbKey, it.SmallKey, it.Placeholder, it.DominantColor, it.Width, it.Height,
		it.FileKey, it.FileName, it.FileMime, it.FileSize,
		created, now, published)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CreateItemWithMediaAndTags inserts an item, its media rows, and tags in one
// transaction. Invalid kind/visibility fail before any write.
func (s *Store) CreateItemWithMediaAndTags(ctx context.Context, it Item, media []Media, tags []string) (int64, error) {
	switch it.Kind {
	case "image", "link", "text", "embed", "document":
	default:
		return 0, fmt.Errorf("invalid kind %q", it.Kind)
	}
	if it.Visibility != "private" {
		it.Visibility = "public"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	created := now
	if !it.CreatedAt.IsZero() {
		created = it.CreatedAt.Unix()
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
	res, err := tx.ExecContext(ctx, `INSERT INTO items
		(kind,title,note,source_url,visibility,category_id,
		 link_title,link_description,link_site_name,embed_provider,embed_html,
		 cover_remote_url,cover_key,thumb_key,small_key,placeholder,dominant_color,width,height,
		 file_key,file_name,file_mime,file_size,
		 created_at,updated_at,published_at)
		VALUES (?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?,?,?, ?,?,?,?, ?,?,?)`,
		it.Kind, it.Title, it.Note, it.SourceURL, it.Visibility, categoryID,
		it.LinkTitle, it.LinkDescription, it.LinkSiteName, it.EmbedProvider, it.EmbedHTML,
		it.CoverRemoteURL, it.CoverKey, it.ThumbKey, it.SmallKey, it.Placeholder, it.DominantColor, it.Width, it.Height,
		it.FileKey, it.FileName, it.FileMime, it.FileSize,
		created, now, published)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, m := range media {
		if _, err := tx.ExecContext(ctx, `INSERT INTO media
			(item_id,variant,storage_key,content_type,width,height,bytes,on_local,on_r2)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			id, m.Variant, m.StorageKey, m.ContentType, m.Width, m.Height, m.Bytes,
			b2i(m.OnLocal), b2i(m.OnR2)); err != nil {
			return 0, err
		}
	}
	for _, name := range tags {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		slug := Slugify(name)
		if slug == "" {
			return 0, fmt.Errorf("invalid tag %q", name)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tags(slug,name) VALUES(?,?) ON CONFLICT(slug) DO NOTHING`, slug, name); err != nil {
			return 0, err
		}
		var tagID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE slug=?`, slug).Scan(&tagID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO item_tags(item_id,tag_id) VALUES(?,?)`, id, tagID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// ReplaceItemMedia clears old media rows, updates item media columns, and inserts
// the new media rows in one transaction.
func (s *Store) ReplaceItemMedia(ctx context.Context, itemID int64, next Item, media []Media) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM media WHERE item_id=?`, itemID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE items SET
			kind=?, cover_remote_url=?, cover_key=?, thumb_key=?, small_key=?, placeholder=?,
			dominant_color=?, width=?, height=?,
			file_key=?, file_name=?, file_mime=?, file_size=?,
			embed_provider=?, embed_html=?,
			updated_at=unixepoch()
		WHERE id=?`,
		next.Kind, next.CoverRemoteURL, next.CoverKey, next.ThumbKey, next.SmallKey, next.Placeholder,
		next.DominantColor, next.Width, next.Height,
		next.FileKey, next.FileName, next.FileMime, next.FileSize,
		next.EmbedProvider, next.EmbedHTML, itemID); err != nil {
		return err
	}
	for _, m := range media {
		if _, err := tx.ExecContext(ctx, `INSERT INTO media
			(item_id,variant,storage_key,content_type,width,height,bytes,on_local,on_r2)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			itemID, m.Variant, m.StorageKey, m.ContentType, m.Width, m.Height, m.Bytes,
			b2i(m.OnLocal), b2i(m.OnR2)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ResetContent deletes all archive content (items, media, tags, categories)
// and remote feed cache/reposts while keeping settings, API tokens, and
// followed remote feed sources. Conditional-fetch validators on kept sources
// are cleared so the next sync redownloads items instead of accepting a stale 304.
// IDs restart from 1.
func (s *Store) ResetContent(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM reposts`,
		`DELETE FROM remote_feed_items`,
		`DELETE FROM item_tags`,
		`DELETE FROM media`,
		`DELETE FROM items`,
		`DELETE FROM tags`,
		`DELETE FROM categories`,
		// Keep remote_feeds rows, but force a full re-fetch after cache wipe.
		`UPDATE remote_feeds SET
			etag='', last_modified='',
			last_fetched_at=0, last_success_at=0, last_error='',
			updated_at=unixepoch()`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	// Restart autoincrement IDs from 1 when the sequence table exists (SQLite).
	for _, name := range []string{"items", "media", "tags", "categories", "remote_feed_items", "reposts"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name=?`, name); err != nil {
			// sqlite_sequence may not exist yet on a never-inserted DB; ignore.
			if !strings.Contains(err.Error(), "no such table") {
				return err
			}
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
func (s *Store) UpdateItem(ctx context.Context, id int64, title, note, sourceURL string, categoryID int64, visibility string) error {
	if visibility != "private" {
		visibility = "public"
	}
	var cat any
	if categoryID != 0 {
		cat = categoryID
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE items
		SET title=?, note=?, source_url=?, category_id=?, visibility=?, updated_at=unixepoch(),
		    published_at=CASE WHEN ?='public' AND published_at IS NULL THEN unixepoch() ELSE published_at END
		WHERE id=?`,
		strings.TrimSpace(title), strings.TrimSpace(note), strings.TrimSpace(sourceURL), cat, visibility, visibility, id)
	return err
}

// UpdateItemMedia replaces all media-related columns of an item (used when the
// owner deletes and re-uploads the file/image without recreating the item). It
// also updates the kind, since the new file may be a different type.
func (s *Store) UpdateItemMedia(ctx context.Context, id int64, it Item) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE items SET
			kind=?, cover_remote_url=?, cover_key=?, thumb_key=?, small_key=?, placeholder=?,
			dominant_color=?, width=?, height=?,
			file_key=?, file_name=?, file_mime=?, file_size=?,
			embed_provider=?, embed_html=?,
			updated_at=unixepoch()
		WHERE id=?`,
		it.Kind, it.CoverRemoteURL, it.CoverKey, it.ThumbKey, it.SmallKey, it.Placeholder,
		it.DominantColor, it.Width, it.Height,
		it.FileKey, it.FileName, it.FileMime, it.FileSize,
		it.EmbedProvider, it.EmbedHTML, id)
	return err
}

// SetItemSmallKey sets just the small (400px) variant key (used by the backfill).
func (s *Store) SetItemSmallKey(ctx context.Context, id int64, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE items SET small_key=? WHERE id=?`, key, id)
	return err
}

// ClearMediaForItem removes an item's media rows (blobs are deleted separately
// by the caller via the media store before this runs).
func (s *Store) ClearMediaForItem(ctx context.Context, itemID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM media WHERE item_id=?`, itemID)
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

// UpsertMedia inserts or updates a media row by (item_id, variant). Used by
// idempotent maintenance jobs (e.g. backfill-variants).
func (s *Store) UpsertMedia(ctx context.Context, m Media) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO media (item_id,variant,storage_key,content_type,width,height,bytes,on_local,on_r2)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(item_id, variant) DO UPDATE SET
			storage_key=excluded.storage_key,
			content_type=excluded.content_type,
			width=excluded.width,
			height=excluded.height,
			bytes=excluded.bytes,
			on_local=excluded.on_local,
			on_r2=excluded.on_r2`,
		m.ItemID, m.Variant, m.StorageKey, m.ContentType, m.Width, m.Height, m.Bytes,
		b2i(m.OnLocal), b2i(m.OnR2))
	return err
}

// ListUnmirroredMedia returns refined variants present locally but not yet on R2
// (used by the reconcile/backfill job once R2 is configured).
func (s *Store) ListUnmirroredMedia(ctx context.Context) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id,item_id,variant,storage_key,content_type,width,height,bytes,on_local,on_r2
		FROM media
		WHERE on_r2=0 AND on_local=1 AND variant IN ('full','thumb','small')
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

// AllMedia returns every media row (used by the orphan scan to know which storage
// keys are referenced).
func (s *Store) AllMedia(ctx context.Context) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id,item_id,variant,storage_key,content_type,width,height,bytes,on_local,on_r2
		FROM media`)
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

// StrayMediaRows returns media rows whose parent item no longer exists (the FK
// cascade should prevent these, but the scan reports/cleans any that slip through).
func (s *Store) StrayMediaRows(ctx context.Context) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id,m.item_id,m.variant,m.storage_key,m.content_type,m.width,m.height,m.bytes,m.on_local,m.on_r2
		FROM media m LEFT JOIN items i ON i.id=m.item_id WHERE i.id IS NULL`)
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

// DeleteMediaRow removes a single media row by id (used to clear stray rows).
func (s *Store) DeleteMediaRow(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM media WHERE id=?`, id)
	return err
}

// MediaByKey looks up a media row by storage_key and joins its parent item for
// visibility checks. Returns sql.ErrNoRows when the key is unknown.
func (s *Store) MediaByKey(ctx context.Context, key string) (Media, Item, error) {
	var m Media
	var it Item
	var onLocal, onR2 int
	err := s.db.QueryRowContext(ctx, `
		SELECT m.id, m.item_id, m.variant, m.storage_key, m.content_type, m.width, m.height, m.bytes,
		       m.on_local, m.on_r2, i.id, i.visibility
		FROM media m
		JOIN items i ON i.id = m.item_id
		WHERE m.storage_key = ?
		LIMIT 1`, key).Scan(
		&m.ID, &m.ItemID, &m.Variant, &m.StorageKey, &m.ContentType, &m.Width, &m.Height, &m.Bytes,
		&onLocal, &onR2, &it.ID, &it.Visibility)
	if err != nil {
		return Media{}, Item{}, err
	}
	m.OnLocal, m.OnR2 = onLocal == 1, onR2 == 1
	return m, it, nil
}

// ListPrivateMediaOnR2 returns private-item media rows still present on R2
// (used by localize-private-media).
func (s *Store) ListPrivateMediaOnR2(ctx context.Context) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.item_id, m.variant, m.storage_key, m.content_type, m.width, m.height, m.bytes, m.on_local, m.on_r2
		FROM media m
		JOIN items i ON i.id = m.item_id
		WHERE i.visibility = 'private' AND m.on_r2 = 1
		ORDER BY m.id`)
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

// MarkMediaLocalOnly sets on_local=1, on_r2=0 after a successful private localize.
func (s *Store) MarkMediaLocalOnly(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media SET on_local=1, on_r2=0 WHERE id=?`, id)
	return err
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

// PublicRevision returns COUNT(*) and MAX(updated_at) for public items, used as
// a cheap ETag/conditional-GET fingerprint for feeds and sitemaps.
func (s *Store) PublicRevision(ctx context.Context) (count int, maxUpdated int64, err error) {
	var max sql.NullInt64
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(updated_at) FROM items WHERE visibility='public'`).
		Scan(&count, &max)
	if max.Valid {
		maxUpdated = max.Int64
	}
	return count, maxUpdated, err
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

// Slugify lowercases ASCII and hyphenates a string for use in URLs, retaining
// Unicode letters and numbers so CJK/etc. categories get non-empty slugs.
func Slugify(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			prevDash = false
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '-' || r == '_' || r == '/' || r == '.' || unicode.IsSpace(r) || unicode.IsPunct(r):
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// ---------- pending shares (PWA share-across-login) ----------

// PendingShare is a short-lived PWA share payload held until the admin logs in.
type PendingShare struct {
	ID        string
	CreatedAt time.Time
	ExpiresAt time.Time
	Title     string
	Text      string
	URL       string
	FileKey   string
	FileName  string
	FileMime  string
	FileSize  int64
}

func (s *Store) CreatePendingShare(ctx context.Context, p PendingShare) error {
	if p.ID == "" {
		return errors.New("pending share id required")
	}
	exp := p.ExpiresAt.Unix()
	if exp == 0 {
		exp = time.Now().Add(30 * time.Minute).Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pending_shares
		(id, created_at, expires_at, title, text, url, file_key, file_name, file_mime, file_size)
		VALUES (?, unixepoch(), ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, exp, p.Title, p.Text, p.URL, p.FileKey, p.FileName, p.FileMime, p.FileSize)
	return err
}

// TakePendingShare atomically deletes and returns a non-expired pending share.
func (s *Store) TakePendingShare(ctx context.Context, id string) (*PendingShare, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var p PendingShare
	var created, expires int64
	err = tx.QueryRowContext(ctx, `
		SELECT id, created_at, expires_at, title, text, url, file_key, file_name, file_mime, file_size
		FROM pending_shares WHERE id=? AND expires_at > unixepoch()`, id).Scan(
		&p.ID, &created, &expires, &p.Title, &p.Text, &p.URL, &p.FileKey, &p.FileName, &p.FileMime, &p.FileSize)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM pending_shares WHERE id=? AND expires_at > unixepoch()`, id)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil // lost race
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	p.ExpiresAt = time.Unix(expires, 0).UTC()
	return &p, nil
}

func (s *Store) DeletePendingShare(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_shares WHERE id=?`, id)
	return err
}

// ListExpiredPendingShares returns expired pending rows (for blob cleanup).
func (s *Store) ListExpiredPendingShares(ctx context.Context) ([]PendingShare, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, expires_at, title, text, url, file_key, file_name, file_mime, file_size
		FROM pending_shares WHERE expires_at <= unixepoch()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingShare
	for rows.Next() {
		var p PendingShare
		var created, expires int64
		if err := rows.Scan(&p.ID, &created, &expires, &p.Title, &p.Text, &p.URL, &p.FileKey, &p.FileName, &p.FileMime, &p.FileSize); err != nil {
			return nil, err
		}
		p.CreatedAt = time.Unix(created, 0).UTC()
		p.ExpiresAt = time.Unix(expires, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// PurgeExpiredPendingShares drops expired pending share rows.
func (s *Store) PurgeExpiredPendingShares(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pending_shares WHERE expires_at <= unixepoch()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- remote feeds ----------

func scanRemoteFeed(sc rowScanner) (RemoteFeed, error) {
	var f RemoteFeed
	var lastFetched, lastSuccess, created, updated int64
	var active int
	err := sc.Scan(
		&f.ID, &f.FeedURL, &f.SiteURL, &f.Title, &f.Description, &f.IconURL,
		&f.ETag, &f.LastModified, &lastFetched, &lastSuccess, &f.LastError,
		&active, &created, &updated,
	)
	if err != nil {
		return f, err
	}
	f.Active = active != 0
	if lastFetched > 0 {
		f.LastFetchedAt = time.Unix(lastFetched, 0).UTC()
	}
	if lastSuccess > 0 {
		f.LastSuccessAt = time.Unix(lastSuccess, 0).UTC()
	}
	f.CreatedAt = time.Unix(created, 0).UTC()
	f.UpdatedAt = time.Unix(updated, 0).UTC()
	return f, nil
}

const remoteFeedColumns = `
	id, feed_url, site_url, title, description, icon_url,
	etag, last_modified, last_fetched_at, last_success_at, last_error,
	active, created_at, updated_at`

// AddRemoteFeed inserts a new active feed, reactivates an inactive one, or
// returns an existing active feed. created=true only on a fresh insert.
func (s *Store) AddRemoteFeed(ctx context.Context, feedURL string) (*RemoteFeed, bool, error) {
	feedURL = strings.TrimSpace(feedURL)
	if feedURL == "" {
		return nil, false, fmt.Errorf("feed URL is required")
	}
	// Existing row?
	var existing RemoteFeed
	var lastFetched, lastSuccess, created, updated int64
	var active int
	err := s.db.QueryRowContext(ctx, `
		SELECT`+remoteFeedColumns+` FROM remote_feeds WHERE feed_url=?`, feedURL).Scan(
		&existing.ID, &existing.FeedURL, &existing.SiteURL, &existing.Title, &existing.Description, &existing.IconURL,
		&existing.ETag, &existing.LastModified, &lastFetched, &lastSuccess, &existing.LastError,
		&active, &created, &updated,
	)
	if err == nil {
		existing.Active = active != 0
		if lastFetched > 0 {
			existing.LastFetchedAt = time.Unix(lastFetched, 0).UTC()
		}
		if lastSuccess > 0 {
			existing.LastSuccessAt = time.Unix(lastSuccess, 0).UTC()
		}
		existing.CreatedAt = time.Unix(created, 0).UTC()
		existing.UpdatedAt = time.Unix(updated, 0).UTC()
		if existing.Active {
			return &existing, false, nil
		}
		// Reactivate inactive source.
		if _, err := s.db.ExecContext(ctx, `
			UPDATE remote_feeds SET active=1, updated_at=unixepoch() WHERE id=?`, existing.ID); err != nil {
			return nil, false, err
		}
		existing.Active = true
		existing.UpdatedAt = time.Now().UTC()
		return &existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO remote_feeds(feed_url, active) VALUES(?, 1)`, feedURL)
	if err != nil {
		return nil, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, false, err
	}
	f, err := s.GetRemoteFeed(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return f, true, nil
}

// ListRemoteFeeds returns followed sources; active feeds first, then title, URL.
func (s *Store) ListRemoteFeeds(ctx context.Context, activeOnly bool) ([]RemoteFeed, error) {
	q := `SELECT` + remoteFeedColumns + ` FROM remote_feeds`
	if activeOnly {
		q += ` WHERE active=1`
	}
	q += ` ORDER BY active DESC, title COLLATE NOCASE, feed_url`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RemoteFeed
	for rows.Next() {
		f, err := scanRemoteFeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetRemoteFeed returns a source by id, or (nil, nil) when missing.
func (s *Store) GetRemoteFeed(ctx context.Context, id int64) (*RemoteFeed, error) {
	f, err := scanRemoteFeed(s.db.QueryRowContext(ctx, `
		SELECT`+remoteFeedColumns+` FROM remote_feeds WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// SetRemoteFeedActive toggles follow state. Missing id returns sql.ErrNoRows.
func (s *Store) SetRemoteFeedActive(ctx context.Context, id int64, active bool) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE remote_feeds SET active=?, updated_at=unixepoch() WHERE id=?`, b2i(active), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SaveRemoteFeedFetch updates feed metadata and upserts remote items in one tx.
// Returns the number of input items processed.
func (s *Store) SaveRemoteFeedFetch(ctx context.Context, feedID int64, upd RemoteFeedUpdate, items []RemoteFeedItem) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	fetched := time.Now().UTC().Unix()
	if !upd.LastFetchedAt.IsZero() {
		fetched = upd.LastFetchedAt.Unix()
	}
	success := fetched
	if !upd.LastSuccessAt.IsZero() {
		success = upd.LastSuccessAt.Unix()
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE remote_feeds SET
			title=?, site_url=?, description=?, icon_url=?,
			etag=?, last_modified=?,
			last_fetched_at=?, last_success_at=?, last_error=?,
			updated_at=unixepoch()
		WHERE id=?`,
		upd.Title, upd.SiteURL, upd.Description, upd.IconURL,
		upd.ETag, upd.LastModified,
		fetched, success, upd.LastError,
		feedID); err != nil {
		return 0, err
	}

	for i := range items {
		it := &items[i]
		pub := fetched
		if !it.PublishedAt.IsZero() {
			pub = it.PublishedAt.Unix()
		}
		fetchedAt := fetched
		if !it.FetchedAt.IsZero() {
			fetchedAt = it.FetchedAt.Unix()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO remote_feed_items(
				feed_id, remote_id, url, external_url, title, content_text,
				image_url, attachment_url, attachment_mime,
				author_name, author_url, published_at, fetched_at, raw_json)
			VALUES (?,?,?,?,?,?, ?,?,?, ?,?,?,?,?)
			ON CONFLICT(feed_id, remote_id) DO UPDATE SET
				url=excluded.url,
				external_url=excluded.external_url,
				title=excluded.title,
				content_text=excluded.content_text,
				image_url=excluded.image_url,
				attachment_url=excluded.attachment_url,
				attachment_mime=excluded.attachment_mime,
				author_name=excluded.author_name,
				author_url=excluded.author_url,
				published_at=excluded.published_at,
				fetched_at=excluded.fetched_at,
				raw_json=excluded.raw_json`,
			feedID, it.RemoteID, it.URL, it.ExternalURL, it.Title, it.ContentText,
			it.ImageURL, it.AttachmentURL, it.AttachmentMime,
			it.AuthorName, it.AuthorURL, pub, fetchedAt, it.RawJSON); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(items), nil
}

// SaveRemoteFeedError records a fetch failure without clearing cached items.
func (s *Store) SaveRemoteFeedError(ctx context.Context, feedID int64, fetchedAt time.Time, msg string) error {
	ts := time.Now().UTC().Unix()
	if !fetchedAt.IsZero() {
		ts = fetchedAt.Unix()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE remote_feeds SET last_fetched_at=?, last_error=?, updated_at=unixepoch()
		WHERE id=?`, ts, msg, feedID)
	return err
}

// MarkRemoteFeedChecked records a successful conditional GET (HTTP 304).
func (s *Store) MarkRemoteFeedChecked(ctx context.Context, feedID int64, fetchedAt time.Time) error {
	ts := time.Now().UTC().Unix()
	if !fetchedAt.IsZero() {
		ts = fetchedAt.Unix()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE remote_feeds SET last_fetched_at=?, last_error='', updated_at=unixepoch()
		WHERE id=?`, ts, feedID)
	return err
}

// ListRemoteFeedItems returns cached remote items newest-first with optional keyset.
func (s *Store) ListRemoteFeedItems(ctx context.Context, f RemoteFeedItemFilter) ([]RemoteFeedItem, error) {
	q := `
		SELECT ri.id, ri.feed_id, COALESCE(rf.title,''), rf.feed_url, ri.remote_id,
			ri.url, ri.external_url, ri.title, ri.content_text, ri.image_url,
			ri.attachment_url, ri.attachment_mime, ri.author_name, ri.author_url,
			ri.published_at, ri.fetched_at, ri.raw_json,
			COALESCE(rp.local_item_id, 0)
		FROM remote_feed_items ri
		JOIN remote_feeds rf ON rf.id = ri.feed_id
		LEFT JOIN reposts rp ON rp.remote_feed_item_id = ri.id`
	var where []string
	var args []any
	if f.ActiveOnly {
		where = append(where, "rf.active=1")
	}
	if f.BeforePublished > 0 && f.BeforeID > 0 {
		where = append(where, `(ri.published_at < ? OR (ri.published_at = ? AND ri.id < ?))`)
		args = append(args, f.BeforePublished, f.BeforePublished, f.BeforeID)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY ri.published_at DESC, ri.id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RemoteFeedItem
	for rows.Next() {
		var it RemoteFeedItem
		var published, fetched int64
		if err := rows.Scan(
			&it.ID, &it.FeedID, &it.FeedTitle, &it.FeedURL, &it.RemoteID,
			&it.URL, &it.ExternalURL, &it.Title, &it.ContentText, &it.ImageURL,
			&it.AttachmentURL, &it.AttachmentMime, &it.AuthorName, &it.AuthorURL,
			&published, &fetched, &it.RawJSON, &it.RepostedItemID,
		); err != nil {
			return nil, err
		}
		it.PublishedAt = time.Unix(published, 0).UTC()
		it.FetchedAt = time.Unix(fetched, 0).UTC()
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetRemoteFeedItem returns one cached item with repost mapping, or (nil, nil).
func (s *Store) GetRemoteFeedItem(ctx context.Context, id int64) (*RemoteFeedItem, error) {
	var it RemoteFeedItem
	var published, fetched int64
	err := s.db.QueryRowContext(ctx, `
		SELECT ri.id, ri.feed_id, COALESCE(rf.title,''), rf.feed_url, ri.remote_id,
			ri.url, ri.external_url, ri.title, ri.content_text, ri.image_url,
			ri.attachment_url, ri.attachment_mime, ri.author_name, ri.author_url,
			ri.published_at, ri.fetched_at, ri.raw_json,
			COALESCE(rp.local_item_id, 0)
		FROM remote_feed_items ri
		JOIN remote_feeds rf ON rf.id = ri.feed_id
		LEFT JOIN reposts rp ON rp.remote_feed_item_id = ri.id
		WHERE ri.id=?`, id).Scan(
		&it.ID, &it.FeedID, &it.FeedTitle, &it.FeedURL, &it.RemoteID,
		&it.URL, &it.ExternalURL, &it.Title, &it.ContentText, &it.ImageURL,
		&it.AttachmentURL, &it.AttachmentMime, &it.AuthorName, &it.AuthorURL,
		&published, &fetched, &it.RawJSON, &it.RepostedItemID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	it.PublishedAt = time.Unix(published, 0).UTC()
	it.FetchedAt = time.Unix(fetched, 0).UTC()
	return &it, nil
}

// CreateRepost creates a public local link item from a remote feed item.
// Idempotent: existing reposts return the same local_item_id with created=false.
func (s *Store) CreateRepost(ctx context.Context, remoteItemID int64) (localItemID int64, created bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	// Existing repost?
	var existingLocal sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT local_item_id FROM reposts WHERE remote_feed_item_id=?`, remoteItemID).Scan(&existingLocal)
	if err == nil {
		if existingLocal.Valid {
			return existingLocal.Int64, false, nil
		}
		// Row exists but local item was deleted (ON DELETE SET NULL) — fall through to recreate.
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}

	var remote RemoteFeedItem
	var published, fetched int64
	err = tx.QueryRowContext(ctx, `
		SELECT ri.id, ri.feed_id, COALESCE(rf.title,''), rf.feed_url, ri.remote_id,
			ri.url, ri.external_url, ri.title, ri.content_text, ri.image_url,
			ri.attachment_url, ri.attachment_mime, ri.author_name, ri.author_url,
			ri.published_at, ri.fetched_at, ri.raw_json
		FROM remote_feed_items ri
		JOIN remote_feeds rf ON rf.id = ri.feed_id
		WHERE ri.id=?`, remoteItemID).Scan(
		&remote.ID, &remote.FeedID, &remote.FeedTitle, &remote.FeedURL, &remote.RemoteID,
		&remote.URL, &remote.ExternalURL, &remote.Title, &remote.ContentText, &remote.ImageURL,
		&remote.AttachmentURL, &remote.AttachmentMime, &remote.AuthorName, &remote.AuthorURL,
		&published, &fetched, &remote.RawJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("remote item not found")
	}
	if err != nil {
		return 0, false, err
	}
	remote.PublishedAt = time.Unix(published, 0).UTC()
	remote.FetchedAt = time.Unix(fetched, 0).UTC()

	sourceURL := chooseRepostSourceURL(remote)
	if sourceURL == "" {
		return 0, false, fmt.Errorf("remote item has no URL to repost")
	}

	now := time.Now().UTC()
	createdUnix := now.Unix()
	res, err := tx.ExecContext(ctx, `INSERT INTO items
		(kind,title,note,source_url,visibility,category_id,
		 link_title,link_description,link_site_name,embed_provider,embed_html,
		 cover_remote_url,cover_key,thumb_key,small_key,placeholder,dominant_color,width,height,
		 file_key,file_name,file_mime,file_size,
		 created_at,updated_at,published_at)
		VALUES (?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?,?,?, ?,?,?,?, ?,?,?)`,
		"link", remote.Title, remote.ContentText, sourceURL, "public", nil,
		remote.Title, remote.ContentText, remote.FeedTitle, "", "",
		remote.ImageURL, "", "", "", "", "", 0, 0,
		"", "", "", 0,
		createdUnix, createdUnix, createdUnix)
	if err != nil {
		return 0, false, err
	}
	localID, err := res.LastInsertId()
	if err != nil {
		return 0, false, err
	}

	// Upsert repost mapping (handles re-repost after local item deletion).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO reposts(remote_feed_item_id, local_item_id, created_at)
		VALUES (?,?,?)
		ON CONFLICT(remote_feed_item_id) DO UPDATE SET local_item_id=excluded.local_item_id`,
		remoteItemID, localID, createdUnix); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return localID, true, nil
}

func chooseRepostSourceURL(remote RemoteFeedItem) string {
	for _, candidate := range []string{remote.URL, remote.ExternalURL} {
		if isAbsoluteHTTPURL(candidate) {
			return candidate
		}
	}
	if isAbsoluteHTTPURL(remote.RemoteID) {
		return remote.RemoteID
	}
	return ""
}

func isAbsoluteHTTPURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}
