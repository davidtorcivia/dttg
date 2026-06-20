package media

import (
	"bytes"
	"context"
	"io"
	"strings"
)

// MirrorStore writes every blob to the local archive (source of truth) and, for
// refined variants, also to R2. Public URLs prefer R2 (CDN) with local fallback.
// Originals (key basename starts with "original") are archived locally only.
type MirrorStore struct {
	local *LocalStore
	r2    *R2Store
}

func NewMirrorStore(local *LocalStore, r2 *R2Store) *MirrorStore {
	return &MirrorStore{local: local, r2: r2}
}

func mirrorable(key string) bool {
	base := key
	if i := strings.LastIndex(key, "/"); i >= 0 {
		base = key[i+1:]
	}
	return !strings.HasPrefix(base, "original")
}

func (m *MirrorStore) Mirrors(key string) bool { return m.r2 != nil && mirrorable(key) }

func (m *MirrorStore) HasR2() bool { return m.r2 != nil }

func (m *MirrorStore) Put(ctx context.Context, key, contentType string, r io.Reader) error {
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
