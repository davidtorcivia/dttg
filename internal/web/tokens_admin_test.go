package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var secretRe = regexp.MustCompile(`id="minted-token-secret">([^<]+)<`)

func adminSessionCookie(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	sid := newSessionID()
	if err := s.store.CreateSession(context.Background(), HashSession(sid), time.Hour); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: sid}, s.csrfToken(sid)
}

func postAdminForm(t *testing.T, h http.Handler, path string, ck *http.Cookie, csrf string, vals url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if vals == nil {
		vals = url.Values{}
	}
	vals.Set("csrf_token", csrf)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdminAuthKeyLifecycle(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	ck, csrf := adminSessionCookie(t, s)

	// 1. Generate a new key via admin UI
	rec := postAdminForm(t, h, "/admin/tokens/create", ck, csrf, url.Values{"name": {"claude-mcp"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create token status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("create token Cache-Control = %q, want no-store", cc)
	}
	m := secretRe.FindStringSubmatch(rec.Body.String())
	if len(m) < 2 {
		t.Fatalf("expected one-time secret in response body:\n%s", rec.Body.String())
	}
	firstSecret := m[1]
	firstPrefix := TokenPrefix(firstSecret)
	if !strings.Contains(rec.Body.String(), firstPrefix+"••••••••") {
		t.Errorf("expected masked prefix %q in token table", firstPrefix)
	}
	for _, want := range []string{"mcp-remote", "claude mcp add", "Bearer " + firstSecret} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("expected minted MCP snippet to contain %q", want)
		}
	}

	// Verify the generated key authenticates against /api/taxonomy and /mcp
	apiReq := httptest.NewRequest(http.MethodGet, "/api/taxonomy", nil)
	apiReq.Header.Set("Authorization", "Bearer "+firstSecret)
	apiRec := httptest.NewRecorder()
	h.ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Fatalf("first secret rejected by /api/taxonomy: %d", apiRec.Code)
	}

	// 2. View settings page again: prefix is visible, full plaintext secret is NOT!
	viewReq := httptest.NewRequest(http.MethodGet, "/admin/settings", nil)
	viewReq.AddCookie(ck)
	viewRec := httptest.NewRecorder()
	h.ServeHTTP(viewRec, viewReq)
	if viewRec.Code != http.StatusOK {
		t.Fatalf("settings GET = %d", viewRec.Code)
	}
	if !strings.Contains(viewRec.Body.String(), firstPrefix+"••••••••") {
		t.Errorf("settings view missing masked prefix")
	}
	if strings.Contains(viewRec.Body.String(), firstSecret) {
		t.Errorf("settings GET leaked plaintext secret after mint!")
	}

	// Look up token ID
	toks, err := s.store.ListTokens(context.Background())
	if err != nil || len(toks) != 1 {
		t.Fatalf("ListTokens = %v, %v", toks, err)
	}
	tokID := strconv.FormatInt(toks[0].ID, 10)

	// 3. Rotate the key via admin UI
	rotRec := postAdminForm(t, h, "/admin/tokens/rotate", ck, csrf, url.Values{"id": {tokID}})
	if rotRec.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s", rotRec.Code, rotRec.Body.String())
	}
	m2 := secretRe.FindStringSubmatch(rotRec.Body.String())
	if len(m2) < 2 {
		t.Fatalf("expected rotated secret in response body")
	}
	secondSecret := m2[1]
	if secondSecret == firstSecret {
		t.Fatalf("rotated secret matched old secret")
	}

	// Old secret MUST fail immediately; new secret MUST succeed!
	oldReq := httptest.NewRequest(http.MethodGet, "/api/taxonomy", nil)
	oldReq.Header.Set("Authorization", "Bearer "+firstSecret)
	oldRec := httptest.NewRecorder()
	h.ServeHTTP(oldRec, oldReq)
	if oldRec.Code != http.StatusUnauthorized {
		t.Fatalf("old secret still accepted after rotation: %d", oldRec.Code)
	}

	newReq := httptest.NewRequest(http.MethodGet, "/api/taxonomy", nil)
	newReq.Header.Set("Authorization", "Bearer "+secondSecret)
	newRec := httptest.NewRecorder()
	h.ServeHTTP(newRec, newReq)
	if newRec.Code != http.StatusOK {
		t.Fatalf("rotated secret rejected: %d", newRec.Code)
	}

	// 4. Delete the key via admin UI
	delRec := postAdminForm(t, h, "/admin/tokens/revoke", ck, csrf, url.Values{"id": {tokID}})
	if delRec.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", delRec.Code)
	}
	afterReq := httptest.NewRequest(http.MethodGet, "/api/taxonomy", nil)
	afterReq.Header.Set("Authorization", "Bearer "+secondSecret)
	afterRec := httptest.NewRecorder()
	h.ServeHTTP(afterRec, afterReq)
	if afterRec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted secret still accepted: %d", afterRec.Code)
	}
}
