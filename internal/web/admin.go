package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"donottouchtheglass/internal/store"
)

// BackupController is satisfied by *backup.Backuper (nil when backups disabled).
type BackupController interface {
	RunOnce(ctx context.Context) error
	Status() (last time.Time, count int, lastErr string)
	LatestRemote(ctx context.Context) (last time.Time, count int, err error)
	RetentionDays() int
}

type settingsView struct {
	Tracking string
	Columns  int // wide-screen board columns (3 or 4)
	// Site identity (admin-editable branding; empty falls back to env defaults).
	Title       string
	Tagline     string
	BaseURL     string
	Description string
	Backup      backupView
	Tokens      []store.APIToken
}

type backupView struct {
	Enabled       bool
	Last          time.Time
	Count         int
	Err           string
	RetentionDays int
}

const trackingRejected = "Tracking snippet must be external <script src=...> only"

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.isAdmin(r) {
			next(w, r)
			return
		}
		// Preserve the return path for GET/HEAD; unsafe methods drop to bare
		// /login so a replayed POST does not re-hit.
		dest := "/login"
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			dest += "?next=" + url.QueryEscape(r.URL.RequestURI())
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
	}
}

// pathID parses the {id} path segment as a positive integer.
func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	const adminPage = 100
	f := store.ItemFilter{IncludePrivate: true, Cards: true, Limit: adminPage}
	f.BeforeCreated, f.BeforeID = parseCursor(r)
	items, err := s.store.ListItems(r.Context(), f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pd := s.page(r, "ADMIN")
	pd.Items = s.views(items)
	if n := len(items); n == adminPage {
		pd.CursorCreated, pd.CursorID = items[n-1].CreatedAt.Unix(), items[n-1].ID
	}
	pd.BoardDone = len(items) < adminPage
	s.render(w, http.StatusOK, "admin.html", pd)
}

// newItemForm holds prefill values for the admin "new item" form.
type newItemForm struct {
	URL, Title, Note, Category, Tags, Visibility string
}

func (s *Server) handleAdminNew(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pd := s.page(r, "NEW")
	pd.Prefill = &newItemForm{
		URL: strings.TrimSpace(q.Get("url")), Title: q.Get("title"), Note: q.Get("note"),
		Category: q.Get("category"), Tags: q.Get("tags"), Visibility: q.Get("visibility"),
	}
	s.render(w, http.StatusOK, "admin_new.html", pd)
}

func (s *Server) handleAdminCreate(w http.ResponseWriter, r *http.Request) {
	in, err := s.parseItemInput(r) // body already capped + parsed by the csrf middleware
	if err == nil {
		var id int64
		if id, err = s.ingest.Create(r.Context(), in); err == nil {
			s.invalidateSiteCache()
			http.Redirect(w, r, itemPath(id), http.StatusSeeOther)
			return
		}
	}
	// Re-render with everything typed so far; only the file must be re-chosen.
	pd := s.page(r, "NEW")
	pd.Error = err.Error()
	pd.Prefill = &newItemForm{
		URL: in.URL, Title: in.Title, Note: in.Note,
		Category: in.Category, Tags: strings.Join(in.Tags, ", "), Visibility: in.Visibility,
	}
	s.render(w, http.StatusUnprocessableEntity, "admin_new.html", pd)
}

func (s *Server) handleAdminEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	s.renderEdit(w, r, id, http.StatusOK, "")
}

// renderEdit shows the edit form for id, optionally with an error message.
func (s *Server) renderEdit(w http.ResponseWriter, r *http.Request, id int64, status int, msg string) {
	it, err := s.store.GetItem(r.Context(), id, true)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if it == nil {
		s.notFound(w, r)
		return
	}
	pd := s.page(r, "EDIT")
	v := s.view(*it)
	pd.Item, pd.TagsCSV, pd.Error = &v, tagsCSV(it.Tags), msg
	s.render(w, status, "admin_edit.html", pd)
}

func (s *Server) handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if err := s.store.UpdateItem(r.Context(), id, store.ItemEdit{
		Title:      r.FormValue("title"),
		Note:       r.FormValue("note"),
		SourceURL:  r.FormValue("source_url"),
		Category:   r.FormValue("category"),
		Visibility: r.FormValue("visibility"),
		Tags:       splitTags(r.FormValue("tags")),
	}); err != nil {
		s.renderEdit(w, r, id, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.invalidateSiteCache()
	// A visibility change moves the blobs: private media off the public bucket,
	// public media onto it.
	if _, err := s.ingest.SyncPlacement(r.Context(), id); err != nil {
		log.Printf("item %d: sync media placement: %v", id, err)
		s.renderEdit(w, r, id, http.StatusInternalServerError, "Saved, but its media could not be moved: "+err.Error())
		return
	}
	http.Redirect(w, r, itemPath(id), http.StatusSeeOther)
}

// handleAdminReplaceMedia swaps the file/image on an item for a freshly uploaded
// one, keeping the item and its metadata (title, note, source, tags, etc.).
func (s *Server) handleAdminReplaceMedia(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	data, name, err := readUpload(r)
	if err == nil && data == nil {
		err = errors.New("choose a file to upload")
	}
	if err == nil {
		err = s.ingest.ReplaceFile(r.Context(), id, data, name)
	}
	if err != nil {
		s.renderEdit(w, r, id, http.StatusUnprocessableEntity, err.Error())
		return
	}
	http.Redirect(w, r, editPath(id), http.StatusSeeOther)
}

func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if err := s.deleteItem(r.Context(), id); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	tracking, _ := s.store.GetSetting(r.Context(), "tracking_script")
	id := s.siteID()
	msg := ""
	// Warn when a stored snippet is no longer renderable under the strict
	// external-script-only sanitizer (inline JS / non-script tags).
	if tracking != "" && sanitizeTrackingSnippet(tracking, "x") == "" {
		msg = trackingRejected
	}
	s.renderSettings(w, r, http.StatusOK, &settingsView{
		Tracking: tracking, Columns: s.siteData(r.Context()).boardColumns,
		Title: id.Title, Tagline: id.Tagline, BaseURL: id.BaseURL, Description: id.Description,
	}, msg)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, sv *settingsView, msg string) {
	if s.backup != nil {
		b := &sv.Backup
		b.Enabled, b.RetentionDays = true, s.backup.RetentionDays()
		_, _, b.Err = s.backup.Status() // last error this process
		// Real backup history from R2 (survives restarts, unlike the process counter).
		if last, count, err := s.backup.LatestRemote(r.Context()); err == nil {
			b.Last, b.Count = last, count
		} else if b.Err == "" {
			b.Err = "could not list R2 backups: " + err.Error()
		}
	}
	sv.Tokens, _ = s.store.ListTokens(r.Context())
	pd := s.page(r, "SETTINGS")
	pd.Settings, pd.Error = sv, msg
	s.render(w, status, "admin_settings.html", pd)
}

func (s *Server) handleAdminSettingsSave(w http.ResponseWriter, r *http.Request) {
	sv := &settingsView{
		Tracking:    r.FormValue("tracking_script"),
		Columns:     3,
		Title:       strings.TrimSpace(r.FormValue("site_title")),
		Tagline:     strings.TrimSpace(r.FormValue("site_tagline")),
		BaseURL:     strings.TrimSpace(r.FormValue("base_url")),
		Description: strings.TrimSpace(r.FormValue("site_description")),
	}
	if r.FormValue("board_columns") == "4" {
		sv.Columns = 4
	}
	var errs []string
	if base, err := validateBaseURL(sv.BaseURL, s.cfg.Dev); err != nil {
		errs = append(errs, err.Error())
	} else {
		sv.BaseURL = base
	}
	if strings.TrimSpace(sv.Tracking) != "" && sanitizeTrackingSnippet(sv.Tracking, "x") == "" {
		errs = append(errs, trackingRejected)
	}
	if len(errs) > 0 {
		s.renderSettings(w, r, http.StatusBadRequest, sv, strings.Join(errs, "; "))
		return
	}
	// Stored values override the env config; blank reverts to the env default
	// (loadSite falls back when a setting is empty).
	for k, v := range map[string]string{
		"site_title":       sv.Title,
		"site_tagline":     sv.Tagline,
		"base_url":         sv.BaseURL,
		"site_description": sv.Description,
		"tracking_script":  sv.Tracking,
		"board_columns":    strconv.Itoa(sv.Columns),
	} {
		if err := s.store.SetSetting(r.Context(), k, v); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.loadSite(r.Context()) // republish branding (and bust the cached share card via its fingerprint)
	s.invalidateSiteCache()
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}

func (s *Server) handleAdminTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if id := strings.TrimSpace(r.FormValue("id")); id != "" {
		if err := s.store.RevokeToken(r.Context(), id); err != nil {
			log.Printf("revoke token %s: %v", id, err)
		}
	}
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}

// validateBaseURL accepts an empty value (falls back to env) or an absolute
// http(s) URL with a host and no userinfo/query/fragment. https is required
// unless dev is true. Trailing slashes are stripped.
func validateBaseURL(raw string, dev bool) (string, error) {
	if raw = strings.TrimSpace(raw); raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", errors.New("invalid base URL")
	case u.Scheme == "" || u.Host == "":
		return "", errors.New("base URL must include a scheme and host")
	case u.User != nil:
		return "", errors.New("base URL must not include userinfo")
	case u.RawQuery != "" || u.Fragment != "":
		return "", errors.New("base URL must not include query or fragment")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(scheme == "http" && dev) {
		return "", errors.New("base URL must use https")
	}
	return strings.TrimRight(scheme+"://"+u.Host+u.Path, "/"), nil
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
