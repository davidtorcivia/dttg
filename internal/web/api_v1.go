package web

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"donottouchtheglass/internal/store"
)

// ---------- shared structured DTOs for /api/v1 and /mcp ----------

type apiPlate struct {
	ID            int64    `json:"id,omitempty"`
	Position      int      `json:"position"`
	URL           string   `json:"url"`
	ThumbURL      string   `json:"thumb_url,omitempty"`
	SmallURL      string   `json:"small_url,omitempty"`
	Width         int      `json:"width,omitempty"`
	Height        int      `json:"height,omitempty"`
	DominantColor string   `json:"dominant_color,omitempty"`
	ColorBucket   string   `json:"color_bucket,omitempty"`
	Palette       []string `json:"palette,omitempty"`
	Alt           string   `json:"alt,omitempty"`
}

type apiItem struct {
	ID            int64      `json:"id"`
	URL           string     `json:"url"`
	MarkdownURL   string     `json:"markdown_url"`
	Kind          string     `json:"kind"`
	Title         string     `json:"title,omitempty"`
	Note          string     `json:"note,omitempty"`
	Quote         string     `json:"quote,omitempty"`
	SourceURL     string     `json:"source_url,omitempty"`
	SiteName      string     `json:"site_name,omitempty"`
	Visibility    string     `json:"visibility"`
	Category      string     `json:"category,omitempty"`
	CategorySlug  string     `json:"category_slug,omitempty"`
	Tags          []string   `json:"tags,omitempty"`
	DominantColor string     `json:"dominant_color,omitempty"`
	ColorBucket   string     `json:"color_bucket,omitempty"`
	ColorLabel    string     `json:"color_label,omitempty"`
	Palette       []string   `json:"palette,omitempty"`
	Width         int        `json:"width,omitempty"`
	Height        int        `json:"height,omitempty"`
	ImageCount    int        `json:"image_count"`
	CoverURL      string     `json:"cover_url,omitempty"`
	ThumbURL      string     `json:"thumb_url,omitempty"`
	FileURL       string     `json:"file_url,omitempty"`
	FileName      string     `json:"file_name,omitempty"`
	FileMime      string     `json:"file_mime,omitempty"`
	FileSize      int64      `json:"file_size,omitempty"`
	Plates        []apiPlate `json:"plates,omitempty"`
	CreatedAt     string     `json:"created_at"`
	UpdatedAt     string     `json:"updated_at"`
	// Optional match metadata populated by color-proximity queries.
	ColorDelta   *float64 `json:"color_delta_oklab,omitempty"`
	MatchedHex   string   `json:"matched_hex,omitempty"`
}

type apiItemDetail struct {
	Item              apiItem   `json:"item"`
	Related           []apiItem `json:"related,omitempty"`
	ChromaticNeighbors []apiItem `json:"chromatic_neighbors,omitempty"`
	PrevID            int64     `json:"prev_id,omitempty"`
	NextID            int64     `json:"next_id,omitempty"`
}

type apiWalkStep struct {
	Step   int     `json:"step"`
	Reason string  `json:"reason"`
	Item   apiItem `json:"item"`
}

type apiPaletteBucket struct {
	Slug    string   `json:"slug"`
	Label   string   `json:"label"`
	Hex     string   `json:"hex"`
	Count   int      `json:"count"`
	Samples []string `json:"samples,omitempty"`
}

type apiTaxonomyEntry struct {
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Count int    `json:"count"`
}

type apiOverview struct {
	SiteTitle      string             `json:"site_title"`
	Tagline        string             `json:"tagline,omitempty"`
	Description    string             `json:"description,omitempty"`
	BaseURL        string             `json:"base_url"`
	TotalItems     int                `json:"total_items"`
	MultiImageCount int               `json:"multi_image_items"`
	OldestItem     string             `json:"oldest_item,omitempty"`
	NewestItem     string             `json:"newest_item,omitempty"`
	Kinds          map[string]int     `json:"kinds"`
	Categories     []apiTaxonomyEntry `json:"categories"`
	TopTags        []apiTaxonomyEntry `json:"top_tags"`
	Palette        []apiPaletteBucket `json:"palette"`
	Endpoints      map[string]string  `json:"endpoints"`
}

// optionalTokenAuth checks whether the request carries a valid bearer token
// without rejecting anonymous callers. Returns (tokenHash, isAuthed).
func (s *Server) optionalTokenAuth(r *http.Request) (string, bool) {
	tok := bearerToken(r)
	if tok == "" {
		return "", false
	}
	hash := HashToken(tok)
	if ok, err := s.store.TokenValid(r.Context(), hash); err == nil && ok {
		return hash, true
	}
	return "", false
}

// toAPIItem converts a store.Item into a rich absolute-URL API representation.
func (s *Server) toAPIItem(it store.Item, includePlates bool) apiItem {
	v := s.view(it)
	swatches := store.ItemPalette(it.DominantColor, it.Palette)
	dom := it.DominantColor
	if dom == "" && len(swatches) > 0 {
		dom = swatches[0]
	}
	bucket := store.ColorBucket(dom)
	out := apiItem{
		ID:            it.ID,
		URL:           s.absURL(v.DetailURL),
		MarkdownURL:   s.absURL(v.DetailURL + ".md"),
		Kind:          it.Kind,
		Title:         it.Title,
		Note:          it.Note,
		Quote:         it.LinkDescription,
		SourceURL:     it.SourceURL,
		SiteName:      it.LinkSiteName,
		Visibility:    it.Visibility,
		Category:      it.CategoryName,
		CategorySlug:  it.CategorySlug,
		DominantColor: dom,
		ColorBucket:   bucket,
		ColorLabel:    store.BucketLabel(bucket),
		Palette:       swatches,
		Width:         it.Width,
		Height:        it.Height,
		ImageCount:    v.GalleryCount(),
		CoverURL:      s.absURL(v.CoverURL),
		ThumbURL:      s.absURL(v.ThumbURL),
		FileURL:       s.absURL(v.FileURL),
		FileName:      it.FileName,
		FileMime:      it.FileMime,
		FileSize:      it.FileSize,
		CreatedAt:     it.CreatedAt.Format(time.RFC3339),
		UpdatedAt:     it.UpdatedAt.Format(time.RFC3339),
	}
	if len(it.Tags) > 0 {
		out.Tags = make([]string, len(it.Tags))
		for i, t := range it.Tags {
			out.Tags[i] = t.Name
		}
	}
	if includePlates && len(v.Images) > 0 {
		out.Plates = make([]apiPlate, 0, len(v.Images))
		for i, img := range v.Images {
			var platePal []string
			if i < len(it.Images) {
				platePal = store.ItemPalette(it.Images[i].DominantColor, it.Images[i].Palette)
			} else if img.DominantColor != "" {
				platePal = []string{img.DominantColor}
			}
			out.Plates = append(out.Plates, apiPlate{
				ID:            img.ID,
				Position:      img.Position,
				URL:           s.absURL(img.CoverURL),
				ThumbURL:      s.absURL(img.ThumbURL),
				SmallURL:      s.absURL(img.SmallURL),
				Width:         img.Width,
				Height:        img.Height,
				DominantColor: img.DominantColor,
				ColorBucket:   store.ColorBucket(img.DominantColor),
				Palette:       platePal,
				Alt:           img.Alt,
			})
		}
	}
	return out
}

// itemQueryParams holds parsed filter parameters shared by /api/v1/items and MCP.
type itemQueryParams struct {
	Query          string
	Kind           string
	Category       string
	Tag            string
	Color          string  // editorial bucket slug
	Hex            string  // perceptual OKLab nearest-color lookup
	MaxDelta       float64 // optional max OKLab ΔE threshold (default 0.24 for hex search)
	MultiOnly      bool
	From           string // YYYY-MM-DD
	To             string // YYYY-MM-DD
	Limit          int
	Offset         int
	IncludePrivate bool
	IncludePlates  bool
}

func parseDateBound(s string, endOfDay bool) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339, s); err2 == nil {
			return t2.Unix()
		}
		return 0
	}
	if endOfDay {
		t = t.Add(24*time.Hour - time.Second)
	}
	return t.Unix()
}

func (s *Server) queryItems(ctx context.Context, p itemQueryParams) ([]apiItem, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 24
	}
	if limit > 100 {
		limit = 100
	}
	offset := p.Offset
	if offset < 0 {
		offset = 0
	}

	colorBucket := strings.ToLower(strings.TrimSpace(p.Color))
	if colorBucket != "" && !store.ValidColorBucket(colorBucket) {
		colorBucket = ""
	}
	targetHex := store.NormalizeHex(p.Hex)
	var targetLab store.OKLab
	hasHex := false
	if targetHex != "" {
		targetLab, hasHex = store.HexToOKLab(targetHex)
	}

	f := store.ItemFilter{
		IncludePrivate: p.IncludePrivate,
		IncludeTags:    true,
		CategorySlug:   store.Slugify(p.Category),
		TagSlug:        store.Slugify(p.Tag),
		Kind:           strings.ToLower(strings.TrimSpace(p.Kind)),
		Query:          strings.TrimSpace(p.Query),
		Color:          colorBucket,
		MultiOnly:      p.MultiOnly,
		CreatedFrom:    parseDateBound(p.From, false),
		CreatedTo:      parseDateBound(p.To, true),
		Limit:          limit,
		Offset:         offset,
	}
	if hasHex {
		// Scan matching set in Go to rank by perceptual OKLab ΔE across all palettes.
		f.Limit = 0
		f.Offset = 0
	}
	items, err := s.store.ListItems(ctx, f)
	if err != nil {
		return nil, err
	}

	if hasHex {
		maxDelta := p.MaxDelta
		if maxDelta <= 0 {
			maxDelta = 0.24
		}
		type scored struct {
			item  store.Item
			delta float64
			hex   string
		}
		var matches []scored
		for _, it := range items {
			var platePals []string
			for _, img := range it.Images {
				if img.Palette != "" {
					platePals = append(platePals, img.Palette)
				} else if img.DominantColor != "" {
					platePals = append(platePals, img.DominantColor)
				}
			}
			d, matched := store.NearestColorMatch(targetLab, it.DominantColor, it.Palette, platePals...)
			if !math.IsInf(d, 1) && d <= maxDelta {
				matches = append(matches, scored{item: it, delta: d, hex: matched})
			}
		}
		sort.Slice(matches, func(i, j int) bool {
			if math.Abs(matches[i].delta-matches[j].delta) < 1e-6 {
				return matches[i].item.ID > matches[j].item.ID
			}
			return matches[i].delta < matches[j].delta
		})
		if offset >= len(matches) {
			return []apiItem{}, nil
		}
		end := offset + limit
		if end > len(matches) {
			end = len(matches)
		}
		out := make([]apiItem, 0, end-offset)
		for _, m := range matches[offset:end] {
			ai := s.toAPIItem(m.item, p.IncludePlates || m.item.ImageCount > 1)
			d := math.Round(m.delta*1000) / 1000
			ai.ColorDelta = &d
			ai.MatchedHex = m.hex
			out = append(out, ai)
		}
		return out, nil
	}

	out := make([]apiItem, 0, len(items))
	for _, it := range items {
		out = append(out, s.toAPIItem(it, p.IncludePlates || it.ImageCount > 1))
	}
	return out, nil
}

// queryItemDetail fetches a full curatorial dossier for one item: all plates,
// explicit related items (shared tags/category), and chromatic OKLab neighbors.
func (s *Server) queryItemDetail(ctx context.Context, id int64, includePrivate bool) (*apiItemDetail, *store.Item, error) {
	it, err := s.store.GetItem(ctx, id, includePrivate)
	if err != nil || it == nil {
		return nil, nil, err
	}
	detail := &apiItemDetail{
		Item: s.toAPIItem(*it, true),
	}
	if prevID, nextID, err := s.store.GetAdjacent(ctx, *it, includePrivate); err == nil {
		detail.PrevID, detail.NextID = prevID, nextID
	}
	if rel, err := s.store.GetRelated(ctx, *it, 6, includePrivate); err == nil && len(rel) > 0 {
		_ = s.store.AttachTags(ctx, rel)
		detail.Related = make([]apiItem, 0, len(rel))
		for _, r := range rel {
			detail.Related = append(detail.Related, s.toAPIItem(r, false))
		}
	}
	// Compute chromatic neighbors by perceptual OKLab distance when the item has color data.
	if lab, ok := store.HexToOKLab(detail.Item.DominantColor); ok {
		pool, err := s.store.ListItems(ctx, store.ItemFilter{IncludePrivate: includePrivate, Cards: true, IncludeTags: true})
		if err == nil {
			type cand struct {
				item  store.Item
				delta float64
				hex   string
			}
			var cands []cand
			for _, other := range pool {
				if other.ID == it.ID || (other.DominantColor == "" && other.Palette == "") {
					continue
				}
				d, hex := store.NearestColorMatch(lab, other.DominantColor, other.Palette)
				if !math.IsInf(d, 1) && d <= 0.16 {
					cands = append(cands, cand{item: other, delta: d, hex: hex})
				}
			}
			sort.Slice(cands, func(i, j int) bool { return cands[i].delta < cands[j].delta })
			if len(cands) > 5 {
				cands = cands[:5]
			}
			for _, c := range cands {
				ai := s.toAPIItem(c.item, false)
				d := math.Round(c.delta*1000) / 1000
				ai.ColorDelta = &d
				ai.MatchedHex = c.hex
				detail.ChromaticNeighbors = append(detail.ChromaticNeighbors, ai)
			}
		}
	}
	return detail, it, nil
}

// queryRandomItems draws count unique random items matching optional filters.
func (s *Server) queryRandomItems(ctx context.Context, p itemQueryParams, count int) ([]apiItem, error) {
	if count <= 0 {
		count = 1
	}
	if count > 12 {
		count = 12
	}
	p.Limit = 0
	p.Offset = 0
	pool, err := s.queryItems(ctx, p)
	if err != nil {
		return nil, err
	}
	if len(pool) <= count {
		rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
		return pool, nil
	}
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	return pool[:count], nil
}

// queryCuratorialWalk takes an N-step associative walk through the archive,
// hopping from plate to plate via shared tags, categories, and OKLab color
// kinship while explaining each curatorial bridge.
func (s *Server) queryCuratorialWalk(ctx context.Context, startID int64, steps int, mode string, includePrivate bool) ([]apiWalkStep, error) {
	if steps < 2 {
		steps = 5
	}
	if steps > 10 {
		steps = 10
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != "color" && mode != "thematic" {
		mode = "balanced"
	}

	pool, err := s.store.ListItems(ctx, store.ItemFilter{IncludePrivate: includePrivate, IncludeTags: true})
	if err != nil || len(pool) == 0 {
		return nil, err
	}

	byID := make(map[int64]int, len(pool))
	tagSets := make([]map[string]string, len(pool))
	labs := make([]*store.OKLab, len(pool))
	for i, it := range pool {
		byID[it.ID] = i
		ts := make(map[string]string, len(it.Tags))
		for _, t := range it.Tags {
			ts[t.Slug] = t.Name
		}
		tagSets[i] = ts
		if lab, ok := store.HexToOKLab(it.DominantColor); ok {
			l := lab
			labs[i] = &l
		}
	}

	curIdx := -1
	if startID > 0 {
		if idx, ok := byID[startID]; ok {
			curIdx = idx
		}
	}
	if curIdx < 0 {
		curIdx = rand.IntN(len(pool))
	}

	visited := map[int]bool{curIdx: true}
	walk := []apiWalkStep{{
		Step:   1,
		Reason: "opening plate",
		Item:   s.toAPIItem(pool[curIdx], true),
	}}

	for step := 2; step <= steps && len(visited) < len(pool); step++ {
		cur := pool[curIdx]
		type candidate struct {
			idx    int
			score  float64
			reason string
		}
		var cands []candidate
		for j, other := range pool {
			if visited[j] {
				continue
			}
			var sc float64
			var reasons []string

			// Shared tags
			var sharedTags []string
			for slug, name := range tagSets[curIdx] {
				if _, ok := tagSets[j][slug]; ok {
					sharedTags = append(sharedTags, name)
				}
			}
			sort.Strings(sharedTags)
			if len(sharedTags) > 0 {
				w := 2.4
				if mode == "thematic" {
					w = 4.0
				} else if mode == "color" {
					w = 0.8
				}
				sc += w * float64(len(sharedTags))
				reasons = append(reasons, "shared tag: "+strings.Join(sharedTags, ", "))
			}

			// Shared category
			if cur.CategoryID != 0 && cur.CategoryID == other.CategoryID {
				w := 1.4
				if mode == "thematic" {
					w = 2.4
				} else if mode == "color" {
					w = 0.4
				}
				sc += w
				reasons = append(reasons, "same category: "+cur.CategoryName)
			}

			// Chromatic OKLab kinship across primary + series palettes
			if labs[curIdx] != nil && (other.DominantColor != "" || other.Palette != "") {
				d, matched := store.NearestColorMatch(*labs[curIdx], other.DominantColor, other.Palette)
				if !math.IsInf(d, 1) && d < 0.22 {
					w := 2.8
					if mode == "color" {
						w = 5.2
					} else if mode == "thematic" {
						w = 0.9
					}
					sc += w * (1.0 - d/0.22)
					if d < 0.14 {
						reasons = append(reasons, "chromatic kinship ("+matched+", ΔE "+strconv.FormatFloat(d, 'f', 2, 64)+")")
					}
				}
			}

			if sc > 0 {
				// Tiny jitter keeps repeated walks from the same plate fresh.
				sc += rand.Float64() * 0.45
				reason := "associative drift"
				if len(reasons) > 0 {
					reason = strings.Join(reasons, " · ")
				}
				cands = append(cands, candidate{idx: j, score: sc, reason: reason})
			}
		}

		var pick candidate
		if len(cands) > 0 {
			sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
			topK := 3
			if len(cands) < topK {
				topK = len(cands)
			}
			pick = cands[rand.IntN(topK)]
		} else {
			// Serendipitous jump to an unvisited plate when a cluster runs dry.
			var unvisited []int
			for j := range pool {
				if !visited[j] {
					unvisited = append(unvisited, j)
				}
			}
			pick = candidate{idx: unvisited[rand.IntN(len(unvisited))], reason: "serendipitous leap"}
		}
		visited[pick.idx] = true
		curIdx = pick.idx
		walk = append(walk, apiWalkStep{
			Step:   step,
			Reason: pick.reason,
			Item:   s.toAPIItem(pool[curIdx], true),
		})
	}
	return walk, nil
}

// queryPaletteSummary returns the archive's color distribution across OKLCH buckets.
func (s *Server) queryPaletteSummary(ctx context.Context, includePrivate bool) ([]apiPaletteBucket, error) {
	items, err := s.store.ListItems(ctx, store.ItemFilter{IncludePrivate: includePrivate, Cards: true})
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(store.PaletteSwatches))
	samples := make(map[string][]string, len(store.PaletteSwatches))
	seenHex := make(map[string]map[string]bool, len(store.PaletteSwatches))
	for _, sw := range store.PaletteSwatches {
		seenHex[sw.Slug] = map[string]bool{}
	}
	for _, it := range items {
		swatches := store.ItemPalette(it.DominantColor, it.Palette)
		if len(swatches) == 0 {
			continue
		}
		// Count each item in every bucket represented in its top 2 salient swatches
		// (or primary dominant color) so multi-image chromatic diversity is visible.
		matchedBuckets := map[string]bool{}
		limit := 2
		if len(swatches) < limit {
			limit = len(swatches)
		}
		for i := 0; i < limit; i++ {
			b := store.ColorBucket(swatches[i])
			if b == "" {
				continue
			}
			if !matchedBuckets[b] {
				matchedBuckets[b] = true
				counts[b]++
			}
			if len(samples[b]) < 6 && !seenHex[b][swatches[i]] {
				seenHex[b][swatches[i]] = true
				samples[b] = append(samples[b], swatches[i])
			}
		}
	}
	out := make([]apiPaletteBucket, 0, len(store.PaletteSwatches))
	for _, sw := range store.PaletteSwatches {
		out = append(out, apiPaletteBucket{
			Slug:    sw.Slug,
			Label:   sw.Label,
			Hex:     sw.Hex,
			Count:   counts[sw.Slug],
			Samples: samples[sw.Slug],
		})
	}
	return out, nil
}

func (s *Server) queryOverview(ctx context.Context, includePrivate bool) (apiOverview, error) {
	st, _ := s.store.PublicStats(ctx)
	total := st.Count
	if includePrivate {
		if n, err := s.store.CountItems(ctx, true); err == nil {
			total = n
		}
	}
	kinds, _ := s.store.KindCounts(ctx, includePrivate)
	cats, _ := s.store.ListCategories(ctx, includePrivate)
	tags, _ := s.store.ListTags(ctx, includePrivate)
	pal, _ := s.queryPaletteSummary(ctx, includePrivate)

	multiItems, _ := s.store.ListItems(ctx, store.ItemFilter{IncludePrivate: includePrivate, MultiOnly: true, Cards: true})

	catOut := make([]apiTaxonomyEntry, 0, len(cats))
	for _, c := range cats {
		if c.Count > 0 {
			catOut = append(catOut, apiTaxonomyEntry{Name: c.Name, Slug: c.Slug, Count: c.Count})
		}
	}
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].Count == tags[j].Count {
			return tags[i].Name < tags[j].Name
		}
		return tags[i].Count > tags[j].Count
	})
	tagOut := make([]apiTaxonomyEntry, 0, len(tags))
	for _, t := range tags {
		if t.Count > 0 {
			tagOut = append(tagOut, apiTaxonomyEntry{Name: t.Name, Slug: t.Slug, Count: t.Count})
		}
		if len(tagOut) >= 40 {
			break
		}
	}

	id := s.siteID()
	ov := apiOverview{
		SiteTitle:       id.Title,
		Tagline:         id.Tagline,
		Description:     s.metaDescription(),
		BaseURL:         s.siteBaseURL(),
		TotalItems:      total,
		MultiImageCount: len(multiItems),
		Kinds:           kinds,
		Categories:      catOut,
		TopTags:         tagOut,
		Palette:         pal,
		Endpoints: map[string]string{
			"mcp":           s.absURL("/mcp"),
			"llms_txt":      s.absURL("/llms.txt"),
			"llms_full_txt": s.absURL("/llms-full.txt"),
			"items":         s.absURL("/api/v1/items"),
			"item_detail":   s.absURL("/api/v1/items/{id}"),
			"item_markdown": s.absURL("/item/{id}.md"),
			"random":        s.absURL("/api/v1/random"),
			"walk":          s.absURL("/api/v1/walk"),
			"palette":       s.absURL("/api/v1/palette"),
			"taxonomy":      s.absURL("/api/v1/taxonomy"),
			"feed_json":     s.absURL("/feed.json"),
			"feed_rss":      s.absURL("/feed.xml"),
		},
	}
	if !st.Oldest.IsZero() {
		ov.OldestItem = st.Oldest.Format(time.RFC3339)
	}
	if !st.Newest.IsZero() {
		ov.NewestItem = st.Newest.Format(time.RFC3339)
	}
	return ov, nil
}

// ---------- /api/v1 HTTP handlers ----------

func (s *Server) handleAPIV1Overview(w http.ResponseWriter, r *http.Request) {
	_, authed := s.optionalTokenAuth(r)
	if !authed && s.feedCached(w, r) {
		return
	}
	if authed {
		w.Header().Set("Cache-Control", "no-store")
	}
	ov, err := s.queryOverview(r.Context(), authed)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "overview failed")
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

func (s *Server) handleAPIV1Items(w http.ResponseWriter, r *http.Request) {
	_, authed := s.optionalTokenAuth(r)
	if authed {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=60")
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	maxDelta, _ := strconv.ParseFloat(q.Get("max_delta"), 64)
	multi := q.Get("multi") == "1" || strings.EqualFold(q.Get("multi"), "true")
	includePlates := q.Get("plates") == "1" || strings.EqualFold(q.Get("plates"), "true")

	items, err := s.queryItems(r.Context(), itemQueryParams{
		Query:          q.Get("q"),
		Kind:           q.Get("kind"),
		Category:       q.Get("category"),
		Tag:            q.Get("tag"),
		Color:          q.Get("color"),
		Hex:            q.Get("hex"),
		MaxDelta:       maxDelta,
		MultiOnly:      multi,
		From:           q.Get("from"),
		To:             q.Get("to"),
		Limit:          limit,
		Offset:         offset,
		IncludePrivate: authed,
		IncludePlates:  includePlates,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query items failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(items),
		"items": items,
	})
}

func (s *Server) handleAPIV1Item(w http.ResponseWriter, r *http.Request) {
	_, authed := s.optionalTokenAuth(r)
	if authed {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=60")
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid item id")
		return
	}
	detail, _, err := s.queryItemDetail(r.Context(), id, authed)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "load item failed")
		return
	}
	if detail == nil {
		writeJSONError(w, http.StatusNotFound, "item not found")
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleAPIV1Random(w http.ResponseWriter, r *http.Request) {
	_, authed := s.optionalTokenAuth(r)
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	count, _ := strconv.Atoi(q.Get("count"))
	if count <= 0 {
		count = 1
	}
	multi := q.Get("multi") == "1" || strings.EqualFold(q.Get("multi"), "true")
	items, err := s.queryRandomItems(r.Context(), itemQueryParams{
		Kind:           q.Get("kind"),
		Category:       q.Get("category"),
		Tag:            q.Get("tag"),
		Color:          q.Get("color"),
		Hex:            q.Get("hex"),
		MultiOnly:      multi,
		IncludePrivate: authed,
		IncludePlates:  true,
	}, count)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "random query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(items),
		"items": items,
	})
}

func (s *Server) handleAPIV1Walk(w http.ResponseWriter, r *http.Request) {
	_, authed := s.optionalTokenAuth(r)
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	startID, _ := strconv.ParseInt(q.Get("start"), 10, 64)
	steps, _ := strconv.Atoi(q.Get("steps"))
	mode := q.Get("mode")
	walk, err := s.queryCuratorialWalk(r.Context(), startID, steps, mode, authed)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "walk failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":  firstNonEmpty(mode, "balanced"),
		"count": len(walk),
		"steps": walk,
	})
}

func (s *Server) handleAPIV1Palette(w http.ResponseWriter, r *http.Request) {
	_, authed := s.optionalTokenAuth(r)
	q := r.URL.Query()
	hex := store.NormalizeHex(q.Get("hex"))
	if hex != "" {
		if authed {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=60")
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		maxDelta, _ := strconv.ParseFloat(q.Get("max_delta"), 64)
		items, err := s.queryItems(r.Context(), itemQueryParams{
			Hex:            hex,
			MaxDelta:       maxDelta,
			Limit:          limit,
			IncludePrivate: authed,
		})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "palette match failed")
			return
		}
		bucket := store.ColorBucket(hex)
		writeJSON(w, http.StatusOK, map[string]any{
			"query_hex":   hex,
			"bucket":      bucket,
			"bucket_label": store.BucketLabel(bucket),
			"count":       len(items),
			"items":       items,
		})
		return
	}
	if !authed && s.feedCached(w, r) {
		return
	}
	if authed {
		w.Header().Set("Cache-Control", "no-store")
	}
	buckets, err := s.queryPaletteSummary(r.Context(), authed)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "palette summary failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": buckets})
}

func (s *Server) handleAPIV1Taxonomy(w http.ResponseWriter, r *http.Request) {
	_, authed := s.optionalTokenAuth(r)
	if !authed && s.feedCached(w, r) {
		return
	}
	if authed {
		w.Header().Set("Cache-Control", "no-store")
	}
	cats, _ := s.store.ListCategories(r.Context(), authed)
	tags, _ := s.store.ListTags(r.Context(), authed)
	catOut := make([]apiTaxonomyEntry, 0, len(cats))
	for _, c := range cats {
		if c.Count > 0 {
			catOut = append(catOut, apiTaxonomyEntry{Name: c.Name, Slug: c.Slug, Count: c.Count})
		}
	}
	tagOut := make([]apiTaxonomyEntry, 0, len(tags))
	for _, t := range tags {
		if t.Count > 0 {
			tagOut = append(tagOut, apiTaxonomyEntry{Name: t.Name, Slug: t.Slug, Count: t.Count})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"categories": catOut,
		"tags":       tagOut,
	})
}
