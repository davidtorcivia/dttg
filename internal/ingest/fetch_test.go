package ingest

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchConditionalAndSizeLimit(t *testing.T) {
	var got *http.Request
	s := &Service{
		http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = r
			h := make(http.Header)
			h.Set("Content-Type", "application/feed+json; charset=utf-8")
			h.Set("ETag", `"abc"`)
			h.Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     h,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				Request:    r,
			}, nil
		})},
	}

	res, err := s.Fetch(context.Background(), "https://example.com/feed.json", FetchOptions{
		Accept:       "application/feed+json, application/json;q=0.9",
		ETag:         `"prev"`,
		LastModified: "Sun, 01 Jan 2006 00:00:00 GMT",
		MaxBytes:     1024,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got == nil {
		t.Fatal("request not captured")
	}
	if got.Header.Get("Accept") != "application/feed+json, application/json;q=0.9" {
		t.Errorf("Accept = %q", got.Header.Get("Accept"))
	}
	if got.Header.Get("If-None-Match") != `"prev"` {
		t.Errorf("If-None-Match = %q", got.Header.Get("If-None-Match"))
	}
	if got.Header.Get("If-Modified-Since") != "Sun, 01 Jan 2006 00:00:00 GMT" {
		t.Errorf("If-Modified-Since = %q", got.Header.Get("If-Modified-Since"))
	}
	if res.NotModified {
		t.Error("expected NotModified=false")
	}
	if res.ContentType != "application/feed+json" {
		t.Errorf("ContentType = %q", res.ContentType)
	}
	if res.ETag != `"abc"` {
		t.Errorf("ETag = %q", res.ETag)
	}
	if string(res.Body) != `{"ok":true}` {
		t.Errorf("Body = %q", res.Body)
	}

	// 304 Not Modified
	s.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := make(http.Header)
		h.Set("ETag", `"abc"`)
		h.Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		return &http.Response{
			StatusCode: http.StatusNotModified,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})}
	res304, err := s.Fetch(context.Background(), "https://example.com/feed.json", FetchOptions{
		ETag: `"abc"`,
	})
	if err != nil {
		t.Fatalf("Fetch 304: %v", err)
	}
	if !res304.NotModified {
		t.Error("expected NotModified=true")
	}
	if res304.ETag != `"abc"` {
		t.Errorf("304 ETag = %q", res304.ETag)
	}

	// Body longer than MaxBytes
	big := strings.Repeat("x", 20)
	s.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       io.NopCloser(strings.NewReader(big)),
			Request:    r,
		}, nil
	})}
	if _, err := s.Fetch(context.Background(), "https://example.com/feed.json", FetchOptions{MaxBytes: 10}); err == nil {
		t.Fatal("expected size limit error")
	}
}
