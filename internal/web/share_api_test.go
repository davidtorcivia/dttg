package web

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"donottouchtheglass/internal/ingest"
)

func TestShareCrossOriginRejected(t *testing.T) {
	s := newTestServer(t)
	s.ingest = ingest.New(s.store, s.media)
	req := httptest.NewRequest(http.MethodPost, "/share", strings.NewReader("url=https://example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site share status = %d, want 403", rec.Code)
	}
}

func TestShareUnauthPendingSurvivesLogin(t *testing.T) {
	s := newTestServer(t)
	s.ingest = ingest.New(s.store, s.media)
	s.cfg.DataDir = t.TempDir()

	form := "url=https://example.com/x&title=Shared&text=hello"
	req := httptest.NewRequest(http.MethodPost, "/share", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://localhost:8080")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauth share status = %d body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/login?next=") || !strings.Contains(loc, "share") {
		t.Fatalf("expected login redirect to pending share, got %q", loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	next := u.Query().Get("next")
	if !strings.HasPrefix(next, "/share/pending/") {
		t.Fatalf("next path = %q", next)
	}
	pid := strings.TrimPrefix(next, "/share/pending/")

	sid := "share-sess"
	if err := s.store.CreateSession(context.Background(), HashSession(sid), time.Hour); err != nil {
		t.Fatal(err)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/share/pending/"+pid, nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusSeeOther {
		t.Fatalf("pending recover status = %d body=%s", rec2.Code, rec2.Body.String())
	}
	dest := rec2.Header().Get("Location")
	if !strings.Contains(dest, "/admin/new") || !strings.Contains(dest, "example.com") {
		t.Fatalf("pending recover dest = %q", dest)
	}
}

func TestAPICreateTokenAuth(t *testing.T) {
	s := newTestServerWithIngest(t)
	h := s.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/items", strings.NewReader(`{"kind":"text","note":"n"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/items", strings.NewReader(`{"kind":"text","note":"n"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d", rec.Code)
	}

	plain := "good-token-xyz"
	if _, err := s.store.CreateToken(context.Background(), "t", HashToken(plain)); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/items", strings.NewReader(`{"kind":"text","note":"hello","title":"T"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plain)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["id"] == nil {
		t.Fatalf("missing id in response: %v", body)
	}
}

func TestAPITaxonomyAndCORS(t *testing.T) {
	s := newTestServerWithIngest(t)
	plain := "tax-token"
	if _, err := s.store.CreateToken(context.Background(), "tax", HashToken(plain)); err != nil {
		t.Fatal(err)
	}
	_, _ = s.store.GetOrCreateCategory(context.Background(), "Texture")
	_, _ = s.store.GetOrCreateTag(context.Background(), "film")

	req := httptest.NewRequest(http.MethodOptions, "/api/taxonomy", nil)
	req.Header.Set("Origin", "moz-extension://abc")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("missing CORS allow origin")
	}

	req = httptest.NewRequest(http.MethodGet, "/api/taxonomy", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("taxonomy status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Texture") {
		t.Fatalf("taxonomy missing category: %s", rec.Body.String())
	}
}

func TestShareMultipartFileAsAdmin(t *testing.T) {
	s := newTestServerWithIngest(t)
	sid := "share-admin"
	if err := s.store.CreateSession(context.Background(), HashSession(sid), time.Hour); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("hello glass")); err != nil {
		t.Fatal(err)
	}
	_ = mw.WriteField("title", "shared note")
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/share", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("admin file share status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Location"), "/admin/items/") {
		t.Fatalf("expected edit redirect, got %q", rec.Header().Get("Location"))
	}
}
