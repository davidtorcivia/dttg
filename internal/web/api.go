package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"donottouchtheglass/internal/ingest"
	"donottouchtheglass/internal/store"
)

const maxUpload = 30 << 20 // 30 MB

// errUploadTooLarge is returned when a multipart body or file part exceeds maxUpload.
var errUploadTooLarge = errors.New("upload too large")

type apiTokenHashKey struct{}

// tokenAuth wraps an API handler, requiring a valid bearer token. The token
// hash is stashed in the context so create-path rate limits can key on it.
func (s *Server) tokenAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing api token")
			return
		}
		hash := HashToken(tok)
		if ok, err := s.store.TokenValid(r.Context(), hash); err != nil || !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid api token")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), apiTokenHashKey{}, hash)))
	}
}

// cors lets the browser extension (an unpredictable moz-extension:// origin)
// call the token-authed API cross-origin. Safe to allow any origin here because
// every request is gated on the bearer token — there is no cookie/credential to
// hijack. Preflight OPTIONS is answered before auth (it carries no token).
func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Token")
		h.Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-API-Token"))
}

// limit spends one token of key's budget, answering 429 + Retry-After when it's
// exhausted. Returns whether the request may proceed.
func limit(w http.ResponseWriter, tb *tokenBucket, key string) bool {
	ok, retry := tb.allow(key)
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeJSONError(w, http.StatusTooManyRequests, "rate limited")
	}
	return ok
}

// handleAPICreateItem archives an item from JSON or multipart and returns its id+url.
func (s *Server) handleAPICreateItem(w http.ResponseWriter, r *http.Request) {
	tokenHash, _ := r.Context().Value(apiTokenHashKey{}).(string)
	if !limit(w, s.apiCreateTokenRL, tokenHash) || !limit(w, s.apiCreateIPRL, s.clientIP(r)) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1) // before any parse / decode
	in, err := s.parseItemInput(r)
	switch {
	case errors.Is(err, errUploadTooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	case err != nil:
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.ingest.Create(r.Context(), in)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.invalidateSiteCache()
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "url": s.absURL(itemPath(id))})
}

// handleAPITaxonomy returns the archive's categories and tags so the extension
// can autocomplete them. Token-authed (owner-only); never cached by the browser.
func (s *Server) handleAPITaxonomy(w http.ResponseWriter, r *http.Request) {
	cats, _ := s.store.ListCategories(r.Context(), true)
	tags, _ := s.store.ListTags(r.Context(), true)
	catNames := make([]string, len(cats))
	for i, c := range cats {
		catNames[i] = c.Name
	}
	tagNames := make([]string, len(tags))
	for i, t := range tags {
		tagNames[i] = t.Name
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"categories": catNames, "tags": tagNames})
}

// parseItemInput reads an ingest.Input from a JSON body, multipart form, or
// urlencoded form (the extension/bookmarklet/admin all funnel through here).
func (s *Server) parseItemInput(r *http.Request) (ingest.Input, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Kind       string   `json:"kind"`
			URL        string   `json:"url"`
			Source     string   `json:"source"`
			Title      string   `json:"title"`
			Note       string   `json:"note"`
			Category   string   `json:"category"`
			Visibility string   `json:"visibility"`
			Tags       []string `json:"tags"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			return ingest.Input{}, fmt.Errorf("invalid json: %w", err)
		}
		return ingest.Input{
			Kind: body.Kind, URL: strings.TrimSpace(body.URL), Source: strings.TrimSpace(body.Source),
			Title: body.Title, Note: body.Note, Category: body.Category,
			Visibility: body.Visibility, Tags: body.Tags,
		}, nil
	}
	// ParseMultipartForm falls back to ParseForm for urlencoded bodies.
	if err := r.ParseMultipartForm(maxUpload); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		if isMaxBytesError(err) {
			return ingest.Input{}, errUploadTooLarge
		}
		return ingest.Input{}, fmt.Errorf("parse form: %w", err)
	}
	in := ingest.Input{
		Kind:       r.FormValue("kind"),
		URL:        strings.TrimSpace(r.FormValue("url")),
		Source:     strings.TrimSpace(r.FormValue("source")),
		Title:      r.FormValue("title"),
		Note:       r.FormValue("note"),
		Category:   r.FormValue("category"),
		Visibility: r.FormValue("visibility"),
		Tags:       splitTags(r.FormValue("tags")),
	}
	var err error
	in.FileBytes, in.FileName, err = readUpload(r)
	return in, err
}

// readUpload returns the "file" form part, if any, capped at maxUpload.
func readUpload(r *http.Request) ([]byte, string, error) {
	f, fh, err := r.FormFile("file")
	if errors.Is(err, http.ErrMissingFile) || errors.Is(err, http.ErrNotMultipart) {
		return nil, "", nil
	}
	if err != nil {
		if isMaxBytesError(err) {
			return nil, "", errUploadTooLarge
		}
		return nil, "", fmt.Errorf("file: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
	switch {
	case err != nil:
		return nil, "", fmt.Errorf("read upload: %w", err)
	case len(data) > maxUpload:
		return nil, "", errUploadTooLarge
	}
	return data, fh.Filename, nil
}

// handleShare receives content shared to the installed PWA (Android share sheet).
// Authenticated sessions ingest immediately. Unauthenticated shares are stashed
// in pending_shares (plus a local pending file) and recovered after login.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin share rejected", http.StatusForbidden)
		return
	}
	admin := s.isAdmin(r)
	if !admin && !limit(w, s.shareRL, s.clientIP(r)) { // anonymous stashes write to disk
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1)
	if err := r.ParseMultipartForm(maxUpload); isMaxBytesError(err) {
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	text := strings.TrimSpace(r.FormValue("text"))
	shared := strings.TrimSpace(r.FormValue("url"))
	if shared == "" {
		shared = firstURL(text)
	}
	data, name, err := readUpload(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errUploadTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}

	if admin {
		if len(data) > 0 {
			s.createAndEdit(w, r, ingest.Input{FileBytes: data, FileName: name, Title: title})
			return
		}
		http.Redirect(w, r, newItemURL(shared, title, text), http.StatusSeeOther)
		return
	}

	// Unauthenticated: stash and send through login.
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	p := store.PendingShare{ID: hex.EncodeToString(b), ExpiresAt: time.Now().Add(30 * time.Minute),
		Title: title, Text: text, URL: shared}
	if len(data) > 0 {
		ext := filepath.Ext(name)
		if len(ext) > 8 {
			ext = ""
		}
		p.FileKey, p.FileName, p.FileSize, p.FileMime = p.ID+ext, name, int64(len(data)), http.DetectContentType(data)
		if err := os.MkdirAll(filepath.Dir(s.pendingPath(p.FileKey)), 0o700); err != nil {
			s.serverError(w, r, err)
			return
		}
		if err := os.WriteFile(s.pendingPath(p.FileKey), data, 0o600); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if err := s.store.CreatePendingShare(r.Context(), p); err != nil {
		s.removePending(p.FileKey)
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape("/share/pending/"+p.ID), http.StatusSeeOther)
}

// handleSharePending recovers a stashed PWA share after login.
func (s *Server) handleSharePending(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.TakePendingShare(r.Context(), r.PathValue("id"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if p == nil {
		http.Error(w, "share expired or not found", http.StatusNotFound)
		return
	}
	if p.FileKey == "" {
		http.Redirect(w, r, newItemURL(p.URL, p.Title, p.Text), http.StatusSeeOther)
		return
	}
	defer s.removePending(p.FileKey)
	data, err := os.ReadFile(s.pendingPath(p.FileKey))
	if err != nil {
		http.Error(w, "shared file missing", http.StatusGone)
		return
	}
	s.createAndEdit(w, r, ingest.Input{FileBytes: data, FileName: p.FileName, Title: p.Title})
}

// createAndEdit archives a shared file and lands on its edit form.
func (s *Server) createAndEdit(w http.ResponseWriter, r *http.Request, in ingest.Input) {
	id, err := s.ingest.Create(r.Context(), in)
	if err != nil {
		http.Error(w, "could not archive shared file: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, editPath(id), http.StatusSeeOther)
}

func editPath(id int64) string { return "/admin/items/" + strconv.FormatInt(id, 10) + "/edit" }

// newItemURL is the prefilled admin "new item" form for a shared link/text.
func newItemURL(link, title, text string) string {
	q := url.Values{}
	for k, v := range map[string]string{"url": link, "title": title} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if text != "" && text != link {
		q.Set("note", text)
	}
	return "/admin/new?" + q.Encode()
}

// sameOrigin accepts POSTs from this site (PWA share sheet, same-origin forms):
// explicit cross-site Fetch Metadata is rejected, and an Origin must match.
func (s *Server) sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site":
		return false
	case "same-site":
		return true // multi-subdomain setups behind one eTLD+1
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (strings.TrimRight(origin, "/") == s.siteBaseURL() || strings.EqualFold(u.Host, r.Host))
}

func firstURL(text string) string {
	for _, f := range strings.Fields(text) {
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			return f
		}
	}
	return ""
}

func splitTags(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isMaxBytesError reports whether err is (or wraps) an http.MaxBytesReader
// overflow; multipart parsing sometimes surfaces it only as text.
func isMaxBytesError(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe) || (err != nil && strings.Contains(err.Error(), "request body too large"))
}
