package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"donottouchtheglass/internal/ingest"
	"donottouchtheglass/internal/store"
)

// pic returns a stable, always-resolving image URL with a chosen aspect ratio
// (Lorem Picsum) — good for varied masonry heights in the demo board.
func pic(seed string, w, h int) string {
	return fmt.Sprintf("https://picsum.photos/seed/%s/%d/%d", seed, w, h)
}

// seed runs demo content through the real ingest pipeline the first time the
// archive is empty, so images are downloaded, refined, and self-hosted (they
// load even when the browser has no outbound internet). No-op once non-empty;
// run `dnttg reset-content` first to re-seed.
func seed(ctx context.Context, st *store.Store, svc *ingest.Service) error {
	n, err := st.CountItems(ctx, true)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	inputs := seedInputs()
	log.Printf("seeding %d items (downloading + refining media)…", len(inputs))

	const workers = 5
	jobs := make(chan ingest.Input)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, failed int
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for in := range jobs {
				if _, err := svc.Create(ctx, in); err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					log.Printf("seed: %q failed: %v", in.Title, err)
					continue
				}
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	for _, in := range inputs {
		jobs <- in
	}
	close(jobs)
	wg.Wait()
	log.Printf("seed complete: %d ok, %d failed", ok, failed)
	return nil
}

func seedInputs() []ingest.Input {
	const u = "https://images.unsplash.com/"
	const opt = "?q=80&w=1600&auto=format&fit=crop"

	type s struct {
		kind, title, category, note, url string
		tags                             []string
		daysAgo                          int
	}

	items := []s{
		// curated editorial imagery (known-good Unsplash IDs from the mockup)
		{"image", "Structural light study", "Architecture", "A breathtaking interplay of light and shadow, highlighting pure structural forms. The negative space speaks volumes.", u + "photo-1513694203232-719a280e022f" + opt, []string{"light", "concrete", "form"}, 1},
		{"image", "Cast figure", "Sculpture", "", u + "photo-1582555172866-f73bb12a2ab3" + opt, []string{"form", "monochrome"}, 3},
		{"image", "Grain & contrast", "Photography", "", u + "photo-1616423640778-28d1b53229bd" + opt, []string{"film", "texture"}, 6},
		{"image", "Editorial spread", "Graphic Design", "", u + "photo-1572949645841-094f3a9c4c94" + opt, []string{"layout", "typography"}, 9},
		{"image", "Quiet room", "Interior", "", u + "photo-1600210492486-724fe5c67fb0" + opt, []string{"space", "calm"}, 12},
		{"image", "Grid system", "Layout", "", u + "photo-1550684376-efcbd6e3f031" + opt, []string{"grid", "typography"}, 16},
		{"image", "Surface", "Texture", "", u + "photo-1518005020951-eccb494ad742" + opt, []string{"material"}, 20},
		{"image", "Composition no. 8", "Art", "", u + "photo-1541701494587-cb58502866ab" + opt, []string{"color", "abstract"}, 25},

		// volume + masonry rhythm via varied aspect ratios
		{"image", "Brutalist stair", "Architecture", "", pic("dnttg-arch-1", 820, 1180), []string{"concrete", "form"}, 28},
		{"image", "Facade rhythm", "Architecture", "", pic("dnttg-arch-2", 1200, 820), []string{"pattern", "grid"}, 30},
		{"image", "Long exposure", "Photography", "", pic("dnttg-photo-1", 900, 1200), []string{"motion", "night"}, 32},
		{"image", "Street geometry", "Photography", "", pic("dnttg-photo-2", 1180, 760), []string{"street", "geometry"}, 34},
		{"image", "Ridge line", "Nature", "", pic("dnttg-nat-1", 1000, 1000), []string{"landscape"}, 36},
		{"image", "Canopy", "Nature", "", pic("dnttg-nat-2", 800, 1180), []string{"green", "organic"}, 38},
		{"image", "Weathered steel", "Texture", "", pic("dnttg-tex-1", 1200, 800), []string{"material", "rust"}, 40},
		{"image", "Paper fold", "Texture", "", pic("dnttg-tex-2", 920, 920), []string{"paper", "fold"}, 42},
		{"image", "Stair well", "Interior", "", pic("dnttg-int-1", 820, 1140), []string{"space"}, 44},
		{"image", "Long table", "Interior", "", pic("dnttg-int-2", 1180, 820), []string{"furniture", "calm"}, 46},
		{"image", "Field study", "Color", "", pic("dnttg-col-1", 1000, 760), []string{"palette", "abstract"}, 48},
		{"image", "Gradient", "Color", "", pic("dnttg-col-2", 760, 1120), []string{"gradient"}, 50},
		{"image", "Specimen", "Type", "", pic("dnttg-typ-1", 1200, 880), []string{"typography", "specimen"}, 52},
		{"image", "Still life", "Objects", "", pic("dnttg-obj-1", 900, 1160), []string{"object", "studio"}, 54},
		{"image", "Tools", "Objects", "", pic("dnttg-obj-2", 1120, 820), []string{"object"}, 56},
		{"image", "Marble fold", "Sculpture", "", pic("dnttg-scu-1", 820, 1180), []string{"marble", "form"}, 58},
		{"image", "Untitled", "Art", "", pic("dnttg-art-1", 1000, 1240), []string{"abstract"}, 60},
		{"image", "Poster", "Layout", "", pic("dnttg-lay-1", 860, 1180), []string{"poster", "grid"}, 62},

		// other block kinds
		{"link", "Are.na — visual research", "Graphic Design", "The reference point for connected, slow collecting.", "https://www.are.na", []string{"reference", "tools"}, 5},
		{"link", "Fonts In Use", "Type", "", "https://fontsinuse.com", []string{"typography", "reference"}, 14},
		{"text", "On looking", "Art", "Do not touch the glass. The image is held at a distance so it can be seen whole — looking is the discipline, not possession.", "", []string{"note", "manifesto"}, 8},
		{"text", "Note on palette", "Color", "Keep the ground warm and quiet so the work can be loud. Off-white is not white.", "", []string{"note", "color"}, 18},
		{"embed", "Process film", "Photography", "", "https://www.youtube.com/watch?v=aqz-KE-bpKQ", []string{"video", "process"}, 22},
	}

	out := make([]ingest.Input, 0, len(items))
	for _, si := range items {
		out = append(out, ingest.Input{
			Kind:       si.kind,
			URL:        si.url,
			Title:      si.title,
			Note:       si.note,
			Category:   si.category,
			Tags:       si.tags,
			Visibility: "public",
			CreatedAt:  time.Now().AddDate(0, 0, -si.daysAgo),
		})
	}
	return out
}
