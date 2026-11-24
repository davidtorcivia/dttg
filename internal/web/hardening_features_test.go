package web

import (
	"compress/gzip"
	"context"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"donottouchtheglass/internal/ingest"
	"donottouchtheglass/internal/store"
)

func TestMediaSandboxingHeaders(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	h := s.Handler()

	// 1. SVG must be forced as attachment + sandboxed CSP (no inline XSS)
	svgKey := "items/svg1/original.svg"
	id1, _ := s.store.CreateItem(ctx, store.Item{Kind: "document", Visibility: "public", FileKey: svgKey, FileMime: "image/svg+xml"})
	_ = s.media.Put(ctx, svgKey, "image/svg+xml", []byte(`<svg onload="alert(1)"/>`), false)
	_ = s.store.UpsertMedia(ctx, store.Media{ItemID: id1, Variant: "file", StorageKey: svgKey, ContentType: "image/svg+xml", OnLocal: true})

	rec := getReq(t, h, "/media/"+svgKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("svg status = %d", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("svg Content-Disposition = %q, want attachment", cd)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("svg CSP = %q, want sandbox", csp)
	}

	// 2. PDF allows same-origin iframe embedding
	pdfKey := "items/pdf1/file.pdf"
	id2, _ := s.store.CreateItem(ctx, store.Item{Kind: "document", Visibility: "public", FileKey: pdfKey, FileMime: "application/pdf"})
	_ = s.media.Put(ctx, pdfKey, "application/pdf", []byte("%PDF-1.4"), false)
	_ = s.store.UpsertMedia(ctx, store.Media{ItemID: id2, Variant: "file", StorageKey: pdfKey, ContentType: "application/pdf", OnLocal: true})

	recPDF := getReq(t, h, "/media/"+pdfKey)
	if xfo := recPDF.Header().Get("X-Frame-Options"); xfo != "SAMEORIGIN" {
		t.Errorf("pdf X-Frame-Options = %q, want SAMEORIGIN", xfo)
	}
	if csp := recPDF.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("pdf CSP = %q, want frame-ancestors 'self'", csp)
	}

	// 3. JPEG renders inline with sandbox CSP and no attachment header
	jpgKey := "items/jpg1/full.jpg"
	id3, _ := s.store.CreateItem(ctx, store.Item{Kind: "image", Visibility: "public", CoverKey: jpgKey})
	_ = s.media.Put(ctx, jpgKey, "image/jpeg", []byte("fakejpg"), false)
	_ = s.store.UpsertMedia(ctx, store.Media{ItemID: id3, Variant: "full", StorageKey: jpgKey, ContentType: "image/jpeg", OnLocal: true})

	recJPG := getReq(t, h, "/media/"+jpgKey)
	if cd := recJPG.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("jpg Content-Disposition = %q, want empty", cd)
	}
	if csp := recJPG.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("jpg CSP = %q, want sandbox", csp)
	}
}

func TestRateLimiterBoundsAndCFConnectingIP(t *testing.T) {
	// Hard memory cap on loginLimiter and tokenBucket
	ll := newLoginLimiter()
	ll.max = 5
	for i := 0; i < 20; i++ {
		ll.fail("198.51.100." + strconv.Itoa(i))
	}
	if len(ll.attempts) > 5 {
		t.Errorf("loginLimiter grew to %d, want <= 5", len(ll.attempts))
	}

	tb := newTokenBucket(1, 2)
	tb.max = 5
	for i := 0; i < 20; i++ {
		tb.allow("198.51.100." + strconv.Itoa(i))
	}
	if len(tb.buckets) > 5 {
		t.Errorf("tokenBucket grew to %d, want <= 5", len(tb.buckets))
	}

	// CF-Connecting-IP preferred when TrustProxy is enabled
	s := newTestServer(t)
	s.cfg.TrustProxy = true
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("CF-Connecting-IP", "203.0.113.42")
	req.Header.Set("X-Real-IP", "198.51.100.7")
	req.Header.Set("X-Forwarded-For", "198.51.100.8")
	if got := s.clientIP(req); got != "203.0.113.42" {
		t.Errorf("clientIP = %q, want 203.0.113.42", got)
	}
}

func TestGzipAndStaticCacheAndNoStore(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	// Gzipped HTML response
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip Content-Encoding, got %q", rec.Header().Get("Content-Encoding"))
	}
	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	body, err := io.ReadAll(gr)
	_ = gr.Close()
	if err != nil || !strings.Contains(string(body), "TEST ARCHIVE") {
		t.Fatalf("decompressed board body invalid: err=%v len=%d", err, len(body))
	}

	// Immutable cache on versioned static assets
	recStatic := getReq(t, h, "/static/css/app.css?v=12345")
	if cc := recStatic.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("versioned static Cache-Control = %q, want immutable", cc)
	}

	// Login page is no-store
	recLogin := getReq(t, h, "/login")
	if cc := recLogin.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("login Cache-Control = %q, want no-store", cc)
	}
}

func TestAdminFeaturesSecurityAndWorkflows(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	h := s.Handler()

	sid := "primary-sess"
	otherSID := "other-device-sess"
	_ = s.store.CreateSession(ctx, HashSession(sid), time.Hour)
	_ = s.store.CreateSession(ctx, HashSession(otherSID), time.Hour)
	csrf := s.csrfToken(sid)

	postForm := func(path string, v url.Values) *httptest.ResponseRecorder {
		v.Set("csrf_token", csrf)
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 1. Revoke other sessions keeps primary alive and invalidates other
	recRev := postForm("/admin/sessions/revoke-others", url.Values{})
	if recRev.Code != http.StatusSeeOther {
		t.Fatalf("revoke-others status = %d", recRev.Code)
	}
	if ok, _ := s.store.SessionValid(ctx, HashSession(sid)); !ok {
		t.Error("primary session was revoked")
	}
	if ok, _ := s.store.SessionValid(ctx, HashSession(otherSID)); ok {
		t.Error("other session still valid after revoke-others")
	}

	// 2. Create two items: one red image gallery (2 imgs), one blue text note
	id1, err := s.ingest.Create(ctx, ingest.Input{
		Title: "Red Series", Visibility: "public",
		Files: []ingest.UploadFile{
			{Bytes: makePNGBytes(t, 200, 200, color.RGBA{200, 40, 40, 255}), Name: "r1.png"},
			{Bytes: makePNGBytes(t, 200, 200, color.RGBA{180, 30, 30, 255}), Name: "r2.png"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.ingest.Create(ctx, ingest.Input{
		Title: "Text Item", Note: "hello world", Visibility: "public",
		Source: "https://example.com/unique-article",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Reject javascript: source_url on admin update
	recBadURL := postForm("/admin/items/"+strconv.FormatInt(id2, 10), url.Values{
		"title": {"Text Item"}, "source_url": {"javascript:alert(1)"}, "visibility": {"public"},
	})
	if recBadURL.Code != http.StatusUnprocessableEntity {
		t.Errorf("javascript source_url status = %d, want 422", recBadURL.Code)
	}

	// 4. Duplicate URL detection on /admin/new -> 409 Conflict, force=1 -> 303 Created
	recDup := postForm("/admin/items", url.Values{
		"url": {"https://example.com/unique-article"}, "title": {"Dup"}, "kind": {"text"}, "note": {"n"},
	})
	if recDup.Code != http.StatusConflict || !strings.Contains(recDup.Body.String(), "Already in the glass") {
		t.Fatalf("duplicate create = %d, want 409 with warning", recDup.Code)
	}
	recForce := postForm("/admin/items", url.Values{
		"source": {"https://example.com/unique-article"}, "title": {"Forced"}, "kind": {"text"}, "note": {"forced"}, "force": {"1"},
	})
	if recForce.Code != http.StatusSeeOther {
		t.Fatalf("forced create = %d, want 303", recForce.Code)
	}

	// 5. Reorder images + edit per-image caption
	it1, _ := s.store.GetItem(ctx, id1, true)
	imgA, imgB := it1.Images[0].ID, it1.Images[1].ID
	recReord := postForm("/admin/items/"+strconv.FormatInt(id1, 10)+"/images/reorder", url.Values{
		"order": {strconv.FormatInt(imgB, 10) + "," + strconv.FormatInt(imgA, 10)},
	})
	if recReord.Code != http.StatusSeeOther {
		t.Fatalf("reorder status = %d", recReord.Code)
	}
	it1After, _ := s.store.GetItem(ctx, id1, true)
	if it1After.Images[0].ID != imgB || it1After.CoverKey != it1.Images[1].CoverKey {
		t.Errorf("reorder did not swap cover: got first=%d want %d", it1After.Images[0].ID, imgB)
	}
	recAlt := postForm("/admin/items/"+strconv.FormatInt(id1, 10)+"/images/"+strconv.FormatInt(imgB, 10)+"/alt", url.Values{
		"alt": {"Updated caption"},
	})
	if recAlt.Code != http.StatusSeeOther {
		t.Fatalf("alt update status = %d", recAlt.Code)
	}
	it1After, _ = s.store.GetItem(ctx, id1, true)
	if it1After.Images[0].Alt != "Updated caption" {
		t.Errorf("caption = %q, want Updated caption", it1After.Images[0].Alt)
	}

	// 6. Color palette filter + /random redirect
	redBoard := getReq(t, h, "/?color=red").Body.String()
	if !strings.Contains(redBoard, `data-id="`+strconv.FormatInt(id1, 10)+`"`) {
		t.Errorf("color=red board missing red item %d", id1)
	}
	if strings.Contains(redBoard, `data-id="`+strconv.FormatInt(id2, 10)+`"`) {
		t.Errorf("color=red board should not include colorless text item %d", id2)
	}
	recRand := getReq(t, h, "/random")
	if recRand.Code != http.StatusSeeOther || !strings.HasPrefix(recRand.Header().Get("Location"), "/item/") {
		t.Errorf("/random = %d loc=%q", recRand.Code, recRand.Header().Get("Location"))
	}

	// 7. Admin filters + bulk actions (set category, make private, delete)
	reqFilt := httptest.NewRequest(http.MethodGet, "/admin?kind=image", nil)
	reqFilt.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	recFilt := httptest.NewRecorder()
	h.ServeHTTP(recFilt, reqFilt)
	if !strings.Contains(recFilt.Body.String(), "Red Series") || strings.Contains(recFilt.Body.String(), "Text Item") {
		t.Errorf("admin ?kind=image filter failed")
	}

	recBulkCat := postForm("/admin/items/bulk", url.Values{
		"ids": {strconv.FormatInt(id1, 10), strconv.FormatInt(id2, 10)}, "action": {"category"}, "bulk_category": {"Batch"},
	})
	if recBulkCat.Code != http.StatusSeeOther {
		t.Fatalf("bulk category status = %d", recBulkCat.Code)
	}
	if it, _ := s.store.GetItem(ctx, id1, true); it.CategoryName != "Batch" {
		t.Errorf("bulk category not applied: %q", it.CategoryName)
	}

	recBulkPriv := postForm("/admin/items/bulk", url.Values{
		"ids": {strconv.FormatInt(id1, 10)}, "action": {"private"},
	})
	if recBulkPriv.Code != http.StatusSeeOther {
		t.Fatalf("bulk private status = %d", recBulkPriv.Code)
	}
	if it, _ := s.store.GetItem(ctx, id1, false); it != nil {
		t.Error("bulk private item still visible publicly")
	}

	recBulkDel := postForm("/admin/items/bulk", url.Values{
		"ids": {strconv.FormatInt(id2, 10)}, "action": {"delete"},
	})
	if recBulkDel.Code != http.StatusSeeOther {
		t.Fatalf("bulk delete status = %d", recBulkDel.Code)
	}
	if it, _ := s.store.GetItem(ctx, id2, true); it != nil {
		t.Error("bulk deleted item still exists")
	}
}
