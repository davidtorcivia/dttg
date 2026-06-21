package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
)

// embedHostAllow is the set of hosts whose oEmbed iframes we trust.
var embedHostAllow = []string{"youtube.com", "youtube-nocookie.com", "youtu.be", "player.vimeo.com", "vimeo.com"}

var iframeSrcRe = regexp.MustCompile(`(?i)<iframe[^>]*\ssrc=["']([^"']+)["']`)

// sanitizeEmbedHTML reduces oEmbed HTML to a single minimal iframe whose src is on
// the allowlist — so we never store/serve arbitrary provider HTML. Returns "" if
// no allowlisted iframe is found (the caller decides how to fall back). The
// .embed-wrap CSS sizes the iframe, so width/height are dropped.
func sanitizeEmbedHTML(raw string) string {
	m := iframeSrcRe.FindStringSubmatch(raw)
	if len(m) < 2 {
		return ""
	}
	src := m[1]
	u, err := url.Parse(src)
	if err != nil || u.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(u.Host)
	allowed := false
	for _, h := range embedHostAllow {
		if host == h || strings.HasSuffix(host, "."+h) {
			allowed = true
			break
		}
	}
	if !allowed {
		return ""
	}
	return `<iframe src="` + html.EscapeString(src) + `" loading="lazy" ` +
		`referrerpolicy="strict-origin-when-cross-origin" ` +
		`allow="autoplay; encrypted-media; picture-in-picture; fullscreen" allowfullscreen></iframe>`
}

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
	clean := sanitizeEmbedHTML(data.HTML)
	if clean == "" {
		clean = data.HTML // trusted provider, unexpected markup — keep it (CSP frame-src still constrains)
	}
	return &embedInfo{Provider: provider, HTML: clean, ThumbnailURL: data.ThumbnailURL, Title: data.Title}, nil
}
