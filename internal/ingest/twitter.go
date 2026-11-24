package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	// tweet URL with optional /photo/N or /video/N suffix, supporting x.com,
	// twitter.com (including www/mobile), and FixTweet proxy domains.
	tweetURLRe = regexp.MustCompile(`(?i)^https?://(?:(?:www|mobile)\.)?(?:x|twitter|fxtwitter|vxtwitter|fixupx|fixvx)\.com/(?:(i/web)|([^/]+))/status(?:es)?/(\d+)(?:/(?:photo|video)/(\d+))?`)
)

// IsTweetURL reports whether raw is a supported X/Twitter status URL.
func IsTweetURL(raw string) bool {
	_, _, ok := isTweetURL(raw)
	return ok
}

// CleanTweetURL returns the canonical x.com tweet URL (drops /photo/N, proxy
// domains, mobile/www prefixes, and query params).
func CleanTweetURL(raw string) string { return cleanTweetURL(raw) }

// isTweetURL reports whether raw is a tweet URL, returning the tweet id and an
// optional 1-based photo index (from /photo/N, 0 if absent).
func isTweetURL(raw string) (id string, photoIdx int, ok bool) {
	m := tweetURLRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", 0, false
	}
	if m[4] != "" {
		photoIdx, _ = strconv.Atoi(m[4])
	}
	return m[3], photoIdx, true
}

// cleanTweetURL returns the canonical x.com tweet URL (drops /photo/N, proxy
// domains, mobile/www prefixes, and query params).
func cleanTweetURL(raw string) string {
	m := tweetURLRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return raw
	}
	user := m[2]
	if user == "" {
		user = "i"
	}
	return "https://x.com/" + user + "/status/" + m[3]
}

type tweetPhoto struct {
	URL string
	Alt string
}

type tweetMedia struct {
	Text       string
	Author     string
	Handle     string
	Photos     []tweetPhoto
	VideoURL   string // direct .mp4 of the tweet's video, if any
	VideoThumb string // poster frame for the video
}

// photo returns the URL of the 1-based idx photo, or the first photo, or "".
func (t *tweetMedia) photo(idx int) string {
	if idx >= 1 && idx <= len(t.Photos) {
		return t.Photos[idx-1].URL
	}
	if len(t.Photos) > 0 {
		return t.Photos[0].URL
	}
	return ""
}

// fetchTweet pulls a tweet's text + all individual photo URLs (with alt text)
// via the FixTweet (fxtwitter) API — no auth, returns each photo separately.
func (s *Service) fetchTweet(ctx context.Context, id string) (*tweetMedia, error) {
	base := s.tweetAPIBase
	if base == "" {
		base = "https://api.fxtwitter.com"
	}
	res, err := s.fetch(ctx, strings.TrimRight(base, "/")+"/status/"+id)
	if err != nil {
		return nil, err
	}
	var data struct {
		Code  int `json:"code"`
		Tweet struct {
			Text   string `json:"text"`
			Author struct {
				Name       string `json:"name"`
				ScreenName string `json:"screen_name"`
			} `json:"author"`
			Media struct {
				Photos []struct {
					URL     string `json:"url"`
					AltText string `json:"altText"`
				} `json:"photos"`
				Videos []struct {
					URL          string `json:"url"`
					ThumbnailURL string `json:"thumbnail_url"`
				} `json:"videos"`
				All []struct {
					Type    string `json:"type"`
					URL     string `json:"url"`
					AltText string `json:"altText"`
				} `json:"all"`
			} `json:"media"`
		} `json:"tweet"`
	}
	if err := json.Unmarshal(res.Body, &data); err != nil {
		return nil, err
	}
	if data.Code != 200 {
		return nil, fmt.Errorf("fxtwitter: code %d", data.Code)
	}
	author := strings.TrimSpace(data.Tweet.Author.Name)
	handle := strings.TrimSpace(data.Tweet.Author.ScreenName)
	if author == "" && handle != "" {
		author = "@" + handle
	}
	t := &tweetMedia{
		Text:   strings.TrimSpace(data.Tweet.Text),
		Author: author,
		Handle: handle,
	}
	seen := map[string]bool{}
	addPhoto := func(u, alt string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		t.Photos = append(t.Photos, tweetPhoto{URL: u, Alt: strings.TrimSpace(alt)})
	}
	for _, p := range data.Tweet.Media.Photos {
		addPhoto(p.URL, p.AltText)
	}
	// Some tweets (mixed media or cards) expose photo entries only in media.all.
	if len(t.Photos) == 0 {
		for _, a := range data.Tweet.Media.All {
			if strings.EqualFold(a.Type, "photo") {
				addPhoto(a.URL, a.AltText)
			}
		}
	}
	for _, v := range data.Tweet.Media.Videos {
		if v.URL != "" {
			t.VideoURL, t.VideoThumb = v.URL, v.ThumbnailURL
			break // archive the first video
		}
	}
	return t, nil
}
