// Command genicons renders the PWA app icons (a minimal "loupe" mark) as PNGs.
// Run from the repo root: go run ./tools/genicons
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

func main() {
	out := filepath.Join("internal", "web", "static", "icons")
	if err := os.MkdirAll(out, 0o755); err != nil {
		panic(err)
	}
	must(write(filepath.Join(out, "icon-192.png"), 192, 0.62))
	must(write(filepath.Join(out, "icon-512.png"), 512, 0.62))
	// maskable: keep the mark inside the safe zone (more padding)
	must(write(filepath.Join(out, "icon-maskable-512.png"), 512, 0.46))
}

func write(path string, size int, ringFrac float64) error {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{0x11, 0x11, 0x11, 0xff}
	fg := color.RGBA{0xff, 0xff, 0xff, 0xff}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, bg)
		}
	}
	s := float64(size)
	cx, cy := s*0.44, s*0.44
	R := s * ringFrac / 2
	W := s * 0.05
	hx0 := cx + R*math.Cos(math.Pi/4)
	hy0 := cy + R*math.Sin(math.Pi/4)
	hx1 := hx0 + s*0.16
	hy1 := hy0 + s*0.16
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x), float64(y)
			d := math.Hypot(fx-cx, fy-cy)
			if math.Abs(d-R) <= W/2 || distToSeg(fx, fy, hx0, hy0, hx1, hy1) <= W/2 {
				img.Set(x, y, fg)
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
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

func must(err error) {
	if err != nil {
		panic(err)
	}
}
