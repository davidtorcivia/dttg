package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log"
	"net/http"
	"strings"
)

// csrfToken derives a per-session CSRF token (HMAC of the session id), so no
// extra storage is needed and it's bound to the logged-in session.
func (s *Server) csrfToken(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, s.csrfKey)
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validCSRF checks the submitted csrf_token against the session-bound token.
func (s *Server) validCSRF(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	want := s.csrfToken(c.Value)
	got := r.FormValue("csrf_token")
	return want != "" && got != "" && hmac.Equal([]byte(got), []byte(want))
}

// csrf rejects state-changing POSTs that lack a valid session-bound token (the
// session cookie is SameSite=Lax, so this is defense-in-depth).
func (s *Server) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !s.validCSRF(r) {
			http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

type nonceKey struct{}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

func nonceFromContext(ctx context.Context) string {
	n, _ := ctx.Value(nonceKey{}).(string)
	return n
}

// securityHeaders sets standard hardening headers + a per-request nonce CSP, and
// stashes the nonce in the request context so templates can tag their inline
// <script> elements (theme bootstrap, JSON-LD, analytics).
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := newNonce()
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=(), payment=()")
		if s.cfg.HTTPSBase() {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		h.Set("Content-Security-Policy", contentSecurityPolicy(nonce))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nonceKey{}, nonce)))
	})
}

// contentSecurityPolicy is intentionally NOT a strict (nonce/'strict-dynamic')
// policy: the site runs behind Cloudflare, whose Rocket Loader + injected scripts
// (analytics beacon, email obfuscation) are same-origin /cdn-cgi/ assets with no
// nonce, and strict-dynamic would block them — which kills ALL JS. So script-src
// allows 'self' + same-origin + https + inline, while the other directives still
// lock down framing, objects, base-uri and form actions. (nonce kept unused for a
// possible future strict policy if Rocket Loader is disabled.)
func contentSecurityPolicy(_ string) string {
	return strings.Join([]string{
		"default-src 'self'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"img-src 'self' data: https:",
		"media-src 'self' https:",
		"font-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"script-src 'self' 'unsafe-inline' https:",
		"frame-src https://www.youtube.com https://www.youtube-nocookie.com https://youtube.com https://player.vimeo.com",
		"connect-src 'self' https:",
	}, "; ")
}

// recoverPanic turns a panic in any handler into a 500 instead of crashing the
// whole process.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v (%s %s)", rec, r.Method, r.URL.Path)
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// injectNonce adds the CSP nonce to every <script ...> in trusted admin HTML (the
// analytics snippet) so it's allowed under the strict script-src.
func injectNonce(html, nonce string) string {
	if nonce == "" || html == "" {
		return html
	}
	return strings.ReplaceAll(html, "<script", `<script nonce="`+nonce+`"`)
}
