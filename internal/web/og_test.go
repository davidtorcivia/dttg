package web

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"donottouchtheglass/internal/store"
)

func syntheticJPEG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 240, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 240; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func seedImageItems(t *testing.T, s *Server, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		key := "items/og" + strconv.Itoa(i) + "/full.jpg"
		col := color.RGBA{uint8(30 + i*25), 90, uint8(180 - i*12), 255}
		jpegBytes := syntheticJPEG(t, col)
		if err := s.media.Put(ctx, key, "image/jpeg", int64(len(jpegBytes)), bytes.NewReader(jpegBytes)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.CreateItem(ctx, store.Item{
			Kind: "image", Title: "Item " + strconv.Itoa(i), Visibility: "public",
			CoverKey: key, SmallKey: key,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSiteOGImage(t *testing.T) {
	s := newTestServer(t)
	seedImageItems(t, s, 8)
	h := s.Handler()

	rec := getReq(t, h, "/og.jpg")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("content-type %q", ct)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Width != ogW || cfg.Height != ogH {
		t.Fatalf("dims %dx%d, want %dx%d", cfg.Width, cfg.Height, ogW, ogH)
	}

	// A conditional request with the same ETag is a cache hit (304).
	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/og.jpg", nil)
	req.Header.Set("If-None-Match", etag)
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("conditional GET: got %d, want 304", rec2.Code)
	}
}

// With no items (or none with covers) the endpoint still serves a valid card.
func TestSiteOGImageEmptyFallback(t *testing.T) {
	s := newTestServer(t)
	rec := getReq(t, s.Handler(), "/og.jpg")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Width != ogW || cfg.Height != ogH {
		t.Fatalf("dims %dx%d, want %dx%d", cfg.Width, cfg.Height, ogW, ogH)
	}
}

// The default (non-item) page meta points at the generated card with a large card.
func TestDefaultMetaUsesGeneratedCard(t *testing.T) {
	s := newTestServer(t)
	body := getReq(t, s.Handler(), "/").Body.String()
	if !strings.Contains(body, `property="og:image" content="http://localhost:8080/og.jpg"`) {
		t.Errorf("board og:image should be the generated /og.jpg card")
	}
	if !strings.Contains(body, `name="twitter:card" content="summary_large_image"`) {
		t.Errorf("board should use a summary_large_image twitter card")
	}
}
