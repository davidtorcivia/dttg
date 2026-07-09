package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"

	nethtml "golang.org/x/net/html"
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
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if s.cfg.HTTPSBase() {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		h.Set("Content-Security-Policy", contentSecurityPolicy(nonce))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nonceKey{}, nonce)))
	})
}

// contentSecurityPolicy is a strict nonce-backed policy for scripts. Inline app
// scripts (theme bootstrap, JSON-LD) and the sanitized tracking snippet must
// carry the per-request nonce. style-src keeps 'unsafe-inline' for template
// card colors / column counts. If Cloudflare Rocket Loader injects scripts that
// break under this policy, disable Rocket Loader for the hostname rather than
// reopening script-src to 'unsafe-inline' or https:.
func contentSecurityPolicy(nonce string) string {
	scriptSrc := "script-src 'self'"
	if nonce != "" {
		scriptSrc += " 'nonce-" + nonce + "'"
	}
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
		scriptSrc,
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

// sanitizeTrackingSnippet parses an admin-provided analytics snippet and emits
// only external <script src="http(s)://..."> tags. Inline script bodies,
// non-script tags, and disallowed attributes are dropped. A CSP nonce is added
// to each emitted tag. Returns empty HTML when nothing valid remains.
func sanitizeTrackingSnippet(snippet, nonce string) template.HTML {
	if strings.TrimSpace(snippet) == "" {
		return ""
	}
	nodes, err := nethtml.ParseFragment(strings.NewReader(snippet), &nethtml.Node{
		Type:     nethtml.ElementNode,
		Data:     "div",
		DataAtom: 0,
	})
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(*nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n.Type == nethtml.ElementNode && strings.EqualFold(n.Data, "script") {
			if tag := emitSafeTrackingScript(n, nonce); tag != "" {
				b.WriteString(tag)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return template.HTML(b.String()) //nolint:gosec // attributes escaped; only allowlisted tags
}

func emitSafeTrackingScript(n *nethtml.Node, nonce string) string {
	var src string
	type attr struct{ key, val string }
	var keep []attr
	for _, a := range n.Attr {
		key := strings.ToLower(strings.TrimSpace(a.Key))
		val := strings.TrimSpace(a.Val)
		switch {
		case key == "src":
			src = val
		case key == "async" || key == "defer":
			keep = append(keep, attr{key: key})
		case key == "type" || key == "crossorigin" || key == "referrerpolicy":
			if val != "" {
				keep = append(keep, attr{key: key, val: val})
			}
		case strings.HasPrefix(key, "data-") && key != "data-":
			keep = append(keep, attr{key: key, val: val})
		}
	}
	if !validTrackingSrc(src) {
		return ""
	}
	var b strings.Builder
	b.WriteString("<script")
	if nonce != "" {
		b.WriteString(` nonce="`)
		b.WriteString(html.EscapeString(nonce))
		b.WriteString(`"`)
	}
	b.WriteString(` src="`)
	b.WriteString(html.EscapeString(src))
	b.WriteString(`"`)
	for _, a := range keep {
		b.WriteByte(' ')
		b.WriteString(a.key)
		if a.val != "" {
			b.WriteString(`="`)
			b.WriteString(html.EscapeString(a.val))
			b.WriteString(`"`)
		}
	}
	b.WriteString("></script>")
	return b.String()
}

func validTrackingSrc(src string) bool {
	u, err := url.Parse(src)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	switch scheme {
	case "https":
		return true
	case "http":
		// Dev-only: allow plain http for local analytics collectors.
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	default:
		return false
	}
}
