// Package web is the HTTP layer: routing, server-rendered templates, and auth.
// Templates and static assets are embedded so the binary is self-contained.
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"donottouchtheglass/internal/config"
	"donottouchtheglass/internal/ingest"
	"donottouchtheglass/internal/media"
	"donottouchtheglass/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	cfg              config.Config
	store            *store.Store
	media            media.Store
	ingest           *ingest.Service
	backup           BackupController
	tmpl             *template.Template
	weather          weatherCache
	loginRL          *loginLimiter
	translateRL      *tokenBucket
	apiCreateIPRL    *tokenBucket
	apiCreateTokenRL *tokenBucket
	shareRL          *tokenBucket // unauthenticated /share stashes (disk writes)
	translateCache   *translateCache
	ogCache          *ogCache
	siteOG           siteOGCache                  // cached default (site-wide) OG share card
	site             atomic.Pointer[siteIdentity] // admin-editable branding, lock-free reads
	siteCache        siteCache
	csrfKey          []byte
}

// parseTemplates builds the template set with the shared FuncMap.
func parseTemplates() (*template.Template, error) {
	assetVer := contentHash("static/css/app.css", "static/js/app.js")
	funcs := template.FuncMap{
		"shortDate": func(t time.Time) string { return strings.ToUpper(t.Format("Jan 02 06")) },
		"longDate":  func(t time.Time) string { return strings.ToUpper(t.Format("Jan 02, 2006")) },
		"pad3":      func(n int64) string { return fmt.Sprintf("%03d", n) },
		"filesize":  humanSize,
		"fileext":   fileExt,
		"host":      hostname,
		"hasPrefix": strings.HasPrefix,
		"safeHTML":  func(s string) template.HTML { return template.HTML(s) }, //nolint:gosec // sanitized oEmbed iframe
		"asset":     func(p string) string { return p + "?v=" + assetVer },
		// clampW returns the true pixel width of a responsive variant: the nominal
		// srcset step (400/800/1600) capped to the image's actual width. Variants are
		// produced with imaging.Fit, which never upscales, so a 459px source yields a
		// 459px "full.jpg" — labelling it "1600w" makes a width:auto <img> apply a
		// density downscale and render tiny. Accurate descriptors keep it full size.
		// width<=0 (unknown dimensions) falls back to the nominal step.
		"clampW": func(nominal, width int) int {
			if width > 0 && width < nominal {
				return width
			}
			return nominal
		},
	}
	return template.New("dnttg").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
}

// New builds the server. It starts no goroutines; call Start for the
// background loops.
func New(cfg config.Config, st *store.Store, ms media.Store, ing *ingest.Service, bc BackupController) (*Server, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	// Windows' registry-backed mime table often lacks these; set them explicitly
	// so self-hosted fonts are served with the right Content-Type.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
	_ = mime.AddExtensionType(".webp", "image/webp")
	_ = mime.AddExtensionType(".pdf", "application/pdf")

	ctx := context.Background()
	csrfKey, err := persistentKey(ctx, st, "csrf_key")
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, store: st, media: ms, ingest: ing, backup: bc, tmpl: tmpl,
		loginRL:          newLoginLimiter(),
		translateRL:      newTokenBucket(20.0/(10*60), 5), // 20 / 10 min, burst 5
		apiCreateIPRL:    newTokenBucket(20.0/60, 5),      // 20 / min per IP
		apiCreateTokenRL: newTokenBucket(60.0/3600, 5),    // 60 / hour per token
		shareRL:          newTokenBucket(6.0/3600, 3),     // 6 / hour per IP
		translateCache:   newTranslateCache(),
		ogCache:          newOGCache(256),
		csrfKey:          csrfKey,
	}
	s.loadSite(ctx) // publish admin-editable branding (title/url/etc.)
	return s, nil
}

// persistentKey returns a random 32-byte secret stored under name, creating it
// on first use, so CSRF tokens in open admin tabs survive a restart/redeploy.
func persistentKey(ctx context.Context, st *store.Store, name string) ([]byte, error) {
	if v, err := st.GetSetting(ctx, name); err != nil {
		return nil, err
	} else if k, err := hex.DecodeString(v); err == nil && len(k) == 32 {
		return k, nil
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k, st.SetSetting(ctx, name, hex.EncodeToString(k))
}

// Start runs the background loops (weather refresh, expired session and
// pending-share purges) until ctx is cancelled.
func (s *Server) Start(ctx context.Context) {
	go every(ctx, 15*time.Minute, s.refreshWeather)
	go every(ctx, 30*time.Minute, func() {
		if n, err := s.store.PurgeExpiredSessions(ctx); err != nil {
			log.Printf("session purge: %v", err)
		} else if n > 0 {
			log.Printf("purged %d expired sessions", n)
		}
		keys, err := s.store.PurgeExpiredPendingShares(ctx)
		if err != nil {
			log.Printf("pending share purge: %v", err)
		}
		for _, k := range keys {
			s.removePending(k)
		}
	})
}

// every runs fn now and then on each tick until ctx is done.
func every(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		fn()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", cacheControl("public, max-age=604800",
		http.StripPrefix("/static/", http.FileServer(http.FS(staticSub)))))
	mux.HandleFunc("GET /media/{key...}", s.handleMedia)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.Ping(r.Context()); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready"))
	})
	mux.HandleFunc("GET /api/weather", s.handleWeather)

	// PWA (root scope) — installability + Android share target
	mux.HandleFunc("GET /manifest.webmanifest", serveEmbedded("manifest.webmanifest", "application/manifest+json"))
	mux.HandleFunc("GET /sw.js", serveEmbedded("sw.js", "application/javascript"))
	mux.HandleFunc("GET /offline.html", serveEmbedded("offline.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /favicon.svg", serveEmbedded("favicon.svg", "image/svg+xml"))
	mux.Handle("GET /favicon.ico", http.RedirectHandler("/favicon.svg", http.StatusMovedPermanently))

	mux.HandleFunc("GET /", s.notFound) // the site's 404 page for unknown paths
	mux.HandleFunc("GET /{$}", s.handleBoard)
	mux.HandleFunc("GET /category/{slug}", s.handleBoard)
	mux.HandleFunc("GET /tag/{slug}", s.handleBoard)
	mux.HandleFunc("GET /item/{id}", s.handleDetail)
	mux.HandleFunc("GET /item/{id}/og.jpg", s.handleOGImage)
	mux.HandleFunc("GET /og.jpg", s.handleSiteOGImage)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /search/cards", s.handleSearchCards)
	mux.HandleFunc("GET /api/search", s.handleAPISearch)
	mux.HandleFunc("GET /api/stats", s.handleAPIStats)
	mux.HandleFunc("POST /api/translate", s.handleTranslate)
	mux.HandleFunc("GET /board/more", s.handleBoardMore)
	// Feeds are CORS-open so cross-origin browser apps can fetch them directly
	// (a simple GET sends no preflight; the header on the response is enough).
	mux.HandleFunc("GET /feed.json", cors(s.handleFeedJSON))
	mux.HandleFunc("GET /feed.xml", cors(s.handleFeedRSS))
	mux.HandleFunc("GET /sitemap.xml", s.handleSitemap)
	mux.HandleFunc("GET /robots.txt", s.handleRobots)

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit) // no CSRF: no session yet (login CSRF is benign)
	mux.HandleFunc("POST /logout", s.csrf(s.handleLogout))

	// API (bearer-token auth) — used by the extension + bookmarklet. CORS-enabled
	// so the extension can call it cross-origin; OPTIONS answers the preflight.
	apiCreate := cors(s.tokenAuth(s.handleAPICreateItem))
	apiTaxonomy := cors(s.tokenAuth(s.handleAPITaxonomy))
	mux.HandleFunc("POST /api/items", apiCreate)
	mux.HandleFunc("OPTIONS /api/items", apiCreate)
	mux.HandleFunc("GET /api/taxonomy", apiTaxonomy)
	mux.HandleFunc("OPTIONS /api/taxonomy", apiTaxonomy)

	// Admin (session auth). POSTs are CSRF-checked.
	admin := func(pattern string, h http.HandlerFunc) {
		if strings.HasPrefix(pattern, "POST ") {
			h = s.csrf(h)
		}
		mux.HandleFunc(pattern, s.requireAdmin(h))
	}
	admin("GET /admin", s.handleAdminDashboard)
	admin("GET /admin/new", s.handleAdminNew)
	admin("POST /admin/items", s.handleAdminCreate)
	admin("GET /admin/items/{id}/edit", s.handleAdminEdit)
	admin("POST /admin/items/{id}", s.handleAdminUpdate)
	admin("POST /admin/items/{id}/media", s.handleAdminReplaceMedia)
	admin("POST /admin/items/{id}/delete", s.handleAdminDelete)
	admin("GET /admin/settings", s.handleAdminSettings)
	admin("POST /admin/settings", s.handleAdminSettingsSave)
	admin("POST /admin/tokens/revoke", s.handleAdminTokenRevoke)
	admin("GET /admin/maintenance", s.handleMaintenance)
	admin("POST /admin/maintenance/cleanup", s.handleMaintenanceCleanup)
	admin("POST /admin/backup", s.handleAdminBackup)
	// Remote feed following + reposts (Plan A federation)
	admin("GET /feed", s.handleRemoteFeedPage)
	admin("POST /feed/sources", s.handleRemoteFeedAddSource)
	admin("POST /feed/sources/{id}/sync", s.handleRemoteFeedFetchSource)
	admin("POST /feed/sources/{id}/unfollow", s.handleRemoteFeedUnfollowSource)
	admin("POST /feed/sync", s.handleRemoteFeedSyncAll)
	admin("POST /feed/items/{id}/repost", s.handleRemoteFeedRepost)
	admin("GET /share/pending/{id}", s.handleSharePending)

	// PWA share target — same-origin + session when available; unauthenticated
	// shares are stashed in pending_shares and recovered after login.
	mux.HandleFunc("POST /share", s.handleShare)

	return logRequests(s.securityHeaders(s.withAdmin(recoverPanic(mux))))
}

// handleMedia serves media bytes through an auth-aware gateway. Public item media
// is cacheable; private media requires an admin session and is never disclosed
// (404, not 403) to anonymous clients. ServeContent adds Range support, so
// videos can seek and PDFs load incrementally.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(path.Clean("/"+r.PathValue("key")), "/")
	if key == "" || strings.Contains(key, "..") {
		http.NotFound(w, r)
		return
	}
	m, err := s.store.MediaByKey(r.Context(), key)
	if err != nil || (m.Private && !s.isAdmin(r)) {
		http.NotFound(w, r)
		return
	}
	rc, err := s.media.Open(r.Context(), m.StorageKey)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rc.Close()

	if m.Private {
		w.Header().Set("Cache-Control", "private, no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if m.ContentType != "" {
		w.Header().Set("Content-Type", m.ContentType)
	}
	if rs, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(w, r, path.Base(key), time.Time{}, rs) // sniffs Content-Type if unset
		return
	}
	_, _ = io.Copy(w, rc)
}

// serveEmbedded serves a single embedded static file at a root path with a
// specific content type (used for the PWA manifest + service worker).
func serveEmbedded(name, contentType string) http.HandlerFunc {
	data, _ := staticFS.ReadFile("static/" + name)
	if name == "sw.js" {
		// Version the SW cache by the offline page's content, so editing
		// offline.html alone still changes sw.js and re-caches it.
		data = []byte(strings.Replace(string(data), "dnttg-v1", "dnttg-"+contentHash("static/offline.html"), 1))
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if name == "sw.js" {
			w.Header().Set("Service-Worker-Allowed", "/")
			w.Header().Set("Cache-Control", "no-cache") // let SW updates be picked up promptly
		}
		_, _ = w.Write(data)
	}
}

// cacheControl wraps a handler, adding a Cache-Control header.
func cacheControl(value string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", value)
		next.ServeHTTP(w, r)
	})
}

// contentHash is a short FNV hash of embedded static files, used to
// fingerprint asset URLs (?v=…) so every change busts browser/CDN caches.
func contentHash(files ...string) string {
	h := fnv.New64a()
	for _, p := range files {
		b, _ := staticFS.ReadFile(p)
		_, _ = h.Write(b)
	}
	return fmt.Sprintf("%x", h.Sum64())
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%-4s %-24s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// pendingPath is where an unauthenticated PWA share's file waits for login.
func (s *Server) pendingPath(key string) string {
	return filepath.Join(s.cfg.DataDir, "pending", filepath.Base(key))
}

func (s *Server) removePending(key string) {
	if key != "" {
		_ = os.Remove(s.pendingPath(key))
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fileExt(name string) string {
	if e := strings.ToUpper(strings.TrimPrefix(filepath.Ext(name), ".")); e != "" {
		return e
	}
	return "FILE"
}

// hostname returns the bare host (no www) of a URL, or "".
func hostname(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Host, "www.")
}
