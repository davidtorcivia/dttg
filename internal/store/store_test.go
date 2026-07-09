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

func TestSearch(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	id1, _ := st.CreateItem(ctx, Item{Kind: "image", Title: "Long exposure night", Visibility: "public"})
	_, _ = st.CreateItem(ctx, Item{Kind: "text", Title: "Daytime study", Note: "bright sunlight", Visibility: "public"})

	// FTS prefix match on title
	if res, err := st.SearchItems(ctx, SearchFilter{Query: "expos", IncludePrivate: false, Limit: 200}); err != nil {
		t.Fatal(err)
	} else if len(res) != 1 || res[0].ID != id1 {
		t.Fatalf("search 'expos' returned %d results", len(res))
	}
	// match in note
	if res, _ := st.SearchItems(ctx, SearchFilter{Query: "sunlight", IncludePrivate: false}); len(res) != 1 {
		t.Errorf("search 'sunlight' = %d, want 1", len(res))
	}
	// no match
	if res, _ := st.SearchItems(ctx, SearchFilter{Query: "zzzznotfound", IncludePrivate: false}); len(res) != 0 {
		t.Errorf("nonsense search = %d, want 0", len(res))
	}
	// punctuation-only query must not error (LIKE fallback)
	if _, err := st.SearchItems(ctx, SearchFilter{Query: "!!!", IncludePrivate: false}); err != nil {
		t.Errorf("punctuation query errored: %v", err)
	}
	// update keeps the FTS index in sync
	if err := st.UpdateItem(ctx, id1, "Renamed moonrise", "", "", 0, "public"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if res, _ := st.SearchItems(ctx, SearchFilter{Query: "exposure", IncludePrivate: false}); len(res) != 0 {
		t.Errorf("stale FTS row after update: %d results for old title", len(res))
	}
	if res, _ := st.SearchItems(ctx, SearchFilter{Query: "moonrise", IncludePrivate: false}); len(res) != 1 {
		t.Errorf("FTS not updated: %d results for new title", len(res))
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

func TestRemoteFeedUpsertAndList(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	feed, created, err := st.AddRemoteFeed(ctx, "https://peer.example/feed.json")
	if err != nil {
		t.Fatalf("AddRemoteFeed: %v", err)
	}
	if !created {
		t.Fatal("expected created=true")
	}
	if feed == nil || feed.ID == 0 {
		t.Fatal("expected feed with id")
	}

	now := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	older := now.Add(-time.Hour)
	n, err := st.SaveRemoteFeedFetch(ctx, feed.ID, RemoteFeedUpdate{
		Title: "Peer", SiteURL: "https://peer.example", Description: "archive",
		LastFetchedAt: now, LastSuccessAt: now,
	}, []RemoteFeedItem{
		{RemoteID: "a", URL: "https://peer.example/item/a", Title: "Alpha", PublishedAt: older, FetchedAt: now},
		{RemoteID: "b", URL: "https://peer.example/item/b", Title: "Beta", PublishedAt: now, FetchedAt: now},
	})
	if err != nil {
		t.Fatalf("SaveRemoteFeedFetch: %v", err)
	}
	if n != 2 {
		t.Fatalf("processed = %d, want 2", n)
	}

	// Upsert same remote_id with a new title.
	if _, err := st.SaveRemoteFeedFetch(ctx, feed.ID, RemoteFeedUpdate{
		Title: "Peer", LastFetchedAt: now, LastSuccessAt: now,
	}, []RemoteFeedItem{
		{RemoteID: "a", URL: "https://peer.example/item/a", Title: "Alpha Updated", PublishedAt: older, FetchedAt: now},
	}); err != nil {
		t.Fatalf("SaveRemoteFeedFetch upsert: %v", err)
	}

	items, err := st.ListRemoteFeedItems(ctx, RemoteFeedItemFilter{Limit: 10, ActiveOnly: true})
	if err != nil {
		t.Fatalf("ListRemoteFeedItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items)=%d, want 2", len(items))
	}
	// Newest first by PublishedAt, then ID.
	if items[0].Title != "Beta" {
		t.Errorf("first title = %q, want Beta", items[0].Title)
	}
	if items[1].Title != "Alpha Updated" {
		t.Errorf("second title = %q, want Alpha Updated", items[1].Title)
	}
}

func TestCreateRepostIsIdempotent(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	feed, _, err := st.AddRemoteFeed(ctx, "https://peer.example/feed.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := st.SaveRemoteFeedFetch(ctx, feed.ID, RemoteFeedUpdate{
		Title: "Peer Site", LastFetchedAt: now, LastSuccessAt: now,
	}, []RemoteFeedItem{{
		RemoteID: "r1", URL: "https://peer.example/item/1", Title: "Remote Title",
		ContentText: "Remote body", ImageURL: "https://peer.example/img.jpg",
		PublishedAt: now, FetchedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	items, err := st.ListRemoteFeedItems(ctx, RemoteFeedItemFilter{Limit: 1})
	if err != nil || len(items) != 1 {
		t.Fatalf("seed item: %v len=%d", err, len(items))
	}
	remoteID := items[0].ID

	localID, created, err := st.CreateRepost(ctx, remoteID)
	if err != nil {
		t.Fatalf("CreateRepost: %v", err)
	}
	if !created || localID == 0 {
		t.Fatalf("first repost created=%v localID=%d", created, localID)
	}

	localID2, created2, err := st.CreateRepost(ctx, remoteID)
	if err != nil {
		t.Fatalf("CreateRepost second: %v", err)
	}
	if created2 {
		t.Error("second repost should not create")
	}
	if localID2 != localID {
		t.Errorf("localID2=%d, want %d", localID2, localID)
	}

	got, err := st.GetItem(ctx, localID, false)
	if err != nil || got == nil {
		t.Fatalf("GetItem: %v %v", got, err)
	}
	if got.Kind != "link" || got.Visibility != "public" {
		t.Errorf("kind/vis = %s/%s", got.Kind, got.Visibility)
	}
	if got.SourceURL != "https://peer.example/item/1" {
		t.Errorf("SourceURL = %q", got.SourceURL)
	}
	if got.Title != "Remote Title" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Note != "Remote body" {
		t.Errorf("Note = %q", got.Note)
	}
	if got.LinkSiteName != "Peer Site" {
		t.Errorf("LinkSiteName = %q", got.LinkSiteName)
	}
	if got.CoverRemoteURL != "https://peer.example/img.jpg" {
		t.Errorf("CoverRemoteURL = %q", got.CoverRemoteURL)
	}
}

func TestResetContentClearsRemoteCacheKeepsSources(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	feed, _, err := st.AddRemoteFeed(ctx, "https://peer.example/feed.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := st.SaveRemoteFeedFetch(ctx, feed.ID, RemoteFeedUpdate{
		Title: "Peer", ETag: `"v1"`, LastModified: "Mon, 02 Jan 2006 15:04:05 GMT",
		LastFetchedAt: now, LastSuccessAt: now,
	}, []RemoteFeedItem{{
		RemoteID: "x", URL: "https://peer.example/x", Title: "X",
		PublishedAt: now, FetchedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	items, _ := st.ListRemoteFeedItems(ctx, RemoteFeedItemFilter{Limit: 1})
	if len(items) != 1 {
		t.Fatal("expected remote item")
	}
	if _, _, err := st.CreateRepost(ctx, items[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountItems(ctx, true); n != 1 {
		t.Fatalf("items before reset = %d", n)
	}

	if err := st.ResetContent(ctx); err != nil {
		t.Fatalf("ResetContent: %v", err)
	}

	feeds, err := st.ListRemoteFeeds(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 || feeds[0].FeedURL != "https://peer.example/feed.json" {
		t.Fatalf("feeds after reset = %+v", feeds)
	}
	if feeds[0].ETag != "" || feeds[0].LastModified != "" {
		t.Fatalf("validators not cleared: etag=%q last_modified=%q", feeds[0].ETag, feeds[0].LastModified)
	}
	if !feeds[0].LastFetchedAt.IsZero() || !feeds[0].LastSuccessAt.IsZero() {
		t.Fatalf("fetch timestamps not cleared: fetched=%v success=%v", feeds[0].LastFetchedAt, feeds[0].LastSuccessAt)
	}
	left, err := st.ListRemoteFeedItems(ctx, RemoteFeedItemFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("remote items after reset = %d", len(left))
	}
	if n, _ := st.CountItems(ctx, true); n != 0 {
		t.Fatalf("items after reset = %d", n)
	}
}
