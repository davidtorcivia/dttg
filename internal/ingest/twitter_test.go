package ingest

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"donottouchtheglass/internal/media"
	"donottouchtheglass/internal/store"
)

func makeTestPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestTweetURLParsingAndCleaning(t *testing.T) {
	cases := []struct {
		raw       string
		wantID    string
		wantIdx   int
		wantOK    bool
		wantClean string
	}{
		{"https://x.com/alice/status/1234567890", "1234567890", 0, true, "https://x.com/alice/status/1234567890"},
		{"https://twitter.com/bob/status/999/photo/2?s=20", "999", 2, true, "https://x.com/bob/status/999"},
		{"https://mobile.twitter.com/carol/statuses/42", "42", 0, true, "https://x.com/carol/status/42"},
		{"https://www.x.com/dave/status/777/photo/3", "777", 3, true, "https://x.com/dave/status/777"},
		{"https://fxtwitter.com/eve/status/555/photo/1", "555", 1, true, "https://x.com/eve/status/555"},
		{"https://vxtwitter.com/frank/status/888", "888", 0, true, "https://x.com/frank/status/888"},
		{"https://x.com/i/web/status/321", "321", 0, true, "https://x.com/i/status/321"},
		{"https://example.com/status/123", "", 0, false, "https://example.com/status/123"},
	}
	for _, tc := range cases {
		id, idx, ok := isTweetURL(tc.raw)
		if id != tc.wantID || idx != tc.wantIdx || ok != tc.wantOK {
			t.Errorf("isTweetURL(%q) = (%q,%d,%v), want (%q,%d,%v)", tc.raw, id, idx, ok, tc.wantID, tc.wantIdx, tc.wantOK)
		}
		if got := cleanTweetURL(tc.raw); got != tc.wantClean {
			t.Errorf("cleanTweetURL(%q) = %q, want %q", tc.raw, got, tc.wantClean)
		}
	}
}

func TestMultiImageTweetImport(t *testing.T) {
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

	p1 := makeTestPNG(t, 600, 400, color.RGBA{200, 50, 50, 255})
	p2 := makeTestPNG(t, 500, 700, color.RGBA{50, 200, 50, 255})
	p3 := makeTestPNG(t, 800, 800, color.RGBA{50, 50, 200, 255})

	svc := New(st, ms)
	svc.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://api.fxtwitter.com/status/123456":
			body := `{
				"code": 200,
				"tweet": {
					"text": "Three studies in warm concrete.",
					"author": {"name": "Studio Archive", "screen_name": "studioarch"},
					"media": {
						"photos": [
							{"url": "https://pbs.twimg.com/media/one.png", "altText": "South facade"},
							{"url": "https://pbs.twimg.com/media/two.png", "altText": "Stairwell"},
							{"url": "https://pbs.twimg.com/media/three.png", "altText": "Courtyard"}
						]
					}
				}
			}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		case "https://pbs.twimg.com/media/one.png":
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(p1)), Request: r}, nil
		case "https://pbs.twimg.com/media/two.png":
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(p2)), Request: r}, nil
		case "https://pbs.twimg.com/media/three.png":
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(p3)), Request: r}, nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
	})}

	id, err := svc.Create(ctx, Input{URL: "https://fxtwitter.com/studioarch/status/123456/photo/2"})
	if err != nil {
		t.Fatalf("Create tweet: %v", err)
	}
	it, err := st.GetItem(ctx, id, true)
	if err != nil || it == nil {
		t.Fatalf("GetItem: %v %v", it, err)
	}
	if it.Kind != "image" {
		t.Errorf("Kind = %q, want image", it.Kind)
	}
	if it.SourceURL != "https://x.com/studioarch/status/123456" {
		t.Errorf("SourceURL = %q", it.SourceURL)
	}
	if it.Title != "Studio Archive" {
		t.Errorf("Title = %q", it.Title)
	}
	if it.LinkDescription != "Three studies in warm concrete." {
		t.Errorf("LinkDescription = %q", it.LinkDescription)
	}
	if it.ImageCount != 3 || len(it.Images) != 3 {
		t.Fatalf("ImageCount=%d len(Images)=%d, want 3", it.ImageCount, len(it.Images))
	}
	if it.Images[0].Alt != "South facade" || it.Images[1].Alt != "Stairwell" || it.Images[2].Alt != "Courtyard" {
		t.Errorf("alt texts wrong: %+v", it.Images)
	}
	if it.Images[0].Width != 600 || it.Images[1].Width != 500 || it.Images[2].Width != 800 {
		t.Errorf("widths wrong: %d, %d, %d", it.Images[0].Width, it.Images[1].Width, it.Images[2].Width)
	}
	// 3 images * 4 variants = 12 media rows, all with unique variants and blobs present.
	rows, err := st.ListMedia(ctx, id)
	if err != nil || len(rows) != 12 {
		t.Fatalf("media rows = %d, err=%v, want 12", len(rows), err)
	}
}

func TestMultiImageUploadAddDeleteAndPlacement(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ts := &tierStore{local: map[string][]byte{}, remote: map[string][]byte{}}
	svc := New(st, ts)

	p1 := makeTestPNG(t, 200, 200, color.RGBA{255, 0, 0, 255})
	p2 := makeTestPNG(t, 300, 200, color.RGBA{0, 255, 0, 255})
	p3 := makeTestPNG(t, 400, 200, color.RGBA{0, 0, 255, 255})

	id, err := svc.Create(ctx, Input{
		Title:      "Pair",
		Visibility: "public",
		Files: []UploadFile{
			{Bytes: p1, Name: "a.png"},
			{Bytes: p2, Name: "b.png"},
		},
	})
	if err != nil {
		t.Fatalf("Create multi-upload: %v", err)
	}
	if len(ts.remote) != 8 {
		t.Fatalf("public remote blobs = %d, want 8", len(ts.remote))
	}

	// Append a 3rd image
	if err := svc.AddImages(ctx, id, []UploadFile{{Bytes: p3, Name: "c.png"}}, nil); err != nil {
		t.Fatalf("AddImages: %v", err)
	}
	if len(ts.remote) != 12 {
		t.Fatalf("remote blobs after add = %d, want 12", len(ts.remote))
	}

	// Flip item to private -> SyncPlacement moves all 12 blobs off remote onto local
	if err := st.UpdateItem(ctx, id, store.ItemEdit{Title: "Pair", Visibility: "private"}); err != nil {
		t.Fatal(err)
	}
	moved, err := svc.SyncPlacement(ctx, id)
	if err != nil || moved != 12 {
		t.Fatalf("SyncPlacement moved=%d err=%v, want 12", moved, err)
	}
	if len(ts.remote) != 0 || len(ts.local) != 12 {
		t.Fatalf("after private sync: remote=%d local=%d", len(ts.remote), len(ts.local))
	}

	// Delete image #2 -> removes its 4 blobs from storage
	it, _ := st.GetItem(ctx, id, true)
	if err := svc.DeleteImage(ctx, id, it.Images[1].ID); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	if len(ts.local) != 8 {
		t.Fatalf("local blobs after deleting 1 image = %d, want 8", len(ts.local))
	}
}
