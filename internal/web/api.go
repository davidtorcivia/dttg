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

type apiTokenHashKey struct{}

func withAPITokenHash(ctx context.Context, hash string) context.Context {
	return context.WithValue(ctx, apiTokenHashKey{}, hash)
}

func apiTokenHash(ctx context.Context) string {
	if v, ok := ctx.Value(apiTokenHashKey{}).(string); ok {
		return v
	}
	return ""
}

const maxUpload = 30 << 20 // 30 MB

// errUploadTooLarge is returned when a multipart body or file part exceeds maxUpload.
var errUploadTooLarge = errors.New("upload too large")

// tokenAuth wraps an API handler, requiring a valid bearer token.
func (s *Server) tokenAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing api token")
			return
		}
		ok, err := s.store.TokenValid(r.Context(), HashToken(tok))
		if err != nil || !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid api token")
			return
		}
		// Stash the hashed token so create-path rate limits can key on it without
		// re-parsing the Authorization header.
		r = r.WithContext(withAPITokenHash(r.Context(), HashToken(tok)))
		next(w, r)
	}
}

// cors lets the browser extension (an unpredictable moz-extension:// origin)
// call the token-authed API cross-origin. Safe to allow any origin here because
// every request is gated on the bearer token — there is no cookie/credential to
// hijack. Preflight OPTIONS is answered before auth (it carries no token).
func (s *Server) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Token")
		w.Header().Set("Access-Control-Max-Age", "86400")
		w.Header().Add("Vary", "Origin")
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

// handleAPICreateItem archives an item from JSON or multipart and returns its id+url.
func (s *Server) handleAPICreateItem(w http.ResponseWriter, r *http.Request) {
	// Per-token and per-IP create budgets (token auth already ran).
	if s.apiCreateTokenRL != nil {
		if th := apiTokenHash(r.Context()); th != "" {
			if ok, retry := s.apiCreateTokenRL.allow(th); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
				writeJSONError(w, http.StatusTooManyRequests, "rate limited")
				return
			}
		}
	}
	if s.apiCreateIPRL != nil {
		if ok, retry := s.apiCreateIPRL.allow(s.clientIP(r)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			writeJSONError(w, http.StatusTooManyRequests, "rate limited")
			return
		}
	}

	// Cap request body before any multipart parse / JSON decode.
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1)

	in, err := s.parseItemInput(r)
	if err != nil {
		if errors.Is(err, errUploadTooLarge) || isMaxBytesError(err) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "upload too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.ingest.Create(r.Context(), in)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.invalidateSiteCache()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":  id,
		"url": s.siteBaseURL() + "/item/" + strconv.FormatInt(id, 10),
	})
}

// handleAPITaxonomy returns the archive's categories and tags so the extension
// can autocomplete them. Token-authed (owner-only); never cached by the browser.
func (s *Server) handleAPITaxonomy(w http.ResponseWriter, r *http.Request) {
	cats, _ := s.store.ListCategories(r.Context(), true)
	tags, _ := s.store.ListTags(r.Context())
	catNames := make([]string, 0, len(cats))
	for _, c := range cats {
		catNames = append(catNames, c.Name)
	}
	tagNames := make([]string, 0, len(tags))
	for _, t := range tags {
		tagNames = append(tagNames, t.Name)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"categories": catNames,
		"tags":       tagNames,
	})
}

// parseItemInput reads an ingest.Input from a JSON body, multipart form, or
// urlencoded form (the extension/bookmarklet/admin all funnel through here).
func (s *Server) parseItemInput(r *http.Request) (ingest.Input, error) {
	ct := r.Header.Get("Content-Type")

	if strings.HasPrefix(ct, "application/json") {
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
			if isMaxBytesError(err) {
				return ingest.Input{}, errUploadTooLarge
			}
			return ingest.Input{}, fmt.Errorf("invalid json: %w", err)
		}
		return ingest.Input{
			Kind: body.Kind, URL: strings.TrimSpace(body.URL), Source: strings.TrimSpace(body.Source),
			Title: body.Title, Note: body.Note, Category: body.Category,
			Visibility: body.Visibility, Tags: body.Tags,
		}, nil
	}

	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxUpload); err != nil {
			if isMaxBytesError(err) {
				return ingest.Input{}, errUploadTooLarge
			}
			return ingest.Input{}, fmt.Errorf("parse form: %w", err)
		}
	} else {
		if err := r.ParseForm(); err != nil && isMaxBytesError(err) {
			return ingest.Input{}, errUploadTooLarge
		}
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
	if f, fh, err := r.FormFile("file"); err == nil {
		defer f.Close()
		data, rerr := io.ReadAll(io.LimitReader(f, maxUpload+1))
		if rerr != nil {
			return in, fmt.Errorf("read upload: %w", rerr)
		}
		if len(data) > maxUpload {
			return in, errUploadTooLarge
		}
		in.FileBytes = data
		in.FileName = fh.Filename
	} else if !errors.Is(err, http.ErrMissingFile) && !errors.Is(err, http.ErrNotMultipart) {
		return in, fmt.Errorf("file: %w", err)
	}
	return in, nil
}

// handleShare receives content shared to the installed PWA (Android share sheet).
// Authenticated sessions ingest immediately. Unauthenticated shares are stashed
// in pending_shares (and optional local pending file) then recovered after login.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	if !s.validSameOriginPost(r) {
		http.Error(w, "cross-origin share rejected", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		if isMaxBytesError(err) {
			http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
			return
		}
		_ = r.ParseForm()
	}

	title := strings.TrimSpace(r.FormValue("title"))
	shared := strings.TrimSpace(r.FormValue("url"))
	text := strings.TrimSpace(r.FormValue("text"))
	if shared == "" {
		shared = firstURL(text)
	}

	var fileBytes []byte
	var fileName string
	if f, fh, err := r.FormFile("file"); err == nil {
		defer f.Close()
		data, rerr := io.ReadAll(io.LimitReader(f, maxUpload+1))
		if rerr != nil {
			http.Error(w, "could not read upload", http.StatusBadRequest)
			return
		}
		if len(data) > maxUpload {
			http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
			return
		}
		fileBytes, fileName = data, fh.Filename
	}

	if s.isAdmin(r) {
		if len(fileBytes) > 0 {
			in := ingest.Input{FileBytes: fileBytes, FileName: fileName, Title: title}
			if id, cerr := s.ingest.Create(r.Context(), in); cerr == nil {
				s.invalidateSiteCache()
				http.Redirect(w, r, "/admin/items/"+strconv.FormatInt(id, 10)+"/edit", http.StatusSeeOther)
				return
			} else {
				http.Error(w, "could not archive shared file: "+cerr.Error(), http.StatusUnprocessableEntity)
				return
			}
		}
		q := url.Values{}
		if shared != "" {
			q.Set("url", shared)
		}
		if title != "" {
			q.Set("title", title)
		}
		if text != "" && text != shared {
			q.Set("note", text)
		}
		http.Redirect(w, r, "/admin/new?"+q.Encode(), http.StatusSeeOther)
		return
	}

	// Unauthenticated: stash and send through login.
	id, err := newPendingShareID()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p := store.PendingShare{
		ID:        id,
		ExpiresAt: time.Now().Add(30 * time.Minute),
		Title:     title,
		Text:      text,
		URL:       shared,
	}
	if len(fileBytes) > 0 {
		dir := filepath.Join(s.cfg.DataDir, "pending")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			s.serverError(w, r, err)
			return
		}
		key := id + filepath.Ext(fileName)
		path := filepath.Join(dir, key)
		if err := os.WriteFile(path, fileBytes, 0o600); err != nil {
			s.serverError(w, r, err)
			return
		}
		p.FileKey = key
		p.FileName = fileName
		p.FileSize = int64(len(fileBytes))
		if len(fileBytes) >= 512 {
			p.FileMime = http.DetectContentType(fileBytes[:512])
		} else {
			p.FileMime = http.DetectContentType(fileBytes)
		}
	}
	if err := s.store.CreatePendingShare(r.Context(), p); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape("/share/pending/"+id), http.StatusSeeOther)
}

func newPendingShareID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// handleSharePending recovers a stashed PWA share after login.
func (s *Server) handleSharePending(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := s.store.TakePendingShare(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if p == nil {
		http.Error(w, "share expired or not found", http.StatusNotFound)
		return
	}
	defer func() {
		if p.FileKey != "" {
			_ = os.Remove(filepath.Join(s.cfg.DataDir, "pending", p.FileKey))
		}
	}()

	if p.FileKey != "" {
		path := filepath.Join(s.cfg.DataDir, "pending", p.FileKey)
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			http.Error(w, "shared file missing", http.StatusGone)
			return
		}
		in := ingest.Input{FileBytes: data, FileName: p.FileName, Title: p.Title}
		nid, cerr := s.ingest.Create(r.Context(), in)
		if cerr != nil {
			http.Error(w, "could not archive shared file: "+cerr.Error(), http.StatusUnprocessableEntity)
			return
		}
		s.invalidateSiteCache()
		http.Redirect(w, r, "/admin/items/"+strconv.FormatInt(nid, 10)+"/edit", http.StatusSeeOther)
		return
	}

	q := url.Values{}
	if p.URL != "" {
		q.Set("url", p.URL)
	}
	if p.Title != "" {
		q.Set("title", p.Title)
	}
	if p.Text != "" && p.Text != p.URL {
		q.Set("note", p.Text)
	}
	http.Redirect(w, r, "/admin/new?"+q.Encode(), http.StatusSeeOther)
}

// validSameOriginPost accepts share/POST traffic from the same site (PWA share
// sheet, same-origin forms). Rejects explicit cross-site Sec-Fetch-Site.
func (s *Server) validSameOriginPost(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
		return false
	}
	// same-origin / none (user-initiated, no referrer context) are fine.
	if site := r.Header.Get("Sec-Fetch-Site"); site == "same-origin" || site == "none" || site == "" {
		// If Origin is present, it must match configured site or request host.
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		base := strings.TrimRight(s.siteBaseURL(), "/")
		if base != "" && strings.TrimRight(origin, "/") == base {
			return true
		}
		// Fall back to request host (dev / missing base_url).
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		host := r.Host
		if host == "" {
			host = r.Header.Get("Host")
		}
		return strings.EqualFold(u.Host, host)
	}
	// same-site is acceptable for multi-subdomain setups behind one eTLD+1.
	if r.Header.Get("Sec-Fetch-Site") == "same-site" {
		return true
	}
	return false
}

func firstURL(text string) string {
	for _, f := range strings.Fields(text) {
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			return f
		}
	}
	return ""
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func splitTags(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isMaxBytesError reports whether err is (or wraps) http.MaxBytesError / the
// classic "http: request body too large" message from MaxBytesReader.
func isMaxBytesError(err error) bool {
	if err == nil {
		return false
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return true
	}
	// Older/path-wrapped forms surface as plain text.
	msg := err.Error()
	return strings.Contains(msg, "http: request body too large") ||
		strings.Contains(msg, "request body too large")
}
