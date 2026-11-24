package ingest

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/gif"
	"math"
	"sort"

	"github.com/disintegration/imaging"

	"donottouchtheglass/internal/store"
)

// ProcessedImage is the refined output for one source image.
type ProcessedImage struct {
	FullJPEG      []byte
	ThumbJPEG     []byte
	SmallJPEG     []byte   // ~400px wide — the smallest responsive srcset step (phones)
	Animated      bool     // true when the source is a multi-frame animated GIF
	AnimatedGIF   []byte   // original animated GIF bytes when Animated is true
	Placeholder   string   // data:image/jpeg;base64,... (tiny blur-up)
	DominantColor string   // #rrggbb — perceptual OKLab lead color
	Palette       []string // up to 5 distinct perceptual #rrggbb swatches
	Width         int
	Height        int
}

// PaletteCSV returns the canonical comma-separated swatch string for storage.
func (p *ProcessedImage) PaletteCSV() string {
	if p == nil {
		return ""
	}
	return store.PaletteToCSV(p.Palette)
}

const (
	fullMax          = 1600
	thumbMax         = 800
	smallMax         = 400
	phMax            = 24
	maxPixels        = 40_000_000 // ~40MP — reject camera dumps that would OOM
	maxAnimatedThumb = 4 << 20    // animate board cards for GIFs <= 4 MB; larger GIFs animate on detail
)

// ProcessImage decodes raw image bytes and produces refined variants: full
// (<=1600px), thumb (<=800px) and small (<=400px) JPEGs, a tiny base64 blur-up
// placeholder, and a perceptual OKLab palette + dominant color.
// Multi-frame animated GIFs preserve their animation alongside static poster/thumb variants.
func ProcessImage(raw []byte) (*ProcessedImage, error) {
	// Bound decode work: Config only reads headers for most formats.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode image config: %w", err)
	}
	if cfg.Width > 0 && cfg.Height > 0 && int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, fmt.Errorf("image too large (%dx%d > %d pixels)", cfg.Width, cfg.Height, maxPixels)
	}

	var animated bool
	if format == "gif" {
		if g, err := gif.DecodeAll(bytes.NewReader(raw)); err == nil && len(g.Image) > 1 {
			animated = true
		}
	}

	img, err := imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}

	// Largest first, then derive smaller steps from the already-resized image.
	// Record the stored full variant's true dimensions so width/height attributes,
	// srcset descriptors, and "expand full size" reflect the actual served image.
	full := imaging.Fit(img, fullMax, fullMax, imaging.Lanczos)
	fb := full.Bounds()
	pi := &ProcessedImage{
		Width:    fb.Dx(),
		Height:   fb.Dy(),
		Animated: animated,
	}
	if animated {
		pi.Width, pi.Height = img.Bounds().Dx(), img.Bounds().Dy()
		pi.AnimatedGIF = raw
	}
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
	pi.DominantColor, pi.Palette = ExtractPalette(small, 5)
	return pi, nil
}

// ExtractPaletteFromBytes decodes raw image bytes and extracts its perceptual
// OKLab dominant color and up to maxSwatches palette colors. Used by backfill.
func ExtractPaletteFromBytes(raw []byte, maxSwatches int) (dominant string, palette []string, err error) {
	img, err := imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
	if err != nil {
		return "", nil, err
	}
	dom, pal := ExtractPalette(img, maxSwatches)
	return dom, pal, nil
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.JPEG, imaging.JPEGQuality(quality)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ExtractPalette computes a perceptually accurate dominant color and up to
// maxSwatches distinct palette colors using deterministic k-means++ clustering
// in OKLab space. Unlike naive 1×1 RGB averaging (which collapses complementary
// colors into muddy beige/gray), this preserves genuine chromatic accents while
// keeping truly monochrome/editorial-paper/obsidian artworks faithful.
func ExtractPalette(img image.Image, maxSwatches int) (dominant string, palette []string) {
	if maxSwatches <= 0 {
		maxSwatches = 5
	}
	sample := imaging.Fit(img, 48, 48, imaging.Linear)
	b := sample.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return "", nil
	}

	type px struct {
		lab    store.OKLab
		weight float64
	}
	pixels := make([]px, 0, w*h)
	var totalWeight, chromaticWeight float64

	// Mild center-weighting (1.0 at center -> 0.55 at outer border) downweights
	// screenshots' white borders, scanner margins, and letterboxing without
	// ignoring full-bleed backgrounds.
	cx, cy := float64(w-1)/2, float64(h-1)/2
	maxR := math.Hypot(cx, cy)
	if maxR == 0 {
		maxR = 1
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		dy := float64(y-b.Min.Y) - cy
		for x := b.Min.X; x < b.Max.X; x++ {
			r16, g16, b16, a16 := sample.At(x, y).RGBA()
			if a16 < 0x4000 { // ignore mostly-transparent pixels
				continue
			}
			lab := store.RGBToOKLab(uint8(r16>>8), uint8(g16>>8), uint8(b16>>8))
			dx := float64(x-b.Min.X) - cx
			distFrac := math.Hypot(dx, dy) / maxR
			pw := 1.0 - 0.45*distFrac*distFrac
			pixels = append(pixels, px{lab: lab, weight: pw})
			totalWeight += pw
			if lab.Chroma() >= 0.045 {
				chromaticWeight += pw
			}
		}
	}
	if len(pixels) == 0 {
		return "", nil
	}

	const k = 6
	// Deterministic farthest-point (k-means++) initialization in OKLab space.
	// Seed 0 is the pixel closest to the weighted mean so results are 100%
	// reproducible across runs and platforms.
	var meanL, meanA, meanB float64
	for _, p := range pixels {
		meanL += p.lab.L * p.weight
		meanA += p.lab.A * p.weight
		meanB += p.lab.B * p.weight
	}
	mean := store.OKLab{L: meanL / totalWeight, A: meanA / totalWeight, B: meanB / totalWeight}

	centers := make([]store.OKLab, 0, k)
	best0 := 0
	bestD0 := math.Inf(1)
	for i, p := range pixels {
		if d := store.OKLabDistance(p.lab, mean); d < bestD0 {
			bestD0 = d
			best0 = i
		}
	}
	centers = append(centers, pixels[best0].lab)

	minDist := make([]float64, len(pixels))
	for i, p := range pixels {
		d := store.OKLabDistance(p.lab, centers[0])
		minDist[i] = d * d
	}
	for len(centers) < k {
		bestIdx := -1
		bestScore := -1.0
		for i, p := range pixels {
			sc := minDist[i] * p.weight
			if sc > bestScore {
				bestScore = sc
				bestIdx = i
			}
		}
		if bestIdx < 0 || bestScore <= 1e-9 {
			break
		}
		c := pixels[bestIdx].lab
		centers = append(centers, c)
		for i, p := range pixels {
			d := store.OKLabDistance(p.lab, c)
			if d2 := d * d; d2 < minDist[i] {
				minDist[i] = d2
			}
		}
	}

	// Lloyd's k-means iterations in OKLab (8 iterations converges tightly on <=2304px).
	nK := len(centers)
	sumL := make([]float64, nK)
	sumA := make([]float64, nK)
	sumB := make([]float64, nK)
	sumW := make([]float64, nK)
	for iter := 0; iter < 8; iter++ {
		for j := range sumW {
			sumL[j], sumA[j], sumB[j], sumW[j] = 0, 0, 0, 0
		}
		for _, p := range pixels {
			best := 0
			bestD := math.Inf(1)
			for j, c := range centers {
				dl, da, db := p.lab.L-c.L, p.lab.A-c.A, p.lab.B-c.B
				if d2 := dl*dl + da*da + db*db; d2 < bestD {
					bestD = d2
					best = j
				}
			}
			sumL[best] += p.lab.L * p.weight
			sumA[best] += p.lab.A * p.weight
			sumB[best] += p.lab.B * p.weight
			sumW[best] += p.weight
		}
		for j := range centers {
			if sumW[j] > 0 {
				centers[j] = store.OKLab{L: sumL[j] / sumW[j], A: sumA[j] / sumW[j], B: sumB[j] / sumW[j]}
			}
		}
	}

	type cluster struct {
		lab   store.OKLab
		share float64
		score float64
	}
	// If the image has meaningful chromatic content (>=12% chromatic pixels),
	// elevate visually salient chromatic clusters over big neutral margins while
	// still keeping truly monochrome photos/drawings strictly monochrome.
	isChromaticImage := chromaticWeight/totalWeight >= 0.12
	var clusters []cluster
	for j, c := range centers {
		if sumW[j] <= 0 {
			continue
		}
		share := sumW[j] / totalWeight
		chroma := c.Chroma()
		sc := share
		if isChromaticImage {
			// Gentle perceptual salience curve: meaningful chroma + mid-tone readability
			// wins lead swatch over bland white/black frames when area is non-trivial.
			if share >= 0.04 {
				// Avoid boosting near-white/near-black noise; reward real pigment.
				midtone := 1.0 - math.Pow(math.Abs(c.L-0.55)/0.55, 2)*0.45
				if midtone < 0.4 {
					midtone = 0.4
				}
				sc = share * (1.0 + 9.5*chroma*midtone)
			}
			// Penalize extreme paper/letterbox backgrounds slightly for the #1 slot
			// if there is a strong chromatic subject present.
			if chroma < 0.032 && (c.L > 0.88 || c.L < 0.14) {
				sc *= 0.45
			}
		}
		clusters = append(clusters, cluster{lab: c, share: share, score: sc})
	}
	if len(clusters) == 0 {
		hex := store.OKLabToHex(mean)
		return hex, []string{hex}
	}

	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].score > clusters[j].score
	})

	// Deduplicate near-identical clusters (ΔE_OK < 0.055) and collect up to maxSwatches.
	var picked []store.OKLab
	for _, cl := range clusters {
		if len(picked) >= maxSwatches {
			break
		}
		tooClose := false
		for _, prev := range picked {
			if store.OKLabDistance(cl.lab, prev) < 0.055 {
				tooClose = true
				break
			}
		}
		if tooClose {
			continue
		}
		picked = append(picked, cl.lab)
		palette = append(palette, store.OKLabToHex(cl.lab))
	}
	if len(palette) == 0 {
		hex := store.OKLabToHex(clusters[0].lab)
		return hex, []string{hex}
	}
	return palette[0], palette
}
