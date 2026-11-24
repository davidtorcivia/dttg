// Package ingest turns raw inputs (a URL, uploaded file(s), or a note) into an
// archived item: it detects the kind, fetches/scrapes/oEmbeds as needed, refines
// images into responsive variants (concurrently for multi-image series), stores
// blobs (local archive + R2 mirror), and inserts the item with its gallery
// images, category, and tags.
package ingest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"donottouchtheglass/internal/media"
	"donottouchtheglass/internal/store"
)

type Service struct {
	store        *store.Store
	media        media.Store
	http         *http.Client
	tweetAPIBase string // overridable in tests; defaults to https://api.fxtwitter.com
	bskyAPIBase  string // overridable in tests; defaults to https://public.api.bsky.app
}

func New(st *store.Store, ms media.Store) *Service {
	return &Service{store: st, media: ms, http: newSafeHTTPClient(20 * time.Second)}
}

// UploadFile is one uploaded file payload.
type UploadFile struct {
	Bytes []byte
	Name  string
}

// Input describes something to archive. Exactly one of Files/FileBytes, URL/URLs,
// or Note is the primary source; the rest are metadata.
type Input struct {
	Kind       string // optional hint: image|link|text|embed|document ("" = auto)
	URL        string
	URLs       []string // multiple image URLs for a single gallery post
	Source     string   // canonical source link (e.g. the page/tweet an image is from); falls back to URL
	FileBytes  []byte
	FileName   string
	Files      []UploadFile // multiple uploaded images for a single gallery post
	Title      string
	Note       string
	Category   string
	Tags       []string
	Visibility string // "" => public
	CreatedAt  time.Time
}

type rawImage struct {
	data []byte
	ct   string
	alt  string
}

// payload is the media found for an item: one or more images to refine and/or a
// file (a document, or a video to self-host) stored as-is.
type payload struct {
	images   []rawImage
	file     []byte
	fileCT   string
	fileName string
}

const (
	videoEmbedMax     = 25 << 20 // self-host + embed videos up to 25 MB; larger ones stay links
	maxGalleryImgs    = 20       // cap per-post gallery size
	ingestConcurrency = 4        // bounded worker pool for concurrent fetch/resize/R2 upload
)

// classifyUpload decides the kind of an uploaded file from its sniffed type.
func classifyUpload(data []byte, name string) (string, payload, error) {
	ct := sniffContentType(name, data)
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image", payload{images: []rawImage{{data: data, ct: ct}}}, nil
	case strings.HasPrefix(ct, "video/"):
		if len(data) > videoEmbedMax {
			return "", payload{}, fmt.Errorf("video too large to self-host (max %d MB)", videoEmbedMax>>20)
		}
		return "embed", payload{file: data, fileCT: ct, fileName: name}, nil
	}
	return "document", payload{file: data, fileCT: ct, fileName: name}, nil
}

// classifyUploads handles one or more uploaded files. Multiple files must all be images.
func classifyUploads(files []UploadFile) (string, payload, error) {
	if len(files) == 1 {
		return classifyUpload(files[0].Bytes, files[0].Name)
	}
	if len(files) > maxGalleryImgs {
		return "", payload{}, fmt.Errorf("too many images (max %d per post)", maxGalleryImgs)
	}
	var p payload
	for _, f := range files {
		if len(f.Bytes) == 0 {
			continue
		}
		ct := sniffContentType(f.Name, f.Bytes)
		if !strings.HasPrefix(ct, "image/") {
			return "", payload{}, fmt.Errorf("multiple file uploads support images only (%q is %s)", f.Name, ct)
		}
		p.images = append(p.images, rawImage{data: f.Bytes, ct: ct})
	}
	if len(p.images) == 0 {
		return "", payload{}, errors.New("ingest: empty upload")
	}
	return "image", p, nil
}

func hashUploads(files []UploadFile) string {
	if len(files) == 0 {
		return ""
	}
	h := sha256.New()
	wrote := false
	for _, f := range files {
		if len(f.Bytes) == 0 {
			continue
		}
		wrote = true
		_, _ = h.Write(f.Bytes)
		_, _ = h.Write([]byte{0})
	}
	if !wrote {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NormalizeSourceURL cleans canonical source URLs (e.g. tweets) for storage and
// duplicate detection.
func NormalizeSourceURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if _, _, ok := isTweetURL(raw); ok {
		return cleanTweetURL(raw)
	}
	return raw
}

// CheckDuplicate checks whether an identical uploaded payload or canonical source
// URL already exists in the archive.
func (s *Service) CheckDuplicate(ctx context.Context, in Input) (*store.Item, error) {
	files := in.Files
	if len(files) == 0 && len(in.FileBytes) > 0 {
		files = []UploadFile{{Bytes: in.FileBytes, Name: in.FileName}}
	}
	contentHash := hashUploads(files)
	src := strings.TrimSpace(in.Source)
	if src == "" {
		src = strings.TrimSpace(in.URL)
	}
	src = NormalizeSourceURL(src)
	if src == "" && contentHash == "" {
		return nil, nil
	}
	return s.store.FindDuplicate(ctx, src, contentHash)
}

// Create archives the input and returns the new item id.
func (s *Service) Create(ctx context.Context, in Input) (int64, error) {
	files := in.Files
	if len(files) == 0 && len(in.FileBytes) > 0 {
		files = []UploadFile{{Bytes: in.FileBytes, Name: in.FileName}}
	}
	var cleanURLs []string
	for _, u := range in.URLs {
		if u = strings.TrimSpace(u); u != "" {
			cleanURLs = append(cleanURLs, u)
		}
	}
	rawURL := strings.TrimSpace(in.URL)
	if rawURL == "" && len(cleanURLs) == 1 {
		rawURL = cleanURLs[0]
		cleanURLs = nil
	}

	source := strings.TrimSpace(in.Source)
	if source == "" {
		if rawURL != "" {
			source = rawURL
		} else if len(cleanURLs) > 0 {
			source = cleanURLs[0]
		}
	}
	if source != "" {
		if err := validateHTTPURL(source); err != nil {
			return 0, err
		}
	}
	it := store.Item{
		Kind:        strings.TrimSpace(in.Kind),
		Title:       strings.TrimSpace(in.Title),
		Note:        strings.TrimSpace(in.Note),
		SourceURL:   NormalizeSourceURL(source),
		Visibility:  in.Visibility,
		ContentHash: hashUploads(files),
		CreatedAt:   in.CreatedAt,
	}
	if it.Visibility != "private" {
		it.Visibility = "public"
	}

	var p payload
	switch {
	case len(files) > 0:
		var err error
		if it.Kind, p, err = classifyUploads(files); err != nil {
			return 0, err
		}
	case len(cleanURLs) > 0:
		if len(cleanURLs) > maxGalleryImgs {
			cleanURLs = cleanURLs[:maxGalleryImgs]
		}
		it.Kind = "image"
		photos := make([]tweetPhoto, len(cleanURLs))
		for i, u := range cleanURLs {
			photos[i] = tweetPhoto{URL: u}
		}
		p.images = s.fetchPhotosConcurrent(ctx, photos)
		if len(p.images) == 0 {
			return 0, errors.New("ingest: none of the provided image URLs could be fetched")
		}
	case rawURL != "":
		if err := validateHTTPURL(rawURL); err != nil {
			return 0, err
		}
		p = s.fromURL(ctx, rawURL, &it)
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
	if len(p.images) > 0 {
		imgs, mRows, firstErr, storeErr := s.processAndStoreImages(ctx, it.Visibility == "private", p.images, true)
		if storeErr != nil {
			return 0, storeErr
		}
		rows = mRows
		switch {
		case len(imgs) > 0:
			applyCoverFromImage(&it, imgs[0], len(imgs))
			it.Images = imgs
		case it.Kind != "image": // a link/embed preview that failed to decode: no cover
		case rawURL != "":
			it.CoverRemoteURL = rawURL // keep the remote image rather than failing
		default:
			return 0, fmt.Errorf("ingest: image processing failed: %w", firstErr)
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

func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("invalid URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("URL scheme %q not allowed (must be http or https)", u.Scheme)
	}
	return nil
}

func applyCoverFromImage(it *store.Item, img store.ItemImage, count int) {
	it.CoverKey, it.ThumbKey, it.SmallKey = img.CoverKey, img.ThumbKey, img.SmallKey
	it.Placeholder, it.DominantColor = img.Placeholder, img.DominantColor
	it.Width, it.Height = img.Width, img.Height
	it.ImageCount = count
}

// fetchPhotosConcurrent downloads up to maxGalleryImgs photos concurrently with
// a bounded worker pool, preserving their exact original order.
func (s *Service) fetchPhotosConcurrent(ctx context.Context, photos []tweetPhoto) []rawImage {
	if len(photos) == 0 {
		return nil
	}
	if len(photos) > maxGalleryImgs {
		photos = photos[:maxGalleryImgs]
	}
	results := make([]*rawImage, len(photos))
	sem := make(chan struct{}, ingestConcurrency)
	var wg sync.WaitGroup
	for i, ph := range photos {
		wg.Add(1)
		go func(idx int, p tweetPhoto) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if r, err := s.fetch(ctx, p.URL); err == nil && strings.HasPrefix(r.ContentType, "image/") && len(r.Body) > 0 {
				results[idx] = &rawImage{data: r.Body, ct: r.ContentType, alt: p.Alt}
			}
		}(i, ph)
	}
	wg.Wait()
	out := make([]rawImage, 0, len(photos))
	for _, r := range results {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}

// processAndStoreImages decodes/resizes and uploads a slice of images concurrently
// (bounded to ingestConcurrency), preserving input order and cleaning up all
// written blobs if any storage error occurs.
func (s *Service) processAndStoreImages(ctx context.Context, private bool, rawImgs []rawImage, primaryFirst bool) (imgs []store.ItemImage, rows []store.Media, firstDecodeErr, storeErr error) {
	type outcome struct {
		img       store.ItemImage
		media     []store.Media
		decodeErr error
		storeErr  error
	}
	out := make([]outcome, len(rawImgs))
	sem := make(chan struct{}, ingestConcurrency)
	var wg sync.WaitGroup
	for i, raw := range rawImgs {
		wg.Add(1)
		go func(idx int, r rawImage) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			proc, err := ProcessImage(r.data)
			if err != nil {
				out[idx].decodeErr = err
				return
			}
			isPrimary := primaryFirst && idx == 0
			imgRow, mRows, err := s.storeImageEntry(ctx, private, r.data, r.ct, r.alt, proc, isPrimary)
			if err != nil {
				out[idx].storeErr = err
				return
			}
			out[idx].img = imgRow
			out[idx].media = mRows
		}(i, raw)
	}
	wg.Wait()

	for _, o := range out {
		if o.storeErr != nil && storeErr == nil {
			storeErr = o.storeErr
		}
		if o.decodeErr != nil && firstDecodeErr == nil {
			firstDecodeErr = o.decodeErr
		}
		if len(o.media) > 0 {
			o.img.Position = len(imgs)
			imgs = append(imgs, o.img)
			rows = append(rows, o.media...)
		}
	}
	if storeErr != nil {
		s.cleanup(ctx, rows)
		return nil, nil, firstDecodeErr, storeErr
	}
	return imgs, rows, firstDecodeErr, nil
}

// fromURL fetches rawURL and fills it's kind/metadata, returning any media to store.
func (s *Service) fromURL(ctx context.Context, rawURL string, it *store.Item) payload {
	var p payload
	fetchImage := func(u, alt string) bool {
		if r, err := s.fetch(ctx, u); err == nil && strings.HasPrefix(r.ContentType, "image/") {
			p.images = append(p.images, rawImage{data: r.Body, ct: r.ContentType, alt: alt})
			return true
		}
		return false
	}
	applySocial := func(author, siteName, text string, photos []tweetPhoto, videoURL, posterURL string) payload {
		it.LinkDescription = text
		it.LinkSiteName = siteName
		setIfEmpty(&it.Title, author)
		if len(photos) > 0 {
			p.images = s.fetchPhotosConcurrent(ctx, photos)
			if len(p.images) > 0 {
				it.Kind = "image"
			}
		} else if videoURL != "" {
			if r, err := s.fetch(ctx, videoURL); err == nil && strings.HasPrefix(r.ContentType, "video/") && len(r.Body) > 0 && len(r.Body) <= videoEmbedMax {
				it.Kind = "embed"
				p.file, p.fileCT, p.fileName = r.Body, r.ContentType, fileNameFromURL(r.FinalURL)
			}
			if posterURL != "" {
				fetchImage(posterURL, "")
			}
		}
		if it.Kind == "" {
			it.Kind = "link"
		}
		return p
	}

	// Tweets: pull ALL photos (with alt text) concurrently via FixTweet.
	if id, _, ok := isTweetURL(rawURL); ok {
		if t, err := s.fetchTweet(ctx, id); err == nil {
			it.SourceURL = cleanTweetURL(rawURL)
			return applySocial(t.Author, "x.com", t.Text, t.Photos, t.VideoURL, t.VideoThumb)
		}
	}

	// Bluesky: pull ALL photos (with alt text) via public AppView API.
	if actor, rkey, ok := isBlueskyURL(rawURL); ok {
		if sp, err := s.fetchBluesky(ctx, actor, rkey); err == nil {
			return applySocial(sp.Author, sp.SiteName, sp.Text, sp.Photos, sp.VideoURL, sp.Poster)
		}
	}

	// Mastodon / Fediverse: pull ALL photos/video via public Mastodon v1 status API.
	if scheme, host, statusID, ok := isMastodonURL(rawURL); ok {
		if sp, err := s.fetchMastodon(ctx, scheme, host, statusID); err == nil {
			return applySocial(sp.Author, sp.SiteName, sp.Text, sp.Photos, sp.VideoURL, sp.Poster)
		}
	}

	// Embed providers (YouTube/Vimeo via oEmbed).
	if it.Kind == "" || it.Kind == "embed" {
		if prov := embedProviderFor(rawURL); prov != "" {
			if info, err := s.fetchEmbed(ctx, rawURL, prov); err == nil {
				it.Kind, it.EmbedProvider, it.EmbedHTML = "embed", info.Provider, info.HTML
				setIfEmpty(&it.Title, info.Title)
				if info.ThumbnailURL != "" {
					fetchImage(info.ThumbnailURL, "")
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
		it.Kind = "image"
		p.images = append(p.images, rawImage{data: r.Body, ct: r.ContentType})
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
			fetchImage(meta.ImageURL, "")
		}
	}
	return p
}

// RefreshTweet re-fetches a previously archived tweet via FixTweet and appends
// any photos beyond those already stored on the item (e.g. upgrading older
// single-photo tweet imports into full multi-image series). Returns how many
// new images were added.
func (s *Service) RefreshTweet(ctx context.Context, itemID int64) (int, error) {
	it, err := s.store.GetItem(ctx, itemID, true)
	if err != nil {
		return 0, err
	}
	if it == nil {
		return 0, fmt.Errorf("item %d not found", itemID)
	}
	tweetID, _, ok := isTweetURL(it.SourceURL)
	if !ok {
		return 0, errors.New("item source URL is not a tweet")
	}
	t, err := s.fetchTweet(ctx, tweetID)
	if err != nil {
		return 0, fmt.Errorf("fetch tweet: %w", err)
	}
	existing := len(it.Images)
	if len(t.Photos) <= existing {
		return 0, nil
	}
	missing := t.Photos[existing:]
	rawImgs := s.fetchPhotosConcurrent(ctx, missing)
	if len(rawImgs) == 0 {
		return 0, errors.New("could not download additional tweet photos")
	}
	imgs, rows, decErr, storeErr := s.processAndStoreImages(ctx, it.Visibility == "private", rawImgs, false)
	if storeErr != nil {
		return 0, storeErr
	}
	if len(imgs) == 0 {
		return 0, fmt.Errorf("process additional tweet photos: %w", decErr)
	}
	if err := s.store.AddItemImages(ctx, itemID, imgs, rows); err != nil {
		s.cleanup(ctx, rows)
		return 0, err
	}
	return len(imgs), nil
}

// ReplaceFile swaps the media on an existing item for a freshly uploaded file.
func (s *Service) ReplaceFile(ctx context.Context, itemID int64, data []byte, name string) error {
	return s.ReplaceFiles(ctx, itemID, []UploadFile{{Bytes: data, Name: name}})
}

// ReplaceFiles swaps the media on an existing item for one or more uploaded files.
func (s *Service) ReplaceFiles(ctx context.Context, itemID int64, files []UploadFile) error {
	if len(files) == 0 || len(files[0].Bytes) == 0 {
		return errors.New("ingest: empty file")
	}
	it, err := s.store.GetItem(ctx, itemID, true)
	if err != nil {
		return err
	}
	if it == nil {
		return fmt.Errorf("ingest: item %d not found", itemID)
	}
	kind, p, err := classifyUploads(files)
	if err != nil {
		return err
	}
	next := store.Item{Kind: kind, Visibility: it.Visibility}
	var rows []store.Media
	if len(p.images) > 0 {
		imgs, mRows, decErr, storeErr := s.processAndStoreImages(ctx, it.Visibility == "private", p.images, true)
		if storeErr != nil {
			return storeErr
		}
		if len(imgs) == 0 {
			return fmt.Errorf("process image: %w", decErr)
		}
		applyCoverFromImage(&next, imgs[0], len(imgs))
		next.Images = imgs
		rows = mRows
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

// AddImages appends one or more uploaded image files and/or image URLs to an
// existing item's gallery.
func (s *Service) AddImages(ctx context.Context, itemID int64, files []UploadFile, urls []string) error {
	it, err := s.store.GetItem(ctx, itemID, true)
	if err != nil {
		return err
	}
	if it == nil {
		return fmt.Errorf("ingest: item %d not found", itemID)
	}
	var rawImgs []rawImage
	for _, f := range files {
		if len(f.Bytes) == 0 {
			continue
		}
		ct := sniffContentType(f.Name, f.Bytes)
		if !strings.HasPrefix(ct, "image/") {
			return fmt.Errorf("file %q is not an image (%s)", f.Name, ct)
		}
		rawImgs = append(rawImgs, rawImage{data: f.Bytes, ct: ct})
	}
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		r, err := s.fetch(ctx, u)
		if err != nil {
			return fmt.Errorf("fetch image: %w", err)
		}
		if !strings.HasPrefix(r.ContentType, "image/") {
			return fmt.Errorf("URL %q is not an image (%s)", u, r.ContentType)
		}
		rawImgs = append(rawImgs, rawImage{data: r.Body, ct: r.ContentType})
	}
	if len(rawImgs) == 0 {
		return errors.New("choose at least one image or enter an image URL")
	}
	if len(it.Images)+len(rawImgs) > maxGalleryImgs {
		return fmt.Errorf("too many images (max %d per post)", maxGalleryImgs)
	}

	imgs, rows, decErr, storeErr := s.processAndStoreImages(ctx, it.Visibility == "private", rawImgs, false)
	if storeErr != nil {
		return storeErr
	}
	if len(imgs) == 0 {
		return fmt.Errorf("process image: %w", decErr)
	}
	if err := s.store.AddItemImages(ctx, itemID, imgs, rows); err != nil {
		s.cleanup(ctx, rows)
		return err
	}
	return nil
}

// DeleteImage removes one image from an item's gallery and deletes its blobs.
func (s *Service) DeleteImage(ctx context.Context, itemID, imageID int64) error {
	keys, err := s.store.DeleteItemImage(ctx, itemID, imageID)
	if err != nil {
		return err
	}
	for _, k := range keys {
		_ = s.media.Delete(ctx, k)
	}
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

// storeImageEntry stores one image's original plus refined variants under a fresh
// asset prefix, uploading the variants concurrently. Animated GIFs preserve their
// animation as full.gif (mirrorable to R2).
func (s *Service) storeImageEntry(ctx context.Context, private bool, raw []byte, ct, alt string, proc *ProcessedImage, isPrimary bool) (store.ItemImage, []store.Media, error) {
	asset := randAsset()
	prefix := "items/" + asset + "/"
	ct = orDefault(ct, "application/octet-stream")
	vname := func(v string) string {
		if isPrimary {
			return v
		}
		return "img:" + asset + ":" + v
	}

	fullKey, fullCT, fullData := prefix+"full.jpg", "image/jpeg", proc.FullJPEG
	if proc.Animated && len(proc.AnimatedGIF) > 0 {
		fullKey, fullCT, fullData = prefix+"full.gif", "image/gif", proc.AnimatedGIF
	}

	rows := []store.Media{
		{Variant: vname("original"), StorageKey: prefix + "original" + extFor(ct, "", ".bin"), ContentType: ct},
		{Variant: vname("full"), StorageKey: fullKey, ContentType: fullCT, Width: proc.Width, Height: proc.Height},
		{Variant: vname("thumb"), StorageKey: prefix + "thumb.jpg", ContentType: "image/jpeg"},
		{Variant: vname("small"), StorageKey: prefix + "small.jpg", ContentType: "image/jpeg"},
	}
	blobs := [][]byte{raw, fullData, proc.ThumbJPEG, proc.SmallJPEG}

	errs := make([]error, len(rows))
	var wg sync.WaitGroup
	for i := range rows {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = s.put(ctx, &rows[idx], blobs[idx], private)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			var written []store.Media
			for i, e := range errs {
				if e == nil {
					written = append(written, rows[i])
				}
			}
			s.cleanup(ctx, written)
			return store.ItemImage{}, nil, err
		}
	}

	thumbKey, smallKey := rows[2].StorageKey, rows[3].StorageKey
	if proc.Animated && len(proc.AnimatedGIF) <= maxAnimatedThumb {
		// Small animated GIFs animate on board cards too, while larger ones use static
		// thumbs on the board and animate cleanly on the detail page & lightbox.
		thumbKey, smallKey = rows[1].StorageKey, ""
	}
	img := store.ItemImage{
		CoverKey:      rows[1].StorageKey,
		ThumbKey:      thumbKey,
		SmallKey:      smallKey,
		Placeholder:   proc.Placeholder,
		DominantColor: proc.DominantColor,
		Width:         proc.Width,
		Height:        proc.Height,
		Alt:           alt,
	}
	return img, rows, nil
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
