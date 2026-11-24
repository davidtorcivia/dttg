package ingest

import (
	"image"
	"image/color"
	"testing"

	"donottouchtheglass/internal/store"
)

func TestExtractPaletteChromaticOnPaperBorder(t *testing.T) {
	// Simulate a bold cobalt artwork framed by a generous warm-white paper border.
	// Old 1x1 RGB averaging collapsed this to muddy gray-blue; OKLab clustering
	// must identify cobalt as the dominant pigment AND include paper in the palette.
	img := image.NewRGBA(image.Rect(0, 0, 120, 120))
	paper := color.RGBA{R: 245, G: 243, B: 236, A: 255}
	cobalt := color.RGBA{R: 38, G: 96, B: 178, A: 255}
	for y := 0; y < 120; y++ {
		for x := 0; x < 120; x++ {
			if x >= 25 && x < 95 && y >= 25 && y < 95 {
				img.Set(x, y, cobalt)
			} else {
				img.Set(x, y, paper)
			}
		}
	}
	dom, pal := ExtractPalette(img, 5)
	if got := store.ColorBucket(dom); got != "blue" {
		t.Fatalf("dominant = %s (bucket %q), want blue; palette = %v", dom, got, pal)
	}
	if len(pal) < 2 {
		t.Fatalf("expected at least 2 distinct swatches (cobalt + paper), got %v", pal)
	}
}

func TestExtractPaletteMonochromeStaysNeutral(t *testing.T) {
	// A black-and-white archival print should remain cleanly achromatic.
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			if (x+y)%4 == 0 {
				img.Set(x, y, color.RGBA{R: 28, G: 27, B: 25, A: 255})
			} else {
				img.Set(x, y, color.RGBA{R: 232, G: 229, B: 222, A: 255})
			}
		}
	}
	dom, pal := ExtractPalette(img, 5)
	b := store.ColorBucket(dom)
	if b != "light" && b != "neutral" && b != "dark" {
		t.Fatalf("monochrome print dominant = %s (bucket %q), want achromatic; pal = %v", dom, b, pal)
	}
}
