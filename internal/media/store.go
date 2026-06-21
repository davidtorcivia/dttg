// Package media abstracts where refined images live. LocalStore is the
// always-present source-of-truth archive; R2Store and a mirroring wrapper
// (added with the M2 ingest pipeline) layer Cloudflare R2 on top behind the
// same interface, so the rest of the app never hardcodes a host.
package media

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ObjectInfo describes a stored blob (used by the orphan/maintenance scan).
type ObjectInfo struct {
	Key  string
	Size int64
}

// Store is a content-addressed-ish blob store keyed by a forward-slash relative
// key such as "items/42/full.webp".
type Store interface {
	Put(ctx context.Context, key, contentType string, r io.Reader) error
	Open(key string) (io.ReadCloser, error)
	URL(key string) string
	Delete(ctx context.Context, key string) error
	// Mirrors reports whether a Put for this key is also written to R2 (so the
	// ingest layer can record on_r2 accurately).
	Mirrors(key string) bool
	// List enumerates every stored blob (for orphan detection).
	List(ctx context.Context) ([]ObjectInfo, error)
}

// LocalStore writes blobs under Root and serves them from PublicBase (e.g. "/media").
type LocalStore struct {
	Root       string
	PublicBase string
}

func NewLocalStore(root, publicBase string) (*LocalStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &LocalStore{Root: root, PublicBase: strings.TrimRight(publicBase, "/")}, nil
}

func (l *LocalStore) path(key string) string {
	return filepath.Join(l.Root, filepath.FromSlash(key))
}

func (l *LocalStore) Put(_ context.Context, key, _ string, r io.Reader) error {
	p := l.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func (l *LocalStore) Open(key string) (io.ReadCloser, error) { return os.Open(l.path(key)) }

func (l *LocalStore) URL(key string) string { return l.PublicBase + "/" + strings.TrimLeft(key, "/") }

func (l *LocalStore) Delete(_ context.Context, key string) error {
	err := os.Remove(l.path(key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (l *LocalStore) Mirrors(string) bool { return false }

func (l *LocalStore) List(_ context.Context) ([]ObjectInfo, error) {
	var out []ObjectInfo
	err := filepath.WalkDir(l.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(l.Root, p)
		if rerr != nil {
			return rerr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		out = append(out, ObjectInfo{Key: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	if os.IsNotExist(err) {
		return out, nil
	}
	return out, err
}
