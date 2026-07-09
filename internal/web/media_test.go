package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"donottouchtheglass/internal/store"
)

func TestPrivateMediaGateway(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	h := s.Handler()

	privKey := "items/priv/full.jpg"
	pubKey := "items/pub/full.jpg"
	payload := "fake-jpeg-bytes"

	privID, err := s.store.CreateItem(ctx, store.Item{
		Kind: "image", Title: "Secret", Visibility: "private",
		CoverKey: privKey,
	})
	if err != nil {
		t.Fatalf("create private item: %v", err)
	}
	if err := s.media.Put(ctx, privKey, "image/jpeg", int64(len(payload)), strings.NewReader(payload)); err != nil {
		t.Fatalf("put private blob: %v", err)
	}
	if _, err := s.store.AddMedia(ctx, store.Media{
		ItemID: privID, Variant: "full", StorageKey: privKey,
		ContentType: "image/jpeg", Bytes: int64(len(payload)), OnLocal: true,
	}); err != nil {
		t.Fatalf("add private media: %v", err)
	}

	pubID, err := s.store.CreateItem(ctx, store.Item{
		Kind: "image", Title: "Public", Visibility: "public",
		CoverKey: pubKey,
	})
	if err != nil {
		t.Fatalf("create public item: %v", err)
	}
	if err := s.media.Put(ctx, pubKey, "image/jpeg", int64(len(payload)), strings.NewReader(payload)); err != nil {
		t.Fatalf("put public blob: %v", err)
	}
	if _, err := s.store.AddMedia(ctx, store.Media{
		ItemID: pubID, Variant: "full", StorageKey: pubKey,
		ContentType: "image/jpeg", Bytes: int64(len(payload)), OnLocal: true,
	}); err != nil {
		t.Fatalf("add public media: %v", err)
	}

	// Anonymous: private media is 404 (existence not disclosed).
	rec := getReq(t, h, "/media/"+privKey)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("anon private media status = %d, want 404", rec.Code)
	}

	// Admin session: private media is 200 with private cache headers.
	sid := "admin-media-sess"
	if err := s.store.CreateSession(ctx, HashSession(sid), time.Hour); err != nil {
		t.Fatalf("create session: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/media/"+privKey, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin private media status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("private Cache-Control = %q, want private, no-store", got)
	}
	if body := rec.Body.String(); body != payload {
		t.Errorf("private body = %q, want %q", body, payload)
	}

	// Public media is 200 without a cookie and immutable-cacheable.
	rec = getReq(t, h, "/media/"+pubKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("anon public media status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("public Cache-Control = %q", got)
	}
	if body := rec.Body.String(); body != payload {
		t.Errorf("public body = %q, want %q", body, payload)
	}
}

func TestPrivateMediaMissingAndTraversal(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	if rec := getReq(t, h, "/media/items/does-not-exist.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("missing key status = %d, want 404", rec.Code)
	}
	// Call handleMedia directly: ServeMux redirects paths containing /../ before
	// the handler runs. path.Clean + ".." rejection must still 404.
	for _, key := range []string{"..", "../etc/passwd", "items/x/../../etc/passwd", "."} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/media/"+key, nil)
		req.SetPathValue("key", key)
		s.handleMedia(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("key %q status = %d, want 404", key, rec.Code)
		}
	}
	// Cleaned in-segment path with no media row → 404 via the mux.
	if rec := getReq(t, h, "/media/items/nope.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("missing cleaned key status = %d, want 404", rec.Code)
	}
}
