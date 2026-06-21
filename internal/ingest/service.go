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
	Source     string // canonical source link (e.g. the page/tweet an image is from); falls back to URL
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
	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = strings.TrimSpace(in.URL)
	}
	it := store.Item{
		Title:      strings.TrimSpace(in.Title),
		Note:       strings.TrimSpace(in.Note),
		SourceURL:  source,
		Visibility: in.visibility(),
		CreatedAt:  in.CreatedAt,
	}

	var imageBytes []byte
	var imageCT string
	var docBytes []byte
	var docCT, docName string
	var videoBytes []byte
	var videoCT, videoName string
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
		// 0) tweets: pull the specific photo (and text) via FixTweet, not the OG
		//    composite of every image in the tweet.
		tweetHandled := false
		if id, photoIdx, ok := isTweetURL(in.URL); ok {
			if t, err := s.fetchTweet(ctx, id); err == nil && t != nil {
				it.LinkDescription = t.Text
				it.LinkSiteName = "x.com"
				setIfEmpty(&it.Title, t.Author)
				it.SourceURL = cleanTweetURL(in.URL)
				// A specific /photo/N (or the first photo) wins; otherwise pull a video.
				if photoURL := t.photo(photoIdx); photoURL != "" {
					if r, e := s.fetch(ctx, photoURL); e == nil && strings.HasPrefix(r.ContentType, "image/") {
						kind, imageBytes, imageCT = "image", r.Body, r.ContentType
					}
				} else if t.VideoURL != "" {
					// Self-host the video when it's small enough to embed inline.
					if r, e := s.fetch(ctx, t.VideoURL); e == nil && strings.HasPrefix(r.ContentType, "video/") && len(r.Body) > 0 && len(r.Body) < videoEmbedMax {
						kind, videoBytes, videoCT = "embed", r.Body, r.ContentType
						videoName = fileNameFromURL(r.FinalURL)
					}
					// Grab the poster either way, so the board card and (for oversized
					// videos) the detail page show a frame instead of going blank.
					if t.VideoThumb != "" {
						if ri, e := s.fetch(ctx, t.VideoThumb); e == nil && strings.HasPrefix(ri.ContentType, "image/") {
							imageBytes, imageCT = ri.Body, ri.ContentType
						}
					}
				}
				if kind == "" {
					kind = "link" // text-only tweet (or media fetch failed): keep the text as the quote
				}
				tweetHandled = true
			}
		}
		// 1) embed providers (YouTube/Vimeo via oEmbed)
		if !tweetHandled && (kind == "" || kind == "embed") {
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
		if !tweetHandled && kind != "embed" {
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
			case strings.HasPrefix(r.ContentType, "video/"):
				if len(r.Body) > 0 && len(r.Body) < videoEmbedMax {
					kind = "embed"
					videoBytes, videoCT = r.Body, r.ContentType
					videoName = fileNameFromURL(r.FinalURL)
				} else {
					kind = "link" // too large to embed inline; keep it as a link
				}
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

	// Store a small self-hosted video to embed inline.
	if len(videoBytes) > 0 {
		asset := randAsset()
		ext := extForVideo(videoCT, videoName)
		fileKey := fmt.Sprintf("items/%s/video%s", asset, ext)
		ct := orDefault(videoCT, "application/octet-stream")
		if err := s.media.Put(ctx, fileKey, ct, bytes.NewReader(videoBytes)); err != nil {
			return 0, err
		}
		if videoName == "" {
			videoName = "video" + ext
		}
		it.FileKey = fileKey
		it.FileName = videoName
		it.FileMime = ct
		it.FileSize = int64(len(videoBytes))
		it.EmbedProvider = "Video"
		setIfEmpty(&it.Title, videoName)
		mediaRows = append(mediaRows,
			store.Media{Variant: "video", StorageKey: fileKey, ContentType: ct, Bytes: int64(len(videoBytes)), OnLocal: true, OnR2: s.media.Mirrors(fileKey)},
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

// ReplaceFile swaps the media on an existing item for a freshly uploaded file
// (image, document, or small video), keeping the item and its metadata. New
// blobs are stored first; only then are the old blobs + media rows removed, so a
// failure mid-way never leaves the item without media. The item's kind is updated
// to match the new file.
func (s *Service) ReplaceFile(ctx context.Context, itemID int64, fileBytes []byte, fileName string) error {
	if len(fileBytes) == 0 {
		return fmt.Errorf("ingest: empty file")
	}
	it, err := s.store.GetItem(ctx, itemID, true)
	if err != nil {
		return err
	}
	if it == nil {
		return fmt.Errorf("ingest: item %d not found", itemID)
	}

	ct := sniffContentType(fileName, fileBytes)
	var next store.Item // only the media-related fields are used
	var mediaRows []store.Media

	switch {
	case strings.HasPrefix(ct, "image/"):
		proc, perr := ProcessImage(fileBytes)
		if perr != nil {
			return fmt.Errorf("process image: %w", perr)
		}
		asset := randAsset()
		origKey := fmt.Sprintf("items/%s/original%s", asset, extForContentType(ct))
		fullKey := fmt.Sprintf("items/%s/full.jpg", asset)
		thumbKey := fmt.Sprintf("items/%s/thumb.jpg", asset)
		if err := s.putAll(ctx, []blob{
			{origKey, orDefault(ct, "application/octet-stream"), fileBytes},
			{fullKey, "image/jpeg", proc.FullJPEG},
			{thumbKey, "image/jpeg", proc.ThumbJPEG},
		}); err != nil {
			return err
		}
		next.Kind = "image"
		next.CoverKey = fullKey
		next.ThumbKey = thumbKey
		next.Placeholder = proc.Placeholder
		next.DominantColor = proc.DominantColor
		next.Width, next.Height = proc.Width, proc.Height
		mediaRows = append(mediaRows,
			store.Media{Variant: "original", StorageKey: origKey, ContentType: ct, Bytes: int64(len(fileBytes)), OnLocal: true, OnR2: s.media.Mirrors(origKey)},
			store.Media{Variant: "full", StorageKey: fullKey, ContentType: "image/jpeg", Width: proc.Width, Height: proc.Height, Bytes: int64(len(proc.FullJPEG)), OnLocal: true, OnR2: s.media.Mirrors(fullKey)},
			store.Media{Variant: "thumb", StorageKey: thumbKey, ContentType: "image/jpeg", Bytes: int64(len(proc.ThumbJPEG)), OnLocal: true, OnR2: s.media.Mirrors(thumbKey)},
		)

	case strings.HasPrefix(ct, "video/"):
		if len(fileBytes) >= videoEmbedMax {
			return fmt.Errorf("video too large to self-host (max %d MB)", videoEmbedMax>>20)
		}
		asset := randAsset()
		ext := extForVideo(ct, fileName)
		fileKey := fmt.Sprintf("items/%s/video%s", asset, ext)
		c := orDefault(ct, "application/octet-stream")
		if err := s.media.Put(ctx, fileKey, c, bytes.NewReader(fileBytes)); err != nil {
			return err
		}
		name := fileName
		if name == "" {
			name = "video" + ext
		}
		next.Kind = "embed"
		next.EmbedProvider = "Video"
		next.FileKey, next.FileName, next.FileMime, next.FileSize = fileKey, name, c, int64(len(fileBytes))
		mediaRows = append(mediaRows,
			store.Media{Variant: "video", StorageKey: fileKey, ContentType: c, Bytes: int64(len(fileBytes)), OnLocal: true, OnR2: s.media.Mirrors(fileKey)})

	default:
		asset := randAsset()
		ext := extForDoc(ct, fileName)
		fileKey := fmt.Sprintf("items/%s/file%s", asset, ext)
		c := orDefault(ct, "application/octet-stream")
		if err := s.media.Put(ctx, fileKey, c, bytes.NewReader(fileBytes)); err != nil {
			return err
		}
		name := fileName
		if name == "" {
			name = "file" + ext
		}
		next.Kind = "document"
		next.FileKey, next.FileName, next.FileMime, next.FileSize = fileKey, name, c, int64(len(fileBytes))
		mediaRows = append(mediaRows,
			store.Media{Variant: "file", StorageKey: fileKey, ContentType: c, Bytes: int64(len(fileBytes)), OnLocal: true, OnR2: s.media.Mirrors(fileKey)})
	}

	// New blobs are stored; now drop the old ones and rewrite the item's media.
	if old, lerr := s.store.ListMediaForItem(ctx, itemID); lerr == nil {
		for _, m := range old {
			_ = s.media.Delete(ctx, m.StorageKey)
		}
	}
	if err := s.store.ClearMediaForItem(ctx, itemID); err != nil {
		return err
	}
	if err := s.store.UpdateItemMedia(ctx, itemID, next); err != nil {
		return err
	}
	for _, mr := range mediaRows {
		mr.ItemID = itemID
		if _, err := s.store.AddMedia(ctx, mr); err != nil {
			return err
		}
	}
	return nil
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

const videoEmbedMax = 20 << 20 // self-host + embed videos up to ~20 MB; larger ones stay links

func extForVideo(ct, name string) string {
	if e := strings.ToLower(filepath.Ext(name)); e != "" && len(e) <= 6 {
		return e
	}
	switch ct {
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/ogg":
		return ".ogv"
	case "video/quicktime":
		return ".mov"
	}
	return ".mp4"
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
