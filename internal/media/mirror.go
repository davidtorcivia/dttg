package media

import (
	"context"
	"io"
	"path"
	"strings"
)

// MirrorStore splits storage between the local archive and R2:
//   - originals (basename starts with "original") — local only (source of truth)
//   - refined image variants (full/thumb/small) — R2 only, since they're derived
//     and regenerable from the local original (saves disk)
//   - everything else (videos, documents) — local + R2 (not regenerable)
//   - anything belonging to a private item — local only
type MirrorStore struct {
	local *LocalStore
	r2    *R2Store
}

func NewMirrorStore(local *LocalStore, r2 *R2Store) *MirrorStore {
	return &MirrorStore{local: local, r2: r2}
}

func mirrorable(key string) bool { return !strings.HasPrefix(path.Base(key), "original") }

// imageVariant reports a derived, regenerable image variant (kept on R2 only).
func imageVariant(key string) bool {
	switch path.Base(key) {
	case "full.jpg", "thumb.jpg", "small.jpg":
		return true
	}
	return false
}

func (m *MirrorStore) Placement(key string, private bool) (onLocal, onR2 bool) {
	switch {
	case private || !mirrorable(key):
		return true, false
	case imageVariant(key):
		return false, true
	}
	return true, true
}

func (m *MirrorStore) Put(ctx context.Context, key, contentType string, data []byte, private bool) error {
	onLocal, onR2 := m.Placement(key, private)
	if onLocal {
		if err := m.local.Put(ctx, key, contentType, data, private); err != nil {
			return err
		}
	}
	if onR2 {
		if err := m.r2.put(ctx, key, contentType, data); err != nil {
			return err
		}
	}
	// Written to the target tier(s); now drop any stale copy elsewhere.
	if !onLocal {
		return m.local.Delete(ctx, key)
	}
	if !onR2 && mirrorable(key) {
		return m.r2.delete(ctx, key)
	}
	return nil
}

// Open reads from the tier the key normally lives on, falling back to the
// other (image variants written before R2 was configured are still local).
func (m *MirrorStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	first, second := m.local.Open, m.r2.open
	if imageVariant(key) {
		first, second = second, first
	}
	if rc, err := first(ctx, key); err == nil {
		return rc, nil
	}
	return second(ctx, key)
}

func (m *MirrorStore) URL(key string) string {
	if mirrorable(key) {
		return m.r2.url(key)
	}
	return m.local.URL(key)
}

func (m *MirrorStore) Delete(ctx context.Context, key string) error {
	err := m.local.Delete(ctx, key)
	if e := m.r2.delete(ctx, key); err == nil {
		err = e
	}
	return err
}

// List merges local + R2 objects (deduped by key) so the orphan scan sees every
// stored blob regardless of which tier holds it.
func (m *MirrorStore) List(ctx context.Context) ([]ObjectInfo, error) {
	out, err := m.local.List(ctx)
	if err != nil {
		return nil, err
	}
	remote, err := m.r2.list(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(out))
	for _, o := range out {
		seen[o.Key] = true
	}
	for _, o := range remote {
		if !seen[o.Key] {
			out = append(out, o)
		}
	}
	return out, nil
}
