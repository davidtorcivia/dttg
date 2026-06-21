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
	fullMax  = 1600
	thumbMax = 800
	smallMax = 400
	phMax    = 24
)

// ProcessImage decodes raw image bytes and produces refined variants: full
// (<=1600px), thumb (<=800px) and small (<=400px) JPEGs, a tiny base64 blur-up
// placeholder, and the average ("dominant") color for an elegant loading state.
func ProcessImage(raw []byte) (*ProcessedImage, error) {
	img, err := imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	b := img.Bounds()
	pi := &ProcessedImage{Width: b.Dx(), Height: b.Dy()}

	if pi.FullJPEG, err = encodeJPEG(imaging.Fit(img, fullMax, fullMax, imaging.Lanczos), 82); err != nil {
		return nil, err
	}
	if pi.ThumbJPEG, err = encodeJPEG(imaging.Fit(img, thumbMax, thumbMax, imaging.Lanczos), 78); err != nil {
		return nil, err
	}
	if pi.SmallJPEG, err = encodeJPEG(imaging.Fit(img, smallMax, smallMax, imaging.Lanczos), 76); err != nil {
		return nil, err
	}
	phJPEG, err := encodeJPEG(imaging.Fit(img, phMax, phMax, imaging.Linear), 40)
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
