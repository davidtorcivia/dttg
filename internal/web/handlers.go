package web

import (
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
	}
	// Inject the analytics snippet on public views only (don't track admin/self).
	if !isAdmin {
		if ts, _ := s.store.GetSetting(r.Context(), "tracking_script"); ts != "" {
			pd.TrackingScript = template.HTML(ts) //nolint:gosec // admin-provided, trusted
		}
	}
	return pd
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
	s.render(w, "detail.html", pd)
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
