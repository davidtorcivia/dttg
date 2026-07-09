package web

import (
	"context"
	"html/template"
	"sync"
	"sync/atomic"
	"time"

	"donottouchtheglass/internal/store"
)

// siteCache holds hot, request-path data that is expensive to re-query on every
// board page: board column count, category lists (public + admin), and a
// pre-sanitized tracking snippet source (nonce is applied per request).
type siteCache struct {
	mu            sync.RWMutex
	boardColumns  int
	catsPublic    []store.Category
	catsAdmin     []store.Category
	trackingSrc   string // raw stored snippet; sanitize per-request with nonce
	revision      int64
	loadedAt      time.Time
}

func newSiteCache() *siteCache {
	return &siteCache{boardColumns: 3}
}

func (c *siteCache) invalidate() {
	c.mu.Lock()
	c.loadedAt = time.Time{}
	c.mu.Unlock()
	atomic.AddInt64(&c.revision, 1)
}

func (c *siteCache) ensure(ctx context.Context, s *Server) {
	c.mu.RLock()
	ok := !c.loadedAt.IsZero()
	c.mu.RUnlock()
	if ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loadedAt.IsZero() {
		return
	}
	cols := 3
	if v, _ := s.store.GetSetting(ctx, "board_columns"); v == "4" {
		cols = 4
	}
	c.boardColumns = cols
	if cats, err := s.store.ListCategories(ctx, false); err == nil {
		c.catsPublic = cats
	}
	if cats, err := s.store.ListCategories(ctx, true); err == nil {
		c.catsAdmin = cats
	}
	if ts, err := s.store.GetSetting(ctx, "tracking_script"); err == nil {
		c.trackingSrc = ts
	} else {
		c.trackingSrc = ""
	}
	c.loadedAt = time.Now()
}

func (s *Server) cachedBoardColumns(ctx context.Context) int {
	if s.siteCache == nil {
		return s.boardColumns(ctx)
	}
	s.siteCache.ensure(ctx, s)
	s.siteCache.mu.RLock()
	defer s.siteCache.mu.RUnlock()
	return s.siteCache.boardColumns
}

func (s *Server) cachedCategories(ctx context.Context, includePrivate bool) []store.Category {
	if s.siteCache == nil {
		cats, _ := s.store.ListCategories(ctx, includePrivate)
		return cats
	}
	s.siteCache.ensure(ctx, s)
	s.siteCache.mu.RLock()
	defer s.siteCache.mu.RUnlock()
	if includePrivate {
		return append([]store.Category(nil), s.siteCache.catsAdmin...)
	}
	return append([]store.Category(nil), s.siteCache.catsPublic...)
}

func (s *Server) cachedTrackingHTML(ctx context.Context, nonce string) template.HTML {
	if s.siteCache == nil {
		ts, _ := s.store.GetSetting(ctx, "tracking_script")
		return sanitizeTrackingSnippet(ts, nonce)
	}
	s.siteCache.ensure(ctx, s)
	s.siteCache.mu.RLock()
	src := s.siteCache.trackingSrc
	s.siteCache.mu.RUnlock()
	return sanitizeTrackingSnippet(src, nonce)
}

func (s *Server) invalidateSiteCache() {
	if s.siteCache != nil {
		s.siteCache.invalidate()
	}
}
