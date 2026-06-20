package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

var (
	// tweet URL with optional 1-based /photo/N index
	tweetURLRe = regexp.MustCompile(`(?i)^https?://(?:x|twitter|fxtwitter|vxtwitter|fixupx)\.com/[^/]+/status/(\d+)(?:/photo/(\d+))?`)
	// canonical tweet URL (drops /photo/N, query, etc.)
	tweetBaseRe = regexp.MustCompile(`(?i)^(https?://(?:x|twitter)\.com/[^/]+/status/\d+)`)
)

// isTweetURL reports whether raw is a tweet URL, returning the tweet id and an
// optional 1-based photo index (from /photo/N, 0 if absent).
func isTweetURL(raw string) (id string, photoIdx int, ok bool) {
	m := tweetURLRe.FindStringSubmatch(raw)
	if m == nil {
		return "", 0, false
	}
	if m[2] != "" {
		photoIdx, _ = strconv.Atoi(m[2])
	}
	return m[1], photoIdx, true
}

// cleanTweetURL returns the canonical tweet URL (drops /photo/N and query).
func cleanTweetURL(raw string) string {
	if m := tweetBaseRe.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	return raw
}

type tweetMedia struct {
	Text   string
	Author string
	Photos []string
}

// photo returns the URL of the 1-based idx photo, or the first photo, or "".
func (t *tweetMedia) photo(idx int) string {
	if idx >= 1 && idx <= len(t.Photos) {
		return t.Photos[idx-1]
	}
	if len(t.Photos) > 0 {
		return t.Photos[0]
	}
	return ""
}

// fetchTweet pulls a tweet's text + individual photo URLs via the FixTweet
// (fxtwitter) API — no auth, returns each photo separately.
func (s *Service) fetchTweet(ctx context.Context, id string) (*tweetMedia, error) {
	res, err := s.fetch(ctx, "https://api.fxtwitter.com/status/"+id)
	if err != nil {
		return nil, err
	}
	var data struct {
		Code  int `json:"code"`
		Tweet struct {
			Text   string `json:"text"`
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
			Media struct {
				Photos []struct {
					URL string `json:"url"`
				} `json:"photos"`
			} `json:"media"`
		} `json:"tweet"`
	}
	if err := json.Unmarshal(res.Body, &data); err != nil {
		return nil, err
	}
	if data.Code != 200 {
		return nil, fmt.Errorf("fxtwitter: code %d", data.Code)
	}
	t := &tweetMedia{Text: data.Tweet.Text, Author: data.Tweet.Author.Name}
	for _, p := range data.Tweet.Media.Photos {
		if p.URL != "" {
			t.Photos = append(t.Photos, p.URL)
		}
	}
	return t, nil
}
