package web

import (
	"bytes"
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
	SmallURL  string // ~400px variant (smallest srcset step); empty for old items
	FileURL   string // document/file blob
	DetailURL string
	Eager     bool // board: above-the-fold (first row) — load eagerly at high priority
}

// metaTags drives SEO / Open Graph / Twitter Card output.
type metaTags struct {
	Description string
	Image       string // absolute URL
	URL         string // canonical absolute URL
	Type        string // website | article
}

type pageData struct {
	Cfg            config.Config
	IsAdmin        bool
	Categories     []store.Category
	PageTitle      string
	ActiveCat      string
	ActiveTag      string
	Error          string
	Next           string // login form: safe return path after auth
	Items          []itemView
	Item           *itemView
	TagsCSV        string             // edit form: current tags as comma-separated text
	SearchQuery    string             // search page: current query
	TrackingScript template.HTML      // sanitized analytics snippet on public pages
	Settings       *settingsView      // admin settings page
	Prefill        *newItemForm       // admin "new" form prefill (bookmarklet/query)
	Meta           metaTags           // SEO / OG tags
	PrevURL        string             // detail page: newer item
	NextURL        string             // detail page: older item
	Related        []itemView         // detail page: related items
	Stats          *store.Stats       // board footer / colophon
	ColorScheme    string             // <meta name="color-scheme"> — explicit theme from cookie, else "light dark"
	ThemeAttr      string             // server-rendered <html data-theme> from cookie ("dark"/"light"/""), kills the FOUC
	BoardColumns   int                // masonry columns on wide screens (3 or 4)
	JSONLD         template.HTML      // detail page: schema.org structured data (already JSON-escaped)
	Nonce          string             // per-request CSP nonce for inline <script> tags
	CSRFToken      string             // session-bound token for admin POST forms
	Maintenance    *maintenanceReport // admin maintenance/orphan scan
	// Keyset cursor for infinite scroll / "load more" (last item on the page).
	CursorCreated int64
	CursorID      int64
	BoardDone     bool // a short page => no more pages
	RemoteFeeds   []store.RemoteFeed
	RemoteItems   []store.RemoteFeedItem
}

const boardPage = 48 // items per board page (initial load + each infinite-scroll batch)

func (s *Server) page(r *http.Request, title string) pageData {
	isAdmin := s.isAdmin(r)
	// The client mirrors an explicit light/dark choice into this cookie so the server
	// can declare color-scheme in the served HTML head from byte 0. That makes the
	// browser's very first paint (including the canvas it shows between page
	// navigations) match the chosen theme — which is what kills the white flash in
	// Firefox, where a JS-set color-scheme lands too late. No cookie => follow the OS.
	colorScheme, themeAttr := "light dark", ""
	if c, err := r.Cookie("dnttg-theme"); err == nil && (c.Value == "dark" || c.Value == "light") {
		colorScheme, themeAttr = c.Value, c.Value
	}
	// Overlay the admin-editable branding onto the per-request config copy so every
	// template ({{.Cfg.SiteTitle}} etc.) renders the effective values.
	site := s.siteID()
	cfg := s.cfg
	cfg.SiteTitle, cfg.SiteTagline, cfg.BaseURL = site.Title, site.Tagline, site.BaseURL
	sd := s.siteData(r.Context())
	pd := pageData{
		Cfg:          cfg,
		IsAdmin:      isAdmin,
		Categories:   sd.catsPublic,
		PageTitle:    title,
		ColorScheme:  colorScheme,
		ThemeAttr:    themeAttr,
		Nonce:        nonceFromContext(r.Context()),
		BoardColumns: sd.boardColumns,
		Meta: metaTags{
			Description: s.metaDescription(),
			URL:         s.absURL(r.URL.Path),
			Image:       s.absURL("/og.jpg"), // generated default share card
			Type:        "website",
		},
	}
	if isAdmin {
		pd.Categories = sd.catsAdmin
		if c, err := r.Cookie(sessionCookie); err == nil {
			pd.CSRFToken = s.csrfToken(c.Value) // for admin POST forms
		}
	} else {
		// Inject the analytics snippet on public views only (don't track admin/self).
		pd.TrackingScript = sanitizeTrackingSnippet(sd.tracking, pd.Nonce)
	}
	return pd
}

// absURL turns a relative path into an absolute URL using the configured base.
func (s *Server) absURL(u string) string {
	if u == "" || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return s.siteBaseURL() + u
}

// mediaURL resolves a storage key for rendering. Private item media always goes
// through the app gateway so R2/CDN URLs never leak private bytes. Public media
// may use the media store URL (R2/CDN when configured).
func (s *Server) mediaURL(key, visibility string) string {
	switch {
	case key == "":
		return ""
	case visibility == "private":
		return "/media/" + strings.TrimLeft(key, "/")
	}
	return s.media.URL(key)
}

// AltText is a meaningful image alt: the title, else the filename, else the kind
// (so untitled items aren't announced as empty by screen readers).
func (v itemView) AltText() string {
	for _, c := range []string{v.Title, v.FileName, v.Kind} {
		if c = strings.TrimSpace(c); c != "" {
			return c
		}
	}
	return "untitled"
}

func (s *Server) view(it store.Item) itemView {
	v := itemView{
		Item:      it,
		CoverURL:  s.mediaURL(it.CoverKey, it.Visibility),
		ThumbURL:  s.mediaURL(firstNonEmpty(it.ThumbKey, it.CoverKey), it.Visibility),
		SmallURL:  s.mediaURL(it.SmallKey, it.Visibility),
		FileURL:   s.mediaURL(it.FileKey, it.Visibility),
		DetailURL: itemPath(it.ID),
	}
	// Remote-hosted covers (not yet processed) fall back to their original URL.
	if v.CoverURL == "" {
		v.CoverURL, v.ThumbURL = it.CoverRemoteURL, it.CoverRemoteURL
	}
	return v
}

func (s *Server) views(items []store.Item) []itemView {
	out := make([]itemView, len(items))
	for i, it := range items {
		out[i] = s.view(it)
	}
	return out
}

func itemPath(id int64) string { return "/item/" + strconv.FormatInt(id, 10) }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseCursor reads a "created:id" keyset cursor (unix seconds + item id).
func parseCursor(r *http.Request) (created, id int64) {
	a, b, _ := strings.Cut(r.URL.Query().Get("cursor"), ":")
	created, _ = strconv.ParseInt(a, 10, 64)
	id, _ = strconv.ParseInt(b, 10, 64)
	return created, id
}

// writeCards renders items as board-card HTML fragments (infinite scroll, live search).
func (s *Server) writeCards(w http.ResponseWriter, items []store.Item) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, it := range items {
		if err := s.tmpl.ExecuteTemplate(w, "card", s.view(it)); err != nil {
			log.Printf("render card: %v", err)
			return
		}
	}
}

func (s *Server) search(r *http.Request, limit int) ([]store.Item, error) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		return nil, nil
	}
	return s.store.SearchItems(r.Context(), store.SearchFilter{Query: q, IncludePrivate: s.isAdmin(r), Limit: limit})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	items, err := s.search(r, 200)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pd := s.page(r, "SEARCH")
	pd.SearchQuery = strings.TrimSpace(r.URL.Query().Get("q"))
	pd.Items = s.views(items)
	s.render(w, http.StatusOK, "search.html", pd)
}

// handleAPISearch returns compact JSON results for the live search overlay.
// Public results for anonymous visitors; private included when logged in.
func (s *Server) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	items, err := s.search(r, 12)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "search failed")
		return
	}
	results := []map[string]any{}
	for _, v := range s.views(items) {
		results = append(results, map[string]any{
			"title":    v.Title,
			"url":      v.DetailURL,
			"thumb":    v.ThumbURL,
			"kind":     v.Kind,
			"category": v.CategoryName,
			"date":     strings.ToUpper(v.CreatedAt.Format("Jan 02")),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleSearchCards renders search results as board-card HTML (live results on
// the /search page).
func (s *Server) handleSearchCards(w http.ResponseWriter, r *http.Request) {
	items, err := s.search(r, boardPage)
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	s.writeCards(w, items)
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	f := store.ItemFilter{IncludePrivate: s.isAdmin(r), Cards: true, Limit: boardPage}
	if strings.HasPrefix(r.URL.Path, "/category/") {
		f.CategorySlug = r.PathValue("slug")
	} else {
		f.TagSlug = r.PathValue("slug")
	}
	items, err := s.store.ListItems(r.Context(), f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pd := s.page(r, s.siteTitle())
	pd.ActiveCat, pd.ActiveTag = f.CategorySlug, f.TagSlug
	pd.Items = s.views(items)
	// The masonry distributes cards round-robin, so the first row is the first
	// N items — load those eagerly at high priority to help the board's LCP.
	for i := 0; i < len(pd.Items) && i < pd.BoardColumns; i++ {
		pd.Items[i].Eager = true
	}
	if n := len(items); n > 0 {
		pd.CursorCreated, pd.CursorID = items[n-1].CreatedAt.Unix(), items[n-1].ID
	}
	pd.BoardDone = len(items) < boardPage
	if st, err := s.store.PublicStats(r.Context()); err == nil {
		pd.Stats = &st
	}
	s.render(w, http.StatusOK, "index.html", pd)
}

// handleBoardMore returns the next page of board cards as an HTML fragment
// (used by infinite scroll).
func (s *Server) handleBoardMore(w http.ResponseWriter, r *http.Request) {
	f := store.ItemFilter{
		IncludePrivate: s.isAdmin(r),
		Cards:          true,
		Limit:          boardPage,
		CategorySlug:   r.URL.Query().Get("cat"),
		TagSlug:        r.URL.Query().Get("tag"),
	}
	f.BeforeCreated, f.BeforeID = parseCursor(r)
	items, err := s.store.ListItems(r.Context(), f)
	if err != nil {
		http.Error(w, "could not load items", http.StatusInternalServerError)
		return
	}
	s.writeCards(w, items)
}

// handleAPIStats returns public archive vitals (for the colophon / easter eggs).
func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	st, _ := s.store.PublicStats(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
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
	isAdmin := s.isAdmin(r)
	it, err := s.store.GetItem(r.Context(), id, isAdmin)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if it == nil {
		s.notFound(w, r)
		return
	}
	pd := s.page(r, firstNonEmpty(it.Title, s.siteTitle()))
	v := s.view(*it)
	pd.Item = &v

	// SEO / Open Graph for this item
	pd.Meta.Type = "article"
	pd.Meta.URL = s.absURL(v.DetailURL)
	pd.Meta.Description = firstNonEmpty(itemDescription(*it), pd.Meta.Description)
	pd.Meta.Image = s.absURL(v.DetailURL + "/og.jpg")
	pd.JSONLD = s.itemJSONLD(v, pd.Nonce)

	// Prev/next within the board ordering (newer / older)
	if prevID, nextID, err := s.store.GetAdjacent(r.Context(), *it, isAdmin); err == nil {
		if prevID != 0 {
			pd.PrevURL = itemPath(prevID)
		}
		if nextID != 0 {
			pd.NextURL = itemPath(nextID)
		}
	}
	if rel, err := s.store.GetRelated(r.Context(), *it, 6, isAdmin); err == nil {
		pd.Related = s.views(rel)
	}
	s.render(w, http.StatusOK, "detail.html", pd)
}

func itemDescription(it store.Item) string {
	return strings.TrimSpace(firstNonEmpty(strings.TrimSpace(it.Note), strings.TrimSpace(it.LinkDescription), it.Title))
}

// itemJSONLD builds schema.org structured data for a detail page, as a whole
// <script> element: json.Marshal escapes <, > and & as \u00xx so there's no
// </script> breakout, and emitting it in HTML context avoids html/template
// applying JS-string escaping inside the tag.
func (s *Server) itemJSONLD(v itemView, nonce string) template.HTML {
	it := v.Item
	name := firstNonEmpty(it.Title, s.siteTitle())
	ld := map[string]any{
		"@context":      "https://schema.org",
		"name":          name,
		"url":           s.absURL(v.DetailURL),
		"datePublished": it.CreatedAt.Format(time.RFC3339),
		"dateModified":  it.UpdatedAt.Format(time.RFC3339),
	}
	if d := itemDescription(it); d != "" {
		ld["description"] = d
	}
	switch {
	case isVideoItem(it) && v.FileURL != "":
		ld["@type"] = "VideoObject"
		ld["contentUrl"] = s.absURL(v.FileURL)
		ld["thumbnailUrl"] = s.absURL(v.DetailURL + "/og.jpg")
		ld["uploadDate"] = ld["datePublished"]
	case v.CoverURL != "":
		ld["@type"] = "ImageObject"
		ld["contentUrl"] = s.absURL(v.CoverURL)
		ld["thumbnailUrl"] = s.absURL(v.ThumbURL)
		if it.Width > 0 && it.Height > 0 {
			ld["width"], ld["height"] = it.Width, it.Height
		}
	default:
		ld["@type"] = "Article"
		ld["headline"] = name
	}
	b, err := json.Marshal(ld)
	if err != nil {
		return ""
	}
	return template.HTML(`<script type="application/ld+json" nonce="` + nonce + `">` + string(b) + `</script>`) //nolint:gosec
}

// render executes a page template fully into a buffer, then writes it once with
// a Content-Length. Buffered delivery doesn't trigger the Firefox theme flash
// (a streamed document can paint a frame before the server-declared theme
// settles), and a template error can't emit a half-written page.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache") // HTML is dynamic; always revalidate
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("server error: %v (%s %s)", err, r.Method, r.URL.Path)
	s.render(w, http.StatusInternalServerError, "500.html", s.page(r, "ERROR"))
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusNotFound, "404.html", s.page(r, "NOT FOUND"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
