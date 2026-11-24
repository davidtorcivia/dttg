package web

import (
	"context"
	"sync"
	"sync/atomic"

	"donottouchtheglass/internal/store"
)

// siteData is hot request-path data that is expensive to re-query on every page:
// board column count, category lists (public + admin), and the raw tracking
// snippet (sanitized per request, since the CSP nonce differs). Snapshots are
// immutable and read lock-free via atomic.Pointer; only a cache miss serializes
// on buildMu (so invalidation never stalls behind a slow read).
type siteData struct {
	boardColumns int
	catsPublic   []store.Category
	catsAdmin    []store.Category
	tracking     string
	publicStats  store.Stats
	hasStats     bool
}

type siteCache struct {
	snap    atomic.Pointer[siteData]
	buildMu sync.Mutex
}

func (s *Server) siteData(ctx context.Context) *siteData {
	if d := s.siteCache.snap.Load(); d != nil {
		return d
	}
	s.siteCache.buildMu.Lock()
	defer s.siteCache.buildMu.Unlock()
	if d := s.siteCache.snap.Load(); d != nil {
		return d
	}
	d := &siteData{boardColumns: 3}
	if v, _ := s.store.GetSetting(ctx, "board_columns"); v == "4" {
		d.boardColumns = 4
	}
	d.catsPublic, _ = s.store.ListCategories(ctx, false)
	d.catsAdmin, _ = s.store.ListCategories(ctx, true)
	d.tracking, _ = s.store.GetSetting(ctx, "tracking_script")
	if st, err := s.store.PublicStats(ctx); err == nil {
		d.publicStats = st
		d.hasStats = true
	}
	s.siteCache.snap.Store(d)
	return d
}

func (s *Server) invalidateSiteCache() {
	s.siteCache.snap.Store(nil)
	s.pageCache.clear()
}
