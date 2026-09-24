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
	return firstNonEmpty(it.Title, it.FileName, it.SourceURL, "Untitled")
}

func (s *Server) handleFeedJSON(w http.ResponseWriter, r *http.Request) {
	if s.feedCached(w, r) {
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
		ExternalURL   string         `json:"external_url,omitempty"`
		Title         string         `json:"title"`
		ContentText   string         `json:"content_text,omitempty"`
		Summary       string         `json:"summary,omitempty"`
		Image         string         `json:"image,omitempty"`
		DatePublished string         `json:"date_published"`
		DateModified  string         `json:"date_modified,omitempty"`
		Attachments   []jfAttachment `json:"attachments,omitempty"`
	}
	arr := make([]jf, 0, len(items)) // the spec requires an array, never null
	for _, it := range items {
		v := s.view(it)
		item := jf{
			ID:            s.absURL(v.DetailURL),
			URL:           s.absURL(v.DetailURL),
			ExternalURL:   it.SourceURL,
			Title:         itemTitleOr(it),
			ContentText:   it.Note,
			Summary:       firstNonEmpty(it.LinkDescription, it.Note, it.Title),
			Image:         s.absURL(v.CoverURL),
			DatePublished: it.CreatedAt.Format(time.RFC3339),
			DateModified:  it.UpdatedAt.Format(time.RFC3339),
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
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":       "https://jsonfeed.org/version/1.1",
		"title":         s.siteTitle(),
		"home_page_url": s.siteBaseURL(),
		"feed_url":      s.siteBaseURL() + "/feed.json",
		"description":   s.metaDescription(),
		"icon":          s.absURL("/static/icons/icon-512.png"),
		"favicon":       s.absURL("/favicon.svg"),
		"items":         arr,
	})
}

func (s *Server) handleFeedRSS(w http.ResponseWriter, r *http.Request) {
	if s.feedCached(w, r) {
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
	writeXML(w, doc)
}

// feedCached sets the ETag/Cache-Control for a public feed or sitemap and
// answers 304 when the client already has the current revision. The ETag
// covers public item count, max(updated_at), and site identity, so settings
// changes (title/base_url) invalidate caches too.
func (s *Server) feedCached(w http.ResponseWriter, r *http.Request) bool {
	st, err := s.store.PublicStats(r.Context())
	if err != nil {
		return false
	}
	id := s.siteID()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%d|%s|%s|%s", st.Count, st.Updated, id.Title, id.BaseURL, id.Description)))
	et := `W/"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", et)
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Header.Get("If-None-Match") == et {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}
