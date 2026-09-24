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
