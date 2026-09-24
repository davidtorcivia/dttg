package web

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"donottouchtheglass/internal/store"
)

// The CSP (script-src 'self' 'nonce-…') silently blocks inline event handlers,
// so e.g. onsubmit="return confirm(…)" would never run. Use data-confirm.
func TestTemplatesHaveNoInlineHandlers(t *testing.T) {
	re := regexp.MustCompile(`\son[a-z]+\s*=`)
	_ = fs.WalkDir(templatesFS, "templates", func(p string, d fs.DirEntry, _ error) error {
		if b, _ := fs.ReadFile(templatesFS, p); !d.IsDir() && re.Match(b) {
			t.Errorf("%s: inline event handler %q", p, re.Find(b))
		}
		return nil
	})
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/admin?x=1": "/admin?x=1", "": "/", "https://evil.com": "/",
		"//evil.com": "/", `/\evil.com`: "/", "/ok\r\nSet-Cookie:x": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMediaRangeRequests(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	key := "items/vid/video.mp4"
	id, _ := s.store.CreateItem(ctx, store.Item{Kind: "embed", Visibility: "public", FileKey: key, FileMime: "video/mp4"})
	_ = s.media.Put(ctx, key, "video/mp4", []byte("0123456789"), false)
	_ = s.store.UpsertMedia(ctx, store.Media{ItemID: id, Variant: "video", StorageKey: key, ContentType: "video/mp4", OnLocal: true})

	req := httptest.NewRequest(http.MethodGet, "/media/"+key, nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "2345" {
		t.Fatalf("range = %d %q, want 206 \"2345\"", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestAnonymousShareIsRateLimited(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	codes := []int{}
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/share", strings.NewReader("url=https%3A%2F%2Fexample.com"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[0] != http.StatusSeeOther || codes[4] != http.StatusTooManyRequests {
		t.Fatalf("share codes = %v, want 303 first and 429 once the burst is spent", codes)
	}
}

// With R2-hosted media but no R2 configured, the orphan scan must refuse rather
// than report (and offer to delete) every R2-hosted item as broken.
func TestScanRefusesWithoutR2(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	id, _ := s.store.CreateItem(ctx, store.Item{Kind: "image", Visibility: "public", CoverKey: "items/r2/full.jpg"})
	_ = s.store.UpsertMedia(ctx, store.Media{ItemID: id, Variant: "full", StorageKey: "items/r2/full.jpg", OnR2: true})
	if _, err := s.scanOrphans(ctx); err == nil {
		t.Fatal("scan should refuse when rows point at R2 but R2 isn't configured")
	}
}
