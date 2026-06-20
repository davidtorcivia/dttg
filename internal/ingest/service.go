// Package ingest turns raw inputs (a URL, an uploaded file, or a note) into an
// archived item: it detects the kind, fetches/scrapes/oEmbeds as needed, refines
// images into responsive variants, stores blobs (local archive + R2 mirror), and
// inserts the item with its category and tags.
package ingest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"donottouchtheglass/internal/media"
	"donottouchtheglass/internal/store"
)

type Service struct {
	store *store.Store
	media media.Store
	http  *http.Client
}

func New(st *store.Store, ms media.Store) *Service {
	return &Service{
		store: st,
		media: ms,
		http:  &http.Client{Timeout: 20 * time.Second},
	}
}

// Input describes something to archive. Exactly one of FileBytes / URL / Note is
// the primary source; the rest are metadata.
type Input struct {
	Kind       string // optional hint: image|link|text|embed|document ("" = auto)
	URL        string
	FileBytes  []byte
	FileName   string
	Title      string
	Note       string
	Category   string
	Tags       []string
	Visibility string // "" => public
	CreatedAt  time.Time
}

func (in Input) visibility() string {
	if in.Visibility == "private" {
		return "private"
	}
	return "public"
}

// Create archives the input and returns the new item id.
func (s *Service) Create(ctx context.Context, in Input) (int64, error) {
	it := store.Item{
		Title:      strings.TrimSpace(in.Title),
		Note:       strings.TrimSpace(in.Note),
		SourceURL:  strings.TrimSpace(in.URL),
		Visibility: in.visibility(),
		CreatedAt:  in.CreatedAt,
	}

	var imageBytes []byte
	var imageCT string
	var docBytes []byte
	var docCT, docName string
	kind := strings.TrimSpace(in.Kind)

	switch {
	case len(in.FileBytes) > 0:
		ct := sniffContentType(in.FileName, in.FileBytes)
		if strings.HasPrefix(ct, "image/") {
			kind, imageBytes, imageCT = "image", in.FileBytes, ct
		} else {
			kind, docBytes, docCT, docName = "document", in.FileBytes, ct, in.FileName
		}

	case in.URL != "":
		// 1) embed providers (YouTube/Vimeo via oEmbed)
		if kind == "" || kind == "embed" {
			if prov := embedProviderFor(in.URL); prov != "" {
				if info, err := s.fetchEmbed(ctx, in.URL, prov); err == nil {
					kind = "embed"
					it.EmbedProvider = info.Provider
					it.EmbedHTML = info.HTML
					setIfEmpty(&it.Title, info.Title)
					if info.ThumbnailURL != "" {
						if r, e := s.fetch(ctx, info.ThumbnailURL); e == nil && strings.HasPrefix(r.ContentType, "image/") {
							imageBytes, imageCT = r.Body, r.ContentType
						}
					}
				}
			}
		}
		// 2) otherwise fetch and decide image / document / link
		if kind != "embed" {
			r, err := s.fetch(ctx, in.URL)
			switch {
			case err != nil:
				if kind == "" {
					kind = "link"
				}
				if kind == "image" {
					it.CoverRemoteURL = in.URL
				}
			case strings.HasPrefix(r.ContentType, "image/") || kind == "image":
				kind = "image"
				imageBytes, imageCT = r.Body, r.ContentType
			case isDocumentCT(r.ContentType) || kind == "document":
				kind = "document"
				docBytes, docCT = r.Body, r.ContentType
				docName = fileNameFromURL(r.FinalURL)
			default:
				kind = "link"
				meta := parseLinkMeta(r.Body, r.FinalURL)
				it.LinkTitle = meta.Title
				it.LinkDescription = meta.Description
				it.LinkSiteName = meta.SiteName
				setIfEmpty(&it.Title, meta.Title)
				if meta.ImageURL != "" {
					if ri, e := s.fetch(ctx, meta.ImageURL); e == nil && strings.HasPrefix(ri.ContentType, "image/") {
						imageBytes, imageCT = ri.Body, ri.ContentType
					}
				}
			}
		}

	case strings.TrimSpace(in.Note) != "":
		kind = "text"

	default:
		return 0, fmt.Errorf("ingest: empty input (need file, url, or note)")
	}

	if kind == "" {
		kind = "link"
	}
	it.Kind = kind
	if it.LinkSiteName == "" && it.SourceURL != "" {
		it.LinkSiteName = hostOf(it.SourceURL)
	}

	var mediaRows []store.Media

	// Refine + store the cover image, if any.
	if len(imageBytes) > 0 {
		if proc, err := ProcessImage(imageBytes); err == nil {
			asset := randAsset()
			origKey := fmt.Sprintf("items/%s/original%s", asset, extForContentType(imageCT))
			fullKey := fmt.Sprintf("items/%s/full.jpg", asset)
			thumbKey := fmt.Sprintf("items/%s/thumb.jpg", asset)

			if err := s.putAll(ctx, []blob{
				{origKey, orDefault(imageCT, "application/octet-stream"), imageBytes},
				{fullKey, "image/jpeg", proc.FullJPEG},
				{thumbKey, "image/jpeg", proc.ThumbJPEG},
			}); err != nil {
				return 0, err
			}

			it.CoverKey = fullKey
			it.ThumbKey = thumbKey
			it.Placeholder = proc.Placeholder
			it.DominantColor = proc.DominantColor
			it.Width, it.Height = proc.Width, proc.Height

			mediaRows = append(mediaRows,
				store.Media{Variant: "original", StorageKey: origKey, ContentType: imageCT, Bytes: int64(len(imageBytes)), OnLocal: true, OnR2: s.media.Mirrors(origKey)},
				store.Media{Variant: "full", StorageKey: fullKey, ContentType: "image/jpeg", Width: proc.Width, Height: proc.Height, Bytes: int64(len(proc.FullJPEG)), OnLocal: true, OnR2: s.media.Mirrors(fullKey)},
				store.Media{Variant: "thumb", StorageKey: thumbKey, ContentType: "image/jpeg", Bytes: int64(len(proc.ThumbJPEG)), OnLocal: true, OnR2: s.media.Mirrors(thumbKey)},
			)
		} else if kind == "image" && in.URL != "" {
			it.CoverRemoteURL = in.URL // processing failed; degrade to remote display
		}
	}

	// Store the document/file, if any.
	if len(docBytes) > 0 {
		asset := randAsset()
		ext := extForDoc(docCT, docName)
		fileKey := fmt.Sprintf("items/%s/file%s", asset, ext)
		ct := orDefault(docCT, "application/octet-stream")
		if err := s.media.Put(ctx, fileKey, ct, bytes.NewReader(docBytes)); err != nil {
			return 0, err
		}
		if docName == "" {
			docName = "file" + ext
		}
		it.FileKey = fileKey
		it.FileName = docName
		it.FileMime = ct
		it.FileSize = int64(len(docBytes))
		setIfEmpty(&it.Title, docName)
		mediaRows = append(mediaRows,
			store.Media{Variant: "file", StorageKey: fileKey, ContentType: ct, Bytes: int64(len(docBytes)), OnLocal: true, OnR2: s.media.Mirrors(fileKey)},
		)
	}

	if c := strings.TrimSpace(in.Category); c != "" {
		catID, err := s.store.GetOrCreateCategory(ctx, c)
		if err != nil {
			return 0, err
		}
		it.CategoryID = catID
	}

	id, err := s.store.CreateItem(ctx, it)
	if err != nil {
		return 0, err
	}
	for _, mr := range mediaRows {
		mr.ItemID = id
		if _, err := s.store.AddMedia(ctx, mr); err != nil {
			return 0, err
		}
	}
	for _, t := range in.Tags {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		tagID, err := s.store.GetOrCreateTag(ctx, t)
		if err != nil {
			continue
		}
		_ = s.store.AttachTag(ctx, id, tagID)
	}
	return id, nil
}

type blob struct {
	key, contentType string
	data             []byte
}

func (s *Service) putAll(ctx context.Context, blobs []blob) error {
	for _, b := range blobs {
		if err := s.media.Put(ctx, b.key, b.contentType, bytes.NewReader(b.data)); err != nil {
			return fmt.Errorf("store %s: %w", b.key, err)
		}
	}
	return nil
}

func randAsset() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func sniffContentType(name string, data []byte) string {
	ct := http.DetectContentType(data)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	// http.DetectContentType returns application/zip for docx/xlsx/pptx and
	// octet-stream for many files — prefer a known extension mapping.
	if ct == "application/octet-stream" || ct == "application/zip" {
		if byExt := mimeByExt(name); byExt != "" {
			return byExt
		}
	}
	return ct
}

func mimeByExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".pdf":
		return "application/pdf"
	case ".doc":
		return "application/msword"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xls":
		return "application/vnd.ms-excel"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".ppt":
		return "application/vnd.ms-powerpoint"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".txt":
		return "text/plain"
	case ".md":
		return "text/markdown"
	case ".csv":
		return "text/csv"
	case ".rtf":
		return "application/rtf"
	case ".epub":
		return "application/epub+zip"
	}
	return ""
}

func isDocumentCT(ct string) bool {
	if strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "text/html") {
		return false
	}
	switch ct {
	case "application/pdf",
		"application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.ms-excel",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.ms-powerpoint",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/rtf", "application/epub+zip",
		"text/plain", "text/markdown", "text/csv":
		return true
	}
	return false
}

func extForDoc(ct, name string) string {
	if e := strings.ToLower(filepath.Ext(name)); e != "" && len(e) <= 6 {
		return e
	}
	switch ct {
	case "application/pdf":
		return ".pdf"
	case "application/msword":
		return ".doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "text/csv":
		return ".csv"
	}
	return ".bin"
}

func fileNameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := u.Path
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p
}
