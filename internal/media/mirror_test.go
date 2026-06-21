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
