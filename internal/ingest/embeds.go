package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type embedInfo struct {
	Provider     string
	HTML         string
	ThumbnailURL string
	Title        string
}

// embedProviderFor returns a supported oEmbed provider name for a URL, or "".
func embedProviderFor(rawurl string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	h := strings.ToLower(u.Host)
	switch {
	case strings.Contains(h, "youtube.com"), strings.Contains(h, "youtu.be"):
		return "YouTube"
	case strings.Contains(h, "vimeo.com"):
		return "Vimeo"
	}
	return ""
}

func (s *Service) fetchEmbed(ctx context.Context, rawurl, provider string) (*embedInfo, error) {
	q := url.Values{"url": {rawurl}, "format": {"json"}}
	var endpoint string
	switch provider {
	case "YouTube":
		endpoint = "https://www.youtube.com/oembed?" + q.Encode()
	case "Vimeo":
		endpoint = "https://vimeo.com/api/oembed.json?" + q.Encode()
	default:
		return nil, fmt.Errorf("unsupported embed provider %q", provider)
	}
	res, err := s.fetch(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var data struct {
		HTML         string `json:"html"`
		ThumbnailURL string `json:"thumbnail_url"`
		Title        string `json:"title"`
	}
	if err := json.Unmarshal(res.Body, &data); err != nil {
		return nil, err
	}
	return &embedInfo{Provider: provider, HTML: data.HTML, ThumbnailURL: data.ThumbnailURL, Title: data.Title}, nil
}
