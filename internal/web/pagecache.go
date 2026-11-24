package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	nonceSentinel   = "__DNTTG_CSP_NONCE_PLACEHOLDER__"
	pageCacheMax    = 256
	publicHTMLCache = "public, max-age=30, stale-while-revalidate=300"
)

type cachedPage struct {
	body []byte // rendered HTML containing nonceSentinel in place of the CSP nonce
	etag string
}

// pageCache is a bounded in-memory cache of rendered public HTML pages keyed by
// (path+query, themeAttr). Every response swaps nonceSentinel for a fresh
// per-request cryptographic CSP nonce so strict nonce-CSP security stays intact
// while eliminating SQLite queries and template execution on hot anonymous reads.
// Any content/settings write clears the entire map via invalidateSiteCache().
type pageCache struct {
	mu      sync.RWMutex
	entries map[string]cachedPage
}

func (c *pageCache) get(key string) (cachedPage, bool) {
	c.mu.RLock()
	p, ok := c.entries[key]
	c.mu.RUnlock()
	return p, ok
}

func (c *pageCache) set(key string, p cachedPage) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]cachedPage, 64)
	}
	if len(c.entries) >= pageCacheMax {
		// Simple bounded reset when full — avoids O(N) eviction overhead and
		// keeps RAM strictly bounded (~3-4 MB worst case).
		c.entries = make(map[string]cachedPage, 64)
	}
	c.entries[key] = p
	c.mu.Unlock()
}

func (c *pageCache) clear() {
	c.mu.Lock()
	c.entries = nil
	c.mu.Unlock()
}

// hasSessionCookie reports whether the request carries an admin session cookie.
// Used to guarantee logged-in admins NEVER hit or populate the public page cache.
func hasSessionCookie(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && c.Value != ""
}

// pageCacheKey constructs a normalized cache key for an anonymous public GET,
// ignoring arbitrary tracking/cache-bust query params (e.g. utm_*, fbclid) so
// shared links still hit the cache and random query strings cannot churn it.
func pageCacheKey(r *http.Request) string {
	theme := ""
	if c, err := r.Cookie("dnttg-theme"); err == nil && (c.Value == "dark" || c.Value == "light") {
		theme = c.Value
	}
	key := r.URL.Path + "|t=" + theme
	if col := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("color"))); col != "" {
		key += "|c=" + col
	}
	return key
}

// servePublicCached serves a public HTML page from the in-memory page cache
// (or renders, caches, and serves it on a miss) with weak ETag / 304 support,
// stale-while-revalidate headers, per-request CSP nonce substitution, and
// Server-Timing metrics. Logged-in admins bypass the cache completely.
func (s *Server) servePublicCached(w http.ResponseWriter, r *http.Request, tmplName string, build func(nonceOverride string) (pageData, int, bool)) {
	start := time.Now()
	// Admins (or anyone with a session cookie) always get live, uncached HTML.
	if hasSessionCookie(r) || s.isAdmin(r) {
		pd, status, ok := build("")
		if !ok {
			return
		}
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Set("Vary", "Cookie, Accept-Encoding, Accept")
		dur := float64(time.Since(start).Microseconds()) / 1000.0
		w.Header().Set("Server-Timing", fmt.Sprintf("app;dur=%.2f, cache;desc=bypass", dur))
		s.render(w, status, tmplName, pd)
		return
	}

	key := pageCacheKey(r)
	if entry, ok := s.pageCache.get(key); ok {
		s.writeCachedHTML(w, r, entry, "hit", start)
		return
	}

	// Cache miss: render once with the sentinel nonce, hash for ETag, and store.
	pd, status, ok := build(nonceSentinel)
	if !ok {
		return
	}
	if status != http.StatusOK {
		// Never cache error/404 responses; swap real nonce and serve directly.
		pd.Nonce = nonceFromContext(r.Context())
		s.render(w, status, tmplName, pd)
		return
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, tmplName, pd); err != nil {
		s.serverError(w, r, err)
		return
	}
	raw := append([]byte(nil), buf.Bytes()...)
	sum := sha256.Sum256(raw)
	entry := cachedPage{
		body: raw,
		etag: `W/"` + hex.EncodeToString(sum[:8]) + `"`,
	}
	s.pageCache.set(key, entry)
	s.writeCachedHTML(w, r, entry, "miss", start)
}

func (s *Server) writeCachedHTML(w http.ResponseWriter, r *http.Request, entry cachedPage, state string, start time.Time) {
	h := w.Header()
	h.Set("ETag", entry.etag)
	h.Set("Cache-Control", publicHTMLCache)
	h.Set("Vary", "Cookie, Accept-Encoding, Accept")

	if inm := strings.TrimSpace(r.Header.Get("If-None-Match")); inm != "" && inm == entry.etag {
		dur := float64(time.Since(start).Microseconds()) / 1000.0
		h.Set("Server-Timing", fmt.Sprintf("app;dur=%.2f, cache;desc=%s-304", dur, state))
		w.WriteHeader(http.StatusNotModified)
		return
	}

	nonce := nonceFromContext(r.Context())
	out := bytes.ReplaceAll(entry.body, []byte(nonceSentinel), []byte(nonce))
	dur := float64(time.Since(start).Microseconds()) / 1000.0
	h.Set("Server-Timing", fmt.Sprintf("app;dur=%.2f, cache;desc=%s", dur, state))
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
