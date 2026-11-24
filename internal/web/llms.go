package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"donottouchtheglass/internal/store"
)

// wantsMarkdown reports whether the client explicitly requested text/markdown
// (without preferring text/html) via Accept content negotiation.
func wantsMarkdown(r *http.Request) bool {
	acc := strings.ToLower(r.Header.Get("Accept"))
	if acc == "" {
		return false
	}
	return strings.Contains(acc, "text/markdown") && !strings.Contains(acc, "text/html")
}

// handleLLMsTxt serves /llms.txt — a concise, structured manifest describing
// the archive and every machine-readable / LLM-native endpoint.
func (s *Server) handleLLMsTxt(w http.ResponseWriter, r *http.Request) {
	if s.feedCached(w, r) {
		return
	}
	ov, err := s.queryOverview(r.Context(), false)
	if err != nil {
		http.Error(w, "could not build llms.txt", http.StatusInternalServerError)
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", ov.SiteTitle)
	if ov.Description != "" {
		fmt.Fprintf(&b, "> %s\n\n", ov.Description)
	}
	fmt.Fprintf(&b, "A curated visual & archival collection of %d public plates (%d multi-image series).\n", ov.TotalItems, ov.MultiImageCount)
	if ov.OldestItem != "" && ov.NewestItem != "" {
		fmt.Fprintf(&b, "Archive span: %s to %s.\n", ov.OldestItem[:10], ov.NewestItem[:10])
	}
	b.WriteString("\n## Model Context Protocol (MCP)\n\n")
	fmt.Fprintf(&b, "- **Endpoint**: `%s` (Streamable HTTP, JSON-RPC 2.0, MCP 2025-03-26 / 2024-11-05)\n", ov.Endpoints["mcp"])
	fmt.Fprintf(&b, "- **Discovery**: `%s`\n", s.absURL("/.well-known/mcp.json"))
	b.WriteString("- **Public Read + Vision Tools**: `archive_overview`, `browse_archive`, `search_archive`, `get_item` (supports `include_images: true` for vision models), `curatorial_walk` (associative visual/thematic drift), `color_palette` (perceptual OKLab ΔE matching), `serendipity`\n")
	b.WriteString("- **Authenticated Write/Curate Tools** (`Authorization: Bearer <token>`): `archive_item`, `curate_item`, `list_unannotated`\n")
	b.WriteString("- **Resources**: `dnttg://archive/overview`, `dnttg://archive/recent`, `dnttg://archive/palette`, `dnttg://item/{id}`, `dnttg://tag/{slug}`, `dnttg://color/{bucket}`\n")
	b.WriteString("- **Prompts**: `curate-exhibition`, `visual-essay`, `chromatic-study`, `describe-plates`\n")

	b.WriteString("\n## Markdown & Web-Agent Views\n\n")
	fmt.Fprintf(&b, "- [Full Archive Catalog (Markdown)](%s): Compact token-efficient index of every public item with metadata, palettes, and image URLs.\n", ov.Endpoints["llms_full_txt"])
	fmt.Fprintf(&b, "- Item Markdown: `%s` (or send `Accept: text/markdown` to any `/item/{id}` or `/` URL)\n", ov.Endpoints["item_markdown"])
	fmt.Fprintf(&b, "- [JSON Feed 1.1](%s) · [RSS 2.0](%s)\n", ov.Endpoints["feed_json"], ov.Endpoints["feed_rss"])

	b.WriteString("\n## Public JSON Discovery API\n\n")
	fmt.Fprintf(&b, "- `GET %s` — Archive vitals, taxonomy, chromatic distribution\n", ov.Endpoints["items"][:len(ov.Endpoints["items"])-len("/items")]+"/overview")
	fmt.Fprintf(&b, "- `GET %s` — Filter by `?q=`, `?kind=`, `?category=`, `?tag=`, `?color=`, `?hex=#rrggbb`, `?multi=1`, `?from=YYYY-MM-DD`, `?to=YYYY-MM-DD`, `?limit=`, `?offset=`\n", ov.Endpoints["items"])
	fmt.Fprintf(&b, "- `GET %s` — Full plate dossier, related items, and chromatic OKLab neighbors\n", ov.Endpoints["item_detail"])
	fmt.Fprintf(&b, "- `GET %s` — Serendipitous draw (`?count=1..12`, optional filters)\n", ov.Endpoints["random"])
	fmt.Fprintf(&b, "- `GET %s` — Curatorial associative walk (`?start={id}&steps=5&mode=balanced|color|thematic`)\n", ov.Endpoints["walk"])
	fmt.Fprintf(&b, "- `GET %s` — Chromatic spectrum or nearest-color search (`?hex=#rrggbb&max_delta=0.22`)\n", ov.Endpoints["palette"])
	fmt.Fprintf(&b, "- `GET %s` — Public categories and tags with counts\n", ov.Endpoints["taxonomy"])

	if len(ov.Categories) > 0 {
		b.WriteString("\n## Categories\n\n")
		for _, c := range ov.Categories {
			fmt.Fprintf(&b, "- [%s](%s) (`%s`, %d items)\n", c.Name, s.absURL("/category/"+c.Slug), c.Slug, c.Count)
		}
	}
	if len(ov.Palette) > 0 {
		b.WriteString("\n## Chromatic Buckets (OKLCH)\n\n")
		for _, p := range ov.Palette {
			if p.Count > 0 {
				fmt.Fprintf(&b, "- `%s` (%s, %s): %d items — %s\n", p.Slug, p.Label, p.Hex, p.Count, s.absURL("/?color="+p.Slug))
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// handleLLMsFullTxt serves /llms-full.txt — a compact, token-efficient Markdown
// catalog of the entire public archive.
func (s *Server) handleLLMsFullTxt(w http.ResponseWriter, r *http.Request) {
	if s.feedCached(w, r) {
		return
	}
	items, err := s.store.ListItems(r.Context(), store.ItemFilter{IncludeTags: true})
	if err != nil {
		http.Error(w, "could not build llms-full.txt", http.StatusInternalServerError)
		return
	}
	id := s.siteID()
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — Full Public Archive Catalog\n\n", id.Title)
	if desc := s.metaDescription(); desc != "" {
		fmt.Fprintf(&b, "> %s\n\n", desc)
	}
	fmt.Fprintf(&b, "Total public items: %d · Base URL: %s · MCP: %s\n\n---\n\n", len(items), s.siteBaseURL(), s.absURL("/mcp"))
	for _, it := range items {
		ai := s.toAPIItem(it, it.ImageCount > 1)
		title := firstNonEmpty(ai.Title, ai.FileName, "Plate "+fmt.Sprintf("%03d", ai.ID))
		fmt.Fprintf(&b, "## [%03d] %s\n", ai.ID, title)
		meta := []string{
			"URL: " + ai.URL,
			"Markdown: " + ai.MarkdownURL,
			"Kind: " + ai.Kind,
			"Date: " + it.CreatedAt.Format("2006-01-02"),
		}
		if ai.ImageCount > 1 {
			meta = append(meta, fmt.Sprintf("Series: %d plates", ai.ImageCount))
		}
		if ai.Category != "" {
			meta = append(meta, "Category: "+ai.Category)
		}
		if len(ai.Tags) > 0 {
			meta = append(meta, "Tags: "+strings.Join(ai.Tags, ", "))
		}
		if ai.DominantColor != "" {
			palStr := ai.DominantColor
			if len(ai.Palette) > 1 {
				palStr = strings.Join(ai.Palette, ", ")
			}
			meta = append(meta, fmt.Sprintf("Palette: %s (%s)", palStr, ai.ColorLabel))
		}
		if ai.SourceURL != "" {
			meta = append(meta, "Source: "+ai.SourceURL)
		}
		b.WriteString("- " + strings.Join(meta, " | ") + "\n")
		if ai.CoverURL != "" {
			fmt.Fprintf(&b, "- Image: %s\n", ai.CoverURL)
		}
		if ai.Note != "" {
			fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(ai.Note))
		}
		if ai.Quote != "" {
			fmt.Fprintf(&b, "\n> %s\n", strings.ReplaceAll(strings.TrimSpace(ai.Quote), "\n", "\n> "))
		}
		if len(ai.Plates) > 1 {
			b.WriteString("\nPlates:\n")
			for _, pl := range ai.Plates {
				line := fmt.Sprintf("  %d. %s", pl.Position+1, pl.URL)
				if pl.Width > 0 && pl.Height > 0 {
					line += fmt.Sprintf(" (%dx%d)", pl.Width, pl.Height)
				}
				if pl.DominantColor != "" {
					line += " " + pl.DominantColor
				}
				if pl.Alt != "" {
					line += " — " + pl.Alt
				}
				b.WriteString(line + "\n")
			}
		}
		b.WriteString("\n")
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// handleItemMarkdown renders a single item as clean YAML-frontmatter Markdown
// at /item/{id}.md (and via Accept: text/markdown on /item/{id}).
func (s *Server) handleItemMarkdown(w http.ResponseWriter, r *http.Request) {
	rawID := strings.TrimSuffix(r.PathValue("id"), ".md")
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_, authed := s.optionalTokenAuth(r)
	includePriv := authed || s.isAdmin(r)
	detail, _, err := s.queryItemDetail(r.Context(), id, includePriv)
	if err != nil {
		http.Error(w, "could not load item", http.StatusInternalServerError)
		return
	}
	if detail == nil {
		http.NotFound(w, r)
		return
	}
	if includePriv {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=120")
	}
	w.Header().Set("Vary", "Accept")
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(renderItemMarkdown(*detail)))
}

func renderItemMarkdown(d apiItemDetail) string {
	ai := d.Item
	title := firstNonEmpty(ai.Title, ai.FileName, fmt.Sprintf("Plate %03d", ai.ID))
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %d\n", ai.ID)
	fmt.Fprintf(&b, "title: %q\n", title)
	fmt.Fprintf(&b, "url: %q\n", ai.URL)
	fmt.Fprintf(&b, "kind: %s\n", ai.Kind)
	fmt.Fprintf(&b, "date: %s\n", ai.CreatedAt)
	fmt.Fprintf(&b, "visibility: %s\n", ai.Visibility)
	if ai.Category != "" {
		fmt.Fprintf(&b, "category: %q\n", ai.Category)
	}
	if len(ai.Tags) > 0 {
		quoted := make([]string, len(ai.Tags))
		for i, t := range ai.Tags {
			quoted[i] = strconv.Quote(t)
		}
		fmt.Fprintf(&b, "tags: [%s]\n", strings.Join(quoted, ", "))
	}
	if ai.DominantColor != "" {
		fmt.Fprintf(&b, "dominant_color: %q\n", ai.DominantColor)
		fmt.Fprintf(&b, "color_bucket: %s\n", ai.ColorBucket)
	}
	if len(ai.Palette) > 0 {
		quoted := make([]string, len(ai.Palette))
		for i, c := range ai.Palette {
			quoted[i] = strconv.Quote(c)
		}
		fmt.Fprintf(&b, "palette: [%s]\n", strings.Join(quoted, ", "))
	}
	fmt.Fprintf(&b, "image_count: %d\n", ai.ImageCount)
	if ai.SourceURL != "" {
		fmt.Fprintf(&b, "source_url: %q\n", ai.SourceURL)
	}
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", title)
	if ai.Note != "" {
		b.WriteString(strings.TrimSpace(ai.Note) + "\n\n")
	}
	if ai.Quote != "" {
		b.WriteString("> " + strings.ReplaceAll(strings.TrimSpace(ai.Quote), "\n", "\n> ") + "\n\n")
	}
	if len(ai.Plates) > 0 {
		b.WriteString("## Plates\n\n")
		for _, pl := range ai.Plates {
			alt := pl.Alt
			if alt == "" {
				alt = fmt.Sprintf("%s — plate %d", title, pl.Position+1)
			}
			fmt.Fprintf(&b, "### Plate %02d\n", pl.Position+1)
			fmt.Fprintf(&b, "![%s](%s)\n\n", strings.ReplaceAll(alt, "]", "\\]"), pl.URL)
			var details []string
			if pl.Width > 0 && pl.Height > 0 {
				details = append(details, fmt.Sprintf("%dx%d", pl.Width, pl.Height))
			}
			if pl.DominantColor != "" {
				details = append(details, "dominant "+pl.DominantColor)
			}
			if len(pl.Palette) > 1 {
				details = append(details, "palette "+strings.Join(pl.Palette, ", "))
			}
			if len(details) > 0 {
				b.WriteString("- " + strings.Join(details, " · ") + "\n")
			}
			if pl.Alt != "" {
				fmt.Fprintf(&b, "- Alt: %s\n", pl.Alt)
			}
			b.WriteString("\n")
		}
	} else if ai.FileURL != "" {
		fmt.Fprintf(&b, "- File: [%s](%s) (%s, %s)\n\n", firstNonEmpty(ai.FileName, "Download"), ai.FileURL, ai.FileMime, humanSize(ai.FileSize))
	}
	if len(d.Related) > 0 {
		b.WriteString("## Related\n\n")
		for _, r := range d.Related {
			fmt.Fprintf(&b, "- [%03d %s](%s) ([md](%s))\n", r.ID, firstNonEmpty(r.Title, "Untitled"), r.URL, r.MarkdownURL)
		}
		b.WriteString("\n")
	}
	if len(d.ChromaticNeighbors) > 0 {
		b.WriteString("## Chromatic Neighbors (OKLab)\n\n")
		for _, n := range d.ChromaticNeighbors {
			delta := ""
			if n.ColorDelta != nil {
				delta = fmt.Sprintf(" — %s (ΔE %.2f)", n.MatchedHex, *n.ColorDelta)
			}
			fmt.Fprintf(&b, "- [%03d %s](%s)%s\n", n.ID, firstNonEmpty(n.Title, "Untitled"), n.URL, delta)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// handleWellKnownMCP serves /.well-known/mcp.json for automated MCP discovery.
func (s *Server) handleWellKnownMCP(w http.ResponseWriter, r *http.Request) {
	if s.feedCached(w, r) {
		return
	}
	id := s.siteID()
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        id.Title,
		"description": s.metaDescription(),
		"mcp": map[string]any{
			"endpoint":         s.absURL("/mcp"),
			"transport":        "streamable-http",
			"protocolVersions": []string{"2025-03-26", "2024-11-05"},
			"auth": map[string]any{
				"public_read":  true,
				"bearer_write": true,
				"header":       "Authorization: Bearer <api-token>",
			},
		},
		"llms_txt":      s.absURL("/llms.txt"),
		"llms_full_txt": s.absURL("/llms-full.txt"),
		"api_base":      s.absURL("/api/v1"),
	})
}
