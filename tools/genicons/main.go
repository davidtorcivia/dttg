// Command genicons renders the "loupe" mark (a magnifier ring + handle on a dark
// field) as PNGs: the PWA app icons and the Firefox extension icons. Drawing is
// supersampled and box-downsampled for clean anti-aliased edges at every size.
//
// Run from the repo root: go run ./tools/genicons
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

const ss = 4 // supersampling factor (render at sizexss, then average down)

func main() {
	pwa := filepath.Join("internal", "web", "static", "icons")
	ext := filepath.Join("extension", "icons")
	mustDir(pwa)
	mustDir(ext)

	// PWA app icons.
	must(writeIcon(filepath.Join(pwa, "icon-192.png"), 192, 0.62))
	must(writeIcon(filepath.Join(pwa, "icon-512.png"), 512, 0.62))
	// maskable: keep the mark inside the safe zone (more padding).
	must(writeIcon(filepath.Join(pwa, "icon-maskable-512.png"), 512, 0.46))

	// Firefox extension icons: toolbar (16/32), menus (48), listing/store (96/128).
	for _, sz := range []int{16, 32, 48, 96, 128} {
		must(writeIcon(filepath.Join(ext, fmt.Sprintf("icon-%d.png", sz)), sz, 0.62))
	}
}

// writeIcon renders the loupe mark at the given size with ringFrac controlling
// the ring diameter as a fraction of the icon.
func writeIcon(path string, size int, ringFrac float64) error {
	big := size * ss
	img := image.NewRGBA(image.Rect(0, 0, big, big))
	bg := color.RGBA{0x11, 0x11, 0x11, 0xff}
	fg := color.RGBA{0xff, 0xff, 0xff, 0xff}

	s := float64(big)
	// Bias the center up-left so the handle (extending down-right) stays in frame.
	cx, cy := s*0.44, s*0.44
	R := s * ringFrac / 2
	W := s * 0.06 // stroke width
	hx0 := cx + R*math.Cos(math.Pi/4)
	hy0 := cy + R*math.Sin(math.Pi/4)
	hx1 := hx0 + s*0.18
	hy1 := hy0 + s*0.18

	for y := 0; y < big; y++ {
		for x := 0; x < big; x++ {
			img.Set(x, y, bg)
			fx, fy := float64(x)+0.5, float64(y)+0.5
			onRing := math.Abs(math.Hypot(fx-cx, fy-cy)-R) <= W/2
			onHandle := distToSeg(fx, fy, hx0, hy0, hx1, hy1) <= W/2
			if onRing || onHandle {
				img.Set(x, y, fg)
			}
		}
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, downscale(img, size))
}

// downscale box-averages each ss×ss block into one output pixel (anti-aliasing).
func downscale(src *image.RGBA, size int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	n := ss * ss
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a int
			for dy := 0; dy < ss; dy++ {
				for dx := 0; dx < ss; dx++ {
					c := src.RGBAAt(x*ss+dx, y*ss+dy)
					r += int(c.R)
					g += int(c.G)
					b += int(c.B)
					a += int(c.A)
				}
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(b / n), uint8(a / n)})
		}
	}
	return dst
}

func distToSeg(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return math.Hypot(px-ax, py-ay)
	}
	t := ((px-ax)*dx + (py-ay)*dy) / l2
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}

func mustDir(p string) {
	if err := os.MkdirAll(p, 0o755); err != nil {
		panic(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
