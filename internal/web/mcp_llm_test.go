package web

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"donottouchtheglass/internal/store"
)

func makeTestJPEG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 80, 60))
	for y := 0; y < 60; y++ {
		for x := 0; x < 80; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func seedLLMArchive(t *testing.T, s *Server) (pubMultiID, pubRedID, privID int64, apiTok string) {
	t.Helper()
	ctx := context.Background()

	// Create real stored JPEG blobs for vision image tests.
	blueBytes := makeTestJPEG(t, color.RGBA{R: 42, G: 98, B: 175, A: 255})
	goldBytes := makeTestJPEG(t, color.RGBA{R: 212, G: 168, B: 62, A: 255})
	redBytes := makeTestJPEG(t, color.RGBA{R: 185, G: 52, B: 44, A: 255})

	_ = s.media.Put(ctx, "items/m1/full.jpg", "image/jpeg", blueBytes, false)
	_ = s.media.Put(ctx, "items/m1/small.jpg", "image/jpeg", blueBytes, false)
	_ = s.media.Put(ctx, "items/m2/full.jpg", "image/jpeg", goldBytes, false)
	_ = s.media.Put(ctx, "items/m2/small.jpg", "image/jpeg", goldBytes, false)
	_ = s.media.Put(ctx, "items/r1/full.jpg", "image/jpeg", redBytes, false)
	_ = s.media.Put(ctx, "items/r1/small.jpg", "image/jpeg", redBytes, false)

	catID, err := s.store.GetOrCreateCategory(ctx, "Architecture")
	if err != nil {
		t.Fatal(err)
	}

	pubMultiID, err = s.store.CreateItemWithMediaAndTags(ctx, store.Item{
		Kind:          "image",
		Title:         "Aegean Cobalt Series",
		Note:          "Two architectural plates studying cobalt pigment and gilded stone.",
		Visibility:    "public",
		CategoryID:    catID,
		CoverKey:      "items/m1/full.jpg",
		ThumbKey:      "items/m1/small.jpg",
		SmallKey:      "items/m1/small.jpg",
		DominantColor: "#2a62af",
		Palette:       "#2a62af,#d4a83e,#f5f3ec",
		Width:         80,
		Height:        60,
		Images: []store.ItemImage{
			{Position: 0, CoverKey: "items/m1/full.jpg", ThumbKey: "items/m1/small.jpg", SmallKey: "items/m1/small.jpg", DominantColor: "#2a62af", Palette: "#2a62af,#f5f3ec", Width: 80, Height: 60, Alt: "Cobalt dome"},
			{Position: 1, CoverKey: "items/m2/full.jpg", ThumbKey: "items/m2/small.jpg", SmallKey: "items/m2/small.jpg", DominantColor: "#d4a83e", Palette: "#d4a83e,#2b2722", Width: 80, Height: 60, Alt: ""},
		},
	}, nil, []string{"brutalism", "pigment"})
	if err != nil {
		t.Fatal(err)
	}

	pubRedID, err = s.store.CreateItemWithMediaAndTags(ctx, store.Item{
		Kind:          "image",
		Title:         "Crimson Poster",
		Note:          "Shigeo Fukuda exhibition poster in deep vermilion.",
		Visibility:    "public",
		CategoryID:    catID,
		CoverKey:      "items/r1/full.jpg",
		ThumbKey:      "items/r1/small.jpg",
		SmallKey:      "items/r1/small.jpg",
		DominantColor: "#b9342c",
		Palette:       "#b9342c,#f5f3ec",
		Width:         80,
		Height:        60,
	}, nil, []string{"poster", "pigment"})
	if err != nil {
		t.Fatal(err)
	}

	privID, err = s.store.CreateItemWithMediaAndTags(ctx, store.Item{
		Kind:       "text",
		Title:      "Private Curatorial Draft",
		Note:       "SECRET_DRAFT_ONLY_FOR_OWNER",
		Visibility: "private",
	}, nil, []string{"internal"})
	if err != nil {
		t.Fatal(err)
	}

	apiTok = "test-mcp-bearer-token-123"
	if _, err := s.store.CreateToken(ctx, "mcp-test", HashToken(apiTok)); err != nil {
		t.Fatal(err)
	}
	return pubMultiID, pubRedID, privID, apiTok
}

func TestLLMsEndpointsAndMarkdownNegotiation(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	pubMultiID, _, privID, _ := seedLLMArchive(t, s)

	// /llms.txt
	rec := getReq(t, h, "/llms.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("/llms.txt status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"/mcp", "/api/v1/items", "/llms-full.txt", "curatorial_walk"} {
		if !strings.Contains(body, want) {
			t.Errorf("/llms.txt missing %q", want)
		}
	}

	// /llms-full.txt excludes private items and includes multi-image plates
	rec = getReq(t, h, "/llms-full.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("/llms-full.txt status = %d", rec.Code)
	}
	full := rec.Body.String()
	if !strings.Contains(full, "Aegean Cobalt Series") || !strings.Contains(full, "Series: 2 plates") {
		t.Errorf("/llms-full.txt missing public series item")
	}
	if strings.Contains(full, "SECRET_DRAFT_ONLY_FOR_OWNER") {
		t.Errorf("/llms-full.txt leaked private item!")
	}

	// /.well-known/mcp.json
	rec = getReq(t, h, "/.well-known/mcp.json")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "streamable-http") {
		t.Fatalf("/.well-known/mcp.json failed: %d %s", rec.Code, rec.Body.String())
	}

	// /item/{id}.md returns clean YAML-frontmatter Markdown
	rec = getReq(t, h, "/item/"+strconv.FormatInt(pubMultiID, 10)+".md")
	if rec.Code != http.StatusOK {
		t.Fatalf("/item/id.md status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
		t.Errorf("Content-Type = %q, want text/markdown", ct)
	}
	md := rec.Body.String()
	for _, want := range []string{"---", "dominant_color:", "palette:", "## Plates", "Cobalt dome"} {
		if !strings.Contains(md, want) {
			t.Errorf("/item/id.md missing %q\n%s", want, md)
		}
	}

	// Private item .md returns 404 anonymously
	rec = getReq(t, h, "/item/"+strconv.FormatInt(privID, 10)+".md")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("private item .md = %d, want 404", rec.Code)
	}

	// Accept: text/markdown negotiation on /item/{id} and /
	req := httptest.NewRequest(http.MethodGet, "/item/"+strconv.FormatInt(pubMultiID, 10), nil)
	req.Header.Set("Accept", "text/markdown")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/markdown") {
		t.Errorf("Accept: text/markdown on /item/{id} failed: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/markdown")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "## Model Context Protocol") {
		t.Errorf("Accept: text/markdown on / failed: %d", rec.Code)
	}
}

func TestPublicDiscoveryAPIV1(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	pubMultiID, pubRedID, _, _ := seedLLMArchive(t, s)

	// Overview
	rec := getReq(t, h, "/api/v1/overview")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d", rec.Code)
	}
	var ov apiOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if ov.TotalItems != 2 || ov.MultiImageCount != 1 {
		t.Errorf("overview counts = total:%d multi:%d, want 2 & 1", ov.TotalItems, ov.MultiImageCount)
	}

	// Hex perceptual color search matches multi-image series secondary gold plate!
	rec = getReq(t, h, "/api/v1/palette?hex=%23d4a83e")
	if rec.Code != http.StatusOK {
		t.Fatalf("palette hex = %d", rec.Code)
	}
	var palResp struct {
		Count int       `json:"count"`
		Items []apiItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &palResp); err != nil {
		t.Fatal(err)
	}
	if palResp.Count == 0 || palResp.Items[0].ID != pubMultiID {
		t.Fatalf("expected multi-image series (id=%d) to match gold plate hex, got %+v", pubMultiID, palResp.Items)
	}

	// Item detail includes plates + related
	rec = getReq(t, h, "/api/v1/items/"+strconv.FormatInt(pubMultiID, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("item detail = %d", rec.Code)
	}
	var det apiItemDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &det); err != nil {
		t.Fatal(err)
	}
	if len(det.Item.Plates) != 2 || len(det.Related) == 0 || det.Related[0].ID != pubRedID {
		t.Errorf("unexpected detail: plates=%d related=%+v", len(det.Item.Plates), det.Related)
	}

	// Curatorial walk
	rec = getReq(t, h, "/api/v1/walk?start="+strconv.FormatInt(pubMultiID, 10)+"&steps=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("walk = %d", rec.Code)
	}
	var walkResp struct {
		Count int           `json:"count"`
		Steps []apiWalkStep `json:"steps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &walkResp); err != nil {
		t.Fatal(err)
	}
	if walkResp.Count != 2 || walkResp.Steps[0].Item.ID != pubMultiID || walkResp.Steps[1].Item.ID != pubRedID {
		t.Errorf("unexpected walk: %+v", walkResp)
	}
}

func postMCP(t *testing.T, h http.Handler, payload any, token string) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMCPStreamableHTTPFlow(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	pubMultiID, _, privID, apiTok := seedLLMArchive(t, s)

	// 1. Initialize
	rec := postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-03-26"},
	}, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Mcp-Session-Id") == "" {
		t.Fatalf("initialize failed: %d session=%q body=%s", rec.Code, rec.Header().Get("Mcp-Session-Id"), rec.Body.String())
	}

	// 2. Notification returns 202 Accepted with empty body
	rec = postMCP(t, h, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", rec.Code)
	}

	// 3. Anonymous tools/list has 7 public read tools (no write tools)
	rec = postMCP(t, h, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, "")
	var listResp struct {
		Result struct {
			Tools []mcpToolDef `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Result.Tools) != 7 {
		t.Fatalf("anonymous tools count = %d, want 7", len(listResp.Result.Tools))
	}

	// 4. get_item with include_images=true returns text JSON + 2 base64 image blocks!
	rec = postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_item",
			"arguments": map[string]any{"id": pubMultiID, "include_images": true},
		},
	}, "")
	var callResp struct {
		Result struct {
			Content []mcpContent `json:"content"`
			IsError bool         `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &callResp); err != nil {
		t.Fatal(err)
	}
	if callResp.Result.IsError {
		t.Fatalf("get_item returned error: %+v", callResp.Result.Content)
	}
	imgBlocks := 0
	for _, c := range callResp.Result.Content {
		if c.Type == "image" && c.Data != "" && c.MimeType == "image/jpeg" {
			imgBlocks++
		}
	}
	if imgBlocks != 2 {
		t.Errorf("expected 2 inline vision image blocks for multi-image series, got %d", imgBlocks)
	}

	// 5. Anonymous get_item cannot read private item
	rec = postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params":  map[string]any{"name": "get_item", "arguments": map[string]any{"id": privID}},
	}, "")
	if strings.Contains(rec.Body.String(), "SECRET_DRAFT_ONLY_FOR_OWNER") {
		t.Fatalf("anonymous MCP leaked private item!")
	}

	// 6. Authenticated tools/list unlocks 10 tools (including archive_item, curate_item, list_unannotated)
	rec = postMCP(t, h, map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/list"}, apiTok)
	listResp.Result.Tools = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Result.Tools) != 10 {
		t.Fatalf("authenticated tools count = %d, want 10", len(listResp.Result.Tools))
	}

	// 7. Authenticated curate_item updates plate alt text & appends tags
	rec = postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      6,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "curate_item",
			"arguments": map[string]any{
				"id":       pubMultiID,
				"add_tags": []string{"mediterranean"},
				"plate_alts": []map[string]any{
					{"position": 1, "alt": "Gilded stone relief in warm afternoon light"},
				},
			},
		},
	}, apiTok)
	callResp.Result.Content = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &callResp); err != nil {
		t.Fatal(err)
	}
	if callResp.Result.IsError || len(callResp.Result.Content) == 0 {
		t.Fatalf("curate_item returned error: %s", rec.Body.String())
	}
	curatedText := callResp.Result.Content[0].Text
	if !strings.Contains(curatedText, `"updated_plates": 1`) || !strings.Contains(curatedText, "mediterranean") {
		t.Fatalf("curate_item unexpected text: %s", curatedText)
	}

	// 8. Authenticated archive_item creates a note item
	rec = postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "archive_item",
			"arguments": map[string]any{"title": "Field Note", "note": "Saved directly via MCP tool", "tags": []string{"mcp"}},
		},
	}, apiTok)
	callResp.Result.Content = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &callResp); err != nil {
		t.Fatal(err)
	}
	if callResp.Result.IsError || len(callResp.Result.Content) == 0 || !strings.Contains(callResp.Result.Content[0].Text, `"markdown_url"`) {
		t.Fatalf("archive_item failed: %s", rec.Body.String())
	}

	// 9. Resources & Prompts
	rec = postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      8,
		"method":  "resources/read",
		"params":  map[string]any{"uri": "dnttg://item/" + strconv.FormatInt(pubMultiID, 10)},
	}, "")
	if !strings.Contains(rec.Body.String(), "Gilded stone relief in warm afternoon light") {
		t.Fatalf("resources/read missing curated alt text: %s", rec.Body.String())
	}

	rec = postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      9,
		"method":  "prompts/get",
		"params":  map[string]any{"name": "curate-exhibition", "arguments": map[string]string{"theme": "cobalt pigment"}},
	}, "")
	if !strings.Contains(rec.Body.String(), "cobalt pigment") {
		t.Fatalf("prompts/get failed: %s", rec.Body.String())
	}
}
