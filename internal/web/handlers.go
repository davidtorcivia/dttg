package web

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"donottouchtheglass/internal/config"
	"donottouchtheglass/internal/store"
)

// itemView decorates a store.Item with presentation-only fields.
type itemView struct {
	store.Item
	CoverURL  string // full image (detail)
	ThumbURL  string // small image (grid)
	FileURL   string // document/file blob
	DetailURL string
	Eager     bool // board: above-the-fold (first row) — load eagerly at high priority
}

// metaTags drives SEO / Open Graph / Twitter Card output.
type metaTags struct {
	Description string
	Image      string // absolute URL
	URL        string // canonical absolute URL
	Type       string // website | article
}

type pageData struct {
	Cfg            config.Config
	IsAdmin        bool
	Categories     []store.Category
	PageTitle      string
	ActiveCat      string
	ActiveTag      string
	Error          string
	Items          []itemView
	Item           *itemView
	TagsCSV        string        // edit form: current tags as comma-separated text
	SearchQuery    string        // search page: current query
	TrackingScript template.HTML // analytics snippet injected on public pages
	Settings       *settingsView // admin settings page
	Prefill        *newItemForm  // admin "new" form prefill (bookmarklet/query)
	Meta           metaTags      // SEO / OG tags
	PrevURL        string        // detail page: newer item
	NextURL        string        // detail page: older item
	Related        []itemView    // detail page: related items
	Stats          *store.Stats  // board footer / colophon
	ColorScheme    string        // <meta name="color-scheme"> — explicit theme from cookie, else "light dark"
	ThemeAttr      string        // server-rendered <html data-theme> from cookie ("dark"/"light"/""), kills the FOUC
	BoardColumns   int           // masonry columns on wide screens (3 or 4)
	JSONLD         template.HTML // detail page: schema.org structured data (already JSON-escaped)
	Nonce          string        // per-request CSP nonce for inline <script> tags
}

const boardPage = 48 // items per board page (initial load + each infinite-scroll batch)

func (s *Server) page(r *http.Request, title string) pageData {
	isAdmin := s.isAdmin(r)
	cats, err := s.store.ListCategories(r.Context(), isAdmin)
	if err != nil {
		log.Printf("list categories: %v", err)
	}
	// The client mirrors an explicit light/dark choice into this cookie so the server
	// can declare color-scheme in the served HTML head from byte 0. That makes the
	// browser's very first paint (including the canvas it shows between page
	// navigations) match the chosen theme — which is what kills the white flash in
	// Firefox, where a JS-set color-scheme lands too late. No cookie => follow the OS.
	colorScheme := "light dark"
	themeAttr := ""
	if c, err := r.Cookie("dnttg-theme"); err == nil && (c.Value == "dark" || c.Value == "light") {
		colorScheme = c.Value
		themeAttr = c.Value
	}
	nonce := nonceFromContext(r.Context())
	pd := pageData{
		Cfg:         s.cfg,
		IsAdmin:     isAdmin,
		Categories:  cats,
		PageTitle:   title,
		ColorScheme: colorScheme,
		ThemeAttr:   themeAttr,
		Nonce:       nonce,
		Meta: metaTags{
			Description: s.cfg.SiteTitle + " — a personal visual archive.",
			URL:         s.absURL(r.URL.Path),
			Image:       s.absURL("/static/icons/icon-512.png"),
			Type:        "website",
		},
	}
	// Inject the analytics snippet on public views only (don't track admin/self).
	// The nonce is added so it's allowed under the strict CSP script-src.
	if !isAdmin {
		if ts, _ := s.store.GetSetting(r.Context(), "tracking_script"); ts != "" {
			pd.TrackingScript = template.HTML(injectNonce(ts, nonce)) //nolint:gosec // admin-provided, trusted
		}
	}
	return pd
}

// absURL turns a relative path into an absolute URL using the configured base.
func (s *Server) absURL(u string) string {
	if u == "" || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return s.cfg.BaseURL + u
}

// coverURL resolves the full image for an item. The media store decides whether
// that resolves to the R2 custom domain or the local /media path; remote-hosted
// covers (not yet processed) fall back to their original URL.
func (s *Server) coverURL(it store.Item) string {
	if it.CoverKey != "" {
		return s.media.URL(it.CoverKey)
	}
	return it.CoverRemoteURL
}

// thumbURL resolves the small grid image, preferring the thumb variant.
func (s *Server) thumbURL(it store.Item) string {
	switch {
	case it.ThumbKey != "":
		return s.media.URL(it.ThumbKey)
	case it.CoverKey != "":
		return s.media.URL(it.CoverKey)
	default:
		return it.CoverRemoteURL
	}
}

// AltText is a meaningful image alt: the title, else the filename, else the kind
// (so untitled items aren't announced as empty by screen readers).
func (v itemView) AltText() string {
	switch {
	case strings.TrimSpace(v.Title) != "":
		return v.Title
	case strings.TrimSpace(v.FileName) != "":
		return v.FileName
	case v.Kind != "":
		return v.Kind
	default:
		return "untitled"
	}
}

func (s *Server) view(it store.Item) itemView {
	v := itemView{
		Item:      it,
		CoverURL:  s.coverURL(it),
		ThumbURL:  s.thumbURL(it),
		DetailURL: "/item/" + strconv.FormatInt(it.ID, 10),
	}
	if it.FileKey != "" {
		v.FileURL = s.media.URL(it.FileKey)
	}
	return v
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	pd := s.page(r, "SEARCH")
	pd.SearchQuery = q
	if q != "" {
		items, err := s.store.SearchItems(r.Context(), q, s.isAdmin(r))
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, it := range items {
			pd.Items = append(pd.Items, s.view(it))
		}
	}
	s.render(w, "search.html", pd)
}

// handleAPISearch returns compact JSON results for the live search overlay.
// Public results for anonymous visitors; private included when logged in.
func (s *Server) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	results := []map[string]any{}
	if q != "" {
		if items, err := s.store.SearchItems(r.Context(), q, s.isAdmin(r)); err == nil {
			for i, it := range items {
				if i >= 12 {
					break
				}
				v := s.view(it)
				results = append(results, map[string]any{
					"title":    it.Title,
					"url":      v.DetailURL,
					"thumb":    v.ThumbURL,
					"kind":     it.Kind,
					"category": it.CategoryName,
					"date":     strings.ToUpper(it.CreatedAt.Format("Jan 02")),
				})
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
}

// handleSearchCards renders search results as board-card HTML (live results on
// the /search page).
func (s *Server) handleSearchCards(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if q == "" {
		return
	}
	items, err := s.store.SearchItems(r.Context(), q, s.isAdmin(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, it := range items {
		if err := s.tmpl.ExecuteTemplate(w, "card", s.view(it)); err != nil {
			log.Printf("render card: %v", err)
		}
	}
}

// boardColumns is the configured masonry column count for wide screens. Only 3
// (default) or 4 are allowed; narrower viewports step down via CSS regardless.
func (s *Server) boardColumns(ctx context.Context) int {
	if v, _ := s.store.GetSetting(ctx, "board_columns"); v == "4" {
		return 4
	}
	return 3
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	f := store.ItemFilter{IncludePrivate: s.isAdmin(r), Limit: boardPage}
	slug := r.PathValue("slug")
	switch {
	case strings.HasPrefix(r.URL.Path, "/category/"):
		f.CategorySlug = slug
	case strings.HasPrefix(r.URL.Path, "/tag/"):
		f.TagSlug = slug
	}

	items, err := s.store.ListItems(r.Context(), f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	pd := s.page(r, s.cfg.SiteTitle)
	pd.ActiveCat = f.CategorySlug
	pd.ActiveTag = f.TagSlug
	pd.BoardColumns = s.boardColumns(r.Context())
	for i, it := range items {
		v := s.view(it)
		// The masonry distributes cards round-robin, so the first row is the first
		// N items — load those eagerly at high priority to help the board's LCP.
		if i < pd.BoardColumns {
			v.Eager = true
		}
		pd.Items = append(pd.Items, v)
	}
	if st, err := s.store.PublicStats(r.Context()); err == nil {
		pd.Stats = &st
	}
	s.render(w, "index.html", pd)
}

// handleBoardMore returns the next page of board cards as an HTML fragment
// (used by infinite scroll).
func (s *Server) handleBoardMore(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	f := store.ItemFilter{
		IncludePrivate: s.isAdmin(r),
		Limit:          boardPage,
		Offset:         offset,
		CategorySlug:   r.URL.Query().Get("cat"),
		TagSlug:        r.URL.Query().Get("tag"),
	}
	items, err := s.store.ListItems(r.Context(), f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, it := range items {
		if err := s.tmpl.ExecuteTemplate(w, "card", s.view(it)); err != nil {
			log.Printf("render card: %v", err)
		}
	}
}

// handleAPIStats returns public archive vitals (for the colophon / easter eggs).
func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	st, _ := s.store.PublicStats(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count":  st.Count,
		"oldest": st.Oldest.Format("Jan 2, 2006"),
		"newest": st.Newest.Format("Jan 2, 2006"),
	})
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	it, err := s.store.GetItem(r.Context(), id, s.isAdmin(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if it == nil {
		s.notFound(w, r)
		return
	}
	pd := s.page(r, it.Title)
	v := s.view(*it)
	pd.Item = &v

	// SEO / Open Graph for this item
	pd.Meta.Type = "article"
	pd.Meta.URL = s.absURL(v.DetailURL)
	if d := itemDescription(*it); d != "" {
		pd.Meta.Description = d
	}
	pd.Meta.Image = s.absURL("/item/" + strconv.FormatInt(it.ID, 10) + "/og.jpg")
	pd.JSONLD = s.itemJSONLD(v, pd.Nonce)
	if it.Title == "" {
		pd.PageTitle = s.cfg.SiteTitle
	}

	// Prev/next within the board ordering (newer / older)
	prevID, nextID, _ := s.store.GetAdjacent(r.Context(), *it, s.isAdmin(r))
	if prevID != 0 {
		pd.PrevURL = "/item/" + strconv.FormatInt(prevID, 10)
	}
	if nextID != 0 {
		pd.NextURL = "/item/" + strconv.FormatInt(nextID, 10)
	}

	// Related items
	if rel, err := s.store.GetRelated(r.Context(), *it, 6, s.isAdmin(r)); err == nil {
		for _, ri := range rel {
			pd.Related = append(pd.Related, s.view(ri))
		}
	}

	s.render(w, "detail.html", pd)
}

func itemDescription(it store.Item) string {
	for _, c := range []string{it.Note, it.LinkDescription, it.Title} {
		if c = strings.TrimSpace(c); c != "" {
			return c
		}
	}
	return ""
}

// itemJSONLD builds schema.org structured data for a detail page. Go's json
// encoder escapes <, > and & as \u00xx, so the result is safe to drop straight
// into a <script type="application/ld+json"> block.
func (s *Server) itemJSONLD(v itemView, nonce string) template.HTML {
	it := v.Item
	name := it.Title
	if name == "" {
		name = s.cfg.SiteTitle
	}
	ld := map[string]any{
		"@context": "https://schema.org",
		"name":     name,
		"url":      s.absURL(v.DetailURL),
	}
	if d := itemDescription(it); d != "" {
		ld["description"] = d
	}
	if !it.CreatedAt.IsZero() {
		ld["datePublished"] = it.CreatedAt.Format(time.RFC3339)
	}
	if !it.UpdatedAt.IsZero() {
		ld["dateModified"] = it.UpdatedAt.Format(time.RFC3339)
	}
	switch {
	case it.Kind == "embed" && strings.HasPrefix(it.FileMime, "video/"):
		ld["@type"] = "VideoObject"
		if v.FileURL != "" {
			ld["contentUrl"] = s.absURL(v.FileURL)
		}
		ld["thumbnailUrl"] = s.absURL("/item/" + strconv.FormatInt(it.ID, 10) + "/og.jpg")
		if !it.CreatedAt.IsZero() {
			ld["uploadDate"] = it.CreatedAt.Format(time.RFC3339)
		}
	case v.CoverURL != "":
		ld["@type"] = "ImageObject"
		ld["contentUrl"] = s.absURL(v.CoverURL)
		if v.ThumbURL != "" {
			ld["thumbnailUrl"] = s.absURL(v.ThumbURL)
		}
		if it.Width > 0 {
			ld["width"] = it.Width
		}
		if it.Height > 0 {
			ld["height"] = it.Height
		}
	default:
		ld["@type"] = "Article"
		ld["headline"] = name
	}
	b, err := json.Marshal(ld)
	if err != nil {
		return ""
	}
	// Build the whole <script> element here and emit it in HTML context: json.Marshal
	// escapes <, > and & as \u00xx so there's no </script> breakout, and this avoids
	// html/template applying JS-string escaping to the value inside a <script> tag.
	attr := ""
	if nonce != "" {
		attr = ` nonce="` + nonce + `"`
	}
	return template.HTML(`<script type="application/ld+json"` + attr + `>` + string(b) + `</script>`) //nolint:gosec
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	// Render fully into a buffer first, then write once with a Content-Length. This
	// sends the page as one atomic body instead of a chunked/streamed one — buffered
	// delivery does not trigger the Firefox theme flash, where a streamed document can
	// paint a frame before the (server-declared) theme settles. It also means a
	// template error can't emit a half-written 200.
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache") // HTML is dynamic; always revalidate
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("server error: %v (%s %s)", err, r.Method, r.URL.Path)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	if terr := s.tmpl.ExecuteTemplate(w, "500.html", s.page(r, "ERROR")); terr != nil {
		log.Printf("render 500: %v", terr)
	}
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if err := s.tmpl.ExecuteTemplate(w, "404.html", s.page(r, "NOT FOUND")); err != nil {
		log.Printf("render 404: %v", err)
	}
}
