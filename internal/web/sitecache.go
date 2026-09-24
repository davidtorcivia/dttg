package web

import (
	"context"
	"sync"

	"donottouchtheglass/internal/store"
)

// siteData is hot request-path data that is expensive to re-query on every page:
// board column count, category lists (public + admin), and the raw tracking
// snippet (sanitized per request, since the CSP nonce differs). A snapshot is
// immutable once built; invalidation just drops it.
type siteData struct {
	boardColumns int
	catsPublic   []store.Category
	catsAdmin    []store.Category
	tracking     string
}

type siteCache struct {
	mu   sync.Mutex
	snap *siteData
}

func (s *Server) siteData(ctx context.Context) *siteData {
	s.siteCache.mu.Lock()
	defer s.siteCache.mu.Unlock()
	if s.siteCache.snap == nil {
		d := &siteData{boardColumns: 3}
		if v, _ := s.store.GetSetting(ctx, "board_columns"); v == "4" {
			d.boardColumns = 4
		}
		d.catsPublic, _ = s.store.ListCategories(ctx, false)
		d.catsAdmin, _ = s.store.ListCategories(ctx, true)
		d.tracking, _ = s.store.GetSetting(ctx, "tracking_script")
		s.siteCache.snap = d
	}
	return s.siteCache.snap
}

func (s *Server) invalidateSiteCache() {
	s.siteCache.mu.Lock()
	s.siteCache.snap = nil
	s.siteCache.mu.Unlock()
}
