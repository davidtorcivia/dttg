package web

import (
	"encoding/xml"
	"log"
	"net/http"
	"strconv"

	"donottouchtheglass/internal/store"
)

type sitemapURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

type sitemapURLSet struct {
	XMLName xml.Name     `xml:"urlset"`
	XMLNS   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

// handleSitemap serves /sitemap.xml: the home page, every public category and
// tag index, and every public item. Mirrors the public visibility rules used by
// the feeds (private items are omitted).
func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	set := sitemapURLSet{XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	add := func(path, lastmod string) {
		set.URLs = append(set.URLs, sitemapURL{Loc: s.absURL(path), LastMod: lastmod})
	}

	add("/", "")
	if cats, err := s.store.ListCategories(r.Context(), false); err == nil {
		for _, c := range cats {
			add("/category/"+c.Slug, "")
		}
	}
	if tags, err := s.store.ListTags(r.Context()); err == nil {
		for _, t := range tags {
			add("/tag/"+t.Slug, "")
		}
	}
	items, err := s.store.ListItems(r.Context(), store.ItemFilter{}) // Limit 0 => all public
	if err != nil {
		s.serverError(w, err)
		return
	}
	for _, it := range items {
		lm := it.UpdatedAt
		if lm.IsZero() {
			lm = it.CreatedAt
		}
		mod := ""
		if !lm.IsZero() {
			mod = lm.UTC().Format("2006-01-02")
		}
		add("/item/"+strconv.FormatInt(it.ID, 10), mod)
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(set); err != nil {
		log.Printf("sitemap: %v", err)
	}
	_, _ = w.Write([]byte("\n"))
}

// handleRobots serves a minimal robots.txt that points crawlers at the sitemap
// (so the sitemap is actually discoverable) and keeps admin/api paths out.
func (s *Server) handleRobots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("User-agent: *\n" +
		"Disallow: /admin\n" +
		"Disallow: /api/\n" +
		"Allow: /\n" +
		"Sitemap: " + s.cfg.BaseURL + "/sitemap.xml\n"))
}
