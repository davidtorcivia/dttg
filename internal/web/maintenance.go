package web

import (
	"context"
	"net/http"

	"donottouchtheglass/internal/store"
)

type orphanFile struct {
	Key  string
	Size int64
}

// maintenanceReport is the dry-run result shown before any deletion.
type maintenanceReport struct {
	OrphanFiles []orphanFile  // blobs in storage referenced by no media row
	OrphanBytes int64         // total size of OrphanFiles
	BrokenItems []itemView    // items whose primary media blob is missing from storage
	StrayRows   []store.Media // media rows whose parent item no longer exists
	ScanError   string        // set if storage/DB couldn't be enumerated (lists then empty)
}

func (r maintenanceReport) Empty() bool {
	return len(r.OrphanFiles) == 0 && len(r.BrokenItems) == 0 && len(r.StrayRows) == 0
}

// scanOrphans enumerates storage + DB and reports what cleanup would remove. It
// never mutates anything.
func (s *Server) scanOrphans(ctx context.Context) (maintenanceReport, error) {
	var rep maintenanceReport
	objs, err := s.media.List(ctx)
	if err != nil {
		return rep, err
	}
	present := make(map[string]bool, len(objs))
	for _, o := range objs {
		present[o.Key] = true
	}
	allMedia, err := s.store.AllMedia(ctx)
	if err != nil {
		return rep, err
	}
	referenced := make(map[string]bool, len(allMedia))
	for _, m := range allMedia {
		referenced[m.StorageKey] = true
	}
	// orphan files: in storage, referenced by no media row
	for _, o := range objs {
		if !referenced[o.Key] {
			rep.OrphanFiles = append(rep.OrphanFiles, orphanFile{Key: o.Key, Size: o.Size})
			rep.OrphanBytes += o.Size
		}
	}
	// broken items: primary media key set but blob missing — use slim projection
	keys, err := s.store.ListItemMediaKeys(ctx, true)
	if err != nil {
		return rep, err
	}
	for _, k := range keys {
		if (k.CoverKey != "" && !present[k.CoverKey]) || (k.FileKey != "" && !present[k.FileKey]) {
			if it, gerr := s.store.GetItem(ctx, k.ID, true); gerr == nil && it != nil {
				rep.BrokenItems = append(rep.BrokenItems, s.view(*it))
			}
		}
	}
	// media rows with no parent item
	rep.StrayRows, err = s.store.StrayMediaRows(ctx)
	if err != nil {
		return rep, err
	}
	return rep, nil
}

func (s *Server) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	pd := s.page(r, "MAINTENANCE")
	rep, err := s.scanOrphans(r.Context())
	if err != nil {
		rep = maintenanceReport{ScanError: err.Error()}
	}
	pd.Maintenance = &rep
	s.render(w, "maintenance.html", pd)
}

// handleMaintenanceCleanup re-scans (so it never acts on stale data) and deletes
// the orphaned blobs, broken items, and stray rows.
func (s *Server) handleMaintenanceCleanup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rep, err := s.scanOrphans(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, o := range rep.OrphanFiles {
		_ = s.media.Delete(ctx, o.Key)
	}
	for _, v := range rep.BrokenItems {
		if rows, e := s.store.ListMediaForItem(ctx, v.ID); e == nil {
			for _, m := range rows {
				_ = s.media.Delete(ctx, m.StorageKey)
			}
		}
		_ = s.store.DeleteItem(ctx, v.ID)
	}
	for _, m := range rep.StrayRows {
		_ = s.media.Delete(ctx, m.StorageKey)
		_ = s.store.DeleteMediaRow(ctx, m.ID)
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, "/admin/maintenance", http.StatusSeeOther)
}
