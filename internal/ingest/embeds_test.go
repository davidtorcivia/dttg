package ingest

import (
	"strings"
	"testing"
)

func TestSanitizeEmbedHTML(t *testing.T) {
	yt := sanitizeEmbedHTML(`<iframe width="200" height="113" src="https://www.youtube.com/embed/abc?feature=oembed" frameborder="0" allow="autoplay" allowfullscreen></iframe>`)
	if !strings.Contains(yt, `src="https://www.youtube.com/embed/abc?feature=oembed"`) {
		t.Errorf("youtube src dropped: %q", yt)
	}
	if strings.Contains(yt, "frameborder") || strings.Contains(yt, `width="200"`) {
		t.Errorf("extra attributes not stripped: %q", yt)
	}
	if v := sanitizeEmbedHTML(`<iframe src="https://player.vimeo.com/video/123" allowfullscreen></iframe>`); !strings.Contains(v, "player.vimeo.com/video/123") {
		t.Errorf("vimeo dropped: %q", v)
	}
	if got := sanitizeEmbedHTML(`<iframe src="https://evil.example/x"></iframe>`); got != "" {
		t.Errorf("non-allowlisted host allowed: %q", got)
	}
	if got := sanitizeEmbedHTML(`<script>alert(1)</script><iframe src="http://www.youtube.com/embed/x"></iframe>`); got != "" {
		t.Errorf("non-https src allowed: %q", got)
	}
	if got := sanitizeEmbedHTML(`<p>no iframe here</p>`); got != "" {
		t.Errorf("non-iframe content not rejected: %q", got)
	}
}
