package ingest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	maxDownload = 27 << 20 // 27 MB per fetch — headroom over the 25 MB video embed cap
	userAgent   = "dnttg/1.0 (+archive bot)"
)

type fetchResult struct {
	Body        []byte
	ContentType string // lowercased, no parameters
	FinalURL    string // after redirects
}

func (s *Service) fetch(ctx context.Context, rawurl string) (*fetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", rawurl, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload))
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return &fetchResult{
		Body:        body,
		ContentType: strings.TrimSpace(strings.ToLower(ct)),
		FinalURL:    resp.Request.URL.String(),
	}, nil
}

// absoluteURL resolves a possibly-relative ref against base.
func absoluteURL(base, ref string) string {
	if ref == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func hostOf(raw string) string {
	p, err := url.Parse(raw)
	if err != nil || p.Host == "" {
		return ""
	}
	return strings.TrimPrefix(p.Host, "www.")
}

func extForContentType(ct string) string {
	switch ct {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/avif":
		return ".avif"
	}
	return ".bin"
}
