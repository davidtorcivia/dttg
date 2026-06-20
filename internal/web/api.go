package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"donottouchtheglass/internal/ingest"
)

const maxUpload = 30 << 20 // 30 MB

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
	in, err := s.parseItemInput(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.ingest.Create(r.Context(), in)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":  id,
		"url": s.cfg.BaseURL + "/item/" + strconv.FormatInt(id, 10),
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
			Kind: body.Kind, URL: strings.TrimSpace(body.URL), Title: body.Title,
			Note: body.Note, Category: body.Category, Visibility: body.Visibility, Tags: body.Tags,
		}, nil
	}

	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxUpload); err != nil {
			return ingest.Input{}, fmt.Errorf("parse form: %w", err)
		}
	} else {
		_ = r.ParseForm()
	}

	in := ingest.Input{
		Kind:       r.FormValue("kind"),
		URL:        strings.TrimSpace(r.FormValue("url")),
		Title:      r.FormValue("title"),
		Note:       r.FormValue("note"),
		Category:   r.FormValue("category"),
		Visibility: r.FormValue("visibility"),
		Tags:       splitTags(r.FormValue("tags")),
	}
	if f, fh, err := r.FormFile("file"); err == nil {
		defer f.Close()
		data, rerr := io.ReadAll(io.LimitReader(f, maxUpload))
		if rerr != nil {
			return in, fmt.Errorf("read upload: %w", rerr)
		}
		in.FileBytes = data
		in.FileName = fh.Filename
	} else if !errors.Is(err, http.ErrMissingFile) && !errors.Is(err, http.ErrNotMultipart) {
		return in, fmt.Errorf("file: %w", err)
	}
	return in, nil
}

// handleShare receives content shared to the installed PWA (Android share sheet).
// A shared file is ingested directly; a shared URL/text prefills the new-item form.
// Admin-gated, so only the owner's installed app (carrying the session) can use it.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseMultipartForm(maxUpload)

	if f, fh, err := r.FormFile("file"); err == nil {
		defer f.Close()
		if data, rerr := io.ReadAll(io.LimitReader(f, maxUpload)); rerr == nil && len(data) > 0 {
			in := ingest.Input{FileBytes: data, FileName: fh.Filename, Title: r.FormValue("title")}
			if id, cerr := s.ingest.Create(r.Context(), in); cerr == nil {
				http.Redirect(w, r, "/admin/items/"+strconv.FormatInt(id, 10)+"/edit", http.StatusSeeOther)
				return
			}
		}
	}

	shared := strings.TrimSpace(r.FormValue("url"))
	text := strings.TrimSpace(r.FormValue("text"))
	if shared == "" {
		shared = firstURL(text)
	}
	q := url.Values{}
	if shared != "" {
		q.Set("url", shared)
	}
	if t := strings.TrimSpace(r.FormValue("title")); t != "" {
		q.Set("title", t)
	}
	if text != "" && text != shared {
		q.Set("note", text)
	}
	http.Redirect(w, r, "/admin/new?"+q.Encode(), http.StatusSeeOther)
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
