package ingest

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"donottouchtheglass/internal/media"
	"donottouchtheglass/internal/store"
)

// tierStore is an in-memory two-tier media.Store: private blobs live "local",
// public ones "remote", mirroring MirrorStore's privacy rule.
type tierStore struct{ local, remote map[string][]byte }

func (t *tierStore) Put(_ context.Context, key, _ string, data []byte, private bool) error {
	if private {
		t.local[key] = data
		delete(t.remote, key)
	} else {
		t.remote[key] = data
		delete(t.local, key)
	}
	return nil
}
func (t *tierStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if b, ok := t.local[key]; ok {
		return io.NopCloser(strings.NewReader(string(b))), nil
	}
	return io.NopCloser(strings.NewReader(string(t.remote[key]))), nil
}
func (t *tierStore) URL(key string) string { return "/" + key }
func (t *tierStore) Delete(_ context.Context, key string) error {
	delete(t.local, key)
	delete(t.remote, key)
	return nil
}
func (t *tierStore) Placement(_ string, private bool) (bool, bool)    { return private, !private }
func (t *tierStore) List(context.Context) ([]media.ObjectInfo, error) { return nil, nil }

// A visibility flip must move blobs: private media off the public tier.
func TestSyncPlacementFollowsVisibility(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ts := &tierStore{local: map[string][]byte{}, remote: map[string][]byte{}}
	svc := New(st, ts)

	id, err := svc.Create(ctx, Input{FileBytes: []byte("%PDF-1.4 hello"), FileName: "a.pdf", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	it, _ := st.GetItem(ctx, id, true)
	if _, ok := ts.remote[it.FileKey]; !ok {
		t.Fatal("public upload should be on the remote tier")
	}
	if err := st.UpdateItem(ctx, id, store.ItemEdit{Visibility: "private"}); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.SyncPlacement(ctx, id); err != nil || n != 1 {
		t.Fatalf("SyncPlacement moved %d, err %v", n, err)
	}
	if _, ok := ts.remote[it.FileKey]; ok {
		t.Fatal("private item's blob is still on the public tier")
	}
	if string(ts.local[it.FileKey]) != "%PDF-1.4 hello" {
		t.Fatal("blob content lost in the move")
	}
	rows, _ := st.ListMedia(ctx, id)
	if len(rows) != 1 || !rows[0].OnLocal || rows[0].OnR2 {
		t.Fatalf("placement flags not updated: %+v", rows)
	}
	if n, _ := svc.SyncPlacement(ctx, 0); n != 0 {
		t.Fatalf("second sync moved %d blobs, want 0 (idempotent)", n)
	}
}
