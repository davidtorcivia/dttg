package ingest

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"

	"github.com/disintegration/imaging"
)

// ProcessedImage is the refined output for one source image.
type ProcessedImage struct {
	FullJPEG      []byte
	ThumbJPEG     []byte
	SmallJPEG     []byte // ~400px wide — the smallest responsive srcset step (phones)
	Placeholder   string // data:image/jpeg;base64,... (tiny blur-up)
	DominantColor string // #rrggbb
	Width         int
	Height        int
}

const (
	fullMax   = 1600
	thumbMax  = 800
	smallMax  = 400
	phMax     = 24
	maxPixels = 40_000_000 // ~40MP — reject camera dumps that would OOM
)

// ProcessImage decodes raw image bytes and produces refined variants: full
// (<=1600px), thumb (<=800px) and small (<=400px) JPEGs, a tiny base64 blur-up
// placeholder, and the average ("dominant") color for an elegant loading state.
// Variants are chained (full → thumb → small) to avoid re-resizing the original.
func ProcessImage(raw []byte) (*ProcessedImage, error) {
	// Bound decode work: Config only reads headers for most formats.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode image config: %w", err)
	}
	if cfg.Width > 0 && cfg.Height > 0 && int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, fmt.Errorf("image too large (%dx%d > %d pixels)", cfg.Width, cfg.Height, maxPixels)
	}

	img, err := imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	b := img.Bounds()
	pi := &ProcessedImage{Width: b.Dx(), Height: b.Dy()}

	// Largest first, then derive smaller steps from the already-resized image.
	full := imaging.Fit(img, fullMax, fullMax, imaging.Lanczos)
	if pi.FullJPEG, err = encodeJPEG(full, 82); err != nil {
		return nil, err
	}
	thumb := imaging.Fit(full, thumbMax, thumbMax, imaging.Lanczos)
	if pi.ThumbJPEG, err = encodeJPEG(thumb, 78); err != nil {
		return nil, err
	}
	small := imaging.Fit(thumb, smallMax, smallMax, imaging.Lanczos)
	if pi.SmallJPEG, err = encodeJPEG(small, 76); err != nil {
		return nil, err
	}
	phJPEG, err := encodeJPEG(imaging.Fit(small, phMax, phMax, imaging.Linear), 40)
	if err != nil {
		return nil, err
	}
	pi.Placeholder = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(phJPEG)
	pi.DominantColor = averageColor(img)
	return pi, nil
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.JPEG, imaging.JPEGQuality(quality)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func averageColor(img image.Image) string {
	c := imaging.Resize(img, 1, 1, imaging.Box).At(0, 0)
	r, g, b, _ := c.RGBA() // 16-bit channels
	return fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}
