// Package ingest turns raw inputs (a URL, an uploaded file, or a note) into an
// archived item: it detects the kind, fetches/scrapes/oEmbeds as needed, refines
// images into responsive variants, stores blobs (local archive + R2 mirror), and
// inserts the item with its category and tags.
package ingest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
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
	return &Service{store: st, media: ms, http: newSafeHTTPClient(20 * time.Second)}
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

// payload is the media found for an item: an image to refine and/or a file
// (a document, or a video to self-host) stored as-is.
type payload struct {
	image    []byte
	imageCT  string
	file     []byte
	fileCT   string
	fileName string
}

const videoEmbedMax = 25 << 20 // self-host + embed videos up to 25 MB; larger ones stay links

// classifyUpload decides the kind of an uploaded file from its sniffed type.
func classifyUpload(data []byte, name string) (string, payload, error) {
	ct := sniffContentType(name, data)
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image", payload{image: data, imageCT: ct}, nil
	case strings.HasPrefix(ct, "video/"):
		if len(data) > videoEmbedMax {
			return "", payload{}, fmt.Errorf("video too large to self-host (max %d MB)", videoEmbedMax>>20)
		}
		return "embed", payload{file: data, fileCT: ct, fileName: name}, nil
	}
	return "document", payload{file: data, fileCT: ct, fileName: name}, nil
}

// Create archives the input and returns the new item id.
func (s *Service) Create(ctx context.Context, in Input) (int64, error) {
	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = strings.TrimSpace(in.URL)
	}
	it := store.Item{
		Kind:       strings.TrimSpace(in.Kind),
		Title:      strings.TrimSpace(in.Title),
		Note:       strings.TrimSpace(in.Note),
		SourceURL:  source,
		Visibility: in.Visibility,
		CreatedAt:  in.CreatedAt,
	}
	if it.Visibility != "private" {
		it.Visibility = "public"
	}

	var p payload
	switch {
	case len(in.FileBytes) > 0:
		var err error
		if it.Kind, p, err = classifyUpload(in.FileBytes, in.FileName); err != nil {
			return 0, err
		}
	case in.URL != "":
		p = s.fromURL(ctx, in.URL, &it)
	case it.Note != "":
		it.Kind = "text"
	default:
		return 0, errors.New("ingest: empty input (need file, url, or note)")
	}
	if it.Kind == "" {
		it.Kind = "link"
	}
	if it.LinkSiteName == "" && it.SourceURL != "" {
		it.LinkSiteName = hostOf(it.SourceURL)
	}

	var rows []store.Media
	if len(p.image) > 0 {
		proc, err := ProcessImage(p.image)
		switch {
		case err == nil:
			if rows, err = s.storeImage(ctx, &it, p.image, p.imageCT, proc); err != nil {
				return 0, err
			}
		case it.Kind != "image": // a link/embed preview that failed to decode: no cover
		case in.URL != "":
			it.CoverRemoteURL = in.URL // keep the remote image rather than failing
		default:
			return 0, fmt.Errorf("ingest: image processing failed: %w", err)
		}
	}
	if len(p.file) > 0 {
		row, err := s.storeFile(ctx, &it, p.file, p.fileCT, p.fileName)
		if err != nil {
			s.cleanup(ctx, rows)
			return 0, err
		}
		rows = append(rows, row)
	}

	if c := strings.TrimSpace(in.Category); c != "" {
		catID, err := s.store.GetOrCreateCategory(ctx, c)
		if err != nil {
			s.cleanup(ctx, rows)
			return 0, err
		}
		it.CategoryID = catID
	}
	id, err := s.store.CreateItemWithMediaAndTags(ctx, it, rows, in.Tags)
	if err != nil {
		s.cleanup(ctx, rows)
	}
	return id, err
}

// fromURL fetches rawURL and fills it's kind/metadata, returning any media to store.
func (s *Service) fromURL(ctx context.Context, rawURL string, it *store.Item) payload {
	var p payload
	// fetchImage pulls a preview/cover image, ignoring failures and non-images.
	fetchImage := func(u string) {
		if r, err := s.fetch(ctx, u); err == nil && strings.HasPrefix(r.ContentType, "image/") {
			p.image, p.imageCT = r.Body, r.ContentType
		}
	}

	// Tweets: pull the specific photo (and text) via FixTweet, not the OG
	// composite of every image in the tweet.
	if id, photoIdx, ok := isTweetURL(rawURL); ok {
		if t, err := s.fetchTweet(ctx, id); err == nil {
			it.LinkDescription = t.Text
			it.LinkSiteName = "x.com"
			setIfEmpty(&it.Title, t.Author)
			it.SourceURL = cleanTweetURL(rawURL)
			// A specific /photo/N (or the first photo) wins; otherwise pull a video.
			if photoURL := t.photo(photoIdx); photoURL != "" {
				fetchImage(photoURL)
				if p.image != nil {
					it.Kind = "image"
				}
			} else if t.VideoURL != "" {
				if r, err := s.fetch(ctx, t.VideoURL); err == nil && strings.HasPrefix(r.ContentType, "video/") && len(r.Body) > 0 && len(r.Body) <= videoEmbedMax {
					it.Kind = "embed"
					p.file, p.fileCT, p.fileName = r.Body, r.ContentType, fileNameFromURL(r.FinalURL)
				}
				// Grab the poster either way, so the board card and (for oversized
				// videos) the detail page show a frame instead of going blank.
				if t.VideoThumb != "" {
					fetchImage(t.VideoThumb)
				}
			}
			if it.Kind == "" {
				it.Kind = "link" // text-only tweet (or media fetch failed): keep the text as the quote
			}
			return p
		}
	}

	// Embed providers (YouTube/Vimeo via oEmbed).
	if it.Kind == "" || it.Kind == "embed" {
		if prov := embedProviderFor(rawURL); prov != "" {
			if info, err := s.fetchEmbed(ctx, rawURL, prov); err == nil {
				it.Kind, it.EmbedProvider, it.EmbedHTML = "embed", info.Provider, info.HTML
				setIfEmpty(&it.Title, info.Title)
				if info.ThumbnailURL != "" {
					fetchImage(info.ThumbnailURL)
				}
				return p
			}
		}
	}

	// Otherwise fetch and decide image / video / document / link.
	r, err := s.fetch(ctx, rawURL)
	switch {
	case err != nil:
		if it.Kind == "image" {
			it.CoverRemoteURL = rawURL
		}
	case strings.HasPrefix(r.ContentType, "image/") || it.Kind == "image":
		it.Kind, p.image, p.imageCT = "image", r.Body, r.ContentType
	case strings.HasPrefix(r.ContentType, "video/"):
		it.Kind = "link" // too large to embed inline; keep it as a link
		if len(r.Body) > 0 && len(r.Body) <= videoEmbedMax {
			it.Kind = "embed"
			p.file, p.fileCT, p.fileName = r.Body, r.ContentType, fileNameFromURL(r.FinalURL)
		}
	case isDocumentCT(r.ContentType) || it.Kind == "document":
		it.Kind = "document"
		p.file, p.fileCT, p.fileName = r.Body, r.ContentType, fileNameFromURL(r.FinalURL)
	default:
		it.Kind = "link"
		meta := parseLinkMeta(r.Body, r.FinalURL)
		it.LinkTitle, it.LinkDescription, it.LinkSiteName = meta.Title, meta.Description, meta.SiteName
		setIfEmpty(&it.Title, meta.Title)
		if meta.ImageURL != "" {
			fetchImage(meta.ImageURL)
		}
	}
	return p
}

// ReplaceFile swaps the media on an existing item for a freshly uploaded file
// (image, document, or small video), keeping the item and its metadata. New
// blobs are stored first; only then are the old blobs + media rows removed, so a
// failure mid-way never leaves the item without media. The item's kind is updated
// to match the new file.
func (s *Service) ReplaceFile(ctx context.Context, itemID int64, data []byte, name string) error {
	if len(data) == 0 {
		return errors.New("ingest: empty file")
	}
	it, err := s.store.GetItem(ctx, itemID, true)
	if err != nil {
		return err
	}
	if it == nil {
		return fmt.Errorf("ingest: item %d not found", itemID)
	}
	kind, p, err := classifyUpload(data, name)
	if err != nil {
		return err
	}
	next := store.Item{Kind: kind, Visibility: it.Visibility} // only the media fields are persisted
	var rows []store.Media
	if p.image != nil {
		proc, err := ProcessImage(p.image)
		if err != nil {
			return fmt.Errorf("process image: %w", err)
		}
		rows, err = s.storeImage(ctx, &next, p.image, p.imageCT, proc)
		if err != nil {
			return err
		}
	} else {
		row, err := s.storeFile(ctx, &next, p.file, p.fileCT, p.fileName)
		if err != nil {
			return err
		}
		rows = []store.Media{row}
	}

	old, err := s.store.ListMedia(ctx, itemID)
	if err == nil {
		err = s.store.ReplaceItemMedia(ctx, itemID, next, rows)
	}
	if err != nil {
		s.cleanup(ctx, rows)
		return err
	}
	s.cleanup(ctx, old)
	return nil
}

// SyncPlacement moves blobs whose storage tier no longer matches their item's
// visibility (itemID 0 = every item): private media comes off the public R2
// bucket, public media goes up to it. It is idempotent and returns how many
// blobs moved; failures are collected so one bad blob doesn't stop the rest.
func (s *Service) SyncPlacement(ctx context.Context, itemID int64) (int, error) {
	list := func(ctx context.Context) ([]store.Media, error) { return s.store.ListMedia(ctx, itemID) }
	if itemID == 0 {
		list = s.store.AllMedia
	}
	rows, err := list(ctx)
	if err != nil {
		return 0, err
	}
	moved := 0
	var errs []error
	for _, m := range rows {
		onLocal, onR2 := s.media.Placement(m.StorageKey, m.Private)
		if onLocal == m.OnLocal && onR2 == m.OnR2 {
			continue
		}
		if err := s.move(ctx, m, onLocal, onR2); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.StorageKey, err))
			continue
		}
		moved++
	}
	return moved, errors.Join(errs...)
}

func (s *Service) move(ctx context.Context, m store.Media, onLocal, onR2 bool) error {
	rc, err := s.media.Open(ctx, m.StorageKey)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return err
	}
	if err := s.media.Put(ctx, m.StorageKey, orDefault(m.ContentType, "application/octet-stream"), data, m.Private); err != nil {
		return err
	}
	return s.store.SetMediaPlacement(ctx, m.ID, onLocal, onR2)
}

// storeImage stores the original plus refined variants under a fresh asset
// prefix and fills it's image fields.
func (s *Service) storeImage(ctx context.Context, it *store.Item, raw []byte, ct string, proc *ProcessedImage) ([]store.Media, error) {
	prefix := "items/" + randAsset() + "/"
	ct = orDefault(ct, "application/octet-stream")
	rows := []store.Media{
		{Variant: "original", StorageKey: prefix + "original" + extFor(ct, "", ".bin"), ContentType: ct},
		{Variant: "full", StorageKey: prefix + "full.jpg", ContentType: "image/jpeg", Width: proc.Width, Height: proc.Height},
		{Variant: "thumb", StorageKey: prefix + "thumb.jpg", ContentType: "image/jpeg"},
		{Variant: "small", StorageKey: prefix + "small.jpg", ContentType: "image/jpeg"},
	}
	for i, data := range [][]byte{raw, proc.FullJPEG, proc.ThumbJPEG, proc.SmallJPEG} {
		if err := s.put(ctx, &rows[i], data, it.Visibility == "private"); err != nil {
			s.cleanup(ctx, rows[:i])
			return nil, err
		}
	}
	it.CoverKey, it.ThumbKey, it.SmallKey = rows[1].StorageKey, rows[2].StorageKey, rows[3].StorageKey
	it.Placeholder, it.DominantColor = proc.Placeholder, proc.DominantColor
	it.Width, it.Height = proc.Width, proc.Height
	return rows, nil
}

// storeFile stores a document, or a video (kind embed) to self-host inline, and
// fills it's file fields.
func (s *Service) storeFile(ctx context.Context, it *store.Item, data []byte, ct, name string) (store.Media, error) {
	ct = orDefault(ct, "application/octet-stream")
	variant, fallback := "file", ".bin"
	if strings.HasPrefix(ct, "video/") {
		variant, fallback = "video", ".mp4"
		it.EmbedProvider = "Video"
	}
	ext := extFor(ct, name, fallback)
	row := store.Media{Variant: variant, StorageKey: "items/" + randAsset() + "/" + variant + ext, ContentType: ct}
	if err := s.put(ctx, &row, data, it.Visibility == "private"); err != nil {
		return row, err
	}
	if name == "" {
		name = variant + ext
	}
	it.FileKey, it.FileName, it.FileMime, it.FileSize = row.StorageKey, name, ct, row.Bytes
	setIfEmpty(&it.Title, name)
	return row, nil
}

// put writes one blob and records its size and storage placement on m.
func (s *Service) put(ctx context.Context, m *store.Media, data []byte, private bool) error {
	if err := s.media.Put(ctx, m.StorageKey, m.ContentType, data, private); err != nil {
		return fmt.Errorf("store %s: %w", m.StorageKey, err)
	}
	m.Bytes = int64(len(data))
	m.OnLocal, m.OnR2 = s.media.Placement(m.StorageKey, private)
	return nil
}

// cleanup best-effort deletes the blobs behind rows (failed ingest, replaced media).
func (s *Service) cleanup(ctx context.Context, rows []store.Media) {
	for _, m := range rows {
		_ = s.media.Delete(ctx, m.StorageKey)
	}
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

// fileTypes maps extensions to MIME types for the files the archive knows how
// to store; the first extension listed for a type is its canonical one.
var fileTypes = [][2]string{
	{".jpg", "image/jpeg"}, {".jpeg", "image/jpeg"}, {".png", "image/png"}, {".webp", "image/webp"},
	{".gif", "image/gif"}, {".avif", "image/avif"},
	{".mp4", "video/mp4"}, {".webm", "video/webm"}, {".ogv", "video/ogg"}, {".mov", "video/quicktime"},
	{".pdf", "application/pdf"}, {".doc", "application/msword"},
	{".docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	{".xls", "application/vnd.ms-excel"},
	{".xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	{".ppt", "application/vnd.ms-powerpoint"},
	{".pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation"},
	{".txt", "text/plain"}, {".md", "text/markdown"}, {".csv", "text/csv"},
	{".rtf", "application/rtf"}, {".epub", "application/epub+zip"},
}

func lookupType(col int, v string) string {
	for _, t := range fileTypes {
		if t[col] == v {
			return t[1-col]
		}
	}
	return ""
}

// extFor keeps a sane extension from name, else derives one from ct.
func extFor(ct, name, fallback string) string {
	if e := strings.ToLower(filepath.Ext(name)); e != "" && len(e) <= 6 {
		return e
	}
	return orDefault(lookupType(1, ct), fallback)
}

func isDocumentCT(ct string) bool {
	return lookupType(1, ct) != "" && !strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "video/")
}

func sniffContentType(name string, data []byte) string {
	ct, _, _ := strings.Cut(http.DetectContentType(data), ";")
	ct = strings.ToLower(strings.TrimSpace(ct))
	// DetectContentType returns application/zip for docx/xlsx/pptx and
	// octet-stream for many files — prefer a known extension mapping.
	if ct == "application/octet-stream" || ct == "application/zip" {
		if byExt := lookupType(0, strings.ToLower(filepath.Ext(name))); byExt != "" {
			return byExt
		}
	}
	return ct
}

func fileNameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	_, name := path.Split(u.Path)
	return name
}
