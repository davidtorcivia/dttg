// Package web is the HTTP layer: routing, server-rendered templates, and auth.
// Templates and static assets are embedded so the binary is self-contained.
package web

import (
	"embed"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
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
	cfg     config.Config
	store   *store.Store
	media   media.Store
	ingest  *ingest.Service
	backup  BackupController
	tmpl    *template.Template
	weather weatherCache
}

func New(cfg config.Config, st *store.Store, ms media.Store, ing *ingest.Service, bc BackupController) (*Server, error) {
	assetVer := assetVersion()
	funcs := template.FuncMap{
		"shortDate": func(t time.Time) string { return strings.ToUpper(t.Format("Jan 02 06")) },
		"longDate":  func(t time.Time) string { return strings.ToUpper(t.Format("Jan 02, 2006")) },
		"pad3":      func(n int64) string { return fmt.Sprintf("%03d", n) },
		"upper":     strings.ToUpper,
		"filesize":  humanSize,
		"fileext":   fileExt,
		"host":      hostname,
		"hasPrefix": strings.HasPrefix,
		"safeHTML":  func(s string) template.HTML { return template.HTML(s) }, //nolint:gosec // admin/oEmbed-trusted
		"asset":     func(p string) string { return p + "?v=" + assetVer },
	}
	tmpl, err := template.New("dnttg").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	// Windows' registry-backed mime table often lacks these; set them explicitly
	// so self-hosted fonts are served with the right Content-Type.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
	_ = mime.AddExtensionType(".webp", "image/webp")
	_ = mime.AddExtensionType(".pdf", "application/pdf")

	s := &Server{cfg: cfg, store: st, media: ms, ingest: ing, backup: bc, tmpl: tmpl}
	s.startWeather()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", cacheControl("public, max-age=604800",
		http.StripPrefix("/static/", http.FileServer(http.FS(staticSub)))))
	mux.Handle("GET /media/", http.StripPrefix("/media/", http.FileServer(http.Dir(s.cfg.MediaDir))))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/weather", s.handleWeather)

	// PWA (root scope) — installability + Android share target
	mux.HandleFunc("GET /manifest.webmanifest", s.serveEmbedded("manifest.webmanifest", "application/manifest+json"))
	mux.HandleFunc("GET /sw.js", s.serveEmbedded("sw.js", "application/javascript"))
	mux.HandleFunc("GET /favicon.svg", s.serveEmbedded("favicon.svg", "image/svg+xml"))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/favicon.svg", http.StatusMovedPermanently)
	})

	mux.HandleFunc("GET /{$}", s.handleBoard)
	mux.HandleFunc("GET /category/{slug}", s.handleBoard)
	mux.HandleFunc("GET /tag/{slug}", s.handleBoard)
	mux.HandleFunc("GET /item/{id}", s.handleDetail)
	mux.HandleFunc("GET /item/{id}/og.jpg", s.handleOGImage)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /search/cards", s.handleSearchCards)
	mux.HandleFunc("GET /api/search", s.handleAPISearch)
	mux.HandleFunc("GET /api/stats", s.handleAPIStats)
	mux.HandleFunc("POST /api/translate", s.handleTranslate)
	mux.HandleFunc("GET /board/more", s.handleBoardMore)
	mux.HandleFunc("GET /feed.json", s.handleFeedJSON)
	mux.HandleFunc("GET /feed.xml", s.handleFeedRSS)

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// API (bearer-token auth) — used by the extension + bookmarklet (M4).
	// CORS-enabled so the browser extension can call them cross-origin; OPTIONS
	// is registered for the preflight the browser sends before the real request.
	apiCreate := s.cors(s.tokenAuth(s.handleAPICreateItem))
	apiTaxonomy := s.cors(s.tokenAuth(s.handleAPITaxonomy))
	mux.HandleFunc("POST /api/items", apiCreate)
	mux.HandleFunc("OPTIONS /api/items", apiCreate)
	mux.HandleFunc("GET /api/taxonomy", apiTaxonomy)
	mux.HandleFunc("OPTIONS /api/taxonomy", apiTaxonomy)

	// Admin (session auth)
	mux.HandleFunc("GET /admin", s.requireAdmin(s.handleAdminDashboard))
	mux.HandleFunc("GET /admin/new", s.requireAdmin(s.handleAdminNew))
	mux.HandleFunc("POST /admin/items", s.requireAdmin(s.handleAdminCreate))
	mux.HandleFunc("GET /admin/items/{id}/edit", s.requireAdmin(s.handleAdminEdit))
	mux.HandleFunc("POST /admin/items/{id}", s.requireAdmin(s.handleAdminUpdate))
	mux.HandleFunc("POST /admin/items/{id}/media", s.requireAdmin(s.handleAdminReplaceMedia))
	mux.HandleFunc("POST /admin/items/{id}/delete", s.requireAdmin(s.handleAdminDelete))
	mux.HandleFunc("GET /admin/settings", s.requireAdmin(s.handleAdminSettings))
	mux.HandleFunc("POST /admin/settings", s.requireAdmin(s.handleAdminSettingsSave))
	mux.HandleFunc("POST /admin/backup", s.requireAdmin(s.handleAdminBackup))

	// PWA share target (the installed app posts shared content here)
	mux.HandleFunc("POST /share", s.requireAdmin(s.handleShare))

	return logRequests(mux)
}

// serveEmbedded serves a single embedded static file at a root path with a
// specific content type (used for the PWA manifest + service worker).
func (s *Server) serveEmbedded(name, contentType string) http.HandlerFunc {
	data, _ := staticFS.ReadFile("static/" + name)
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

// assetVersion is a short content hash of the CSS+JS, used to fingerprint static
// asset URLs (?v=…) so every change busts browser/CDN caches automatically.
func assetVersion() string {
	h := fnv.New64a()
	for _, p := range []string{"static/css/app.css", "static/js/app.js"} {
		if b, err := staticFS.ReadFile(p); err == nil {
			_, _ = h.Write(b)
		}
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
	e := strings.ToUpper(strings.TrimPrefix(filepath.Ext(name), "."))
	if e == "" {
		return "FILE"
	}
	return e
}

// hostname returns the bare host (no www) of a URL, or "".
func hostname(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimPrefix(u.Host, "www.")
}
