package web

import (
	"context"
	"errors"
	"io"
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

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			// Preserve return path for GET/HEAD; unsafe methods (including /share
			// for now) drop to bare /login so a replayed POST does not re-hit.
			dest := "/login"
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				path := r.URL.Path
				if q := r.URL.RawQuery; q != "" {
					path += "?" + q
				}
				dest = "/login?next=" + url.QueryEscape(path)
			}
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	const adminPage = 100
	f := store.ItemFilter{IncludePrivate: true, Limit: adminPage}
	if cur := r.URL.Query().Get("cursor"); cur != "" {
		if parts := strings.SplitN(cur, ":", 2); len(parts) == 2 {
			f.BeforeCreated, _ = strconv.ParseInt(parts[0], 10, 64)
			f.BeforeID, _ = strconv.ParseInt(parts[1], 10, 64)
		}
	}
	items, err := s.store.ListItemCards(r.Context(), f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pd := s.page(r, "ADMIN")
	for _, it := range items {
		pd.Items = append(pd.Items, s.view(it))
	}
	if n := len(items); n == adminPage {
		last := items[n-1]
		pd.CursorCreated = last.CreatedAt.Unix()
		pd.CursorID = last.ID
	}
	pd.BoardDone = len(items) < adminPage
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
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1)
	in, err := s.parseItemInput(r)
	if err != nil {
		status := http.StatusUnprocessableEntity
		msg := err.Error()
		if errors.Is(err, errUploadTooLarge) || isMaxBytesError(err) {
			status = http.StatusRequestEntityTooLarge
			msg = "upload too large"
		}
		pd := s.page(r, "NEW")
		pd.Error = msg
		pd.Prefill = &newItemForm{
			URL: in.URL, Title: in.Title, Note: in.Note,
			Category: in.Category, Tags: strings.Join(in.Tags, ", "), Visibility: in.Visibility,
		}
		w.WriteHeader(status)
		s.render(w, "admin_new.html", pd)
		return
	}
	id, err := s.ingest.Create(r.Context(), in)
	if err != nil {
		pd := s.page(r, "NEW")
		pd.Error = err.Error()
		pd.Prefill = &newItemForm{
			URL: in.URL, Title: in.Title, Note: in.Note,
			Category: in.Category, Tags: strings.Join(in.Tags, ", "), Visibility: in.Visibility,
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "admin_new.html", pd)
		return
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, "/item/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleAdminEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
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
	if err := s.store.UpdateItem(r.Context(), id, r.FormValue("title"), r.FormValue("note"), r.FormValue("source_url"), catID, r.FormValue("visibility")); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.SetItemTags(r.Context(), id, splitTags(r.FormValue("tags"))); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, "/item/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleAdminReplaceMedia swaps the file/image on an item for a freshly uploaded
// one, keeping the item and its metadata (title, note, source, tags, etc.).
func (s *Server) handleAdminReplaceMedia(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	editURL := "/admin/items/" + strconv.FormatInt(id, 10) + "/edit"
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		if isMaxBytesError(err) {
			pd := s.page(r, "EDIT")
			pd.Error = "upload too large"
			if it, gerr := s.store.GetItem(r.Context(), id, true); gerr == nil && it != nil {
				v := s.view(*it)
				pd.Item = &v
				pd.TagsCSV = tagsCSV(it.Tags)
			}
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			s.render(w, "admin_edit.html", pd)
			return
		}
		s.serverError(w, r, err)
		return
	}
	f, fh, err := r.FormFile("file")
	if err != nil {
		http.Redirect(w, r, editURL, http.StatusSeeOther) // no file chosen
		return
	}
	defer f.Close()
	data, rerr := io.ReadAll(io.LimitReader(f, maxUpload+1))
	if rerr != nil {
		s.serverError(w, r, rerr)
		return
	}
	if len(data) > maxUpload {
		pd := s.page(r, "EDIT")
		pd.Error = "upload too large"
		if it, gerr := s.store.GetItem(r.Context(), id, true); gerr == nil && it != nil {
			v := s.view(*it)
			pd.Item = &v
			pd.TagsCSV = tagsCSV(it.Tags)
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		s.render(w, "admin_edit.html", pd)
		return
	}
	if err := s.ingest.ReplaceFile(r.Context(), id, data, fh.Filename); err != nil {
		pd := s.page(r, "EDIT")
		pd.Error = err.Error()
		if it, gerr := s.store.GetItem(r.Context(), id, true); gerr == nil && it != nil {
			v := s.view(*it)
			pd.Item = &v
			pd.TagsCSV = tagsCSV(it.Tags)
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "admin_edit.html", pd)
		return
	}
	http.Redirect(w, r, editURL, http.StatusSeeOther)
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
		s.serverError(w, r, err)
		return
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	tracking, _ := s.store.GetSetting(r.Context(), "tracking_script")
	id := s.siteID()
	sv := &settingsView{
		Tracking:    tracking,
		Columns:     s.boardColumns(r.Context()),
		Title:       id.Title,
		Tagline:     id.Tagline,
		BaseURL:     id.BaseURL,
		Description: id.Description,
	}
	sv.Backup.Enabled = s.backup != nil
	if s.backup != nil {
		_, _, sv.Backup.Err = s.backup.Status() // last error this session
		sv.Backup.RetentionDays = s.backup.RetentionDays()
		// real backup history from R2 (survives restarts, unlike the session counter)
		if last, count, err := s.backup.LatestRemote(r.Context()); err == nil {
			sv.Backup.Last, sv.Backup.Count = last, count
		} else if sv.Backup.Err == "" {
			sv.Backup.Err = "could not list R2 backups: " + err.Error()
		}
	}
	if toks, err := s.store.ListTokens(r.Context()); err == nil {
		sv.Tokens = toks
	}
	pd := s.page(r, "SETTINGS")
	pd.Settings = sv
	// Warn when a previously stored snippet is no longer renderable under the
	// strict external-script-only sanitizer (inline JS / non-script tags).
	if tracking != "" && sanitizeTrackingSnippet(tracking, "x") == "" {
		pd.Error = "Tracking snippet must be external <script src=...> only"
	}
	s.render(w, "admin_settings.html", pd)
}

func (s *Server) handleAdminSettingsSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	baseURLRaw := strings.TrimSpace(r.FormValue("base_url"))
	baseURL, baseErr := validateBaseURL(baseURLRaw, s.cfg.Dev)
	tracking := r.FormValue("tracking_script")
	trackingErr := ""
	if strings.TrimSpace(tracking) != "" && sanitizeTrackingSnippet(tracking, "x") == "" {
		trackingErr = "Tracking snippet must be external <script src=...> only"
	}

	sv := &settingsView{
		Tracking:    tracking,
		Columns:     3,
		Title:       strings.TrimSpace(r.FormValue("site_title")),
		Tagline:     strings.TrimSpace(r.FormValue("site_tagline")),
		BaseURL:     baseURLRaw,
		Description: strings.TrimSpace(r.FormValue("site_description")),
	}
	if r.FormValue("board_columns") == "4" {
		sv.Columns = 4
	}
	if baseErr == nil {
		sv.BaseURL = baseURL
	}

	if baseErr != nil || trackingErr != "" {
		pd := s.page(r, "SETTINGS")
		pd.Settings = sv
		switch {
		case baseErr != nil && trackingErr != "":
			pd.Error = baseErr.Error() + "; " + trackingErr
		case baseErr != nil:
			pd.Error = baseErr.Error()
		default:
			pd.Error = trackingErr
		}
		sv.Backup.Enabled = s.backup != nil
		if s.backup != nil {
			_, _, sv.Backup.Err = s.backup.Status()
			sv.Backup.RetentionDays = s.backup.RetentionDays()
			if last, count, err := s.backup.LatestRemote(r.Context()); err == nil {
				sv.Backup.Last, sv.Backup.Count = last, count
			}
		}
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "admin_settings.html", pd)
		return
	}

	// Site identity. Stored values override the env config; blank reverts to the
	// env default (loadSite falls back when a setting is empty).
	site := map[string]string{
		"site_title":       sv.Title,
		"site_tagline":     sv.Tagline,
		"base_url":         baseURL,
		"site_description": sv.Description,
	}
	for k, v := range site {
		if err := s.store.SetSetting(r.Context(), k, v); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if err := s.store.SetSetting(r.Context(), "tracking_script", tracking); err != nil {
		s.serverError(w, r, err)
		return
	}
	cols := "3"
	if sv.Columns == 4 {
		cols = "4"
	}
	if err := s.store.SetSetting(r.Context(), "board_columns", cols); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.loadSite(r.Context()) // republish branding (and bust the cached share card via its fingerprint)
	s.invalidateSiteCache()
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}

func (s *Server) handleAdminTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
		return
	}
	if err := s.store.RevokeToken(r.Context(), id); err != nil {
		log.Printf("revoke token %s: %v", id, err)
	}
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}

// validateBaseURL accepts an empty value (falls back to env) or an absolute
// http(s) URL with a host and no userinfo/query/fragment. https is required
// unless dev is true. Trailing slashes are stripped.
func validateBaseURL(raw string, dev bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid base URL")
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("base URL must include a scheme and host")
	}
	if u.User != nil {
		return "", errors.New("base URL must not include userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("base URL must not include query or fragment")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		// ok
	case "http":
		if !dev {
			return "", errors.New("base URL must use https")
		}
	default:
		return "", errors.New("base URL must use https")
	}
	out := strings.ToLower(u.Scheme) + "://" + u.Host + u.Path
	return strings.TrimRight(out, "/"), nil
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
