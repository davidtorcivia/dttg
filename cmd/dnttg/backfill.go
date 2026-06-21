package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"donottouchtheglass/internal/ingest"
	"donottouchtheglass/internal/media"
	"donottouchtheglass/internal/store"
)

// assetFromKey extracts the asset id from a storage key like
// "items/{asset}/full.jpg".
func assetFromKey(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) >= 3 && parts[0] == "items" {
		return parts[1]
	}
	return ""
}

// backfillVariants generates the ~400px "small" responsive variant for image
// items that predate it, reusing the existing asset path (derived from the cover
// key) so it doesn't orphan the full/thumb blobs. Idempotent: skips items that
// already have a small_key.
func backfillVariants(ctx context.Context, st *store.Store, ms media.Store) error {
	items, err := st.ListItems(ctx, store.ItemFilter{IncludePrivate: true})
	if err != nil {
		return err
	}
	var done, skipped int
	for _, it := range items {
		if it.CoverKey == "" || it.SmallKey != "" {
			continue
		}
		asset := assetFromKey(it.CoverKey)
		if asset == "" {
			skipped++
			continue
		}
		rc, err := ms.Open(it.CoverKey)
		if err != nil {
			log.Printf("item %d: open cover: %v", it.ID, err)
			skipped++
			continue
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			log.Printf("item %d: read cover: %v", it.ID, err)
			skipped++
			continue
		}
		proc, err := ingest.ProcessImage(data)
		if err != nil {
			log.Printf("item %d: process: %v", it.ID, err)
			skipped++
			continue
		}
		smallKey := fmt.Sprintf("items/%s/small.jpg", asset)
		if err := ms.Put(ctx, smallKey, "image/jpeg", bytes.NewReader(proc.SmallJPEG)); err != nil {
			log.Printf("item %d: put small: %v", it.ID, err)
			skipped++
			continue
		}
		if _, err := st.AddMedia(ctx, store.Media{
			ItemID: it.ID, Variant: "small", StorageKey: smallKey, ContentType: "image/jpeg",
			Bytes: int64(len(proc.SmallJPEG)), OnLocal: true, OnR2: ms.Mirrors(smallKey),
		}); err != nil {
			log.Printf("item %d: add media: %v", it.ID, err)
			skipped++
			continue
		}
		if err := st.SetItemSmallKey(ctx, it.ID, smallKey); err != nil {
			log.Printf("item %d: set small_key: %v", it.ID, err)
			skipped++
			continue
		}
		done++
	}
	log.Printf("backfill: generated small variant for %d items (%d skipped)", done, skipped)
	return nil
}
