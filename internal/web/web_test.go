package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"donottouchtheglass/internal/config"
	"donottouchtheglass/internal/media"
	"donottouchtheglass/internal/store"
)

// newTestServer builds a Server backed by a temp sqlite DB and local media dir,
// without New()'s weather goroutine — hermetic and offline.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ms, err := media.NewLocalStore(filepath.Join(dir, "media"), "/media")
	if err != nil {
		t.Fatalf("media: %v", err)
	}
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	return &Server{
		cfg: config.Config{
			BaseURL:     "http://localhost:8080",
			SiteTitle:   "TEST ARCHIVE",
			SiteTagline: "INDEX",
			MediaDir:    filepath.Join(dir, "media"),
		},
		store:   st,
		media:   ms,
		tmpl:    tmpl,
		loginRL: newLoginLimiter(),
	}
}

func getReq(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestTemplatesParse(t *testing.T) {
	if _, err := parseTemplates(); err != nil {
		t.Fatalf("parse templates: %v", err)
	}
}

func TestBoardAndDetail(t *testing.T) {
	s := newTestServer(t)
	id, err := s.store.CreateItem(context.Background(), store.Item{
		Kind: "text", Title: "Hello Glass", Note: "a note", Visibility: "public",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	h := s.Handler()

	if rec := getReq(t, h, "/"); rec.Code != http.StatusOK {
		t.Fatalf("board status = %d", rec.Code)
	} else if !strings.Contains(rec.Body.String(), "TEST ARCHIVE") {
		t.Errorf("board missing site title")
	}

	rec := getReq(t, h, "/item/"+strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Hello Glass") {
		t.Errorf("detail missing title")
	}
	if !strings.Contains(body, "application/ld+json") {
		t.Errorf("detail missing JSON-LD")
	}

	if rec := getReq(t, h, "/item/999999"); rec.Code != http.StatusNotFound {
		t.Errorf("missing item status = %d, want 404", rec.Code)
	}
}

func TestSitemapAndRobots(t *testing.T) {
	s := newTestServer(t)
	id, _ := s.store.CreateItem(context.Background(), store.Item{Kind: "text", Title: "X", Visibility: "public"})
	h := s.Handler()

	rec := getReq(t, h, "/sitemap.xml")
	if rec.Code != http.StatusOK {
		t.Fatalf("sitemap status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<urlset") {
		t.Errorf("sitemap missing urlset")
	}
	if !strings.Contains(body, "/item/"+strconv.FormatInt(id, 10)) {
		t.Errorf("sitemap missing item url")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Errorf("sitemap content-type = %q", ct)
	}

	rec = getReq(t, h, "/robots.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("robots status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Sitemap: http://localhost:8080/sitemap.xml") {
		t.Errorf("robots missing sitemap line: %q", rec.Body.String())
	}
}

func TestSrcsetEagerAndImageJSONLD(t *testing.T) {
	s := newTestServer(t)
	id, err := s.store.CreateItem(context.Background(), store.Item{
		Kind: "image", Title: "Pic", Visibility: "public",
		CoverKey: "items/x/full.jpg", ThumbKey: "items/x/thumb.jpg", SmallKey: "items/x/small.jpg", Width: 1600, Height: 1000,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	h := s.Handler()

	board := getReq(t, h, "/").Body.String()
	if !strings.Contains(board, "/media/items/x/thumb.jpg 800w") {
		t.Errorf("board card missing srcset thumb descriptor")
	}
	if !strings.Contains(board, "/media/items/x/small.jpg 400w") {
		t.Errorf("board card missing srcset small (400w) descriptor")
	}
	if !strings.Contains(board, `fetchpriority="high"`) {
		t.Errorf("first board card not eager (no fetchpriority)")
	}

	detail := getReq(t, h, "/item/"+strconv.FormatInt(id, 10)).Body.String()
	if !strings.Contains(detail, `"@type":"ImageObject"`) {
		t.Errorf("detail JSON-LD is not ImageObject")
	}
	if !strings.Contains(detail, "srcset=") || !strings.Contains(detail, `fetchpriority="high"`) {
		t.Errorf("detail cover missing srcset/fetchpriority")
	}
}

func TestFeeds(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.store.CreateItem(context.Background(), store.Item{Kind: "text", Title: "Feed Item", Visibility: "public"})
	h := s.Handler()
	for _, p := range []string{"/feed.json", "/feed.xml"} {
		if rec := getReq(t, h, p); rec.Code != http.StatusOK {
			t.Errorf("%s status = %d", p, rec.Code)
		}
	}
}

func TestSecurityHeadersAndNonce(t *testing.T) {
	s := newTestServer(t)
	rec := getReq(t, s.Handler(), "/")
	h := rec.Header()
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing X-Content-Type-Options: nosniff")
	}
	if h.Get("Referrer-Policy") == "" || h.Get("X-Frame-Options") == "" {
		t.Errorf("missing Referrer-Policy / X-Frame-Options")
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src") || !strings.Contains(csp, "nonce-") {
		t.Fatalf("CSP missing script-src nonce: %q", csp)
	}
	if !strings.Contains(rec.Body.String(), "nonce=") {
		t.Errorf("rendered page has no nonce on its scripts")
	}
}

func TestAccessibilityMarkup(t *testing.T) {
	s := newTestServer(t)
	body := getReq(t, s.Handler(), "/").Body.String()
	for _, want := range []string{
		`class="skip-link"`,
		`id="maincontent"`,
		`role="dialog"`,
		`aria-modal="true"`,
		`aria-expanded="false"`, // search toggle
		`aria-pressed=`,         // theme toggle
		`<noscript>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board missing a11y markup: %s", want)
		}
	}
}

func TestReadyProbe(t *testing.T) {
	s := newTestServer(t)
	if rec := getReq(t, s.Handler(), "/ready"); rec.Code != http.StatusOK {
		t.Errorf("/ready = %d, want 200", rec.Code)
	}
}

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter()
	const key = "1.2.3.4"
	if blocked, _ := l.blocked(key); blocked {
		t.Fatal("blocked before any failures")
	}
	for i := 0; i < loginMaxFails; i++ {
		l.fail(key)
	}
	if blocked, _ := l.blocked(key); !blocked {
		t.Fatalf("not blocked after %d failures", loginMaxFails)
	}
	l.reset(key)
	if blocked, _ := l.blocked(key); blocked {
		t.Fatal("still blocked after reset")
	}
}

func TestCSRF(t *testing.T) {
	s := newTestServer(t)
	sid := "test-session"
	if err := s.store.CreateSession(context.Background(), sid, time.Hour); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()

	// POST without a token is rejected
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("logout without CSRF token = %d, want 403", rec.Code)
	}

	// POST with the session-bound token succeeds
	form := url.Values{"csrf_token": {s.csrfToken(sid)}}
	req2 := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusSeeOther {
		t.Errorf("logout with valid CSRF token = %d, want 303", rec2.Code)
	}
}

func TestClientIP(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 70.0.0.1")

	if ip := s.clientIP(req); ip != "203.0.113.9" {
		t.Errorf("clientIP (no trust) = %q, want 203.0.113.9", ip)
	}
	s.cfg.TrustProxy = true
	if ip := s.clientIP(req); ip != "10.0.0.1" {
		t.Errorf("clientIP (trust) = %q, want 10.0.0.1", ip)
	}
}

func TestHelpers(t *testing.T) {
	if got := humanSize(2048); got != "2.0 KB" {
		t.Errorf("humanSize(2048) = %q", got)
	}
	if got := fileExt("foo.PDF"); got != "PDF" {
		t.Errorf("fileExt = %q", got)
	}
	if got := fileExt("noext"); got != "FILE" {
		t.Errorf("fileExt(noext) = %q", got)
	}
	if got := hostname("https://www.example.com/x"); got != "example.com" {
		t.Errorf("hostname = %q", got)
	}
}
