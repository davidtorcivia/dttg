package ingest

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"donottouchtheglass/internal/media"
	"donottouchtheglass/internal/store"
)

func makeAnimatedGIF(t *testing.T) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{220, 40, 40, 255}, color.RGBA{40, 120, 220, 255}}
	f1 := image.NewPaletted(image.Rect(0, 0, 64, 64), pal)
	f2 := image.NewPaletted(image.Rect(0, 0, 64, 64), pal)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			f1.SetColorIndex(x, y, 0)
			f2.SetColorIndex(x, y, 1)
		}
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{
		Image: []*image.Paletted{f1, f2},
		Delay: []int{10, 10},
	}); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

func TestAnimatedGIFPreserved(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ms, err := media.NewLocalStore(filepath.Join(t.TempDir(), "media"), "/media")
	if err != nil {
		t.Fatal(err)
	}
	svc := New(st, ms)

	gifBytes := makeAnimatedGIF(t)
	id, err := svc.Create(ctx, Input{FileBytes: gifBytes, FileName: "loop.gif", Title: "Loop"})
	if err != nil {
		t.Fatalf("Create gif: %v", err)
	}
	it, err := st.GetItem(ctx, id, true)
	if err != nil || it == nil {
		t.Fatalf("GetItem: %v", err)
	}
	if !strings.HasSuffix(it.CoverKey, "/full.gif") {
		t.Errorf("CoverKey = %q, want .../full.gif", it.CoverKey)
	}
	if !strings.HasSuffix(it.ThumbKey, "/full.gif") {
		t.Errorf("small animated GIF ThumbKey = %q, want .../full.gif", it.ThumbKey)
	}
	// Duplicate detection catches identical file bytes via SHA-256 content_hash
	dup, err := svc.CheckDuplicate(ctx, Input{FileBytes: gifBytes, FileName: "copy.gif"})
	if err != nil || dup == nil || dup.ID != id {
		t.Fatalf("CheckDuplicate(same bytes) = %+v, err=%v, want ID %d", dup, err, id)
	}
}

func TestBlueskyAndMastodonMultiImageImport(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ms, err := media.NewLocalStore(filepath.Join(t.TempDir(), "media"), "/media")
	if err != nil {
		t.Fatal(err)
	}

	p1 := makeTestPNG(t, 400, 300, color.RGBA{180, 70, 50, 255})
	p2 := makeTestPNG(t, 300, 400, color.RGBA{60, 140, 180, 255})

	svc := New(st, ms)
	svc.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(r.URL.String(), "https://public.api.bsky.app/xrpc/app.bsky.feed.getPostThread"):
			body := `{
				"thread": {
					"post": {
						"author": {"displayName": "Sky Artist", "handle": "sky.bsky.social"},
						"record": {"text": "Two skies"},
						"embed": {
							"images": [
								{"fullsize": "https://cdn.bsky.app/img/1.png", "alt": "Dawn"},
								{"fullsize": "https://cdn.bsky.app/img/2.png", "alt": "Dusk"}
							]
						}
					}
				}
			}`
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		case r.URL.String() == "https://mastodon.art/api/v1/statuses/987654":
			body := `{
				"content": "<p>Two prints<br>from the studio</p>",
				"account": {"display_name": "Printmaker", "username": "printmaker"},
				"media_attachments": [
					{"type": "image", "url": "https://cdn.bsky.app/img/1.png", "description": "Plate A"},
					{"type": "image", "url": "https://cdn.bsky.app/img/2.png", "description": "Plate B"}
				]
			}`
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		case r.URL.String() == "https://cdn.bsky.app/img/1.png":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(p1)), Request: r}, nil
		case r.URL.String() == "https://cdn.bsky.app/img/2.png":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(p2)), Request: r}, nil
		default:
			return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
	})}

	// Bluesky
	bid, err := svc.Create(ctx, Input{URL: "https://bsky.app/profile/sky.bsky.social/post/3abc"})
	if err != nil {
		t.Fatalf("Bluesky Create: %v", err)
	}
	bit, _ := st.GetItem(ctx, bid, true)
	if bit.Kind != "image" || bit.Title != "Sky Artist" || bit.ImageCount != 2 || len(bit.Images) != 2 {
		t.Fatalf("Bluesky item wrong: %+v", bit)
	}
	if bit.Images[0].Alt != "Dawn" || bit.Images[1].Alt != "Dusk" {
		t.Errorf("Bluesky alts = %+v", bit.Images)
	}

	// Mastodon
	mid, err := svc.Create(ctx, Input{URL: "https://mastodon.art/@printmaker/987654"})
	if err != nil {
		t.Fatalf("Mastodon Create: %v", err)
	}
	mit, _ := st.GetItem(ctx, mid, true)
	if mit.Kind != "image" || mit.Title != "Printmaker" || mit.ImageCount != 2 || len(mit.Images) != 2 {
		t.Fatalf("Mastodon item wrong: %+v", mit)
	}
	if !strings.Contains(mit.LinkDescription, "Two prints") || strings.Contains(mit.LinkDescription, "<p>") {
		t.Errorf("Mastodon text not stripped cleanly: %q", mit.LinkDescription)
	}
	if mit.Images[0].Alt != "Plate A" || mit.Images[1].Alt != "Plate B" {
		t.Errorf("Mastodon alts = %+v", mit.Images)
	}
}

func TestRefreshTweetBackfillsMissingPhotos(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ms, err := media.NewLocalStore(filepath.Join(t.TempDir(), "media"), "/media")
	if err != nil {
		t.Fatal(err)
	}
	p1 := makeTestPNG(t, 300, 300, color.RGBA{200, 100, 50, 255})
	p2 := makeTestPNG(t, 300, 300, color.RGBA{50, 200, 100, 255})
	p3 := makeTestPNG(t, 300, 300, color.RGBA{50, 100, 200, 255})

	svc := New(st, ms)
	// Seed an older 1-photo tweet item (as if saved before multi-image support)
	id, err := svc.Create(ctx, Input{
		FileBytes: p1, FileName: "p1.png",
		Source: "https://x.com/arch/status/999888",
		Title:  "Old Single-Photo Tweet",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://api.fxtwitter.com/status/999888":
			body := `{"code":200,"tweet":{"text":"now 3","author":{"name":"Arch"},"media":{"photos":[
				{"url":"https://pbs.twimg.com/media/1.png","altText":"1"},
				{"url":"https://pbs.twimg.com/media/2.png","altText":"2"},
				{"url":"https://pbs.twimg.com/media/3.png","altText":"3"}
			]}}}`
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		case "https://pbs.twimg.com/media/2.png":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(p2)), Request: r}, nil
		case "https://pbs.twimg.com/media/3.png":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(p3)), Request: r}, nil
		default:
			return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
	})}

	added, err := svc.RefreshTweet(ctx, id)
	if err != nil || added != 2 {
		t.Fatalf("RefreshTweet = (%d, %v), want (2, nil)", added, err)
	}
	it, _ := st.GetItem(ctx, id, true)
	if it.ImageCount != 3 || len(it.Images) != 3 {
		t.Fatalf("after refresh: count=%d len=%d", it.ImageCount, len(it.Images))
	}
	// Idempotent second run
	if added2, err := svc.RefreshTweet(ctx, id); err != nil || added2 != 0 {
		t.Fatalf("second RefreshTweet = (%d, %v), want (0, nil)", added2, err)
	}
}
