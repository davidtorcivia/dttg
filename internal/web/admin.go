package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"donottouchtheglass/internal/store"
)

type adminFilter struct {
	Query      string
	Visibility string
	Kind       string
	Category   string
}

func (f adminFilter) Active() bool {
	return f.Query != "" || f.Visibility != "" || f.Kind != "" || f.Category != ""
}

func (f adminFilter) QueryString(cursorCreated, cursorID int64) string {
	v := url.Values{}
	if f.Query != "" {
		v.Set("q", f.Query)
	}
	if f.Visibility != "" {
		v.Set("vis", f.Visibility)
	}
	if f.Kind != "" {
		v.Set("kind", f.Kind)
	}
	if f.Category != "" {
		v.Set("cat", f.Category)
	}
	if cursorCreated > 0 && cursorID > 0 {
		v.Set("cursor", strconv.FormatInt(cursorCreated, 10)+":"+strconv.FormatInt(cursorID, 10))
	}
	return v.Encode()
}

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
	Title          string
	Tagline        string
	BaseURL        string
	Description    string
	Backup         backupView
	Tokens         []store.APIToken
	ActiveSessions int
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
	q := r.URL.Query()
	af := adminFilter{
		Query:      strings.TrimSpace(q.Get("q")),
		Visibility: strings.TrimSpace(q.Get("vis")),
		Kind:       strings.TrimSpace(q.Get("kind")),
		Category:   strings.TrimSpace(q.Get("cat")),
	}
	f := store.ItemFilter{
		IncludePrivate: true,
		Cards:          true,
		Limit:          adminPage,
		Query:          af.Query,
		Visibility:     af.Visibility,
		Kind:           af.Kind,
		CategorySlug:   af.Category,
	}
	f.BeforeCreated, f.BeforeID = parseCursor(r)
	items, err := s.store.ListItems(r.Context(), f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pd := s.page(r, "ADMIN")
	pd.Items = s.views(items)
	pd.AdminFilter = &af
	pd.Notice = strings.TrimSpace(q.Get("notice"))
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
	if err == nil && r.FormValue("force") != "1" {
		if dup, derr := s.ingest.CheckDuplicate(r.Context(), in); derr == nil && dup != nil {
			pd := s.page(r, "NEW")
			v := s.view(*dup)
			pd.DuplicateOf = &v
			pd.Prefill = &newItemForm{
				URL: in.URL, Title: in.Title, Note: in.Note,
				Category: in.Category, Tags: strings.Join(in.Tags, ", "), Visibility: in.Visibility,
			}
			s.render(w, http.StatusConflict, "admin_new.html", pd)
			return
		}
	}
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

// validateSourceURL permits empty or absolute http(s) URLs with a host and no control chars.
func validateSourceURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.ContainsAny(raw, "\r\n\t") {
		return "", errors.New("source URL contains invalid characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errors.New("invalid source URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("source URL must use http or https")
	}
	return raw, nil
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
	pd.Notice = strings.TrimSpace(r.URL.Query().Get("notice"))
	s.render(w, status, "admin_edit.html", pd)
}

func (s *Server) handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	srcURL, err := validateSourceURL(r.FormValue("source_url"))
	if err != nil {
		s.renderEdit(w, r, id, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.store.UpdateItem(r.Context(), id, store.ItemEdit{
		Title:      r.FormValue("title"),
		Note:       r.FormValue("note"),
		SourceURL:  srcURL,
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

// handleAdminBulk applies batch actions (visibility, category, tags, delete) to
// selected items from the admin dashboard.
func (s *Server) handleAdminBulk(w http.ResponseWriter, r *http.Request) {
	var ids []int64
	for _, raw := range r.Form["ids"] {
		if id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		http.Redirect(w, r, "/admin?notice="+url.QueryEscape("No items selected"), http.StatusSeeOther)
		return
	}
	action := strings.TrimSpace(r.FormValue("action"))
	ctx := r.Context()
	var notice string
	switch action {
	case "public", "private":
		if err := s.store.BulkUpdateVisibility(ctx, ids, action); err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, id := range ids {
			if _, err := s.ingest.SyncPlacement(ctx, id); err != nil {
				log.Printf("bulk sync placement item %d: %v", id, err)
			}
		}
		notice = fmt.Sprintf("Set %d item(s) to %s", len(ids), action)
	case "category":
		cat := strings.TrimSpace(r.FormValue("bulk_category"))
		if err := s.store.BulkSetCategory(ctx, ids, cat); err != nil {
			s.serverError(w, r, err)
			return
		}
		if cat == "" {
			notice = fmt.Sprintf("Cleared category on %d item(s)", len(ids))
		} else {
			notice = fmt.Sprintf("Filed %d item(s) under %s", len(ids), cat)
		}
	case "tags":
		tags := splitTags(r.FormValue("bulk_tags"))
		if err := s.store.BulkAddTags(ctx, ids, tags); err != nil {
			s.serverError(w, r, err)
			return
		}
		notice = fmt.Sprintf("Tagged %d item(s)", len(ids))
	case "delete":
		for _, id := range ids {
			if err := s.deleteItem(ctx, id); err != nil {
				log.Printf("bulk delete item %d: %v", id, err)
			}
		}
		notice = fmt.Sprintf("Deleted %d item(s)", len(ids))
	default:
		http.Redirect(w, r, "/admin?notice="+url.QueryEscape("Choose a bulk action"), http.StatusSeeOther)
		return
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, "/admin?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// handleAdminReplaceMedia swaps the file(s)/image(s) on an item for freshly
// uploaded ones, keeping the item and its metadata (title, note, source, tags, etc.).
func (s *Server) handleAdminReplaceMedia(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	files, err := readUploads(r)
	if err == nil && len(files) == 0 {
		err = errors.New("choose a file to upload")
	}
	if err == nil {
		err = s.ingest.ReplaceFiles(r.Context(), id, files)
	}
	if err != nil {
		s.renderEdit(w, r, id, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, editPath(id), http.StatusSeeOther)
}

// handleAdminAddImages appends one or more uploaded images or an image URL to
// an existing item's gallery.
func (s *Server) handleAdminAddImages(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	files, err := readUploads(r)
	var urls []string
	if u := strings.TrimSpace(r.FormValue("image_url")); u != "" {
		urls = append(urls, u)
	}
	if err == nil {
		err = s.ingest.AddImages(r.Context(), id, files, urls)
	}
	if err != nil {
		s.renderEdit(w, r, id, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, editPath(id), http.StatusSeeOther)
}

// handleAdminMoveImage reorders one image in an item's gallery (left/right/first).
func (s *Server) handleAdminMoveImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	imgID, err := strconv.ParseInt(r.PathValue("imgID"), 10, 64)
	if !ok || err != nil || imgID <= 0 {
		s.notFound(w, r)
		return
	}
	dir := r.FormValue("dir")
	if err := s.store.MoveItemImage(r.Context(), id, imgID, dir); err != nil {
		s.renderEdit(w, r, id, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, editPath(id), http.StatusSeeOther)
}

// handleAdminDeleteImage deletes one image from a multi-image item.
func (s *Server) handleAdminDeleteImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	imgID, err := strconv.ParseInt(r.PathValue("imgID"), 10, 64)
	if !ok || err != nil || imgID <= 0 {
		s.notFound(w, r)
		return
	}
	if err := s.ingest.DeleteImage(r.Context(), id, imgID); err != nil {
		s.renderEdit(w, r, id, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, editPath(id), http.StatusSeeOther)
}

// handleAdminReorderImages persists a drag-and-drop image order (comma-separated IDs).
func (s *Server) handleAdminReorderImages(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	var order []int64
	for _, p := range strings.Split(r.FormValue("order"), ",") {
		if imgID, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil && imgID > 0 {
			order = append(order, imgID)
		}
	}
	if len(order) == 0 {
		s.renderEdit(w, r, id, http.StatusBadRequest, "no image order provided")
		return
	}
	if err := s.store.ReorderItemImages(r.Context(), id, order); err != nil {
		s.renderEdit(w, r, id, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.invalidateSiteCache()
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	http.Redirect(w, r, editPath(id), http.StatusSeeOther)
}

// handleAdminUpdateImageAlt updates the caption/alt text on a single slide.
func (s *Server) handleAdminUpdateImageAlt(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	imgID, err := strconv.ParseInt(r.PathValue("imgID"), 10, 64)
	if !ok || err != nil || imgID <= 0 {
		s.notFound(w, r)
		return
	}
	if err := s.store.UpdateItemImageAlt(r.Context(), id, imgID, r.FormValue("alt")); err != nil {
		s.renderEdit(w, r, id, http.StatusUnprocessableEntity, err.Error())
		return
	}
	http.Redirect(w, r, editPath(id)+"?notice="+url.QueryEscape("Caption saved"), http.StatusSeeOther)
}

// handleAdminRefreshTweet re-fetches a tweet item to pull in any missing photos.
func (s *Server) handleAdminRefreshTweet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	added, err := s.ingest.RefreshTweet(r.Context(), id)
	if err != nil {
		s.renderEdit(w, r, id, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.invalidateSiteCache()
	msg := "Tweet already has all photos"
	if added > 0 {
		msg = fmt.Sprintf("Imported %d additional photo(s) from tweet", added)
	}
	http.Redirect(w, r, editPath(id)+"?notice="+url.QueryEscape(msg), http.StatusSeeOther)
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
	sv.ActiveSessions, _ = s.store.CountActiveSessions(r.Context())
	pd := s.page(r, "SETTINGS")
	pd.Settings, pd.Error = sv, msg
	pd.Notice = strings.TrimSpace(r.URL.Query().Get("notice"))
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

func (s *Server) handleAdminRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	n, err := s.store.DeleteOtherSessions(r.Context(), HashSession(c.Value))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	msg := fmt.Sprintf("Revoked %d other session(s)", n)
	http.Redirect(w, r, "/admin/settings?notice="+url.QueryEscape(msg), http.StatusSeeOther)
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
