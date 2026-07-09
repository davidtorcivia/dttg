package web

import (
	"context"
	"encoding/json"
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
		store:            st,
		media:            ms,
		tmpl:             tmpl,
		loginRL:          newLoginLimiter(),
		translateRL:      newTokenBucket(20.0/(10*60), 5),
		apiCreateIPRL:    newTokenBucket(20.0/60, 5),
		apiCreateTokenRL: newTokenBucket(60.0/3600, 5),
		translateCache:   newTranslateCache(),
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

// A source smaller than the nominal srcset steps must advertise its TRUE width in
// the descriptors, not the nominal 800w/1600w. Inflated descriptors make a
// width:auto <img> (the detail cover) apply a density downscale and render tiny.
func TestSrcsetClampsSmallImage(t *testing.T) {
	s := newTestServer(t)
	id, err := s.store.CreateItem(context.Background(), store.Item{
		Kind: "image", Title: "Small", Visibility: "public",
		CoverKey: "items/s/full.jpg", ThumbKey: "items/s/thumb.jpg", SmallKey: "items/s/small.jpg",
		Width:    459, Height: 332, // full + thumb both 459px (no upscale); small caps at 400px
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	h := s.Handler()
	detail := getReq(t, h, "/item/"+strconv.FormatInt(id, 10)).Body.String()

	for _, want := range []string{
		"/media/items/s/small.jpg 400w",
		"/media/items/s/thumb.jpg 459w",
		"/media/items/s/full.jpg 459w",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail srcset missing clamped descriptor %q", want)
		}
	}
	// the inflated descriptors must be gone for this image
	for _, bad := range []string{"thumb.jpg 800w", "full.jpg 1600w"} {
		if strings.Contains(detail, bad) {
			t.Errorf("detail srcset still has inflated descriptor %q", bad)
		}
	}
}

func TestFeeds(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.store.CreateItem(context.Background(), store.Item{Kind: "text", Title: "Feed Item", Visibility: "public"})
	h := s.Handler()
	for _, p := range []string{"/feed.json", "/feed.xml"} {
		rec := getReq(t, h, p)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d", p, rec.Code)
		}
		// Feeds must be CORS-open so cross-origin browser consumers can fetch them.
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s Access-Control-Allow-Origin = %q, want *", p, got)
		}
	}
}

func TestFeedVideoAttachment(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	// A video with a known byte size, a video whose size was never captured
	// (FileSize == 0), and a plain text item that must carry no attachment.
	_, _ = s.store.CreateItem(ctx, store.Item{
		Kind: "embed", Title: "Clip", Visibility: "public",
		FileKey: "items/clip/video.mp4", FileMime: "video/mp4", FileSize: 1234,
		CoverKey: "items/clip/poster.jpg",
	})
	_, _ = s.store.CreateItem(ctx, store.Item{
		Kind: "embed", Title: "NoSize", Visibility: "public",
		FileKey: "items/nosize/video.mp4", FileMime: "video/mp4", FileSize: 0,
	})
	_, _ = s.store.CreateItem(ctx, store.Item{Kind: "text", Title: "JustText", Visibility: "public"})
	h := s.Handler()

	// JSON Feed: decode and assert per item, so "no attachment" is provable.
	var feed struct {
		Items []struct {
			Title       string `json:"title"`
			Attachments []struct {
				URL         string `json:"url"`
				MimeType    string `json:"mime_type"`
				SizeInBytes int64  `json:"size_in_bytes"`
			} `json:"attachments"`
		} `json:"items"`
	}
	if err := json.Unmarshal(getReq(t, h, "/feed.json").Body.Bytes(), &feed); err != nil {
		t.Fatalf("feed.json is not valid JSON: %v", err)
	}
	byTitle := map[string]int{}
	for _, it := range feed.Items {
		byTitle[it.Title] = len(it.Attachments)
	}
	if byTitle["JustText"] != 0 {
		t.Errorf("text item should have no attachment, got %d", byTitle["JustText"])
	}
	if byTitle["Clip"] != 1 || byTitle["NoSize"] != 1 {
		t.Errorf("video items should each have 1 attachment, got Clip=%d NoSize=%d", byTitle["Clip"], byTitle["NoSize"])
	}
	for _, it := range feed.Items {
		if it.Title == "Clip" {
			a := it.Attachments[0]
			if a.MimeType != "video/mp4" || a.SizeInBytes != 1234 || !strings.HasSuffix(a.URL, "items/clip/video.mp4") {
				t.Errorf("Clip attachment wrong: %+v", a)
			}
		}
		if it.Title == "NoSize" && it.Attachments[0].SizeInBytes != 0 {
			t.Errorf("NoSize attachment should omit size, got %d", it.Attachments[0].SizeInBytes)
		}
	}

	// RSS: the sized clip gets a full enclosure; the text item gets none.
	rssBody := getReq(t, h, "/feed.xml").Body.String()
	for _, want := range []string{"<enclosure", `type="video/mp4"`, `length="1234"`, "items/clip/video.mp4"} {
		if !strings.Contains(rssBody, want) {
			t.Errorf("feed.xml missing %q\n%s", want, rssBody)
		}
	}
	// Two video items -> exactly two enclosures (text item adds none).
	if got := strings.Count(rssBody, "<enclosure"); got != 2 {
		t.Errorf("expected 2 RSS enclosures (one per video), got %d", got)
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
	// Cloudflare-compatible policy: same-origin scripts allowed (no strict-dynamic),
	// but framing/objects/base-uri still locked down.
	for _, want := range []string{"script-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q (got %q)", want, csp)
		}
	}
	if strings.Contains(csp, "strict-dynamic") {
		t.Errorf("CSP must not use strict-dynamic (breaks Cloudflare Rocket Loader): %q", csp)
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

func TestScanOrphans(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	// orphan file: present in storage, referenced by no media row
	_ = s.media.Put(ctx, "items/orphan1/full.jpg", "image/jpeg", 2, strings.NewReader("xx"))

	// healthy item: cover blob present + a media row referencing it
	okID, _ := s.store.CreateItem(ctx, store.Item{Kind: "image", Title: "ok", Visibility: "public", CoverKey: "items/ok/full.jpg"})
	_ = s.media.Put(ctx, "items/ok/full.jpg", "image/jpeg", 2, strings.NewReader("yy"))
	_, _ = s.store.AddMedia(ctx, store.Media{ItemID: okID, Variant: "full", StorageKey: "items/ok/full.jpg", OnLocal: true})

	// broken item: cover key set, but no blob in storage
	brokenID, _ := s.store.CreateItem(ctx, store.Item{Kind: "image", Title: "broken", Visibility: "public", CoverKey: "items/missing/full.jpg"})

	rep, err := s.scanOrphans(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	has := func(files []orphanFile, key string) bool {
		for _, o := range files {
			if o.Key == key {
				return true
			}
		}
		return false
	}
	if !has(rep.OrphanFiles, "items/orphan1/full.jpg") {
		t.Errorf("orphan file not detected: %+v", rep.OrphanFiles)
	}
	if has(rep.OrphanFiles, "items/ok/full.jpg") {
		t.Errorf("referenced file wrongly flagged as orphan")
	}
	brokenFound, okBroken := false, false
	for _, v := range rep.BrokenItems {
		if v.ID == brokenID {
			brokenFound = true
		}
		if v.ID == okID {
			okBroken = true
		}
	}
	if !brokenFound {
		t.Errorf("broken item not detected")
	}
	if okBroken {
		t.Errorf("healthy item wrongly flagged broken")
	}
}

func TestMaintenancePage(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	sid := "admin-sess"
	if err := s.store.CreateSession(ctx, HashSession(sid), time.Hour); err != nil {
		t.Fatal(err)
	}
	_ = s.media.Put(ctx, "items/orphanX/full.jpg", "image/jpeg", 1, strings.NewReader("z"))

	req := httptest.NewRequest(http.MethodGet, "/admin/maintenance", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("maintenance page = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Orphaned media files") {
		t.Errorf("maintenance page missing report section")
	}
	if !strings.Contains(body, "items/orphanX/full.jpg") {
		t.Errorf("orphan not listed on the page")
	}
	// unauthenticated visitors are redirected to login
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/admin/maintenance", nil))
	if rec2.Code != http.StatusSeeOther {
		t.Errorf("maintenance not protected: %d", rec2.Code)
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
	if err := s.store.CreateSession(context.Background(), HashSession(sid), time.Hour); err != nil {
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
	// Prefer right-most XFF when X-Real-IP is absent (proxy-stripped chain).
	if ip := s.clientIP(req); ip != "70.0.0.1" {
		t.Errorf("clientIP (trust XFF) = %q, want 70.0.0.1", ip)
	}
	req.Header.Set("X-Real-IP", "198.51.100.7")
	if ip := s.clientIP(req); ip != "198.51.100.7" {
		t.Errorf("clientIP (trust X-Real-IP) = %q, want 198.51.100.7", ip)
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

func TestRemoteFeedPageRequiresAdmin(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	rec := getReq(t, h, "/feed")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("anon status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login?next=%2Ffeed" {
		t.Errorf("anon Location = %q", loc)
	}

	sid := "feed-admin"
	if err := s.store.CreateSession(context.Background(), HashSession(sid), time.Hour); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/feed", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("admin status = %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "https://example.com/feed.json") {
		t.Error("admin page missing follow form placeholder")
	}
}

func TestRemoteJSONFeedParser(t *testing.T) {
	body := []byte(`{
		"version": "https://jsonfeed.org/version/1.1",
		"title": "Peer Archive",
		"home_page_url": "https://peer.example/",
		"items": [
			{
				"id": "1",
				"url": "/item/1",
				"external_url": "https://peer.example/src",
				"title": "Relative",
				"content_text": "hello",
				"date_published": "2026-01-02T15:04:05Z",
				"image": "/img/a.jpg"
			},
			{
				"id": "2",
				"url": "javascript:alert(1)",
				"title": "Bad scheme",
				"date_published": "2026-01-01T00:00:00Z"
			},
			{
				"id": "",
				"url": "https://peer.example/skip",
				"title": "Blank id"
			}
		]
	}`)
	fetched := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	parsed, err := parseRemoteJSONFeed("https://peer.example/feed.json", body, fetched)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Update.Title != "Peer Archive" {
		t.Errorf("title = %q", parsed.Update.Title)
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("items = %d, want 2 (blank id skipped)", len(parsed.Items))
	}
	a := parsed.Items[0]
	if a.URL != "https://peer.example/item/1" {
		t.Errorf("relative URL = %q", a.URL)
	}
	if a.ImageURL != "https://peer.example/img/a.jpg" {
		t.Errorf("relative image = %q", a.ImageURL)
	}
	b := parsed.Items[1]
	if b.URL != "" {
		t.Errorf("javascript URL should be empty, got %q", b.URL)
	}
}

func TestRemoteFeedRepostFlow(t *testing.T) {
	s := newTestServer(t)
	// csrfToken needs a non-nil key
	s.csrfKey = []byte("0123456789abcdef0123456789abcdef")
	ctx := context.Background()
	sid := "repost-sess"
	if err := s.store.CreateSession(ctx, HashSession(sid), time.Hour); err != nil {
		t.Fatal(err)
	}

	feed, _, err := s.store.AddRemoteFeed(ctx, "https://peer.example/feed.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.store.SaveRemoteFeedFetch(ctx, feed.ID, store.RemoteFeedUpdate{
		Title: "Peer", LastFetchedAt: now, LastSuccessAt: now,
	}, []store.RemoteFeedItem{{
		RemoteID: "r1", URL: "https://peer.example/item/1",
		Title: "Repost Me", ContentText: "body",
		PublishedAt: now, FetchedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	items, err := s.store.ListRemoteFeedItems(ctx, store.RemoteFeedItemFilter{Limit: 1})
	if err != nil || len(items) != 1 {
		t.Fatalf("seed: %v len=%d", err, len(items))
	}
	remoteID := items[0].ID
	h := s.Handler()

	// Missing CSRF -> 403
	req := httptest.NewRequest(http.MethodPost, "/feed/items/"+strconv.FormatInt(remoteID, 10)+"/repost", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("repost without CSRF status = %d", rec.Code)
	}

	// With CSRF -> redirect to local item
	form := url.Values{"csrf_token": {s.csrfToken(sid)}}
	req2 := httptest.NewRequest(http.MethodPost, "/feed/items/"+strconv.FormatInt(remoteID, 10)+"/repost", strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusSeeOther {
		t.Fatalf("repost status = %d body=%s", rec2.Code, rec2.Body.String())
	}
	loc := rec2.Header().Get("Location")
	if !strings.HasPrefix(loc, "/item/") {
		t.Fatalf("repost Location = %q", loc)
	}
	localID, _ := strconv.ParseInt(strings.TrimPrefix(loc, "/item/"), 10, 64)
	if localID == 0 {
		t.Fatal("expected local item id")
	}

	// Other mutating POSTs reject missing CSRF
	for _, path := range []string{
		"/feed/sources",
		"/feed/sources/" + strconv.FormatInt(feed.ID, 10) + "/sync",
		"/feed/sources/" + strconv.FormatInt(feed.ID, 10) + "/unfollow",
		"/feed/sync",
	} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s without CSRF status = %d", path, rr.Code)
		}
	}

	// GET /feed shows Reposted
	req3 := httptest.NewRequest(http.MethodGet, "/feed", nil)
	req3.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("feed page status = %d", rec3.Code)
	}
	if !strings.Contains(rec3.Body.String(), "Reposted") {
		t.Error("feed page missing Reposted marker")
	}

	// Public feed.json contains the repost
	var feedJSON struct {
		Items []struct {
			Title       string `json:"title"`
			ExternalURL string `json:"external_url"`
		} `json:"items"`
	}
	if err := json.Unmarshal(getReq(t, h, "/feed.json").Body.Bytes(), &feedJSON); err != nil {
		t.Fatalf("feed.json: %v", err)
	}
	found := false
	for _, it := range feedJSON.Items {
		if it.Title == "Repost Me" {
			found = true
			if it.ExternalURL != "https://peer.example/item/1" {
				t.Errorf("external_url = %q", it.ExternalURL)
			}
		}
	}
	if !found {
		t.Error("feed.json missing repost title")
	}
}
