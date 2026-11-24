package store

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// PaletteSwatch describes one selectable color family for palette browsing.
type PaletteSwatch struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
	Hex   string `json:"hex"`
}

// PaletteSwatches is the curated set of editorial palette buckets.
var PaletteSwatches = []PaletteSwatch{
	{Slug: "red", Label: "Red", Hex: "#b94a3e"},
	{Slug: "orange", Label: "Ochre", Hex: "#c97a3d"},
	{Slug: "yellow", Label: "Gold", Hex: "#c9a849"},
	{Slug: "green", Label: "Moss", Hex: "#5f7d5b"},
	{Slug: "blue", Label: "Cobalt", Hex: "#4a6c8c"},
	{Slug: "violet", Label: "Plum", Hex: "#765d85"},
	{Slug: "light", Label: "Paper", Hex: "#dcd6c8"},
	{Slug: "neutral", Label: "Stone", Hex: "#8c867a"},
	{Slug: "dark", Label: "Obsidian", Hex: "#2b2722"},
}

// OKLab represents a perceptually uniform color in Björn Ottosson's OKLab space.
// L is perceptual lightness in [0, 1]; a and b are opponent chromatic axes.
type OKLab struct {
	L, A, B float64
}

// Chroma returns the perceptual chroma C = sqrt(a^2 + b^2).
func (c OKLab) Chroma() float64 { return math.Hypot(c.A, c.B) }

// Hue returns the OKLCH hue angle in degrees [0, 360).
func (c OKLab) Hue() float64 {
	h := math.Atan2(c.B, c.A) * (180 / math.Pi)
	if h < 0 {
		h += 360
	}
	return h
}

// OKLabDistance returns the perceptual Euclidean distance (ΔE_OK) between two
// OKLab colors. Typical perceptual thresholds: ~0.04 very close, ~0.10 similar.
func OKLabDistance(x, y OKLab) float64 {
	dl, da, db := x.L-y.L, x.A-y.A, x.B-y.B
	return math.Sqrt(dl*dl + da*da + db*db)
}

func srgbToLinear(u float64) float64 {
	if u <= 0.04045 {
		return u / 12.92
	}
	return math.Pow((u+0.055)/1.055, 2.4)
}

func linearToSRGB(u float64) float64 {
	if u <= 0 {
		return 0
	}
	if u >= 1 {
		return 1
	}
	if u <= 0.0031308 {
		return 12.92 * u
	}
	return 1.055*math.Pow(u, 1/2.4) - 0.055
}

// RGBToOKLab converts 8-bit sRGB channels to OKLab.
func RGBToOKLab(r8, g8, b8 uint8) OKLab {
	r := srgbToLinear(float64(r8) / 255)
	g := srgbToLinear(float64(g8) / 255)
	b := srgbToLinear(float64(b8) / 255)

	l := 0.4122214708*r + 0.5363325363*g + 0.0514459929*b
	m := 0.2119034982*r + 0.6806995451*g + 0.1073969566*b
	s := 0.0883024619*r + 0.2817188376*g + 0.6299787005*b

	l3 := math.Cbrt(l)
	m3 := math.Cbrt(m)
	s3 := math.Cbrt(s)

	return OKLab{
		L: 0.2104542553*l3 + 0.7936177850*m3 - 0.0040720468*s3,
		A: 1.9779984951*l3 - 2.4285922050*m3 + 0.4505937099*s3,
		B: 0.0259040371*l3 + 0.7827717662*m3 - 0.8086757660*s3,
	}
}

// OKLabToRGB converts an OKLab color back to clamped 8-bit sRGB channels.
func OKLabToRGB(c OKLab) (uint8, uint8, uint8) {
	l3 := c.L + 0.3963377774*c.A + 0.2158037573*c.B
	m3 := c.L - 0.1055613458*c.A - 0.0638541728*c.B
	s3 := c.L - 0.0894841775*c.A - 1.2914855480*c.B

	l := l3 * l3 * l3
	m := m3 * m3 * m3
	s := s3 * s3 * s3

	r := +4.0767416621*l - 3.3077115913*m + 0.2309699292*s
	g := -1.2684380046*l + 2.6097574011*m - 0.3413193965*s
	b := -0.0041960863*l - 0.7034186147*m + 1.7076147010*s

	clamp := func(v float64) uint8 {
		u := math.Round(linearToSRGB(v) * 255)
		if u < 0 {
			return 0
		}
		if u > 255 {
			return 255
		}
		return uint8(u)
	}
	return clamp(r), clamp(g), clamp(b)
}

// OKLabToHex formats an OKLab color as lowercase "#rrggbb".
func OKLabToHex(c OKLab) string {
	r, g, b := OKLabToRGB(c)
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

// ParseHex parses "#rrggbb" (or "rrggbb") into 8-bit sRGB channels.
func ParseHex(hexColor string) (r, g, b uint8, ok bool) {
	s := strings.TrimPrefix(strings.TrimSpace(hexColor), "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return 0, 0, 0, false
	}
	rv, err1 := strconv.ParseUint(s[0:2], 16, 8)
	gv, err2 := strconv.ParseUint(s[2:4], 16, 8)
	bv, err3 := strconv.ParseUint(s[4:6], 16, 8)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return uint8(rv), uint8(gv), uint8(bv), true
}

// NormalizeHex returns canonical lowercase "#rrggbb", or "" if invalid.
func NormalizeHex(hexColor string) string {
	r, g, b, ok := ParseHex(hexColor)
	if !ok {
		return ""
	}
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

// HexToOKLab parses a hex color string directly into OKLab.
func HexToOKLab(hexColor string) (OKLab, bool) {
	r, g, b, ok := ParseHex(hexColor)
	if !ok {
		return OKLab{}, false
	}
	return RGBToOKLab(r, g, b), true
}

// ValidColorBucket reports whether slug is a known palette bucket.
func ValidColorBucket(slug string) bool {
	for _, sw := range PaletteSwatches {
		if sw.Slug == slug {
			return true
		}
	}
	return false
}

// BucketLabel returns the human-readable editorial name for a bucket slug.
func BucketLabel(slug string) string {
	for _, sw := range PaletteSwatches {
		if sw.Slug == slug {
			return sw.Label
		}
	}
	return slug
}

// ColorBucket maps a "#rrggbb" hex color into a perceptual OKLCH palette bucket.
// Using OKLab lightness (L) and chroma (C) avoids HSL's notorious distortion
// where near-black charcoal or pale tinted paper gets inflated saturation and
// lands in chromatic buckets like orange or violet.
func ColorBucket(hexColor string) string {
	lab, ok := HexToOKLab(hexColor)
	if !ok {
		return ""
	}
	l := lab.L
	c := lab.Chroma()

	// True perceptual achromatic / near-neutral tones first.
	if l < 0.22 && c < 0.075 {
		return "dark"
	}
	if l > 0.90 && c < 0.06 {
		return "light"
	}
	if c < 0.036 {
		switch {
		case l < 0.36:
			return "dark"
		case l > 0.78:
			return "light"
		default:
			return "neutral"
		}
	}
	// Very muted warm browns / olives / slates still read as stone or obsidian
	// unless chroma crosses a gentle lightness-adjusted floor.
	if c < 0.048 && (l < 0.34 || l > 0.80) {
		if l < 0.34 {
			return "dark"
		}
		return "light"
	}

	h := lab.Hue()
	switch {
	case h < 34 || h >= 338:
		return "red"
	case h < 74:
		return "orange"
	case h < 115:
		return "yellow"
	case h < 185:
		return "green"
	case h < 275:
		return "blue"
	default:
		return "violet"
	}
}

// PaletteFromCSV splits a stored comma-separated palette string into clean "#rrggbb" hexes.
func PaletteFromCSV(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		if h := NormalizeHex(p); h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// PaletteToCSV serializes a slice of hex colors into canonical comma-separated form.
func PaletteToCSV(hexes []string) string {
	var clean []string
	seen := make(map[string]bool, len(hexes))
	for _, h := range hexes {
		if n := NormalizeHex(h); n != "" && !seen[n] {
			seen[n] = true
			clean = append(clean, n)
		}
	}
	return strings.Join(clean, ",")
}

// ItemPalette returns all known palette swatches for an item (falling back to
// DominantColor when Palette hasn't been backfilled yet).
func ItemPalette(dominant, paletteCSV string) []string {
	swatches := PaletteFromCSV(paletteCSV)
	if len(swatches) > 0 {
		return swatches
	}
	if h := NormalizeHex(dominant); h != "" {
		return []string{h}
	}
	return nil
}

// ItemMatchesBucket reports whether an item belongs to the given palette bucket.
// For multi-image series (or items with rich 5-swatch palettes), it matches if
// the dominant color OR one of the primary salient swatches (top 2) lands in the
// bucket, while requiring chromatic buckets to match with real visual presence.
func ItemMatchesBucket(dominant, paletteCSV, bucket string) bool {
	if bucket == "" {
		return true
	}
	if ColorBucket(dominant) == bucket {
		return true
	}
	swatches := PaletteFromCSV(paletteCSV)
	// Check up to the top 3 salient swatches so multi-image series with e.g. a
	// cobalt plate and a crimson plate show up cleanly under both buckets.
	limit := 3
	if len(swatches) < limit {
		limit = len(swatches)
	}
	for i := 0; i < limit; i++ {
		if ColorBucket(swatches[i]) == bucket {
			return true
		}
	}
	return false
}

// NearestColorMatch returns the minimum perceptual OKLab distance (ΔE_OK) from
// targetHex to any swatch in the item's palette (and optional gallery plates),
// along with the closest matching swatch hex. Returns (+Inf, "") if no valid colors.
func NearestColorMatch(target OKLab, dominant, paletteCSV string, platePalettes ...string) (float64, string) {
	bestDist := math.Inf(1)
	bestHex := ""
	check := func(hex string, rankPenalty float64) {
		lab, ok := HexToOKLab(hex)
		if !ok {
			return
		}
		d := OKLabDistance(target, lab) + rankPenalty
		if d < bestDist {
			bestDist = d
			bestHex = NormalizeHex(hex)
		}
	}
	if dominant != "" {
		check(dominant, 0)
	}
	for i, sw := range PaletteFromCSV(paletteCSV) {
		// Slight rank preference (0.008 per slot) favors primary swatches over minor accents.
		check(sw, float64(i)*0.008)
	}
	for _, pp := range platePalettes {
		for i, sw := range PaletteFromCSV(pp) {
			check(sw, 0.006+float64(i)*0.008)
		}
	}
	return bestDist, bestHex
}

// AggregatePalettes combines per-plate palettes from a multi-image series into
// a cohesive series dominant color and up to maxSwatches distinct swatches.
// Clusters near-identical colors across plates (ΔE_OK < 0.065), rewards colors
// that recur across multiple plates or lead a plate, and preserves chromatic
// highlights alongside structural neutrals.
func AggregatePalettes(platePalettes [][]string, maxSwatches int) (dominant string, palette []string) {
	if maxSwatches <= 0 {
		maxSwatches = 5
	}
	type cluster struct {
		sumL, sumA, sumB float64
		weight           float64
		plates           map[int]bool
		rep              OKLab
	}
	var clusters []*cluster
	const mergeThreshold = 0.065

	// Detect whether the series contains any genuinely chromatic lead/accent pigments.
	hasChromatic := false
	for _, swatches := range platePalettes {
		for sIdx, raw := range swatches {
			if sIdx > 1 {
				break
			}
			if lab, ok := HexToOKLab(raw); ok && lab.Chroma() >= 0.055 {
				hasChromatic = true
				break
			}
		}
		if hasChromatic {
			break
		}
	}

	for pIdx, swatches := range platePalettes {
		for sIdx, raw := range swatches {
			lab, ok := HexToOKLab(raw)
			if !ok {
				continue
			}
			// Lead swatch of each plate carries primary weight; secondary swatches taper cleanly.
			rankW := 1.0 / (1.0 + 0.85*float64(sIdx))
			chroma := lab.Chroma()
			w := rankW
			if hasChromatic {
				w *= 1.0 + 7.5*chroma
				// Prevent repeated neutral paper/obsidian backgrounds across multiple plates
				// from drowning out each plate's signature chromatic pigment.
				if sIdx > 0 && chroma < 0.04 {
					w *= 0.45
				}
			}

			// Find closest existing cluster within mergeThreshold.
			best := -1
			bestD := mergeThreshold
			for i, cl := range clusters {
				if d := OKLabDistance(lab, cl.rep); d < bestD {
					bestD = d
					best = i
				}
			}
			if best >= 0 {
				cl := clusters[best]
				cl.sumL += lab.L * w
				cl.sumA += lab.A * w
				cl.sumB += lab.B * w
				cl.weight += w
				cl.plates[pIdx] = true
				cl.rep = OKLab{L: cl.sumL / cl.weight, A: cl.sumA / cl.weight, B: cl.sumB / cl.weight}
			} else {
				clusters = append(clusters, &cluster{
					sumL:   lab.L * w,
					sumA:   lab.A * w,
					sumB:   lab.B * w,
					weight: w,
					plates: map[int]bool{pIdx: true},
					rep:    lab,
				})
			}
		}
	}
	if len(clusters) == 0 {
		return "", nil
	}

	// Score clusters by combined weight * gentle cross-plate consensus bonus.
	score := func(cl *cluster) float64 {
		bonus := 1.0 + 0.22*float64(len(cl.plates)-1)
		return cl.weight * bonus
	}
	sort.Slice(clusters, func(i, j int) bool {
		return score(clusters[i]) > score(clusters[j])
	})

	// Greedily pick distinct swatches so the series palette spans the full
	// visual character of all plates without redundant near-duplicates.
	var picked []OKLab
	for _, cl := range clusters {
		if len(picked) >= maxSwatches {
			break
		}
		tooClose := false
		for _, p := range picked {
			if OKLabDistance(cl.rep, p) < 0.055 {
				tooClose = true
				break
			}
		}
		if tooClose {
			continue
		}
		picked = append(picked, cl.rep)
		palette = append(palette, OKLabToHex(cl.rep))
	}
	if len(palette) == 0 {
		hex := OKLabToHex(clusters[0].rep)
		return hex, []string{hex}
	}
	return palette[0], palette
}
