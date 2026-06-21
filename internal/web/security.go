package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log"
	"net/http"
	"strings"
)

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

// contentSecurityPolicy is the strict-CSP3 recipe: modern browsers honour the
// nonce + 'strict-dynamic' and ignore the rest; old browsers fall back to the
// permissive trailing tokens. style-src keeps 'unsafe-inline' because the UI sets
// element styles from JS (blur-up, transforms, cursor).
func contentSecurityPolicy(nonce string) string {
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
		"script-src 'nonce-" + nonce + "' 'strict-dynamic' 'unsafe-inline' https: 'self'",
		"frame-src https://www.youtube.com https://www.youtube-nocookie.com https://youtube.com https://player.vimeo.com",
		"connect-src 'self'",
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
