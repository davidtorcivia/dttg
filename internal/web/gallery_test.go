package web

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func makePNGBytes(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func TestMultiImageAPIBoardDetailAdminAndFeeds(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	h := s.Handler()

	// Mint API token & create a 2-image post via multipart POST /api/items
	tok := "gallery-api-tok"
	if _, err := s.store.CreateToken(ctx, "gallery", HashToken(tok)); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i, c := range []color.RGBA{{210, 40, 40, 255}, {40, 120, 210, 255}} {
		fw, err := mw.CreateFormFile("file", "plate"+strconv.Itoa(i+1)+".png")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(makePNGBytes(t, 420+i*100, 320, c))
	}
	_ = mw.WriteField("title", "Two Studies")
	_ = mw.WriteField("note", "Notes <with> & html")
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/items", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("API multi-upload status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("decode create response: %v body=%s", err, rec.Body.String())
	}

	// Board renders multi-image card badge, peek controls, and gallery JSON
	board := getReq(t, h, "/").Body.String()
	for _, want := range []string{"card--multi", "1/2", "card-peek-prev", "card-peek-next", "data-gallery="} {
		if !strings.Contains(board, want) {
			t.Errorf("board missing %q", want)
		}
	}

	// Detail page renders carousel, filmstrip, counter, and lightbox nav
	detail := getReq(t, h, itemPath(created.ID)).Body.String()
	for _, want := range []string{"detail-gallery", "gallery-strip", "gallery-prev", "gallery-next", "lightbox-prev", "2 images"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q", want)
		}
	}

	// Feeds include multi-image attachments and escape note HTML in RSS
	var jf struct {
		Items []struct {
			Attachments []struct {
				URL      string `json:"url"`
				MimeType string `json:"mime_type"`
			} `json:"attachments"`
		} `json:"items"`
	}
	if err := json.Unmarshal(getReq(t, h, "/feed.json").Body.Bytes(), &jf); err != nil {
		t.Fatalf("feed.json: %v", err)
	}
	if len(jf.Items) != 1 || len(jf.Items[0].Attachments) != 2 {
		t.Fatalf("feed.json attachments = %+v, want 2", jf.Items)
	}
	rss := getReq(t, h, "/feed.xml").Body.String()
	if strings.Contains(rss, "Notes <with>") {
		t.Errorf("feed.xml did not escape note HTML: %s", rss)
	}

	// Admin session: append a 3rd image, promote it to cover, delete the old 2nd image
	sid := "gallery-admin-sess"
	if err := s.store.CreateSession(ctx, HashSession(sid), time.Hour); err != nil {
		t.Fatal(err)
	}
	csrf := s.csrfToken(sid)

	var addBuf bytes.Buffer
	amw := multipart.NewWriter(&addBuf)
	_ = amw.WriteField("csrf_token", csrf)
	fw, _ := amw.CreateFormFile("file", "plate3.png")
	_, _ = fw.Write(makePNGBytes(t, 600, 450, color.RGBA{40, 180, 90, 255}))
	_ = amw.Close()

	addReq := httptest.NewRequest(http.MethodPost, "/admin/items/"+strconv.FormatInt(created.ID, 10)+"/images", &addBuf)
	addReq.Header.Set("Content-Type", amw.FormDataContentType())
	addReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	addRec := httptest.NewRecorder()
	h.ServeHTTP(addRec, addReq)
	if addRec.Code != http.StatusSeeOther {
		t.Fatalf("admin add images status = %d body=%s", addRec.Code, addRec.Body.String())
	}

	it, _ := s.store.GetItem(ctx, created.ID, true)
	if len(it.Images) != 3 {
		t.Fatalf("expected 3 images after append, got %d", len(it.Images))
	}
	thirdID := it.Images[2].ID
	thirdKey := it.Images[2].CoverKey

	// Promote 3rd to cover
	moveForm := url.Values{"csrf_token": {csrf}, "dir": {"first"}}
	moveReq := httptest.NewRequest(http.MethodPost,
		"/admin/items/"+strconv.FormatInt(created.ID, 10)+"/images/"+strconv.FormatInt(thirdID, 10)+"/move",
		strings.NewReader(moveForm.Encode()))
	moveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	moveReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	moveRec := httptest.NewRecorder()
	h.ServeHTTP(moveRec, moveReq)
	if moveRec.Code != http.StatusSeeOther {
		t.Fatalf("move status = %d body=%s", moveRec.Code, moveRec.Body.String())
	}
	it, _ = s.store.GetItem(ctx, created.ID, true)
	if it.CoverKey != thirdKey {
		t.Errorf("cover after promote = %q, want %q", it.CoverKey, thirdKey)
	}

	// Delete 2nd image
	delID := it.Images[1].ID
	delForm := url.Values{"csrf_token": {csrf}}
	delReq := httptest.NewRequest(http.MethodPost,
		"/admin/items/"+strconv.FormatInt(created.ID, 10)+"/images/"+strconv.FormatInt(delID, 10)+"/delete",
		strings.NewReader(delForm.Encode()))
	delReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	delReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusSeeOther {
		t.Fatalf("delete image status = %d body=%s", delRec.Code, delRec.Body.String())
	}

	// Orphan scan should find 0 orphans and 0 broken items after all mutations!
	rep, err := s.scanOrphans(ctx)
	if err != nil {
		t.Fatalf("scanOrphans: %v", err)
	}
	if !rep.Empty() {
		t.Errorf("expected clean orphan scan, got orphans=%d broken=%d stray=%d",
			len(rep.OrphanFiles), len(rep.BrokenItems), len(rep.StrayRows))
	}

	// Private multi-image item: secondary image key must also 404 for anonymous
	updForm := url.Values{"csrf_token": {csrf}, "title": {"Secret Pair"}, "visibility": {"private"}}
	updReq := httptest.NewRequest(http.MethodPost, "/admin/items/"+strconv.FormatInt(created.ID, 10), strings.NewReader(updForm.Encode()))
	updReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	updReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	updRec := httptest.NewRecorder()
	h.ServeHTTP(updRec, updReq)
	if updRec.Code != http.StatusSeeOther {
		t.Fatalf("update private status = %d", updRec.Code)
	}
	it, _ = s.store.GetItem(ctx, created.ID, true)
	for _, img := range it.Images {
		if rec := getReq(t, h, "/media/"+img.CoverKey); rec.Code != http.StatusNotFound {
			t.Errorf("anon access to private gallery image %s = %d, want 404", img.CoverKey, rec.Code)
		}
		authed := httptest.NewRequest(http.MethodGet, "/media/"+img.CoverKey, nil)
		authed.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		arec := httptest.NewRecorder()
		h.ServeHTTP(arec, authed)
		if arec.Code != http.StatusOK {
			t.Errorf("admin access to private gallery image %s = %d, want 200", img.CoverKey, arec.Code)
		}
	}
}
