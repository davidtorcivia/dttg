package store

import (
	"math"
	"testing"
)

func TestOKLabRoundTrip(t *testing.T) {
	samples := []string{"#000000", "#ffffff", "#b94a3e", "#4a6c8c", "#5f7d5b", "#c9a849", "#765d85", "#f5f3ec", "#2d2f24"}
	for _, hex := range samples {
		r, g, b, ok := ParseHex(hex)
		if !ok {
			t.Fatalf("ParseHex(%q) failed", hex)
		}
		lab := RGBToOKLab(r, g, b)
		r2, g2, b2 := OKLabToRGB(lab)
		if math.Abs(float64(int(r)-int(r2))) > 1 || math.Abs(float64(int(g)-int(g2))) > 1 || math.Abs(float64(int(b)-int(b2))) > 1 {
			t.Errorf("round-trip mismatch for %s: got (%d,%d,%d), want (%d,%d,%d)", hex, r2, g2, b2, r, g, b)
		}
	}
}

func TestOKLCHColorBuckets(t *testing.T) {
	cases := []struct {
		hex  string
		want string
	}{
		// Real edge cases that HSL misclassified previously:
		{"#2d2f24", "dark"},    // near-black charcoal (not green/orange!)
		{"#332b22", "dark"},    // dark espresso charcoal (HSL called this orange!)
		{"#695f4e", "neutral"}, // muted warm stone (HSL called this orange!)
		{"#c0b8ca", "light"},   // pale lavender-paper (L=0.795 C=0.027; HSL called this violet!)
		{"#f5f3ec", "light"},   // warm archival paper
		{"#e6dbd3", "light"},   // pale cream paper
		{"#858585", "neutral"}, // mid stone gray
		// Genuine chromatic pigments:
		{"#a12721", "red"},    // Shigeo Fukuda crimson
		{"#c97a3d", "orange"}, // ochre
		{"#d9b44a", "yellow"}, // gold
		{"#336b24", "green"},  // laser moss green
		{"#4a6c8c", "blue"},   // cobalt
		{"#61c1cd", "blue"},   // cerulean cyan
		{"#765d85", "violet"}, // plum
	}
	for _, tc := range cases {
		if got := ColorBucket(tc.hex); got != tc.want {
			lab, _ := HexToOKLab(tc.hex)
			t.Errorf("ColorBucket(%s) = %q, want %q (L=%.3f C=%.3f H=%.1f)", tc.hex, got, tc.want, lab.L, lab.Chroma(), lab.Hue())
		}
	}
}

func TestAggregatePalettesMultiImage(t *testing.T) {
	plate1 := []string{"#2a5c9e", "#dcd6c8", "#1e2228"} // cobalt + paper + obsidian
	plate2 := []string{"#b8382c", "#e0dad0", "#222020"} // crimson + paper + obsidian
	dom, pal := AggregatePalettes([][]string{plate1, plate2}, 5)
	if dom == "" || len(pal) < 3 {
		t.Fatalf("unexpected aggregated palette: dom=%q pal=%v", dom, pal)
	}
	csv := PaletteToCSV(pal)
	if !ItemMatchesBucket(dom, csv, "blue") {
		t.Errorf("expected multi-image series palette %v to match blue bucket", pal)
	}
	if !ItemMatchesBucket(dom, csv, "red") {
		t.Errorf("expected multi-image series palette %v to match red bucket", pal)
	}
	// NearestColorMatch should find crimson with tiny perceptual ΔE even if cobalt leads.
	target, _ := HexToOKLab("#b8382c")
	dist, matched := NearestColorMatch(target, dom, csv)
	if dist > 0.05 {
		t.Errorf("expected close OKLab match to crimson, got dist=%.4f matched=%s pal=%v", dist, matched, pal)
	}
}
