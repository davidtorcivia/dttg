package web

import (
	"bytes"
	_ "embed"
	"fmt"
	"hash/fnv"
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

// Outfit is the site UI typeface; these static instances (decompressed from the
// shipped woff2) render the default site share card on-brand. Embedded so the
// binary stays self-contained.
//
//go:embed ogfonts/outfit-700.ttf
var outfit700TTF []byte

//go:embed ogfonts/outfit-500.ttf
var outfit500TTF []byte

var ogOutfit700, ogOutfit500 *opentype.Font

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
	ogOutfit700, _ = opentype.Parse(outfit700TTF)
	ogOutfit500, _ = opentype.Parse(outfit500TTF)
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
	site := strings.ToUpper(s.siteTitle())
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

// ---------- default (site-wide) OG share card ----------

const (
	ogFeatW    = 680 // featured-cover width; the 2x3 grid fills the remainder
	ogSiteTake = 7   // 1 featured + 6 grid tiles
)

var (
	ogInk    = color.RGBA{0x1a, 0x17, 0x14, 0xff}
	ogAccent = color.RGBA{0xc7, 0x9a, 0x00, 0xff}
	ogWhite  = color.RGBA{0xff, 0xff, 0xff, 0xff}
)

// siteOGCache is a single-entry cache for the default share card, keyed by a
// fingerprint of the items (and branding) it was built from — so posting/editing
// an item, or renaming the site, naturally invalidates it.
type siteOGCache struct {
	mu   sync.Mutex
	fp   string
	data []byte
}

// handleSiteOGImage renders the 1200x630 default share card used by every
// non-item page (board, category, tag, search): a featured cover + a 2x3 grid of
// recent public covers, branded with the (admin-editable) title and URL.
func (s *Server) handleSiteOGImage(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListItems(r.Context(), store.ItemFilter{Limit: 36}) // public, newest-first
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	picked := make([]store.Item, 0, ogSiteTake)
	for _, it := range items {
		if ogTileKey(it) != "" {
			picked = append(picked, it)
			if len(picked) == ogSiteTake {
				break
			}
		}
	}

	fp := siteOGFingerprint(s.siteTitle(), s.siteHost(), picked)
	etag := `"siteog-` + fp + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	s.siteOG.mu.Lock()
	data := s.siteOG.data
	hit := s.siteOG.fp == fp && data != nil
	s.siteOG.mu.Unlock()

	if !hit {
		imgs := make([]image.Image, 0, len(picked))
		for i, it := range picked {
			key := ogTileKey(it)
			if i == 0 {
				key = ogFeaturedKey(it) // the featured panel wants the larger variant
			}
			if img := s.ogDecode(key); img != nil {
				imgs = append(imgs, img)
			}
		}
		var buf bytes.Buffer
		if err := imaging.Encode(&buf, s.renderSiteOGCard(s.siteTitle(), s.siteHost(), imgs), imaging.JPEG, imaging.JPEGQuality(88)); err != nil {
			s.serverError(w, r, err)
			return
		}
		data = buf.Bytes()
		s.siteOG.mu.Lock()
		s.siteOG.fp, s.siteOG.data = fp, data
		s.siteOG.mu.Unlock()
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("ETag", etag)
	_, _ = w.Write(data)
}

// ogDecode opens a media key and decodes it, returning nil on any failure (a
// missing tile just leaves that cell empty rather than failing the whole card).
func (s *Server) ogDecode(key string) image.Image {
	if key == "" {
		return nil
	}
	rc, err := s.media.Open(key)
	if err != nil {
		return nil
	}
	defer rc.Close()
	img, err := imaging.Decode(rc, imaging.AutoOrientation(true))
	if err != nil {
		return nil
	}
	return img
}

// ogTileKey / ogFeaturedKey pick the best available variant for grid vs featured.
func ogTileKey(it store.Item) string {
	for _, k := range []string{it.SmallKey, it.ThumbKey, it.CoverKey} {
		if k != "" {
			return k
		}
	}
	return ""
}

func ogFeaturedKey(it store.Item) string {
	for _, k := range []string{it.CoverKey, it.SmallKey, it.ThumbKey} {
		if k != "" {
			return k
		}
	}
	return ""
}

func siteOGFingerprint(title, host string, items []store.Item) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s|%s|", title, host)
	for _, it := range items {
		_, _ = fmt.Fprintf(h, "%d:%d,", it.ID, it.UpdatedAt.Unix())
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// renderSiteOGCard draws the P3 layout: featured cover on the left, a 2x3 grid of
// recent covers on the right, a gold seam between, the URL as a top-left kicker,
// and the title anchored bottom-left — all in Outfit. imgs[0] is the featured
// cover; imgs[1:] feed the grid (cycled if fewer than six). With no images it
// falls back to a centered title card so the endpoint never 404s.
func (s *Server) renderSiteOGCard(title, host string, imgs []image.Image) image.Image {
	canvas := image.NewRGBA(image.Rect(0, 0, ogW, ogH))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{ogInk}, image.Point{}, draw.Src)

	if len(imgs) == 0 {
		titleFace := ogFace(ogOutfit700, 72)
		lines := wrapText(titleFace, title, ogW-220)
		if len(lines) > 3 {
			lines = lines[:3]
		}
		ty := (ogH-len(lines)*84)/2 + 60
		for _, ln := range lines {
			drawString(canvas, titleFace, ln, marginXCentered(titleFace, ln), ty, ogWhite)
			ty += 84
		}
		drawTrackedCentered(canvas, ogFace(ogOutfit500, 24), strings.ToUpper(host), ogH-72, ogWhite, 3)
		return canvas
	}

	// featured cover (left)
	feat := imaging.AdjustContrast(imaging.Fill(imgs[0], ogFeatW, ogH, imaging.Center, imaging.Lanczos), 8)
	draw.Draw(canvas, image.Rect(0, 0, ogFeatW, ogH), feat, image.Point{}, draw.Src)

	// 2x3 grid (right), cycling the remaining covers so every cell is filled
	ogSideGrid(canvas, imgs, 2, 3, 5)

	// gold seam between featured and grid
	draw.Draw(canvas, image.Rect(ogFeatW-2, 0, ogFeatW, ogH), &image.Uniform{ogAccent}, image.Point{}, draw.Src)

	// legibility scrims confined to the featured (text) area
	ogScrim(canvas, ogFeatW, 0, 150, 150, true)    // top, for the kicker
	ogScrim(canvas, ogFeatW, 300, ogH, 235, false) // bottom, for the title

	// URL kicker, top-left
	drawTracked(canvas, ogFace(ogOutfit500, 21), strings.ToUpper(host), 56, 66, ogWhite, 3)

	// title, bottom-left
	titleFace := ogFace(ogOutfit700, 58)
	lines := wrapText(titleFace, title, ogFeatW-112)
	if len(lines) > 3 {
		lines = lines[:3]
	}
	const lh = 62
	ty := (ogH - 58) - (len(lines)-1)*lh
	for _, ln := range lines {
		drawString(canvas, titleFace, ln, 56, ty, ogWhite)
		ty += lh
	}
	return canvas
}

// ogSideGrid fills the area right of ogFeatW with a cols x rows grid, cycling imgs
// (starting at the second image so the featured cover isn't immediately repeated).
func ogSideGrid(canvas *image.RGBA, imgs []image.Image, cols, rows, gutter int) {
	sw := ogW - ogFeatW
	idx := 1
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			cx0 := ogFeatW + c*sw/cols
			cx1 := ogFeatW + (c+1)*sw/cols
			cy0 := r * ogH / rows
			cy1 := (r + 1) * ogH / rows
			tw := cx1 - cx0 - gutter
			th := cy1 - cy0 - gutter
			if tw < 1 || th < 1 || len(imgs) == 0 {
				continue
			}
			tile := imaging.AdjustContrast(imaging.Fill(imgs[idx%len(imgs)], tw, th, imaging.Center, imaging.Lanczos), 6)
			idx++
			draw.Draw(canvas, image.Rect(cx0+gutter, cy0+gutter, cx0+gutter+tw, cy0+gutter+th), tile, image.Point{}, draw.Src)
		}
	}
}

// ogScrim paints a vertical black gradient over x in [0,w). For top=true the
// gradient is darkest at y0 and fades down; otherwise it darkens toward y1.
func ogScrim(c *image.RGBA, w, y0, y1 int, peak uint8, top bool) {
	h := y1 - y0
	if h <= 0 {
		return
	}
	for y := 0; y < h; y++ {
		frac := float64(y) / float64(h)
		if top {
			frac = 1 - frac
		}
		a := uint8(frac * float64(peak))
		for x := 0; x < w; x++ {
			blendOver(c, x, y0+y, color.RGBA{0, 0, 0, a})
		}
	}
}

// marginXCentered returns the x for horizontally centering s in the full canvas.
func marginXCentered(face font.Face, s string) int {
	return (ogW - font.MeasureString(face, s).Ceil()) / 2
}

// drawTrackedCentered centers a letter-tracked string horizontally at baseline y.
func drawTrackedCentered(dst draw.Image, face font.Face, s string, y int, col color.Color, tracking int) {
	w := font.MeasureString(face, s).Ceil() + tracking*(len([]rune(s))-1)
	drawTracked(dst, face, s, (ogW-w)/2, y, col, tracking)
}
