package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"donottouchtheglass/internal/store"
)

var cspNonceRe = regexp.MustCompile(`'nonce-([^']+)'`)

func TestPublicPageCacheETagNonceAndAdminBypass(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	id, err := s.store.CreateItem(context.Background(), store.Item{
		Kind: "text", Title: "Cache Plate", Note: "Instantaneous exhibition", Visibility: "public",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. First anonymous GET / is a cache miss that populates ETag + SWR headers.
	rec1 := getReq(t, h, "/")
	if rec1.Code != http.StatusOK {
		t.Fatalf("first GET / = %d", rec1.Code)
	}
	etag := rec1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag on public /")
	}
	if cc := rec1.Header().Get("Cache-Control"); !strings.Contains(cc, "stale-while-revalidate") {
		t.Errorf("Cache-Control = %q, want stale-while-revalidate", cc)
	}
	if !strings.Contains(rec1.Header().Get("Server-Timing"), "cache;desc=miss") {
		t.Errorf("Server-Timing = %q, want cache;desc=miss", rec1.Header().Get("Server-Timing"))
	}
	n1 := cspNonceRe.FindStringSubmatch(rec1.Header().Get("Content-Security-Policy"))
	if len(n1) < 2 || !strings.Contains(rec1.Body.String(), `nonce="`+n1[1]+`"`) {
		t.Fatalf("first response CSP nonce mismatch")
	}
	if strings.Contains(rec1.Body.String(), nonceSentinel) {
		t.Fatalf("nonce sentinel leaked into HTML body")
	}

	// 2. Second anonymous GET / (even with ?utm_source=x) is a cache HIT with a FRESH unique CSP nonce!
	rec2 := getReq(t, h, "/?utm_source=twitter")
	if rec2.Code != http.StatusOK {
		t.Fatalf("second GET / = %d", rec2.Code)
	}
	if !strings.Contains(rec2.Header().Get("Server-Timing"), "cache;desc=hit") {
		t.Errorf("Server-Timing = %q, want cache;desc=hit", rec2.Header().Get("Server-Timing"))
	}
	n2 := cspNonceRe.FindStringSubmatch(rec2.Header().Get("Content-Security-Policy"))
	if len(n2) < 2 || n2[1] == n1[1] {
		t.Fatalf("expected fresh per-request CSP nonce on cache hit (got %v vs %v)", n2, n1)
	}
	if !strings.Contains(rec2.Body.String(), `nonce="`+n2[1]+`"`) {
		t.Fatalf("cached HTML did not substitute fresh CSP nonce %q", n2[1])
	}

	// 3. Conditional GET with If-None-Match returns 304 Not Modified.
	req304 := httptest.NewRequest(http.MethodGet, "/", nil)
	req304.Header.Set("If-None-Match", etag)
	rec304 := httptest.NewRecorder()
	h.ServeHTTP(rec304, req304)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", rec304.Code)
	}

	// 4. Detail page /item/{id} also caches & serves 304 cleanly.
	itemURL := "/item/" + strconv.FormatInt(id, 10)
	d1 := getReq(t, h, itemURL)
	d2 := getReq(t, h, itemURL)
	if d1.Code != http.StatusOK || d2.Code != http.StatusOK || !strings.Contains(d2.Header().Get("Server-Timing"), "cache;desc=hit") {
		t.Fatalf("detail cache failed: d1=%d d2=%d timing=%q", d1.Code, d2.Code, d2.Header().Get("Server-Timing"))
	}

	// 5. Logged-in admin bypasses the public cache completely and gets private, no-cache.
	ck, _ := adminSessionCookie(t, s)
	admReq := httptest.NewRequest(http.MethodGet, "/", nil)
	admReq.AddCookie(ck)
	admRec := httptest.NewRecorder()
	h.ServeHTTP(admRec, admReq)
	if admRec.Code != http.StatusOK {
		t.Fatalf("admin GET / = %d", admRec.Code)
	}
	if cc := admRec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("admin Cache-Control = %q, want private, no-cache", cc)
	}
	if !strings.Contains(admRec.Header().Get("Server-Timing"), "cache;desc=bypass") {
		t.Errorf("admin Server-Timing = %q, want cache;desc=bypass", admRec.Header().Get("Server-Timing"))
	}

	// 6. invalidateSiteCache() clears the page cache immediately.
	s.invalidateSiteCache()
	recAfter := getReq(t, h, "/")
	if !strings.Contains(recAfter.Header().Get("Server-Timing"), "cache;desc=miss") {
		t.Errorf("after invalidation Server-Timing = %q, want cache;desc=miss", recAfter.Header().Get("Server-Timing"))
	}
}
