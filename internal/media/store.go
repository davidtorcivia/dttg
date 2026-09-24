// Package media abstracts where blobs live. LocalStore is the always-present
// source-of-truth archive; MirrorStore layers Cloudflare R2 on top behind the
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

// Store is a blob store keyed by a forward-slash relative key such as
// "items/42/full.jpg".
type Store interface {
	// Put stores a blob where Placement says it belongs, removing any copy from a
	// tier it no longer belongs on (so re-putting after a visibility change moves
	// it). Private blobs never land on the public R2 bucket.
	Put(ctx context.Context, key, contentType string, data []byte, private bool) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	URL(key string) string
	Delete(ctx context.Context, key string) error
	// Placement reports which tiers a Put of key would write to.
	Placement(key string, private bool) (onLocal, onR2 bool)
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

// Put writes via a temp file + rename so a failed write never leaves a
// truncated blob at the final key.
func (l *LocalStore) Put(_ context.Context, key, _ string, data []byte, _ bool) error {
	p := l.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".put-*")
	if err != nil {
		return err
	}
	if err = f.Chmod(0o644); err == nil { // CreateTemp makes 0600; keep blobs readable like os.Create did
		_, err = f.Write(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), p)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

func (l *LocalStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	return os.Open(l.path(key))
}

func (l *LocalStore) URL(key string) string { return l.PublicBase + "/" + strings.TrimLeft(key, "/") }

func (l *LocalStore) Delete(_ context.Context, key string) error {
	err := os.Remove(l.path(key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (l *LocalStore) Placement(string, bool) (bool, bool) { return true, false }

func (l *LocalStore) List(_ context.Context) ([]ObjectInfo, error) {
	var out []ObjectInfo
	err := filepath.WalkDir(l.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(l.Root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, ObjectInfo{Key: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	if os.IsNotExist(err) {
		return out, nil
	}
	return out, err
}
