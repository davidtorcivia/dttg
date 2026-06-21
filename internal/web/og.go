package web

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"donottouchtheglass/internal/store"

	"github.com/disintegration/imaging"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	ogW = 1200
	ogH = 630
)

// ogCache is a tiny FIFO cache of rendered OG cards so a shared/viral link doesn't
// re-render (DB read + decode + draw + JPEG encode) on every request. Validated by
// the item's updated-at, so an edit invalidates the entry.
type ogCacheEntry struct {
	updated int64
	data    []byte
}

type ogCache struct {
	mu    sync.Mutex
	max   int
	m     map[int64]ogCacheEntry
	order []int64
}

func newOGCache(max int) *ogCache { return &ogCache{max: max, m: map[int64]ogCacheEntry{}} }

func (c *ogCache) get(id, updated int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[id]; ok && e.updated == updated {
		return e.data, true
	}
	return nil, false
}

func (c *ogCache) put(id, updated int64, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[id]; !exists {
		c.order = append(c.order, id)
		if len(c.order) > c.max {
			delete(c.m, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.m[id] = ogCacheEntry{updated: updated, data: data}
}

var ogBoldFont, ogRegFont *opentype.Font

func init() {
	ogBoldFont, _ = opentype.Parse(gobold.TTF)
	ogRegFont, _ = opentype.Parse(goregular.TTF)
}

func ogFace(f *opentype.Font, size float64) font.Face {
	fc, _ := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	return fc
}

// handleOGImage renders a 1200x630 social share card for an item on the fly.
func (s *Server) handleOGImage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	it, err := s.store.GetItem(r.Context(), id, false) // public only
	if err != nil || it == nil {
		http.NotFound(w, r)
		return
	}
	updated := it.UpdatedAt.Unix()
	etag := fmt.Sprintf(`"og-%d-%d"`, id, updated)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	var data []byte
	if s.ogCache != nil {
		data, _ = s.ogCache.get(id, updated)
	}
	if data == nil {
		var cover image.Image
		if it.CoverKey != "" {
			if rc, e := s.media.Open(it.CoverKey); e == nil {
				cover, _ = imaging.Decode(rc, imaging.AutoOrientation(true))
				_ = rc.Close()
			}
		}
		var buf bytes.Buffer
		if err := imaging.Encode(&buf, s.renderOGCard(*it, cover), imaging.JPEG, imaging.JPEGQuality(88)); err != nil {
			s.serverError(w, r, err)
			return
		}
		data = buf.Bytes()
		if s.ogCache != nil {
			s.ogCache.put(id, updated, data)
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("ETag", etag)
	_, _ = w.Write(data)
}

func (s *Server) renderOGCard(it store.Item, cover image.Image) image.Image {
	canvas := image.NewRGBA(image.Rect(0, 0, ogW, ogH))
	paper := color.RGBA{0xff, 0xff, 0xff, 0xff}
	ink := color.RGBA{0x11, 0x11, 0x11, 0xff}
	marginX := 72
	site := strings.ToUpper(s.cfg.SiteTitle)
	meta := ogMeta(it)
	title := it.Title
	if title == "" {
		if it.FileName != "" {
			title = it.FileName
		} else {
			title = "Untitled"
		}
	}

	if cover != nil {
		filled := imaging.AdjustContrast(imaging.Fill(cover, ogW, ogH, imaging.Center, imaging.Lanczos), 8)
		draw.Draw(canvas, canvas.Bounds(), filled, image.Point{}, draw.Src)

		// bottom scrim for legibility
		scrimTop := 230
		scrim := image.NewRGBA(image.Rect(0, 0, ogW, ogH-scrimTop))
		h := scrim.Bounds().Dy()
		for y := 0; y < h; y++ {
			row := color.RGBA{0, 0, 0, uint8(float64(y) / float64(h) * 220)}
			for x := 0; x < ogW; x++ {
				scrim.SetRGBA(x, y, row)
			}
		}
		draw.Draw(canvas, image.Rect(0, scrimTop, ogW, ogH), scrim, image.Point{}, draw.Over)

		drawTracked(canvas, ogFace(ogRegFont, 22), site, marginX, 66, paper, 3)
		titleFace := ogFace(ogBoldFont, 66)
		lines := wrapText(titleFace, title, ogW-2*marginX)
		if len(lines) > 3 {
			lines = lines[:3]
		}
		ty := ogH - 72
		for i := len(lines) - 1; i >= 0; i-- {
			drawString(canvas, titleFace, lines[i], marginX, ty, paper)
			ty -= 76
		}
		drawTracked(canvas, ogFace(ogRegFont, 25), strings.ToUpper(meta), marginX, ty-8, paper, 2)
	} else {
		draw.Draw(canvas, canvas.Bounds(), &image.Uniform{ink}, image.Point{}, draw.Src)
		drawTracked(canvas, ogFace(ogRegFont, 22), site, marginX, 72, paper, 3)
		titleFace := ogFace(ogBoldFont, 72)
		lines := wrapText(titleFace, title, ogW-2*marginX)
		if len(lines) > 4 {
			lines = lines[:4]
		}
		ty := (ogH-len(lines)*84)/2 + 66
		for _, ln := range lines {
			drawString(canvas, titleFace, ln, marginX, ty, paper)
			ty += 84
		}
		drawTracked(canvas, ogFace(ogRegFont, 25), strings.ToUpper(meta), marginX, ogH-64, color.RGBA{0xaa, 0xaa, 0xaa, 0xff}, 2)
	}

	drawFrame(canvas, 26, color.RGBA{255, 255, 255, 90})
	return canvas
}

func ogMeta(it store.Item) string {
	parts := []string{}
	if it.CategoryName != "" {
		parts = append(parts, it.CategoryName)
	}
	parts = append(parts, it.CreatedAt.Format("Jan 02, 2006"))
	return strings.Join(parts, "   ·   ")
}

func wrapText(face font.Face, text string, maxW int) []string {
	var lines []string
	cur := ""
	for _, word := range strings.Fields(text) {
		try := word
		if cur != "" {
			try = cur + " " + word
		}
		if font.MeasureString(face, try).Ceil() <= maxW {
			cur = try
		} else {
			if cur != "" {
				lines = append(lines, cur)
			}
			cur = word
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) == 0 {
		lines = []string{text}
	}
	return lines
}

func drawString(dst draw.Image, face font.Face, s string, x, y int, col color.Color) {
	(&font.Drawer{Dst: dst, Src: image.NewUniform(col), Face: face, Dot: fixed.P(x, y)}).DrawString(s)
}

func drawTracked(dst draw.Image, face font.Face, s string, x, y int, col color.Color, tracking int) {
	d := &font.Drawer{Dst: dst, Src: image.NewUniform(col), Face: face, Dot: fixed.P(x, y)}
	for _, r := range s {
		d.DrawString(string(r))
		d.Dot.X += fixed.I(tracking)
	}
}

func drawFrame(dst *image.RGBA, inset int, col color.RGBA) {
	b := dst.Bounds()
	for t := 0; t < 2; t++ {
		for x := inset; x < b.Dx()-inset; x++ {
			blendOver(dst, x, inset+t, col)
			blendOver(dst, x, b.Dy()-inset-1-t, col)
		}
		for y := inset; y < b.Dy()-inset; y++ {
			blendOver(dst, inset+t, y, col)
			blendOver(dst, b.Dx()-inset-1-t, y, col)
		}
	}
}

func blendOver(dst *image.RGBA, x, y int, c color.RGBA) {
	bg := dst.RGBAAt(x, y)
	a := float64(c.A) / 255
	dst.SetRGBA(x, y, color.RGBA{
		uint8(float64(c.R)*a + float64(bg.R)*(1-a)),
		uint8(float64(c.G)*a + float64(bg.G)*(1-a)),
		uint8(float64(c.B)*a + float64(bg.B)*(1-a)),
		255,
	})
}
