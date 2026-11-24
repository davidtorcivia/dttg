package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"strings"

	"donottouchtheglass/internal/ingest"
	"donottouchtheglass/internal/media"
	"donottouchtheglass/internal/store"
)

// reconcile moves every blob to the storage tier its item's visibility calls
// for: public media up to R2 (e.g. the first run after configuring R2), private
// media off it and onto local disk only. Idempotent.
func reconcile(ctx context.Context, st *store.Store, ms media.Store) error {
	if _, ok := ms.(*media.MirrorStore); !ok {
		return errors.New("R2 not configured — set R2_* env vars first")
	}
	moved, err := ingest.New(st, ms).SyncPlacement(ctx, 0)
	fmt.Printf("reconcile: moved %d blobs\n", moved)
	return err
}

// backfillVariants generates the ~400px "small" responsive variant for image
// items that predate it, reusing the existing asset path (derived from the cover
// key) so it doesn't orphan the full/thumb blobs. Idempotent: skips items that
// already have a small_key.
func backfillVariants(ctx context.Context, st *store.Store, ms media.Store) error {
	items, err := st.ListItems(ctx, store.ItemFilter{IncludePrivate: true, Cards: true})
	if err != nil {
		return err
	}
	var done, skipped int
	for _, it := range items {
		if it.CoverKey == "" || it.SmallKey != "" {
			continue
		}
		if err := backfillSmall(ctx, st, ms, it); err != nil {
			log.Printf("item %d: %v", it.ID, err)
			skipped++
			continue
		}
		done++
	}
	log.Printf("backfill: generated small variant for %d items (%d skipped)", done, skipped)
	return nil
}

func backfillSmall(ctx context.Context, st *store.Store, ms media.Store, it store.Item) error {
	dir := path.Dir(it.CoverKey) // "items/{asset}"
	if !strings.HasPrefix(dir, "items/") {
		return fmt.Errorf("unexpected cover key %q", it.CoverKey)
	}
	rc, err := ms.Open(ctx, it.CoverKey)
	if err != nil {
		return fmt.Errorf("open cover: %w", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("read cover: %w", err)
	}
	proc, err := ingest.ProcessImage(data)
	if err != nil {
		return fmt.Errorf("process: %w", err)
	}
	key, private := dir+"/small.jpg", it.Visibility == "private"
	if err := ms.Put(ctx, key, "image/jpeg", proc.SmallJPEG, private); err != nil {
		return fmt.Errorf("put small: %w", err)
	}
	onLocal, onR2 := ms.Placement(key, private)
	if err := st.UpsertMedia(ctx, store.Media{
		ItemID: it.ID, Variant: "small", StorageKey: key, ContentType: "image/jpeg",
		Bytes: int64(len(proc.SmallJPEG)), OnLocal: onLocal, OnR2: onR2,
	}); err != nil {
		return fmt.Errorf("upsert media: %w", err)
	}
	return st.SetItemSmallKey(ctx, it.ID, key)
}

// refreshTweets scans all archived tweet items and fetches any additional photos
// from multi-image tweets that were previously imported as a single photo.
func refreshTweets(ctx context.Context, st *store.Store, ms media.Store) error {
	items, err := st.ListItems(ctx, store.ItemFilter{IncludePrivate: true, Cards: true})
	if err != nil {
		return err
	}
	svc := ingest.New(st, ms)
	var checked, updated, addedTotal, failed int
	for _, it := range items {
		if !ingest.IsTweetURL(it.SourceURL) {
			continue
		}
		checked++
		n, err := svc.RefreshTweet(ctx, it.ID)
		if err != nil {
			log.Printf("item %d (%s): %v", it.ID, it.SourceURL, err)
			failed++
			continue
		}
		if n > 0 {
			updated++
			addedTotal += n
			log.Printf("item %d: appended %d additional photo(s)", it.ID, n)
		}
	}
	fmt.Printf("refresh-tweets: checked %d tweets, updated %d items (+%d photos, %d errors)\n", checked, updated, addedTotal, failed)
	return nil
}

// recolor recomputes perceptual OKLab dominant colors and 5-swatch palettes for
// every image item and every gallery plate from its stored media blobs, then
// aggregates multi-image series palettes onto the parent item row.
func recolor(ctx context.Context, st *store.Store, ms media.Store) error {
	items, err := st.ListItems(ctx, store.ItemFilter{IncludePrivate: true, Cards: true})
	if err != nil {
		return err
	}
	var updated, plates, failed int
	for _, it := range items {
		fullItem, err := st.GetItem(ctx, it.ID, true)
		if err != nil || fullItem == nil {
			continue
		}
		// Multi-image or item_images-backed item.
		if len(fullItem.Images) > 0 {
			var updates []store.ImageColorUpdate
			updatedImgs := make([]store.ItemImage, len(fullItem.Images))
			copy(updatedImgs, fullItem.Images)
			anyOK := false
			for i, img := range fullItem.Images {
				key := firstNonEmptyKey(img.SmallKey, img.ThumbKey, img.CoverKey)
				if key == "" {
					continue
				}
				dom, pal, err := extractStoredPalette(ctx, ms, key)
				if err != nil {
					log.Printf("item %d plate %d (%s): %v", it.ID, img.ID, key, err)
					failed++
					continue
				}
				palCSV := store.PaletteToCSV(pal)
				updatedImgs[i].DominantColor = dom
				updatedImgs[i].Palette = palCSV
				updates = append(updates, store.ImageColorUpdate{
					ImageID:       img.ID,
					DominantColor: dom,
					Palette:       palCSV,
				})
				plates++
				anyOK = true
			}
			if !anyOK {
				continue
			}
			seriesDom, seriesPal := store.SeriesColors(updatedImgs)
			if err := st.UpdateItemColors(ctx, it.ID, seriesDom, seriesPal, updates); err != nil {
				log.Printf("item %d update colors: %v", it.ID, err)
				failed++
				continue
			}
			updated++
			continue
		}
		// Legacy single-cover item without item_images rows.
		key := firstNonEmptyKey(fullItem.SmallKey, fullItem.ThumbKey, fullItem.CoverKey)
		if key == "" {
			continue
		}
		dom, pal, err := extractStoredPalette(ctx, ms, key)
		if err != nil {
			log.Printf("item %d (%s): %v", it.ID, key, err)
			failed++
			continue
		}
		if err := st.UpdateItemColors(ctx, it.ID, dom, store.PaletteToCSV(pal), nil); err != nil {
			log.Printf("item %d update colors: %v", it.ID, err)
			failed++
			continue
		}
		plates++
		updated++
	}
	fmt.Printf("recolor: updated %d items (%d plates, %d errors)\n", updated, plates, failed)
	return nil
}

func firstNonEmptyKey(keys ...string) string {
	for _, k := range keys {
		if k != "" {
			return k
		}
	}
	return ""
}

func extractStoredPalette(ctx context.Context, ms media.Store, key string) (string, []string, error) {
	rc, err := ms.Open(ctx, key)
	if err != nil {
		return "", nil, err
	}
	data, err := io.ReadAll(io.LimitReader(rc, 32<<20))
	_ = rc.Close()
	if err != nil {
		return "", nil, err
	}
	return ingest.ExtractPaletteFromBytes(data, 5)
}
