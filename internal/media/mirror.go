package media

import (
	"bytes"
	"context"
	"io"
	"strings"
)

// MirrorStore splits storage between the local archive and R2:
//   - originals (basename starts with "original") — local only (source of truth)
//   - refined image variants (full/thumb/small) — R2 ONLY when R2 is configured,
//     since they're derived and regenerable from the local original (saves disk)
//   - everything else mirrorable (videos, documents) — local + R2 (not regenerable)
//
// Public URLs prefer R2 for mirrorable keys, with a local fallback.
type MirrorStore struct {
	local *LocalStore
	r2    *R2Store
}

func NewMirrorStore(local *LocalStore, r2 *R2Store) *MirrorStore {
	return &MirrorStore{local: local, r2: r2}
}

func basename(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return key
}

func mirrorable(key string) bool { return !strings.HasPrefix(basename(key), "original") }

// imageVariant reports a derived, regenerable image variant (kept on R2 only).
func imageVariant(key string) bool {
	switch basename(key) {
	case "full.jpg", "thumb.jpg", "small.jpg":
		return true
	}
	return false
}

func (m *MirrorStore) Mirrors(key string) bool { return m.r2 != nil && mirrorable(key) }

func (m *MirrorStore) HasR2() bool { return m.r2 != nil }

func (m *MirrorStore) Put(ctx context.Context, key, contentType string, r io.Reader) error {
	// Regenerable image variants live on R2 only — no local copy.
	if m.r2 != nil && imageVariant(key) {
		return m.r2.Put(ctx, key, contentType, r)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err := m.local.Put(ctx, key, contentType, bytes.NewReader(data)); err != nil {
		return err
	}
	if m.Mirrors(key) {
		if err := m.r2.Put(ctx, key, contentType, bytes.NewReader(data)); err != nil {
			return err
		}
	}
	return nil
}

func (m *MirrorStore) Open(key string) (io.ReadCloser, error) {
	// Image variants live on R2; fall back to local for any written before this
	// change (or not yet reconciled).
	if m.r2 != nil && imageVariant(key) {
		if rc, err := m.r2.Open(key); err == nil {
			return rc, nil
		}
		return m.local.Open(key)
	}
	if rc, err := m.local.Open(key); err == nil {
		return rc, nil
	}
	if m.r2 != nil {
		return m.r2.Open(key)
	}
	return m.local.Open(key) // surfaces the local error
}

func (m *MirrorStore) URL(key string) string {
	if m.Mirrors(key) {
		return m.r2.URL(key)
	}
	return m.local.URL(key)
}

func (m *MirrorStore) Delete(ctx context.Context, key string) error {
	err := m.local.Delete(ctx, key)
	if m.r2 != nil {
		if e := m.r2.Delete(ctx, key); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// OpenLocal / PutR2 support the reconcile/backfill job (push local-only refined
// variants up to R2 once it is configured).
func (m *MirrorStore) OpenLocal(key string) (io.ReadCloser, error) { return m.local.Open(key) }

func (m *MirrorStore) PutR2(ctx context.Context, key, contentType string, r io.Reader) error {
	if m.r2 == nil {
		return nil
	}
	return m.r2.Put(ctx, key, contentType, r)
}
