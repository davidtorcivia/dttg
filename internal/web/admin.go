package web

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"donottouchtheglass/internal/store"
)

// BackupController is satisfied by *backup.Backuper (nil when backups disabled).
type BackupController interface {
	RunOnce(ctx context.Context) error
	Status() (last time.Time, count int, lastErr string)
	RetentionDays() int
}

type settingsView struct {
	Tracking string
	Backup   backupView
}

type backupView struct {
	Enabled       bool
	Last          time.Time
	Count         int
	Err           string
	RetentionDays int
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListItems(r.Context(), store.ItemFilter{IncludePrivate: true, Limit: 1000})
	if err != nil {
		s.serverError(w, err)
		return
	}
	pd := s.page(r, "ADMIN")
	for _, it := range items {
		pd.Items = append(pd.Items, s.view(it))
	}
	s.render(w, "admin.html", pd)
}

// newItemForm holds prefill values for the admin "new item" form.
type newItemForm struct {
	URL, Title, Note, Category, Tags, Visibility string
}

func (s *Server) handleAdminNew(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pd := s.page(r, "NEW")
	pd.Prefill = &newItemForm{
		URL:        strings.TrimSpace(q.Get("url")),
		Title:      q.Get("title"),
		Note:       q.Get("note"),
		Category:   q.Get("category"),
		Tags:       q.Get("tags"),
		Visibility: q.Get("visibility"),
	}
	s.render(w, "admin_new.html", pd)
}

func (s *Server) handleAdminCreate(w http.ResponseWriter, r *http.Request) {
	in, err := s.parseItemInput(r)
	if err == nil {
		var id int64
		if id, err = s.ingest.Create(r.Context(), in); err == nil {
			http.Redirect(w, r, "/item/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
			return
		}
	}
	pd := s.page(r, "NEW")
	pd.Error = err.Error()
	pd.Prefill = &newItemForm{
		URL: in.URL, Title: in.Title, Note: in.Note,
		Category: in.Category, Tags: strings.Join(in.Tags, ", "), Visibility: in.Visibility,
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, "admin_new.html", pd)
}

func (s *Server) handleAdminEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	it, err := s.store.GetItem(r.Context(), id, true)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if it == nil {
		s.notFound(w, r)
		return
	}
	pd := s.page(r, "EDIT")
	v := s.view(*it)
	pd.Item = &v
	pd.TagsCSV = tagsCSV(it.Tags)
	s.render(w, "admin_edit.html", pd)
}

func (s *Server) handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	_ = r.ParseForm()
	var catID int64
	if c := strings.TrimSpace(r.FormValue("category")); c != "" {
		catID, _ = s.store.GetOrCreateCategory(r.Context(), c)
	}
	if err := s.store.UpdateItem(r.Context(), id, r.FormValue("title"), r.FormValue("note"), catID, r.FormValue("visibility")); err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.store.SetItemTags(r.Context(), id, splitTags(r.FormValue("tags"))); err != nil {
		s.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/item/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if mediaRows, err := s.store.ListMediaForItem(r.Context(), id); err == nil {
		for _, m := range mediaRows {
			_ = s.media.Delete(r.Context(), m.StorageKey)
		}
	}
	if err := s.store.DeleteItem(r.Context(), id); err != nil {
		s.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	tracking, _ := s.store.GetSetting(r.Context(), "tracking_script")
	sv := &settingsView{Tracking: tracking}
	sv.Backup.Enabled = s.backup != nil
	if s.backup != nil {
		last, count, lerr := s.backup.Status()
		sv.Backup.Last, sv.Backup.Count, sv.Backup.Err = last, count, lerr
		sv.Backup.RetentionDays = s.backup.RetentionDays()
	}
	pd := s.page(r, "SETTINGS")
	pd.Settings = sv
	s.render(w, "admin_settings.html", pd)
}

func (s *Server) handleAdminSettingsSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if err := s.store.SetSetting(r.Context(), "tracking_script", r.FormValue("tracking_script")); err != nil {
		s.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}

func (s *Server) handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	if s.backup != nil {
		if err := s.backup.RunOnce(r.Context()); err != nil {
			log.Printf("manual backup: %v", err)
		}
	}
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}

func tagsCSV(tags []store.Tag) string {
	names := make([]string, len(tags))
	for i, t := range tags {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}
