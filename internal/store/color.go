package store

import (
	"math"
	"strconv"
	"strings"
)

// PaletteSwatch describes one selectable color family for palette browsing.
type PaletteSwatch struct {
	Slug  string
	Label string
	Hex   string
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

// ValidColorBucket reports whether slug is a known palette bucket.
func ValidColorBucket(slug string) bool {
	for _, sw := range PaletteSwatches {
		if sw.Slug == slug {
			return true
		}
	}
	return false
}

// ColorBucket maps a "#rrggbb" hex dominant color into a perceptual palette bucket.
func ColorBucket(hexColor string) string {
	s := strings.TrimPrefix(strings.TrimSpace(hexColor), "#")
	if len(s) != 6 {
		return ""
	}
	rv, err1 := strconv.ParseUint(s[0:2], 16, 8)
	gv, err2 := strconv.ParseUint(s[2:4], 16, 8)
	bv, err3 := strconv.ParseUint(s[4:6], 16, 8)
	if err1 != nil || err2 != nil || err3 != nil {
		return ""
	}
	r, g, b := float64(rv)/255, float64(gv)/255, float64(bv)/255
	maxC := math.Max(r, math.Max(g, b))
	minC := math.Min(r, math.Min(g, b))
	d := maxC - minC
	l := (maxC + minC) / 2
	var sat, hue float64
	if d > 0 {
		sat = d / (1 - math.Abs(2*l-1))
		switch maxC {
		case r:
			hue = 60 * math.Mod((g-b)/d, 6)
		case g:
			hue = 60 * ((b-r)/d + 2)
		case b:
			hue = 60 * ((r-g)/d + 4)
		}
		if hue < 0 {
			hue += 360
		}
	}
	// Very dark, very bright, or low-saturation tones land in neutral/paper/obsidian.
	if l < 0.16 {
		return "dark"
	}
	if l > 0.84 && sat < 0.25 {
		return "light"
	}
	if sat < 0.14 {
		if l > 0.70 {
			return "light"
		}
		if l < 0.30 {
			return "dark"
		}
		return "neutral"
	}
	switch {
	case hue < 18 || hue >= 342:
		return "red"
	case hue < 44:
		return "orange"
	case hue < 72:
		return "yellow"
	case hue < 170:
		return "green"
	case hue < 258:
		return "blue"
	default:
		return "violet"
	}
}
