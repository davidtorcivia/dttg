package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"donottouchtheglass/internal/config"
	"donottouchtheglass/internal/ingest"
	"donottouchtheglass/internal/media"
)

func TestTranslateRateLimit(t *testing.T) {
	s := newTestServer(t)
	// Tiny burst so the test is fast and deterministic.
	s.translateRL = newTokenBucket(0.001, 3) // effectively no refill during the loop
	h := s.Handler()

	var saw400, saw429 bool
	for i := 0; i < 6; i++ {
		// Empty text fails after the rate-limit check so we never hit Google.
		req := httptest.NewRequest(http.MethodPost, "/api/translate", strings.NewReader(`{"text":""}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.77:9999"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusBadRequest:
			saw400 = true
		case http.StatusTooManyRequests:
			saw429 = true
			if rec.Header().Get("Retry-After") == "" {
				t.Fatalf("429 missing Retry-After")
			}
		default:
			t.Fatalf("translate[%d] unexpected status %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if !saw400 {
		t.Fatal("expected at least one request to pass the rate limit (400 empty text)")
	}
	if !saw429 {
		t.Fatal("expected 429 after burst exhausted")
	}
}

func TestTranslateCacheHit(t *testing.T) {
	s := newTestServer(t)
	// Seed the cache so handleTranslate never dials Google.
	key := translateCacheKey("en", "hola")
	s.translateCache.set(key, "hello", "es")

	req := httptest.NewRequest(http.MethodPost, "/api/translate", strings.NewReader(`{"text":"hola","target":"en"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.10:1"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"hello"`) {
		t.Fatalf("cache miss body=%s", rec.Body.String())
	}
}

func TestAPICreateUploadTooLarge(t *testing.T) {
	s := newTestServerWithIngest(t)
	plain := "smoke-token-xyz"
	if _, err := s.store.CreateToken(context.Background(), "smoke", HashToken(plain)); err != nil {
		t.Fatal(err)
	}

	// Body larger than MaxBytesReader (maxUpload+1) so the handler returns 413
	// without needing a fully valid 30MB multipart parse.
	oversize := make([]byte, maxUpload+8)
	for i := range oversize {
		oversize[i] = 'A'
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(oversize); err != nil {
		t.Fatal(err)
	}
	_ = mw.WriteField("title", "too big")
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/items", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+plain)
	req.RemoteAddr = "203.0.113.20:1"
	req.ContentLength = int64(buf.Len())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d want 413 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upload too large") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestAPICreateTokenRateLimit(t *testing.T) {
	s := newTestServerWithIngest(t)
	s.apiCreateTokenRL = newTokenBucket(0.001, 2)
	s.apiCreateIPRL = newTokenBucket(100, 100) // don't trip IP limit
	plain := "tok-rl-1"
	if _, err := s.store.CreateToken(context.Background(), "rl", HashToken(plain)); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	var saw429 bool
	for i := 0; i < 4; i++ {
		body := `{"title":"n","note":"x","kind":"text"}`
		req := httptest.NewRequest(http.MethodPost, "/api/items", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+plain)
		req.RemoteAddr = "203.0.113.30:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			saw429 = true
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("missing Retry-After")
			}
		}
	}
	if !saw429 {
		t.Fatal("expected token create rate limit 429")
	}
}

func TestTokenBucketAllow(t *testing.T) {
	tb := newTokenBucket(100, 2)
	if !tb.Allow("a") || !tb.Allow("a") {
		t.Fatal("burst should allow 2")
	}
	if tb.Allow("a") {
		t.Fatal("third should deny")
	}
	if !tb.Allow("b") {
		t.Fatal("other key should allow")
	}
}

func newTestServerWithIngest(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	ms, err := media.NewLocalStore(filepath.Join(t.TempDir(), "media2"), "/media")
	if err != nil {
		t.Fatal(err)
	}
	s.media = ms
	s.ingest = ingest.New(s.store, ms)
	if s.cfg.BaseURL == "" {
		s.cfg = config.Config{BaseURL: "http://localhost:8080", SiteTitle: "TEST"}
	}
	s.loadSite(context.Background())
	return s
}
