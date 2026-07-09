package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"donottouchtheglass/internal/media"
	"donottouchtheglass/internal/store"
)

// reconcile pushes local-only refined variants (full/thumb) up to R2. Useful the
// first time R2 is configured on the server, after content was ingested locally.
func reconcile(ctx context.Context, st *store.Store, ms media.Store) error {
	mirror, ok := ms.(*media.MirrorStore)
	if !ok || !mirror.HasR2() {
		return fmt.Errorf("R2 not configured — set R2_* env vars first")
	}
	rows, err := st.ListUnmirroredMedia(ctx)
	if err != nil {
		return err
	}
	var n int
	for _, m := range rows {
		rc, err := mirror.OpenLocal(m.StorageKey)
		if err != nil {
			log.Printf("skip %s: %v", m.StorageKey, err)
			continue
		}
		// Prefer the actual local file size; DB bytes can be stale.
		size := int64(-1)
		if f, ok := rc.(*os.File); ok {
			if st, serr := f.Stat(); serr == nil {
				size = st.Size()
			}
		}
		if size < 0 && m.Bytes > 0 {
			size = m.Bytes
		}
		if size < 0 {
			data, rerr := io.ReadAll(rc)
			rc.Close()
			if rerr != nil {
				log.Printf("skip %s: %v", m.StorageKey, rerr)
				continue
			}
			size = int64(len(data))
			err = mirror.PutR2(ctx, m.StorageKey, m.ContentType, size, bytes.NewReader(data))
		} else {
			err = mirror.PutR2(ctx, m.StorageKey, m.ContentType, size, rc)
			rc.Close()
		}
		if err != nil {
			log.Printf("upload %s: %v", m.StorageKey, err)
			continue
		}
		if err := st.MarkMediaMirrored(ctx, m.ID); err != nil {
			return err
		}
		n++
	}
	fmt.Printf("mirrored %d/%d refined variants to R2\n", n, len(rows))
	return nil
}
