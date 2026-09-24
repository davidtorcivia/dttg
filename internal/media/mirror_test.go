package media

import "testing"

func TestVariantClassification(t *testing.T) {
	imageVariants := map[string]bool{
		"items/abc/full.jpg":     true,
		"items/abc/thumb.jpg":    true,
		"items/abc/small.jpg":    true,
		"items/abc/original.png": false,
		"items/abc/file.pdf":     false,
		"items/abc/clip.mp4":     false,
	}
	for k, want := range imageVariants {
		if got := imageVariant(k); got != want {
			t.Errorf("imageVariant(%q) = %v, want %v", k, got, want)
		}
	}
	if mirrorable("items/x/original.png") {
		t.Error("originals must not be mirrorable")
	}
	if !mirrorable("items/x/full.jpg") || !mirrorable("items/x/clip.mp4") {
		t.Error("non-original media should be mirrorable")
	}
}

func TestPlacement(t *testing.T) {
	m := &MirrorStore{}
	for _, c := range []struct {
		key           string
		private       bool
		local, remote bool
	}{
		{"items/a/full.jpg", false, false, true}, // refined variant: R2 only
		{"items/a/full.jpg", true, true, false},  // private: never on the public bucket
		{"items/a/original.png", false, true, false},
		{"items/a/video.mp4", false, true, true},
		{"items/a/file.pdf", true, true, false},
	} {
		if l, r := m.Placement(c.key, c.private); l != c.local || r != c.remote {
			t.Errorf("Placement(%q, private=%v) = %v,%v want %v,%v", c.key, c.private, l, r, c.local, c.remote)
		}
	}
}
