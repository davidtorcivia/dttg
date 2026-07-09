package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"donottouchtheglass/internal/store"
)

const feedLimit = 50

func isVideoItem(it store.Item) bool {
	return it.Kind == "embed" && strings.HasPrefix(it.FileMime, "video/")
}

func itemTitleOr(it store.Item) string {
	switch {
	case it.Title != "":
		return it.Title
	case it.FileName != "":
		return it.FileName
	case it.SourceURL != "":
		return it.SourceURL
	}
	return "Untitled"
}

func (s *Server) handleFeedJSON(w http.ResponseWriter, r *http.Request) {
	if s.feedNotModified(w, r) {
		return
	}
	items, _ := s.store.ListItems(r.Context(), store.ItemFilter{Limit: feedLimit})
	type jfAttachment struct {
		URL         string `json:"url"`
		MimeType    string `json:"mime_type"`
		SizeInBytes int64  `json:"size_in_bytes,omitempty"`
	}
	type jf struct {
		ID            string         `json:"id"`
		URL           string         `json:"url"`
		Title         string         `json:"title"`
		ContentText   string         `json:"content_text,omitempty"`
		Image         string         `json:"image,omitempty"`
		DatePublished string         `json:"date_published"`
		Attachments   []jfAttachment `json:"attachments,omitempty"`
	}
	var arr []jf
	for _, it := range items {
		v := s.view(it)
		item := jf{
			ID:            s.absURL(v.DetailURL),
			URL:           s.absURL(v.DetailURL),
			Title:         itemTitleOr(it),
			ContentText:   it.Note,
			Image:         s.absURL(v.CoverURL),
			DatePublished: it.CreatedAt.Format(time.RFC3339),
		}
		if isVideoItem(it) && v.FileURL != "" {
			item.Attachments = []jfAttachment{{
				URL:         s.absURL(v.FileURL),
				MimeType:    it.FileMime,
				SizeInBytes: it.FileSize,
			}}
		}
		arr = append(arr, item)
	}
	w.Header().Set("Content-Type", "application/feed+json; charset=utf-8")
	s.setFeedCacheHeaders(w, r)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":       "https://jsonfeed.org/version/1.1",
		"title":         s.siteTitle(),
		"home_page_url": s.siteBaseURL(),
		"feed_url":      s.siteBaseURL() + "/feed.json",
		"items":         arr,
	})
}

func (s *Server) handleFeedRSS(w http.ResponseWriter, r *http.Request) {
	if s.feedNotModified(w, r) {
		return
	}
	items, _ := s.store.ListItems(r.Context(), store.ItemFilter{Limit: feedLimit})
	type rssEnclosure struct {
		URL    string `xml:"url,attr"`
		Length int64  `xml:"length,attr"`
		Type   string `xml:"type,attr"`
	}
	type rssItem struct {
		Title       string        `xml:"title"`
		Link        string        `xml:"link"`
		GUID        string        `xml:"guid"`
		PubDate     string        `xml:"pubDate"`
		Description string        `xml:"description"`
		Enclosure   *rssEnclosure `xml:"enclosure,omitempty"`
	}
	type channel struct {
		Title       string    `xml:"title"`
		Link        string    `xml:"link"`
		Description string    `xml:"description"`
		Items       []rssItem `xml:"item"`
	}
	type rss struct {
		XMLName xml.Name `xml:"rss"`
		Version string   `xml:"version,attr"`
		Channel channel  `xml:"channel"`
	}
	doc := rss{Version: "2.0", Channel: channel{
		Title:       s.siteTitle(),
		Link:        s.siteBaseURL(),
		Description: s.metaDescription(),
	}}
	for _, it := range items {
		v := s.view(it)
		desc := it.Note
		if v.CoverURL != "" {
			desc = `<img src="` + s.absURL(v.CoverURL) + `" alt=""/>` + desc
		}
		ri := rssItem{
			Title:       itemTitleOr(it),
			Link:        s.absURL(v.DetailURL),
			GUID:        s.absURL(v.DetailURL),
			PubDate:     it.CreatedAt.Format(time.RFC1123Z),
			Description: desc,
		}
		if isVideoItem(it) && v.FileURL != "" {
			ri.Enclosure = &rssEnclosure{
				URL:    s.absURL(v.FileURL),
				Length: it.FileSize,
				Type:   it.FileMime,
			}
		}
		doc.Channel.Items = append(doc.Channel.Items, ri)
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	s.setFeedCacheHeaders(w, r)
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(doc)
}

// feedETag is derived from public item count, max(updated_at), and site identity
// so settings changes (title/base_url) invalidate caches.
func (s *Server) feedETag(r *http.Request) string {
	n, maxU, err := s.store.PublicRevision(r.Context())
	if err != nil {
		return ""
	}
	id := s.siteID()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%d|%s|%s|%s", n, maxU, id.Title, id.BaseURL, id.Description)))
	return `W/"` + hex.EncodeToString(sum[:8]) + `"`
}

func (s *Server) setFeedCacheHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	if et := s.feedETag(r); et != "" {
		w.Header().Set("ETag", et)
	}
}

// feedNotModified writes 304 when If-None-Match matches the current revision.
func (s *Server) feedNotModified(w http.ResponseWriter, r *http.Request) bool {
	et := s.feedETag(r)
	if et == "" {
		return false
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == et {
		w.Header().Set("ETag", et)
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}
