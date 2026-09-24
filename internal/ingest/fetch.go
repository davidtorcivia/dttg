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

// FetchOptions controls conditional GET headers and response size limits.
type FetchOptions struct {
	Accept       string
	ETag         string
	LastModified string
	MaxBytes     int64
}

// FetchResult is the outcome of a safe HTTP GET, including conditional-GET state.
type FetchResult struct {
	Body         []byte
	ContentType  string // lowercased, no parameters
	FinalURL     string // after redirects
	ETag         string
	LastModified string
	NotModified  bool
}

// Fetch performs an SSRF-safe GET with optional conditional headers.
func (s *Service) Fetch(ctx context.Context, rawurl string, opt FetchOptions) (*FetchResult, error) {
	if err := validateFetchURL(rawurl); err != nil {
		return nil, fmt.Errorf("fetch blocked: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", orDefault(opt.Accept, "*/*"))
	if opt.ETag != "" {
		req.Header.Set("If-None-Match", opt.ETag)
	}
	if opt.LastModified != "" {
		req.Header.Set("If-Modified-Since", opt.LastModified)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	res := &FetchResult{
		FinalURL:     resp.Request.URL.String(),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}
	if resp.StatusCode == http.StatusNotModified {
		res.NotModified = true
		return res, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", rawurl, resp.StatusCode)
	}
	limit := int64(maxDownload)
	if opt.MaxBytes > 0 {
		limit = opt.MaxBytes
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("download too large: Content-Length %d", resp.ContentLength)
	}
	// Read at most limit+1 so we can detect truncation without storing it.
	if res.Body, err = io.ReadAll(io.LimitReader(resp.Body, limit+1)); err != nil {
		return nil, err
	}
	if int64(len(res.Body)) > limit {
		return nil, fmt.Errorf("download too large")
	}
	ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	res.ContentType = strings.TrimSpace(strings.ToLower(ct))
	return res, nil
}

func (s *Service) fetch(ctx context.Context, rawurl string) (*FetchResult, error) {
	return s.Fetch(ctx, rawurl, FetchOptions{})
}

// absoluteURL resolves a possibly-relative ref against base.
func absoluteURL(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil || ref == "" {
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
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(p.Host, "www.")
}
