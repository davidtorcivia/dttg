package web

import (
	"encoding/xml"
	"log"
	"net/http"
	"strconv"

	"donottouchtheglass/internal/store"
)

// URLs per child sitemap — under the 50k spec cap, leaving room for the home /
// category / tag URLs that ride along in the first chunk.
const sitemapChunk = 40000

type sitemapURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

type sitemapURLSet struct {
	XMLName xml.Name     `xml:"urlset"`
	XMLNS   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

type sitemapRef struct {
	Loc string `xml:"loc"`
}

type sitemapIndex struct {
	XMLName  xml.Name     `xml:"sitemapindex"`
	XMLNS    string       `xml:"xmlns,attr"`
	Sitemaps []sitemapRef `xml:"sitemap"`
}

// handleSitemap serves /sitemap.xml. For a small archive it's a single urlset; once
// the item count exceeds one chunk it becomes a <sitemapindex> pointing at child
// sitemaps (/sitemap.xml?p=N), each a urlset of up to sitemapChunk items.
func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	if s.feedNotModified(w, r) {
		return
	}
	total, err := s.store.CountItems(r.Context(), false)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	s.setFeedCacheHeaders(w, r)

	page := r.URL.Query().Get("p")
	switch {
	case page == "" && total <= sitemapChunk:
		s.writeURLSet(w, r, 0, true) // single file: all items + aux
	case page == "":
		s.writeSitemapIndex(w, total)
	default:
		n, _ := strconv.Atoi(page)
		if n < 1 {
			n = 1
		}
		s.writeURLSet(w, r, n, n == 1) // aux rides in the first chunk
	}
}

// writeURLSet writes a urlset. chunk 0 = every public item (small archive); chunk
// >=1 = the items for that page. includeAux adds home + category + tag indexes.
func (s *Server) writeURLSet(w http.ResponseWriter, r *http.Request, chunk int, includeAux bool) {
	set := sitemapURLSet{XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	add := func(path, lastmod string) {
		set.URLs = append(set.URLs, sitemapURL{Loc: s.absURL(path), LastMod: lastmod})
	}
	if includeAux {
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
	}
	f := store.ItemFilter{} // Limit 0 => all
	if chunk >= 1 {
		f.Limit = sitemapChunk
		f.Offset = (chunk - 1) * sitemapChunk
	}
	items, err := s.store.ListItems(r.Context(), f)
	if err != nil {
		s.serverError(w, r, err)
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
	writeXML(w, set)
}

func (s *Server) writeSitemapIndex(w http.ResponseWriter, total int) {
	idx := sitemapIndex{XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	chunks := (total + sitemapChunk - 1) / sitemapChunk
	for n := 1; n <= chunks; n++ {
		idx.Sitemaps = append(idx.Sitemaps, sitemapRef{Loc: s.absURL("/sitemap.xml?p=" + strconv.Itoa(n))})
	}
	writeXML(w, idx)
}

func writeXML(w http.ResponseWriter, v any) {
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("sitemap encode: %v", err)
	}
	_, _ = w.Write([]byte("\n"))
}

// handleRobots serves a minimal robots.txt that points crawlers at the sitemap
// and keeps admin/api paths out.
func (s *Server) handleRobots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("User-agent: *\n" +
		"Disallow: /admin\n" +
		"Disallow: /api/\n" +
		"Allow: /\n" +
		"Sitemap: " + s.siteBaseURL() + "/sitemap.xml\n"))
}
