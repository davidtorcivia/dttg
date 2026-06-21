package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestItemCRUD(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	if n, err := st.CountItems(ctx, true); err != nil || n != 0 {
		t.Fatalf("CountItems empty = %d, %v", n, err)
	}
	id, err := st.CreateItem(ctx, Item{Kind: "text", Title: "Note", Note: "body", Visibility: "public"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetItem(ctx, id, false)
	if err != nil || got == nil {
		t.Fatalf("get: got=%v err=%v", got, err)
	}
	if got.Title != "Note" {
		t.Errorf("title = %q", got.Title)
	}
	items, err := st.ListItems(ctx, ItemFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("list len = %d, want 1", len(items))
	}
	if n, _ := st.CountItems(ctx, true); n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

func TestPrivateItemVisibility(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	id, err := st.CreateItem(ctx, Item{Kind: "text", Title: "secret", Visibility: "private"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, _ := st.GetItem(ctx, id, false); got != nil {
		t.Errorf("private item visible to public")
	}
	if got, _ := st.GetItem(ctx, id, true); got == nil {
		t.Errorf("private item hidden from admin")
	}
	if items, _ := st.ListItems(ctx, ItemFilter{}); len(items) != 0 {
		t.Errorf("private item in public list: %d", len(items))
	}
	if items, _ := st.ListItems(ctx, ItemFilter{IncludePrivate: true}); len(items) != 1 {
		t.Errorf("private item missing from admin list: %d", len(items))
	}
}

func TestPurgeExpiredSessions(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	_ = st.CreateSession(ctx, "valid", time.Hour)
	_ = st.CreateSession(ctx, "expired", -time.Hour)
	n, err := st.PurgeExpiredSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("purged %d, want >= 1", n)
	}
	if ok, _ := st.SessionValid(ctx, "valid"); !ok {
		t.Error("valid session was purged")
	}
	if ok, _ := st.SessionValid(ctx, "expired"); ok {
		t.Error("expired session still valid after purge")
	}
}

func TestSettingsRoundtrip(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if v, _ := st.GetSetting(ctx, "missing"); v != "" {
		t.Errorf("missing setting = %q", v)
	}
	if err := st.SetSetting(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "k", "v2"); err != nil { // upsert
		t.Fatal(err)
	}
	if v, _ := st.GetSetting(ctx, "k"); v != "v2" {
		t.Errorf("setting = %q, want v2", v)
	}
}

func TestSessions(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.CreateSession(ctx, "sid1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.SessionValid(ctx, "sid1"); !ok {
		t.Error("session not valid after create")
	}
	if err := st.DeleteSession(ctx, "sid1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.SessionValid(ctx, "sid1"); ok {
		t.Error("session still valid after delete")
	}
}
