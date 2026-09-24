package web

import (
	"context"
	"errors"
	"log"
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
	allMedia, err := s.store.AllMedia(ctx)
	if err != nil {
		return rep, err
	}
	// Without R2 configured, every R2-hosted blob would look missing and cleanup
	// would delete those items wholesale — refuse instead.
	_, hasR2 := s.media.Placement("probe/file", false)
	referenced := make(map[string]bool, len(allMedia))
	for _, m := range allMedia {
		if m.OnR2 && !m.OnLocal && !hasR2 {
			return rep, errors.New("media is stored on R2 but R2 isn't configured here; set the R2_* env vars before scanning")
		}
		referenced[m.StorageKey] = true
	}
	present := make(map[string]bool, len(objs))
	for _, o := range objs {
		present[o.Key] = true
		if !referenced[o.Key] { // in storage, referenced by no media row
			rep.OrphanFiles = append(rep.OrphanFiles, orphanFile{Key: o.Key, Size: o.Size})
			rep.OrphanBytes += o.Size
		}
	}
	// Broken items: primary media key set but blob missing (slim projection first).
	keys, err := s.store.ListItemMediaKeys(ctx)
	if err != nil {
		return rep, err
	}
	for _, k := range keys {
		if (k.CoverKey != "" && !present[k.CoverKey]) || (k.FileKey != "" && !present[k.FileKey]) {
			if it, err := s.store.GetItem(ctx, k.ID, true); err == nil && it != nil {
				rep.BrokenItems = append(rep.BrokenItems, s.view(*it))
			}
		}
	}
	rep.StrayRows, err = s.store.StrayMediaRows(ctx)
	return rep, err
}

func (s *Server) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	rep, err := s.scanOrphans(r.Context())
	if err != nil {
		rep = maintenanceReport{ScanError: err.Error()}
	}
	pd := s.page(r, "MAINTENANCE")
	pd.Maintenance = &rep
	s.render(w, http.StatusOK, "maintenance.html", pd)
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
		if err := s.deleteItem(ctx, v.ID); err != nil {
			log.Printf("cleanup item %d: %v", v.ID, err)
		}
	}
	for _, m := range rep.StrayRows {
		_ = s.media.Delete(ctx, m.StorageKey)
		_ = s.store.DeleteMediaRow(ctx, m.ID)
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, "/admin/maintenance", http.StatusSeeOther)
}

// deleteItem removes an item's row first (media rows cascade), then its blobs,
// so a failure never leaves a live item pointing at deleted media.
func (s *Server) deleteItem(ctx context.Context, id int64) error {
	rows, err := s.store.ListMedia(ctx, id)
	if err != nil {
		return err
	}
	if err := s.store.DeleteItem(ctx, id); err != nil {
		return err
	}
	for _, m := range rows {
		if err := s.media.Delete(ctx, m.StorageKey); err != nil {
			log.Printf("delete blob %s: %v (orphan; clean up via maintenance)", m.StorageKey, err)
		}
	}
	return nil
}
