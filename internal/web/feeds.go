package web

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"time"

	"donottouchtheglass/internal/store"
)

const feedLimit = 50

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
	items, _ := s.store.ListItems(r.Context(), store.ItemFilter{Limit: feedLimit})
	type jf struct {
		ID            string `json:"id"`
		URL           string `json:"url"`
		Title         string `json:"title"`
		ContentText   string `json:"content_text,omitempty"`
		Image         string `json:"image,omitempty"`
		DatePublished string `json:"date_published"`
	}
	var arr []jf
	for _, it := range items {
		v := s.view(it)
		arr = append(arr, jf{
			ID:            s.absURL(v.DetailURL),
			URL:           s.absURL(v.DetailURL),
			Title:         itemTitleOr(it),
			ContentText:   it.Note,
			Image:         s.absURL(v.CoverURL),
			DatePublished: it.CreatedAt.Format(time.RFC3339),
		})
	}
	w.Header().Set("Content-Type", "application/feed+json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":       "https://jsonfeed.org/version/1.1",
		"title":         s.cfg.SiteTitle,
		"home_page_url": s.cfg.BaseURL,
		"feed_url":      s.cfg.BaseURL + "/feed.json",
		"items":         arr,
	})
}

func (s *Server) handleFeedRSS(w http.ResponseWriter, r *http.Request) {
	items, _ := s.store.ListItems(r.Context(), store.ItemFilter{Limit: feedLimit})
	type rssItem struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		GUID        string `xml:"guid"`
		PubDate     string `xml:"pubDate"`
		Description string `xml:"description"`
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
		Title:       s.cfg.SiteTitle,
		Link:        s.cfg.BaseURL,
		Description: s.cfg.SiteTitle + " — a personal visual archive.",
	}}
	for _, it := range items {
		v := s.view(it)
		desc := it.Note
		if v.CoverURL != "" {
			desc = `<img src="` + s.absURL(v.CoverURL) + `" alt=""/>` + desc
		}
		doc.Channel.Items = append(doc.Channel.Items, rssItem{
			Title:       itemTitleOr(it),
			Link:        s.absURL(v.DetailURL),
			GUID:        s.absURL(v.DetailURL),
			PubDate:     it.CreatedAt.Format(time.RFC1123Z),
			Description: desc,
		})
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(doc)
}
