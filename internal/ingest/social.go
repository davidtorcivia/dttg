package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

var (
	// Bluesky post URL: https://bsky.app/profile/<handle-or-did>/post/<rkey>
	bskyURLRe = regexp.MustCompile(`(?i)^https?://(?:www\.)?bsky\.app/profile/([^/?#]+)/post/([^/?#]+)`)
	// Mastodon / Fediverse status URL: https://<host>/@<user>/<id> or /users/<user>/statuses/<id>
	mastodonAtRe    = regexp.MustCompile(`(?i)^https?://([^/?#]+)/@[^/?#]+/(\d+)(?:[/?#].*)?$`)
	mastodonUsersRe = regexp.MustCompile(`(?i)^https?://([^/?#]+)/users/[^/?#]+/statuses/(\d+)(?:[/?#].*)?$`)
)

type socialPost struct {
	Author   string
	SiteName string
	Text     string
	Photos   []tweetPhoto
	VideoURL string
	Poster   string
}

func isBlueskyURL(raw string) (actor, rkey string, ok bool) {
	m := bskyURLRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func isMastodonURL(raw string) (scheme, host, statusID string, ok bool) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", "", false
	}
	// Don't mistake non-Mastodon platforms for Mastodon.
	lh := strings.ToLower(u.Hostname())
	for _, skip := range []string{"youtube.com", "tiktok.com", "threads.net", "threads.com", "medium.com", "x.com", "twitter.com", "bsky.app"} {
		if lh == skip || strings.HasSuffix(lh, "."+skip) {
			return "", "", "", false
		}
	}
	if m := mastodonAtRe.FindStringSubmatch(raw); m != nil {
		return u.Scheme, u.Host, m[2], true
	}
	if m := mastodonUsersRe.FindStringSubmatch(raw); m != nil {
		return u.Scheme, u.Host, m[2], true
	}
	return "", "", "", false
}

// fetchBluesky pulls a Bluesky post's text, author, and all embedded photos
// (with alt text) from the public unauthenticated AppView API.
func (s *Service) fetchBluesky(ctx context.Context, actor, rkey string) (*socialPost, error) {
	base := s.bskyAPIBase
	if base == "" {
		base = "https://public.api.bsky.app"
	}
	atURI := "at://" + actor + "/app.bsky.feed.post/" + rkey
	endpoint := strings.TrimRight(base, "/") + "/xrpc/app.bsky.feed.getPostThread?depth=0&parentHeight=0&uri=" + url.QueryEscape(atURI)
	res, err := s.fetch(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	type bskyImg struct {
		Fullsize string `json:"fullsize"`
		Thumb    string `json:"thumb"`
		Alt      string `json:"alt"`
	}
	var data struct {
		Thread struct {
			Post struct {
				Author struct {
					DisplayName string `json:"displayName"`
					Handle      string `json:"handle"`
				} `json:"author"`
				Record struct {
					Text string `json:"text"`
				} `json:"record"`
				Embed struct {
					Images []bskyImg `json:"images"`
					Media  *struct {
						Images []bskyImg `json:"images"`
					} `json:"media"`
				} `json:"embed"`
			} `json:"post"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(res.Body, &data); err != nil {
		return nil, err
	}
	post := data.Thread.Post
	author := strings.TrimSpace(post.Author.DisplayName)
	if author == "" && post.Author.Handle != "" {
		author = "@" + post.Author.Handle
	}
	sp := &socialPost{
		Author:   author,
		SiteName: "bsky.app",
		Text:     strings.TrimSpace(post.Record.Text),
	}
	imgs := post.Embed.Images
	if len(imgs) == 0 && post.Embed.Media != nil {
		imgs = post.Embed.Media.Images
	}
	for _, im := range imgs {
		u := strings.TrimSpace(im.Fullsize)
		if u == "" {
			u = strings.TrimSpace(im.Thumb)
		}
		if u != "" {
			sp.Photos = append(sp.Photos, tweetPhoto{URL: u, Alt: strings.TrimSpace(im.Alt)})
		}
	}
	if sp.Author == "" && sp.Text == "" && len(sp.Photos) == 0 {
		return nil, fmt.Errorf("bsky: empty post payload")
	}
	return sp, nil
}

// fetchMastodon pulls a public Fediverse status via the standard Mastodon v1 API.
func (s *Service) fetchMastodon(ctx context.Context, scheme, host, statusID string) (*socialPost, error) {
	endpoint := scheme + "://" + host + "/api/v1/statuses/" + statusID
	res, err := s.fetch(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var data struct {
		Content string `json:"content"`
		Account struct {
			DisplayName string `json:"display_name"`
			Username    string `json:"username"`
		} `json:"account"`
		MediaAttachments []struct {
			Type        string `json:"type"`
			URL         string `json:"url"`
			PreviewURL  string `json:"preview_url"`
			Description string `json:"description"`
		} `json:"media_attachments"`
	}
	if err := json.Unmarshal(res.Body, &data); err != nil {
		return nil, err
	}
	author := strings.TrimSpace(data.Account.DisplayName)
	if author == "" && data.Account.Username != "" {
		author = "@" + data.Account.Username
	}
	sp := &socialPost{
		Author:   author,
		SiteName: strings.TrimPrefix(strings.ToLower(host), "www."),
		Text:     stripHTMLText(data.Content),
	}
	for _, m := range data.MediaAttachments {
		switch strings.ToLower(m.Type) {
		case "image":
			if u := strings.TrimSpace(m.URL); u != "" {
				sp.Photos = append(sp.Photos, tweetPhoto{URL: u, Alt: strings.TrimSpace(m.Description)})
			}
		case "video", "gifv":
			if sp.VideoURL == "" && m.URL != "" {
				sp.VideoURL = strings.TrimSpace(m.URL)
				sp.Poster = strings.TrimSpace(m.PreviewURL)
			}
		}
	}
	if sp.Author == "" && sp.Text == "" && len(sp.Photos) == 0 && sp.VideoURL == "" {
		return nil, fmt.Errorf("mastodon: empty status")
	}
	return sp, nil
}

// stripHTMLText extracts readable plain text from Mastodon HTML status bodies.
func stripHTMLText(raw string) string {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && (n.Data == "br" || n.Data == "p") && b.Len() > 0 {
			b.WriteByte('\n')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return strings.TrimSpace(b.String())
}
