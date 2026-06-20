package web

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

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
}

func (s *Server) page(r *http.Request, title string) pageData {
	isAdmin := s.isAdmin(r)
	cats, err := s.store.ListCategories(r.Context(), isAdmin)
	if err != nil {
		log.Printf("list categories: %v", err)
	}
	pd := pageData{
		Cfg:        s.cfg,
		IsAdmin:    isAdmin,
		Categories: cats,
		PageTitle:  title,
		Meta: metaTags{
			Description: s.cfg.SiteTitle + " — a personal visual archive.",
			URL:         s.absURL(r.URL.Path),
			Image:       s.absURL("/static/icons/icon-512.png"),
			Type:        "website",
		},
	}
	// Inject the analytics snippet on public views only (don't track admin/self).
	if !isAdmin {
		if ts, _ := s.store.GetSetting(r.Context(), "tracking_script"); ts != "" {
			pd.TrackingScript = template.HTML(ts) //nolint:gosec // admin-provided, trusted
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
			s.serverError(w, err)
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

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	f := store.ItemFilter{IncludePrivate: s.isAdmin(r), Limit: 120}
	slug := r.PathValue("slug")
	switch {
	case strings.HasPrefix(r.URL.Path, "/category/"):
		f.CategorySlug = slug
	case strings.HasPrefix(r.URL.Path, "/tag/"):
		f.TagSlug = slug
	}

	items, err := s.store.ListItems(r.Context(), f)
	if err != nil {
		s.serverError(w, err)
		return
	}

	pd := s.page(r, s.cfg.SiteTitle)
	pd.ActiveCat = f.CategorySlug
	pd.ActiveTag = f.TagSlug
	for _, it := range items {
		pd.Items = append(pd.Items, s.view(it))
	}
	s.render(w, "index.html", pd)
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	it, err := s.store.GetItem(r.Context(), id, s.isAdmin(r))
	if err != nil {
		s.serverError(w, err)
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
	if v.CoverURL != "" {
		pd.Meta.Image = s.absURL(v.CoverURL)
	}
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

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	log.Printf("server error: %v", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if err := s.tmpl.ExecuteTemplate(w, "404.html", s.page(r, "NOT FOUND")); err != nil {
		log.Printf("render 404: %v", err)
	}
}
