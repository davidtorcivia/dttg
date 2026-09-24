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
	// Every helper must therefore drain/close its rows before issuing another query.
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

// Ping verifies database connectivity (used by the readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// BackupTo writes a consistent snapshot of the database to path via VACUUM INTO.
// path is server-controlled (single quotes are escaped defensively).
func (s *Store) BackupTo(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(path, "'", "''")+"'")
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
		if err := s.applyMigration(e.Name()); err != nil {
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
	}
	return nil
}

func (s *Store) applyMigration(name string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name=?`, name).Scan(&n); err != nil || n > 0 {
		return err
	}
	b, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		return err
	}
	// SQLite ignores PRAGMA foreign_keys inside a transaction, so toggle it on the
	// (single) connection around it — table rebuilds must not cascade deletes.
	if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer s.db.Exec(`PRAGMA foreign_keys=ON`) //nolint:errcheck
	return s.tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(string(b)); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO schema_migrations(name) VALUES(?)`, name)
		return err
	})
}

// ---------- helpers ----------

type rowScanner interface{ Scan(dest ...any) error }

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// queryAll runs q and scans every row. Rows are fully drained before it returns,
// which matters with a single pooled connection.
func queryAll[T any](ctx context.Context, db *sql.DB, scan func(rowScanner) (T, error), q string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// queryOne scans a single row, mapping "no rows" to (nil, nil).
func queryOne[T any](sc rowScanner, scan func(rowScanner) (T, error)) (*T, error) {
	v, err := scan(sc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func unixUTC(n int64) time.Time { return time.Unix(n, 0).UTC() }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func visibility(v string) string {
	if v == "private" {
		return "private"
	}
	return "public"
}

// likeEscaper escapes LIKE metacharacters; pair with ESCAPE '\'.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

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
		`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, val)
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

// PurgeExpiredSessions removes sessions past their expiry (run periodically).
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= unixepoch()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
	res, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at=unixepoch() WHERE token_hash=?`, hash)
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

// ListTokens returns all API tokens, newest first.
func (s *Store) ListTokens(ctx context.Context) ([]APIToken, error) {
	return queryAll(ctx, s.db, func(sc rowScanner) (APIToken, error) {
		var t APIToken
		var created int64
		var last sql.NullInt64
		err := sc.Scan(&t.ID, &t.Name, &created, &last)
		t.CreatedAt = unixUTC(created)
		if last.Valid {
			lu := unixUTC(last.Int64)
			t.LastUsedAt = &lu
		}
		return t, err
	}, `SELECT id, name, created_at, last_used_at FROM api_tokens ORDER BY created_at DESC, id DESC`)
}

// RevokeToken deletes a token by numeric id or exact name. Returns sql.ErrNoRows
// when nothing matched.
func (s *Store) RevokeToken(ctx context.Context, idOrName string) error {
	col, arg := "name", any(idOrName)
	if id, err := strconv.ParseInt(idOrName, 10, 64); err == nil {
		col, arg = "id", id
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE `+col+`=?`, arg)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ---------- categories + tags ----------

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

// ListCategories returns categories with item counts. Public callers only see
// categories that hold at least one public item, so private-only category names
// never leak into the nav or sitemap.
func (s *Store) ListCategories(ctx context.Context, includePrivate bool) ([]Category, error) {
	return queryAll(ctx, s.db, func(sc rowScanner) (Category, error) {
		var c Category
		return c, sc.Scan(&c.ID, &c.Slug, &c.Name, &c.Description, &c.Position, &c.Count)
	}, `SELECT c.id, c.slug, c.name, c.description, c.position,
		       (SELECT COUNT(*) FROM items i WHERE i.category_id=c.id AND (?1 OR i.visibility='public')) AS cnt
		FROM categories c WHERE ?1 OR cnt > 0
		ORDER BY c.position, c.name`, includePrivate)
}

func (s *Store) GetOrCreateCategory(ctx context.Context, name string) (int64, error) {
	return getOrCreate(ctx, s.db, "categories", name)
}

// getOrCreate returns the id of the categories/tags row for name's slug,
// creating it if needed. Idempotent and safe under concurrent ingest.
func getOrCreate(ctx context.Context, ex execer, table, name string) (int64, error) {
	name = strings.TrimSpace(name)
	slug := Slugify(name)
	if slug == "" {
		return 0, fmt.Errorf("invalid %s name %q", strings.TrimSuffix(table, "s"), name)
	}
	var id int64
	err := ex.QueryRowContext(ctx, `INSERT INTO `+table+`(slug,name) VALUES(?,?)
		ON CONFLICT(slug) DO UPDATE SET slug=excluded.slug RETURNING id`, slug, name).Scan(&id)
	return id, err
}

// attachTags links names (created as needed) to an item. Unsluggable names are skipped.
func attachTags(ctx context.Context, tx *sql.Tx, itemID int64, names []string) error {
	for _, name := range names {
		if Slugify(name) == "" {
			continue
		}
		tagID, err := getOrCreate(ctx, tx, "tags", name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO item_tags(item_id,tag_id) VALUES(?,?)`, itemID, tagID); err != nil {
			return err
		}
	}
	return nil
}

// ListTags returns tags, most-used first (then alphabetical). Public callers only
// see tags attached to at least one public item.
func (s *Store) ListTags(ctx context.Context, includePrivate bool) ([]Tag, error) {
	return queryAll(ctx, s.db, scanTag, `
		SELECT t.id, t.slug, t.name FROM tags t
		LEFT JOIN item_tags it ON it.tag_id = t.id
		LEFT JOIN items i ON i.id = it.item_id AND (?1 OR i.visibility='public')
		GROUP BY t.id HAVING ?1 OR COUNT(i.id) > 0
		ORDER BY COUNT(i.id) DESC, t.name`, includePrivate)
}

func scanTag(sc rowScanner) (Tag, error) {
	var t Tag
	return t, sc.Scan(&t.ID, &t.Slug, &t.Name)
}

// Slugify lowercases ASCII and hyphenates a string for use in URLs, retaining
// Unicode letters and numbers so CJK/etc. categories get non-empty slugs.
// Existing slugs depend on these exact rules (symbols dropped, non-ASCII case
// kept) — changing them would split categories/tags into duplicates.
func Slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
			fallthrough
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b.WriteRune(r)
			dash = false
		case unicode.IsSpace(r) || unicode.IsPunct(r):
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// ---------- items ----------

type Item struct {
	ID              int64
	Kind            string // image | link | text | embed | document
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

const itemColumns = `
	i.id, i.kind, i.title, i.note, i.source_url, i.visibility,
	COALESCE(i.category_id,0), COALESCE(c.slug,''), COALESCE(c.name,''),
	i.link_title, i.link_description, i.link_site_name, i.embed_provider, i.embed_html,
	i.cover_remote_url, i.cover_key, i.thumb_key, i.small_key, i.placeholder, i.dominant_color, i.width, i.height,
	i.created_at, i.updated_at, i.published_at,
	i.file_key, i.file_name, i.file_mime, i.file_size`

// itemCardColumns is the board/search card projection: it blanks detail-only
// text (embed HTML, long link text) that would inflate image-heavy boards.
var itemCardColumns = strings.NewReplacer(
	"i.link_title,", "'',", "i.link_description,", "'',", "i.embed_html,", "'',").Replace(itemColumns)

const itemFrom = ` FROM items i LEFT JOIN categories c ON c.id = i.category_id`

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
	it.CreatedAt, it.UpdatedAt = unixUTC(createdAt), unixUTC(updatedAt)
	if published.Valid {
		t := unixUTC(published.Int64)
		it.PublishedAt = &t
	}
	return it, err
}

type ItemFilter struct {
	IncludePrivate bool
	Cards          bool // card projection (see itemCardColumns)
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
	cols := itemColumns
	if f.Cards {
		cols = itemCardColumns
	}
	where := []string{"1"}
	var args []any
	if !f.IncludePrivate {
		where = append(where, "i.visibility='public'")
	}
	if f.CategorySlug != "" {
		where = append(where, "c.slug = ?")
		args = append(args, f.CategorySlug)
	}
	if f.TagSlug != "" {
		where = append(where, `i.id IN (SELECT it.item_id FROM item_tags it JOIN tags t ON t.id = it.tag_id WHERE t.slug = ?)`)
		args = append(args, f.TagSlug)
	}
	if f.BeforeCreated > 0 && f.BeforeID > 0 {
		where = append(where, `(i.created_at < ? OR (i.created_at = ? AND i.id < ?))`)
		args = append(args, f.BeforeCreated, f.BeforeCreated, f.BeforeID)
	}
	q := `SELECT` + cols + itemFrom + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY i.created_at DESC, i.id DESC`
	if f.Limit > 0 {
		q += " LIMIT ? OFFSET ?"
		args = append(args, f.Limit, f.Offset)
	}
	return queryAll(ctx, s.db, scanItem, q, args...)
}

func (s *Store) CountItems(ctx context.Context, includePrivate bool) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE ? OR visibility='public'`, includePrivate).Scan(&n)
	return n, err
}

// ItemMediaKeys is a minimal projection for broken-item detection.
type ItemMediaKeys struct {
	ID       int64
	CoverKey string
	FileKey  string
}

// ListItemMediaKeys returns id/cover/file keys for every item.
func (s *Store) ListItemMediaKeys(ctx context.Context) ([]ItemMediaKeys, error) {
	return queryAll(ctx, s.db, func(sc rowScanner) (ItemMediaKeys, error) {
		var k ItemMediaKeys
		return k, sc.Scan(&k.ID, &k.CoverKey, &k.FileKey)
	}, `SELECT id, cover_key, file_key FROM items`)
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

// SearchItems matches item text via the FTS5 index (fast + prefix matching) and
// category/tag names via LIKE, falling back to a LIKE scan over the text columns
// if FTS is unavailable or the query has no indexable terms.
func (s *Store) SearchItems(ctx context.Context, f SearchFilter) ([]Item, error) {
	if f.Limit <= 0 {
		f.Limit = 200
	}
	like := "%" + likeEscaper.Replace(strings.ToLower(strings.TrimSpace(f.Query))) + "%"
	q := `SELECT` + itemCardColumns + itemFrom + ` WHERE (? OR i.visibility='public') AND (
		lower(c.name) LIKE ? ESCAPE '\' OR
		i.id IN (SELECT it.item_id FROM item_tags it JOIN tags t ON t.id=it.tag_id WHERE lower(t.name) LIKE ? ESCAPE '\') OR %s)
		ORDER BY i.created_at DESC, i.id DESC LIMIT ?`
	if match := ftsQuery(f.Query); match != "" {
		items, err := queryAll(ctx, s.db, scanItem, fmt.Sprintf(q, `i.id IN (SELECT rowid FROM items_fts WHERE items_fts MATCH ?)`),
			f.IncludePrivate, like, like, match, f.Limit)
		if err == nil {
			return items, nil
		}
	}
	cols := []string{"i.title", "i.note", "i.link_title", "i.link_description", "i.link_site_name", "i.file_name"}
	args := []any{f.IncludePrivate, like, like}
	for i, c := range cols {
		cols[i] = "lower(" + c + `) LIKE ? ESCAPE '\'`
		args = append(args, like)
	}
	return queryAll(ctx, s.db, scanItem, fmt.Sprintf(q, strings.Join(cols, " OR ")), append(args, f.Limit)...)
}

// GetItem returns a single item with its tags. Returns (nil, nil) when not found
// or when the item is private and includePrivate is false.
func (s *Store) GetItem(ctx context.Context, id int64, includePrivate bool) (*Item, error) {
	it, err := queryOne(s.db.QueryRowContext(ctx, `SELECT`+itemColumns+itemFrom+
		` WHERE i.id = ? AND (? OR i.visibility='public')`, id, includePrivate), scanItem)
	if it == nil || err != nil {
		return nil, err
	}
	it.Tags, err = queryAll(ctx, s.db, scanTag, `SELECT t.id, t.slug, t.name FROM tags t
		JOIN item_tags it ON it.tag_id = t.id WHERE it.item_id = ? ORDER BY t.name`, id)
	return it, err
}

// insertItem inserts it. Zero CreatedAt means "now"; public items get a
// published_at stamp automatically.
func insertItem(ctx context.Context, ex execer, it Item) (int64, error) {
	now := time.Now().Unix()
	created := now
	if !it.CreatedAt.IsZero() {
		created = it.CreatedAt.Unix()
	}
	it.Visibility = visibility(it.Visibility)
	var published any
	switch {
	case it.PublishedAt != nil:
		published = it.PublishedAt.Unix()
	case it.Visibility == "public":
		published = created
	}
	res, err := ex.ExecContext(ctx, `INSERT INTO items
		(kind,title,note,source_url,visibility,category_id,
		 link_title,link_description,link_site_name,embed_provider,embed_html,
		 cover_remote_url,cover_key,thumb_key,small_key,placeholder,dominant_color,width,height,
		 file_key,file_name,file_mime,file_size,
		 created_at,updated_at,published_at)
		VALUES (?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?,?,?, ?,?,?,?, ?,?,?)`,
		it.Kind, it.Title, it.Note, it.SourceURL, it.Visibility, nullID(it.CategoryID),
		it.LinkTitle, it.LinkDescription, it.LinkSiteName, it.EmbedProvider, it.EmbedHTML,
		it.CoverRemoteURL, it.CoverKey, it.ThumbKey, it.SmallKey, it.Placeholder, it.DominantColor, it.Width, it.Height,
		it.FileKey, it.FileName, it.FileMime, it.FileSize,
		created, now, published)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CreateItem inserts a bare item (no media or tags).
func (s *Store) CreateItem(ctx context.Context, it Item) (int64, error) {
	return s.CreateItemWithMediaAndTags(ctx, it, nil, nil)
}

// CreateItemWithMediaAndTags inserts an item, its media rows, and tags in one
// transaction (the kind/visibility CHECK constraints reject bad values).
func (s *Store) CreateItemWithMediaAndTags(ctx context.Context, it Item, media []Media, tags []string) (id int64, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if id, err = insertItem(ctx, tx, it); err != nil {
			return err
		}
		if err := insertMedia(ctx, tx, id, media); err != nil {
			return err
		}
		return attachTags(ctx, tx, id, tags)
	})
	return id, err
}

// ItemEdit is the admin-editable subset of an item.
type ItemEdit struct {
	Title, Note, SourceURL, Category, Visibility string
	Tags                                         []string
}

// UpdateItem applies an admin edit (fields, category by name, tags) atomically.
func (s *Store) UpdateItem(ctx context.Context, id int64, e ItemEdit) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var catID int64
		if strings.TrimSpace(e.Category) != "" {
			var err error
			if catID, err = getOrCreate(ctx, tx, "categories", e.Category); err != nil {
				return err
			}
		}
		vis := visibility(e.Visibility)
		if _, err := tx.ExecContext(ctx, `
			UPDATE items SET title=?, note=?, source_url=?, category_id=?, visibility=?, updated_at=unixepoch(),
			    published_at=COALESCE(published_at, CASE WHEN ?='public' THEN unixepoch() END)
			WHERE id=?`,
			strings.TrimSpace(e.Title), strings.TrimSpace(e.Note), strings.TrimSpace(e.SourceURL),
			nullID(catID), vis, vis, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM item_tags WHERE item_id=?`, id); err != nil {
			return err
		}
		return attachTags(ctx, tx, id, e.Tags)
	})
}

// ReplaceItemMedia swaps an item's media rows and media columns in one
// transaction (used when the owner uploads a replacement file).
func (s *Store) ReplaceItemMedia(ctx context.Context, itemID int64, next Item, media []Media) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
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
		return insertMedia(ctx, tx, itemID, media)
	})
}

// SetItemSmallKey sets just the small (400px) variant key (used by the backfill).
func (s *Store) SetItemSmallKey(ctx context.Context, id int64, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE items SET small_key=? WHERE id=?`, key, id)
	return err
}

// DeleteItem removes an item; media rows and tag links cascade.
func (s *Store) DeleteItem(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM items WHERE id=?`, id)
	return err
}

// ResetContent deletes all archive content (items, media, tags, categories)
// and remote feed cache/reposts while keeping settings, API tokens, and
// followed remote feed sources. Conditional-fetch validators on kept sources
// are cleared so the next sync redownloads items instead of accepting a stale 304.
func (s *Store) ResetContent(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM reposts`,
			`DELETE FROM remote_feed_items`,
			`DELETE FROM item_tags`,
			`DELETE FROM media`,
			`DELETE FROM items`,
			`DELETE FROM tags`,
			`DELETE FROM categories`,
			`UPDATE remote_feeds SET etag='', last_modified='', last_fetched_at=0, last_success_at=0,
				last_error='', updated_at=unixepoch()`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetAdjacent returns the ids of the newer (prev) and older (next) items
// relative to it within the board ordering (created_at DESC, id DESC). Returns 0
// for either end. Respects visibility unless includePrivate.
func (s *Store) GetAdjacent(ctx context.Context, it Item, includePrivate bool) (prevID, nextID int64, err error) {
	c := it.CreatedAt.Unix()
	q := `SELECT COALESCE((SELECT id FROM items WHERE (created_at %[1]s ? OR (created_at = ? AND id %[1]s ?))
		AND (? OR visibility='public') ORDER BY created_at %[2]s, id %[2]s LIMIT 1), 0)`
	if err = s.db.QueryRowContext(ctx, fmt.Sprintf(q, ">", "ASC"), c, c, it.ID, includePrivate).Scan(&prevID); err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx, fmt.Sprintf(q, "<", "DESC"), c, c, it.ID, includePrivate).Scan(&nextID)
	return
}

// GetRelated returns items that share a category or any tag with it (excluding
// it), newest first.
func (s *Store) GetRelated(ctx context.Context, it Item, limit int, includePrivate bool) ([]Item, error) {
	return queryAll(ctx, s.db, scanItem, `SELECT`+itemCardColumns+itemFrom+`
		WHERE i.id != ? AND (? OR i.visibility='public') AND (
			(? != 0 AND i.category_id = ?)
			OR i.id IN (SELECT item_id FROM item_tags WHERE tag_id IN
				(SELECT tag_id FROM item_tags WHERE item_id = ?)))
		ORDER BY i.created_at DESC, i.id DESC LIMIT ?`,
		it.ID, includePrivate, it.CategoryID, it.CategoryID, it.ID, limit)
}

// Stats summarizes the public archive (colophon, easter eggs, and the cheap
// Count/Updated fingerprint behind feed/sitemap ETags).
type Stats struct {
	Count   int
	Oldest  time.Time
	Newest  time.Time
	Updated int64 // max(updated_at), unix seconds
}

func (s *Store) PublicStats(ctx context.Context) (Stats, error) {
	var st Stats
	var oldest, newest, updated sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(created_at), MAX(created_at), MAX(updated_at) FROM items WHERE visibility='public'`).
		Scan(&st.Count, &oldest, &newest, &updated)
	if oldest.Valid {
		st.Oldest, st.Newest, st.Updated = unixUTC(oldest.Int64), unixUTC(newest.Int64), updated.Int64
	}
	return st, err
}

// ---------- media variants ----------

type Media struct {
	ID          int64
	ItemID      int64
	Variant     string // original | full | thumb | small | file | video
	StorageKey  string
	ContentType string
	Width       int
	Height      int
	Bytes       int64
	OnLocal     bool
	OnR2        bool
	Private     bool // read-only: the parent item is private (joined, not stored)
}

const mediaSelect = `SELECT m.id, m.item_id, m.variant, m.storage_key, m.content_type, m.width, m.height,
	m.bytes, m.on_local, m.on_r2, COALESCE(i.visibility,'')='private'
	FROM media m LEFT JOIN items i ON i.id = m.item_id `

func scanMedia(sc rowScanner) (Media, error) {
	var m Media
	return m, sc.Scan(&m.ID, &m.ItemID, &m.Variant, &m.StorageKey, &m.ContentType,
		&m.Width, &m.Height, &m.Bytes, &m.OnLocal, &m.OnR2, &m.Private)
}

func insertMedia(ctx context.Context, tx *sql.Tx, itemID int64, media []Media) error {
	for _, m := range media {
		if _, err := tx.ExecContext(ctx, `INSERT INTO media
			(item_id,variant,storage_key,content_type,width,height,bytes,on_local,on_r2)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			itemID, m.Variant, m.StorageKey, m.ContentType, m.Width, m.Height, m.Bytes,
			b2i(m.OnLocal), b2i(m.OnR2)); err != nil {
			return err
		}
	}
	return nil
}

// UpsertMedia inserts or updates a media row by (item_id, variant). Used by
// idempotent maintenance jobs (e.g. backfill-variants).
func (s *Store) UpsertMedia(ctx context.Context, m Media) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO media (item_id,variant,storage_key,content_type,width,height,bytes,on_local,on_r2)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(item_id, variant) DO UPDATE SET
			storage_key=excluded.storage_key, content_type=excluded.content_type,
			width=excluded.width, height=excluded.height, bytes=excluded.bytes,
			on_local=excluded.on_local, on_r2=excluded.on_r2`,
		m.ItemID, m.Variant, m.StorageKey, m.ContentType, m.Width, m.Height, m.Bytes,
		b2i(m.OnLocal), b2i(m.OnR2))
	return err
}

// ListMedia returns an item's media rows.
func (s *Store) ListMedia(ctx context.Context, itemID int64) ([]Media, error) {
	return queryAll(ctx, s.db, scanMedia, mediaSelect+`WHERE m.item_id = ? ORDER BY m.id`, itemID)
}

// AllMedia returns every media row (orphan scan, placement sync).
func (s *Store) AllMedia(ctx context.Context) ([]Media, error) {
	return queryAll(ctx, s.db, scanMedia, mediaSelect+`ORDER BY m.id`)
}

// StrayMediaRows returns media rows whose parent item no longer exists (the FK
// cascade should prevent these, but the scan reports/cleans any that slip through).
func (s *Store) StrayMediaRows(ctx context.Context) ([]Media, error) {
	return queryAll(ctx, s.db, scanMedia, mediaSelect+`WHERE i.id IS NULL`)
}

// MediaByKey looks up a media row (with its item's privacy) by storage key.
// Returns sql.ErrNoRows when the key is unknown or orphaned.
func (s *Store) MediaByKey(ctx context.Context, key string) (Media, error) {
	return scanMedia(s.db.QueryRowContext(ctx, mediaSelect+`WHERE m.storage_key = ? AND i.id IS NOT NULL`, key))
}

// SetMediaPlacement records which storage tiers hold a media row's blob.
func (s *Store) SetMediaPlacement(ctx context.Context, id int64, onLocal, onR2 bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media SET on_local=?, on_r2=? WHERE id=?`, b2i(onLocal), b2i(onR2), id)
	return err
}

// DeleteMediaRow removes a single media row by id (used to clear stray rows).
func (s *Store) DeleteMediaRow(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM media WHERE id=?`, id)
	return err
}

// ---------- pending shares (PWA share-across-login) ----------

// PendingShare is a short-lived PWA share payload held until the admin logs in.
type PendingShare struct {
	ID        string
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
	if p.ExpiresAt.IsZero() {
		p.ExpiresAt = time.Now().Add(30 * time.Minute)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pending_shares
		(id, expires_at, title, text, url, file_key, file_name, file_mime, file_size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.ExpiresAt.Unix(), p.Title, p.Text, p.URL, p.FileKey, p.FileName, p.FileMime, p.FileSize)
	return err
}

// TakePendingShare atomically deletes and returns a non-expired pending share,
// or (nil, nil) when it is missing, expired, or already taken.
func (s *Store) TakePendingShare(ctx context.Context, id string) (*PendingShare, error) {
	return queryOne(s.db.QueryRowContext(ctx, `DELETE FROM pending_shares WHERE id=? AND expires_at > unixepoch()
		RETURNING id, expires_at, title, text, url, file_key, file_name, file_mime, file_size`, id),
		func(sc rowScanner) (PendingShare, error) {
			var p PendingShare
			var exp int64
			err := sc.Scan(&p.ID, &exp, &p.Title, &p.Text, &p.URL, &p.FileKey, &p.FileName, &p.FileMime, &p.FileSize)
			p.ExpiresAt = unixUTC(exp)
			return p, err
		})
}

// PurgeExpiredPendingShares drops expired pending rows and returns the file
// keys (possibly empty strings) whose stashed blobs the caller should remove.
func (s *Store) PurgeExpiredPendingShares(ctx context.Context) ([]string, error) {
	return queryAll(ctx, s.db, func(sc rowScanner) (string, error) {
		var k string
		return k, sc.Scan(&k)
	}, `DELETE FROM pending_shares WHERE expires_at <= unixepoch() RETURNING file_key`)
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

const remoteFeedSelect = `SELECT id, feed_url, site_url, title, description, icon_url,
	etag, last_modified, last_fetched_at, last_success_at, last_error,
	active, created_at, updated_at FROM remote_feeds `

func scanRemoteFeed(sc rowScanner) (RemoteFeed, error) {
	var f RemoteFeed
	var lastFetched, lastSuccess, created, updated int64
	err := sc.Scan(&f.ID, &f.FeedURL, &f.SiteURL, &f.Title, &f.Description, &f.IconURL,
		&f.ETag, &f.LastModified, &lastFetched, &lastSuccess, &f.LastError,
		&f.Active, &created, &updated)
	if lastFetched > 0 {
		f.LastFetchedAt = unixUTC(lastFetched)
	}
	if lastSuccess > 0 {
		f.LastSuccessAt = unixUTC(lastSuccess)
	}
	f.CreatedAt, f.UpdatedAt = unixUTC(created), unixUTC(updated)
	return f, err
}

// AddRemoteFeed follows feedURL: it inserts a new feed or reactivates an
// existing one. created is true only on a fresh insert.
func (s *Store) AddRemoteFeed(ctx context.Context, feedURL string) (feed *RemoteFeed, created bool, err error) {
	feedURL = strings.TrimSpace(feedURL)
	if feedURL == "" {
		return nil, false, errors.New("feed URL is required")
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO remote_feeds(feed_url) VALUES(?) ON CONFLICT(feed_url) DO NOTHING`, feedURL)
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 { // already known: make sure it's followed again
		if _, err := s.db.ExecContext(ctx, `UPDATE remote_feeds SET active=1, updated_at=unixepoch()
			WHERE feed_url=? AND active=0`, feedURL); err != nil {
			return nil, false, err
		}
	}
	feed, err = queryOne(s.db.QueryRowContext(ctx, remoteFeedSelect+`WHERE feed_url=?`, feedURL), scanRemoteFeed)
	return feed, n == 1, err
}

// ListRemoteFeeds returns followed sources; active feeds first, then title, URL.
func (s *Store) ListRemoteFeeds(ctx context.Context, activeOnly bool) ([]RemoteFeed, error) {
	return queryAll(ctx, s.db, scanRemoteFeed, remoteFeedSelect+`WHERE NOT ? OR active=1
		ORDER BY active DESC, title COLLATE NOCASE, feed_url`, activeOnly)
}

// GetRemoteFeed returns a source by id, or (nil, nil) when missing.
func (s *Store) GetRemoteFeed(ctx context.Context, id int64) (*RemoteFeed, error) {
	return queryOne(s.db.QueryRowContext(ctx, remoteFeedSelect+`WHERE id=?`, id), scanRemoteFeed)
}

// SetRemoteFeedActive toggles follow state. Missing id returns sql.ErrNoRows.
func (s *Store) SetRemoteFeedActive(ctx context.Context, id int64, active bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE remote_feeds SET active=?, updated_at=unixepoch() WHERE id=?`, b2i(active), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func unixOr(t time.Time, def int64) int64 {
	if t.IsZero() {
		return def
	}
	return t.Unix()
}

// SaveRemoteFeedFetch updates feed metadata and upserts remote items in one tx.
// Returns the number of input items processed.
func (s *Store) SaveRemoteFeedFetch(ctx context.Context, feedID int64, upd RemoteFeedUpdate, items []RemoteFeedItem) (int, error) {
	fetched := unixOr(upd.LastFetchedAt, time.Now().Unix())
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE remote_feeds SET title=?, site_url=?, description=?, icon_url=?, etag=?, last_modified=?,
				last_fetched_at=?, last_success_at=?, last_error=?, updated_at=unixepoch()
			WHERE id=?`,
			upd.Title, upd.SiteURL, upd.Description, upd.IconURL, upd.ETag, upd.LastModified,
			fetched, unixOr(upd.LastSuccessAt, fetched), upd.LastError, feedID); err != nil {
			return err
		}
		for _, it := range items {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO remote_feed_items(
					feed_id, remote_id, url, external_url, title, content_text,
					image_url, attachment_url, attachment_mime,
					author_name, author_url, published_at, fetched_at, raw_json)
				VALUES (?,?,?,?,?,?, ?,?,?, ?,?,?,?,?)
				ON CONFLICT(feed_id, remote_id) DO UPDATE SET
					url=excluded.url, external_url=excluded.external_url, title=excluded.title,
					content_text=excluded.content_text, image_url=excluded.image_url,
					attachment_url=excluded.attachment_url, attachment_mime=excluded.attachment_mime,
					author_name=excluded.author_name, author_url=excluded.author_url,
					published_at=excluded.published_at, fetched_at=excluded.fetched_at, raw_json=excluded.raw_json`,
				feedID, it.RemoteID, it.URL, it.ExternalURL, it.Title, it.ContentText,
				it.ImageURL, it.AttachmentURL, it.AttachmentMime,
				it.AuthorName, it.AuthorURL, unixOr(it.PublishedAt, fetched), unixOr(it.FetchedAt, fetched), it.RawJSON); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(items), nil
}

// SaveRemoteFeedError records a fetch failure without clearing cached items.
func (s *Store) SaveRemoteFeedError(ctx context.Context, feedID int64, fetchedAt time.Time, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE remote_feeds SET last_fetched_at=?, last_error=?, updated_at=unixepoch()
		WHERE id=?`, unixOr(fetchedAt, time.Now().Unix()), msg, feedID)
	return err
}

// MarkRemoteFeedChecked records a successful conditional GET (HTTP 304).
func (s *Store) MarkRemoteFeedChecked(ctx context.Context, feedID int64, fetchedAt time.Time) error {
	return s.SaveRemoteFeedError(ctx, feedID, fetchedAt, "")
}

const remoteItemSelect = `SELECT ri.id, ri.feed_id, COALESCE(rf.title,''), rf.feed_url, ri.remote_id,
		ri.url, ri.external_url, ri.title, ri.content_text, ri.image_url,
		ri.attachment_url, ri.attachment_mime, ri.author_name, ri.author_url,
		ri.published_at, ri.fetched_at, ri.raw_json, COALESCE(rp.local_item_id, 0)
	FROM remote_feed_items ri
	JOIN remote_feeds rf ON rf.id = ri.feed_id
	LEFT JOIN reposts rp ON rp.remote_feed_item_id = ri.id `

func scanRemoteFeedItem(sc rowScanner) (RemoteFeedItem, error) {
	var it RemoteFeedItem
	var published, fetched int64
	err := sc.Scan(&it.ID, &it.FeedID, &it.FeedTitle, &it.FeedURL, &it.RemoteID,
		&it.URL, &it.ExternalURL, &it.Title, &it.ContentText, &it.ImageURL,
		&it.AttachmentURL, &it.AttachmentMime, &it.AuthorName, &it.AuthorURL,
		&published, &fetched, &it.RawJSON, &it.RepostedItemID)
	it.PublishedAt, it.FetchedAt = unixUTC(published), unixUTC(fetched)
	return it, err
}

// ListRemoteFeedItems returns cached remote items newest-first with optional keyset.
func (s *Store) ListRemoteFeedItems(ctx context.Context, f RemoteFeedItemFilter) ([]RemoteFeedItem, error) {
	q := remoteItemSelect + `WHERE (NOT ? OR rf.active=1)`
	args := []any{f.ActiveOnly}
	if f.BeforePublished > 0 && f.BeforeID > 0 {
		q += ` AND (ri.published_at < ? OR (ri.published_at = ? AND ri.id < ?))`
		args = append(args, f.BeforePublished, f.BeforePublished, f.BeforeID)
	}
	q += " ORDER BY ri.published_at DESC, ri.id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	return queryAll(ctx, s.db, scanRemoteFeedItem, q, args...)
}

// GetRemoteFeedItem returns one cached item with repost mapping, or (nil, nil).
func (s *Store) GetRemoteFeedItem(ctx context.Context, id int64) (*RemoteFeedItem, error) {
	return queryOne(s.db.QueryRowContext(ctx, remoteItemSelect+`WHERE ri.id=?`, id), scanRemoteFeedItem)
}

// CreateRepost creates a public local link item from a remote feed item.
// Idempotent: existing reposts return the same local_item_id with created=false.
func (s *Store) CreateRepost(ctx context.Context, remoteItemID int64) (localID int64, created bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		remote, err := queryOne(tx.QueryRowContext(ctx, remoteItemSelect+`WHERE ri.id=?`, remoteItemID), scanRemoteFeedItem)
		if err != nil {
			return err
		}
		if remote == nil {
			return errors.New("remote item not found")
		}
		if remote.RepostedItemID != 0 { // a deleted local item leaves NULL (0) — recreate then
			localID = remote.RepostedItemID
			return nil
		}
		sourceURL := chooseRepostSourceURL(*remote)
		if sourceURL == "" {
			return errors.New("remote item has no URL to repost")
		}
		if localID, err = insertItem(ctx, tx, Item{
			Kind: "link", Title: remote.Title, Note: remote.ContentText, SourceURL: sourceURL,
			LinkTitle: remote.Title, LinkSiteName: remote.FeedTitle, CoverRemoteURL: remote.ImageURL,
		}); err != nil {
			return err
		}
		created = true
		_, err = tx.ExecContext(ctx, `INSERT INTO reposts(remote_feed_item_id, local_item_id) VALUES (?,?)
			ON CONFLICT(remote_feed_item_id) DO UPDATE SET local_item_id=excluded.local_item_id`, remoteItemID, localID)
		return err
	})
	return localID, created, err
}

func chooseRepostSourceURL(remote RemoteFeedItem) string {
	for _, c := range []string{remote.URL, remote.ExternalURL, remote.RemoteID} {
		if c = strings.TrimSpace(c); strings.HasPrefix(c, "http://") || strings.HasPrefix(c, "https://") {
			return c
		}
	}
	return ""
}
