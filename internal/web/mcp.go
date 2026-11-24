package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"donottouchtheglass/internal/ingest"
	"donottouchtheglass/internal/store"
)

// Native Streamable HTTP Model Context Protocol (MCP) server at /mcp.
// Implements JSON-RPC 2.0 over HTTP supporting the latest MCP 2026-07-28
// stateless core (server/discover, per-request protocol headers, structuredContent,
// cache hints) plus full backwards compatibility with 2025-11-25, 2025-06-18,
// 2025-03-26, and 2024-11-05 handshake clients. Anonymous clients receive full
// public read + vision inspection tools; valid bearer-token callers unlock
// private visibility plus archive_item, curate_item, and list_unannotated.

const (
	mcpProtocolLatest    = "2026-07-28"
	mcpHandshakeLatest   = "2025-11-25"
	mcpMaxBodyBytes      = 2 << 20 // 2 MB JSON-RPC request cap
	mcpMaxImageBytes     = 512 << 10
	mcpMaxInlineImgs     = 4
)

var mcpSupportedVersions = []string{
	"2026-07-28",
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

func mcpVersionSupported(v string) bool {
	for _, s := range mcpSupportedVersions {
		if s == v {
			return true
		}
	}
	return false
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type mcpContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// mcpCORS wraps /mcp so browser-based MCP inspectors and remote clients work cleanly.
func mcpCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-API-Token, Mcp-Session-Id, MCP-Protocol-Version, Mcp-Method, Mcp-Name")
		h.Set("Access-Control-Expose-Headers", "Mcp-Session-Id, MCP-Protocol-Version")
		h.Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		// If a human/crawler GETs /mcp with JSON or browser accept, return a helpful
		// descriptor; SSE stream requests get 405 per Streamable HTTP spec for
		// stateless servers.
		if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
			w.Header().Set("Allow", "POST, DELETE, OPTIONS")
			http.Error(w, "stateless MCP server does not open unsolicited SSE streams; send JSON-RPC via POST /mcp", http.StatusMethodNotAllowed)
			return
		}
		s.handleWellKnownMCP(w, r)
		return
	case http.MethodPost:
		// handled below
	default:
		w.Header().Set("Allow", "GET, POST, DELETE, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, mcpMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "could not read request body"},
		})
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		writeJSON(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "empty JSON-RPC payload"},
		})
		return
	}

	tokenHash, authed := s.optionalTokenAuth(r)

	// Support JSON-RPC batch arrays as well as single request objects.
	if strings.HasPrefix(trimmed, "[") {
		var reqs []rpcRequest
		if err := json.Unmarshal(body, &reqs); err != nil {
			writeJSON(w, http.StatusBadRequest, rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: "invalid JSON-RPC batch"},
			})
			return
		}
		var resps []rpcResponse
		for _, req := range reqs {
			if resp := s.dispatchMCP(w, r, req, tokenHash, authed); resp != nil {
				resps = append(resps, *resp)
			}
		}
		if len(resps) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, resps)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "invalid JSON-RPC request"},
		})
		return
	}
	resp := s.dispatchMCP(w, r, req, tokenHash, authed)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func isSafeHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	sc := strings.ToLower(u.Scheme)
	return sc == "http" || sc == "https"
}

func newMCPSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) mcpServerInfo() map[string]any {
	id := s.siteID()
	return map[string]any{
		"name":    "donottouchtheglass",
		"title":   id.Title,
		"version": "1.0.0",
		"icons": []map[string]any{
			{"src": s.absURL("/favicon.svg"), "mimeType": "image/svg+xml"},
			{"src": s.absURL("/static/icons/icon-192.png"), "mimeType": "image/png", "sizes": []string{"192x192"}},
		},
	}
}

func (s *Server) mcpInstructions(authed bool) string {
	id := s.siteID()
	instructions := fmt.Sprintf(
		"You are connected to %s (%s), a curated visual & archival collection. "+
			"Use archive_overview to orient yourself, browse_archive or search_archive to find plates, "+
			"color_palette to explore by OKLab perceptual color or hex proximity, curatorial_walk to trace associative visual/thematic threads, "+
			"serendipity for random inspiration, and get_item (with include_images=true when you have vision) to inspect plates closely.",
		id.Title, s.siteBaseURL(),
	)
	if authed {
		instructions += " Authenticated mode is active: private items are visible and archive_item, curate_item, and list_unannotated are unlocked."
	}
	return instructions
}

func (s *Server) dispatchMCP(w http.ResponseWriter, r *http.Request, req rpcRequest, tokenHash string, authed bool) *rpcResponse {
	// Notifications have no id (or null id) and expect no JSON-RPC response body.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil
	}

	// Echo negotiated protocol version header per Streamable HTTP / 2026-07-28 spec.
	reqVer := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))
	if reqVer == "" && len(req.Params) > 0 {
		var metaWrap struct {
			Meta map[string]any `json:"_meta"`
		}
		if json.Unmarshal(req.Params, &metaWrap) == nil {
			if v, ok := metaWrap.Meta["io.modelcontextprotocol/protocolVersion"].(string); ok {
				reqVer = strings.TrimSpace(v)
			}
		}
	}
	if mcpVersionSupported(reqVer) {
		w.Header().Set("MCP-Protocol-Version", reqVer)
	} else if w.Header().Get("MCP-Protocol-Version") == "" {
		w.Header().Set("MCP-Protocol-Version", mcpProtocolLatest)
	}

	cacheScope := "public"
	if authed {
		cacheScope = "private"
	}

	okResp := func(res any) *rpcResponse {
		if isNotification {
			return nil
		}
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: res}
	}
	errResp := func(code int, msg string) *rpcResponse {
		if isNotification {
			return nil
		}
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}}
	}

	switch req.Method {
	case "server/discover":
		// MCP 2026-07-28 stateless discovery endpoint.
		w.Header().Set("MCP-Protocol-Version", mcpProtocolLatest)
		return okResp(map[string]any{
			"resultType":        "complete",
			"supportedVersions": mcpSupportedVersions,
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"subscribe": false, "listChanged": false},
				"prompts":   map[string]any{"listChanged": false},
			},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": s.mcpServerInfo(),
			},
			"instructions": s.mcpInstructions(authed),
			"ttlMs":        300000,
			"cacheScope":   cacheScope,
		})

	case "initialize":
		// Backwards-compatible handshake for 2024-11-05 .. 2025-11-25 (and 2026-07-28 if requested).
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		ver := mcpHandshakeLatest
		if mcpVersionSupported(params.ProtocolVersion) {
			ver = params.ProtocolVersion
		}
		sid := r.Header.Get("Mcp-Session-Id")
		if sid == "" {
			sid = newMCPSessionID()
		}
		w.Header().Set("Mcp-Session-Id", sid)
		w.Header().Set("MCP-Protocol-Version", ver)
		return okResp(map[string]any{
			"protocolVersion": ver,
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"subscribe": false, "listChanged": false},
				"prompts":   map[string]any{"listChanged": false},
			},
			"serverInfo":   s.mcpServerInfo(),
			"instructions": s.mcpInstructions(authed),
		})

	case "ping":
		return okResp(map[string]any{})

	case "tools/list":
		return okResp(map[string]any{
			"tools":      mcpToolDefinitions(authed),
			"ttlMs":      300000,
			"cacheScope": cacheScope,
		})

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
			return errResp(-32602, "invalid tools/call params")
		}
		content, structured, isErr, err := s.callMCPTool(r, params.Name, params.Arguments, tokenHash, authed)
		if err != nil {
			return errResp(-32602, err.Error())
		}
		res := map[string]any{
			"content": content,
			"isError": isErr,
		}
		if structured != nil && !isErr {
			res["structuredContent"] = structured
		}
		return okResp(res)

	case "resources/list":
		return okResp(map[string]any{
			"ttlMs":      300000,
			"cacheScope": cacheScope,
			"resources": []map[string]any{
				{
					"uri":         "dnttg://archive/overview",
					"name":        "Archive Overview",
					"description": "Live vitals, counts by kind, taxonomy, and OKLCH chromatic spectrum of the archive.",
					"mimeType":    "application/json",
				},
				{
					"uri":         "dnttg://archive/recent",
					"name":        "Recent Plates",
					"description": "The 25 most recently archived plates with metadata, palettes, and image URLs.",
					"mimeType":    "application/json",
				},
				{
					"uri":         "dnttg://archive/palette",
					"name":        "Archive Chromatic Spectrum",
					"description": "Perceptual OKLCH color bucket counts and representative hex swatches.",
					"mimeType":    "application/json",
				},
			},
		})

	case "resources/templates/list":
		return okResp(map[string]any{
			"ttlMs":      300000,
			"cacheScope": cacheScope,
			"resourceTemplates": []map[string]any{
				{
					"uriTemplate": "dnttg://item/{id}",
					"name":        "Archived Plate Dossier",
					"description": "Full Markdown dossier for item {id} including all plates, OKLab palettes, notes, related items, and chromatic neighbors.",
					"mimeType":    "text/markdown",
				},
				{
					"uriTemplate": "dnttg://tag/{slug}",
					"name":        "Tag Collection",
					"description": "Items filed under tag {slug}.",
					"mimeType":    "application/json",
				},
				{
					"uriTemplate": "dnttg://color/{bucket}",
					"name":        "Chromatic Bucket",
					"description": "Items matching OKLCH palette bucket {bucket} (red|orange|yellow|green|blue|violet|light|neutral|dark).",
					"mimeType":    "application/json",
				},
			},
		})

	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.URI == "" {
			return errResp(-32602, "missing resource uri")
		}
		contents, err := s.readMCPResource(r.Context(), params.URI, authed)
		if err != nil {
			return errResp(-32002, err.Error())
		}
		return okResp(map[string]any{"contents": contents})

	case "prompts/list":
		return okResp(map[string]any{
			"prompts":    mcpPromptDefinitions(),
			"ttlMs":      300000,
			"cacheScope": cacheScope,
		})

	case "prompts/get":
		var params struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
			return errResp(-32602, "missing prompt name")
		}
		prompt, err := s.getMCPPrompt(params.Name, params.Arguments)
		if err != nil {
			return errResp(-32602, err.Error())
		}
		return okResp(prompt)

	default:
		return errResp(-32601, fmt.Sprintf("method %q not found", req.Method))
	}
}

func mcpToolDefinitions(authed bool) []mcpToolDef {
	readOnly := map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": false}
	tools := []mcpToolDef{
		{
			Name:        "archive_overview",
			Description: "Get a bird's-eye overview of the visual archive: item counts by kind, multi-image series count, categories, top tags, perceptual OKLCH chromatic spectrum, and date span.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Annotations: readOnly,
		},
		{
			Name:        "browse_archive",
			Description: "Browse and filter archived plates by kind, category, tag, OKLCH color bucket, perceptual OKLab hex proximity, multi-image series, or date range.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind":             map[string]any{"type": "string", "enum": []string{"image", "text", "link", "embed", "document"}, "description": "Filter by item kind"},
					"category":         map[string]any{"type": "string", "description": "Category name or slug"},
					"tag":              map[string]any{"type": "string", "description": "Tag name or slug"},
					"color":            map[string]any{"type": "string", "enum": []string{"red", "orange", "yellow", "green", "blue", "violet", "light", "neutral", "dark"}, "description": "Editorial OKLCH color bucket"},
					"hex":              map[string]any{"type": "string", "description": "Target hex color (e.g. #4a6c8c) to rank items by perceptual OKLab distance across all plates"},
					"max_delta":        map[string]any{"type": "number", "description": "Optional OKLab ΔE threshold for hex search (default 0.24; ~0.08 is very close, ~0.18 is broad kinship)"},
					"multi_image_only": map[string]any{"type": "boolean", "description": "Only return multi-image series (2+ plates)"},
					"from":             map[string]any{"type": "string", "description": "Start date YYYY-MM-DD (inclusive)"},
					"to":               map[string]any{"type": "string", "description": "End date YYYY-MM-DD (inclusive)"},
					"limit":            map[string]any{"type": "integer", "description": "Max items to return (default 20, max 100)"},
					"offset":           map[string]any{"type": "integer", "description": "Pagination offset"},
				},
			},
			Annotations: readOnly,
		},
		{
			Name:        "search_archive",
			Description: "Full-text FTS5 search across titles, notes, quotes, source domains, categories, and tags.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"query"},
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Search terms"},
					"limit": map[string]any{"type": "integer", "description": "Max results (default 20, max 100)"},
				},
			},
			Annotations: readOnly,
		},
		{
			Name:        "get_item",
			Description: "Fetch the complete curatorial dossier for a plate by ID: all gallery images (dimensions, OKLab palettes, alt texts), notes, source link, related items, and chromatic OKLab neighbors. Set include_images=true to attach inline image thumbnails so vision-capable models can inspect the artwork directly.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id":             map[string]any{"type": "integer", "description": "Numeric item ID"},
					"include_images": map[string]any{"type": "boolean", "description": "Attach inline JPEG thumbnails as MCP image blocks (default false; capped at 4 plates)"},
					"max_images":     map[string]any{"type": "integer", "description": "Max inline images to attach when include_images=true (1-4, default 4)"},
				},
			},
			Annotations: readOnly,
		},
		{
			Name:        "curatorial_walk",
			Description: "Take an associative N-step walk through the archive starting from an item ID (or random seed), hopping across shared tags, categories, and perceptual OKLab color kinship — with an editorial explanation for each bridge. Great for visual essays, exhibitions, and unexpected connections.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"start_id":       map[string]any{"type": "integer", "description": "Optional starting item ID (random if omitted)"},
					"steps":          map[string]any{"type": "integer", "description": "Number of plates in the walk (2-10, default 5)"},
					"mode":           map[string]any{"type": "string", "enum": []string{"balanced", "color", "thematic"}, "description": "Walk bias: balanced (default), color (OKLab chromatic drift), or thematic (tags & categories)"},
					"include_images": map[string]any{"type": "boolean", "description": "Attach lead thumbnail images for up to 4 walk steps"},
				},
			},
			Annotations: readOnly,
		},
		{
			Name:        "color_palette",
			Description: "Explore the archive chromatically in perceptual OKLab space: omit hex to view the archive's OKLCH bucket spectrum and sample hexes, or pass a target hex (e.g. #c97a3d) to find the closest matching plates across single and multi-image items.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"hex":       map[string]any{"type": "string", "description": "Optional target hex color (e.g. #b94a3e) for nearest-neighbor OKLab matching"},
					"max_delta": map[string]any{"type": "number", "description": "Max OKLab ΔE distance (default 0.22)"},
					"limit":     map[string]any{"type": "integer", "description": "Max matching items (default 15, max 50)"},
				},
			},
			Annotations: readOnly,
		},
		{
			Name:        "serendipity",
			Description: "Draw 1–8 random plates from the archive (optionally scoped by color bucket, kind, tag, category, or multi-image series) for creative prompts, comparisons, or inspiration.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"count":            map[string]any{"type": "integer", "description": "Number of random items (1-8, default 3)"},
					"color":            map[string]any{"type": "string", "enum": []string{"red", "orange", "yellow", "green", "blue", "violet", "light", "neutral", "dark"}},
					"kind":             map[string]any{"type": "string", "enum": []string{"image", "text", "link", "embed", "document"}},
					"tag":              map[string]any{"type": "string"},
					"category":         map[string]any{"type": "string"},
					"multi_image_only": map[string]any{"type": "boolean"},
					"include_images":   map[string]any{"type": "boolean", "description": "Attach inline thumbnails for up to 4 drawn plates"},
				},
			},
			Annotations: readOnly,
		},
	}

	if authed {
		tools = append(tools,
			mcpToolDef{
				Name:        "archive_item",
				Description: "Save a new item into the archive from a URL (web page, image, tweet/Bluesky/Mastodon multi-image post), a list of image URLs, or a text note. Requires bearer-token authentication.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url":        map[string]any{"type": "string", "description": "URL to archive (tweet, Bluesky, Mastodon, image, link, or embed)"},
						"urls":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Multiple image URLs to combine into a single multi-image series"},
						"source":     map[string]any{"type": "string", "description": "Optional canonical source attribution URL"},
						"title":      map[string]any{"type": "string"},
						"note":       map[string]any{"type": "string", "description": "Curatorial note or text body"},
						"category":   map[string]any{"type": "string"},
						"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"visibility": map[string]any{"type": "string", "enum": []string{"public", "private"}},
					},
				},
				Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": true},
			},
			mcpToolDef{
				Name:        "curate_item",
				Description: "Update curatorial metadata on an existing item: title, note, category, tags, visibility, and/or per-plate alt-text descriptions (pair with get_item(include_images=true) to write museum-grade alt text and archival tags). Omitted fields stay unchanged.",
				InputSchema: map[string]any{
					"type":     "object",
					"required": []string{"id"},
					"properties": map[string]any{
						"id":         map[string]any{"type": "integer", "description": "Numeric item ID"},
						"title":      map[string]any{"type": "string"},
						"note":       map[string]any{"type": "string"},
						"source_url": map[string]any{"type": "string"},
						"category":   map[string]any{"type": "string"},
						"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Replaces the item's tag list when provided"},
						"add_tags":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Appends tags without removing existing ones"},
						"visibility": map[string]any{"type": "string", "enum": []string{"public", "private"}},
						"plate_alts": map[string]any{
							"type":        "array",
							"description": "Update alt text / captions on individual plates by image_id or 0-based position",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"image_id": map[string]any{"type": "integer"},
									"position": map[string]any{"type": "integer"},
									"alt":      map[string]any{"type": "string"},
								},
							},
						},
					},
				},
				Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
			},
			mcpToolDef{
				Name:        "list_unannotated",
				Description: "Find items missing alt text on their plates, missing tags, or missing titles so an LLM curator can systematically enrich the archive.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"missing": map[string]any{"type": "string", "enum": []string{"alt", "tags", "title", "any"}, "description": "What gap to look for (default: alt)"},
						"limit":   map[string]any{"type": "integer", "description": "Max items to return (default 20, max 50)"},
					},
				},
				Annotations: readOnly,
			},
		)
	}
	return tools
}

func (s *Server) callMCPTool(r *http.Request, name string, rawArgs json.RawMessage, tokenHash string, authed bool) ([]mcpContent, any, bool, error) {
	ctx := r.Context()
	if len(rawArgs) == 0 {
		rawArgs = []byte("{}")
	}
	jsonResult := func(v any) ([]mcpContent, any, bool, error) {
		b, _ := json.MarshalIndent(v, "", "  ")
		return []mcpContent{{Type: "text", Text: string(b)}}, v, false, nil
	}
	jsonText := func(v any) []mcpContent {
		b, _ := json.MarshalIndent(v, "", "  ")
		return []mcpContent{{Type: "text", Text: string(b)}}
	}
	errContent := func(msg string) ([]mcpContent, any, bool, error) {
		return []mcpContent{{Type: "text", Text: msg}}, nil, true, nil
	}

	switch name {
	case "archive_overview":
		ov, err := s.queryOverview(ctx, authed)
		if err != nil {
			return errContent("failed to load overview: " + err.Error())
		}
		return jsonResult(ov)

	case "browse_archive":
		var a struct {
			Kind           string  `json:"kind"`
			Category       string  `json:"category"`
			Tag            string  `json:"tag"`
			Color          string  `json:"color"`
			Hex            string  `json:"hex"`
			MaxDelta       float64 `json:"max_delta"`
			MultiImageOnly bool    `json:"multi_image_only"`
			From           string  `json:"from"`
			To             string  `json:"to"`
			Limit          int     `json:"limit"`
			Offset         int     `json:"offset"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return nil, nil, false, fmt.Errorf("invalid arguments: %w", err)
		}
		if a.Limit <= 0 {
			a.Limit = 20
		}
		items, err := s.queryItems(ctx, itemQueryParams{
			Kind:           a.Kind,
			Category:       a.Category,
			Tag:            a.Tag,
			Color:          a.Color,
			Hex:            a.Hex,
			MaxDelta:       a.MaxDelta,
			MultiOnly:      a.MultiImageOnly,
			From:           a.From,
			To:             a.To,
			Limit:          a.Limit,
			Offset:         a.Offset,
			IncludePrivate: authed,
		})
		if err != nil {
			return errContent("browse failed: " + err.Error())
		}
		return jsonResult(map[string]any{"count": len(items), "items": items})

	case "search_archive":
		var a struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return nil, nil, false, fmt.Errorf("invalid arguments: %w", err)
		}
		if strings.TrimSpace(a.Query) == "" {
			return nil, nil, false, errors.New("query is required")
		}
		if a.Limit <= 0 {
			a.Limit = 20
		}
		items, err := s.queryItems(ctx, itemQueryParams{
			Query:          a.Query,
			Limit:          a.Limit,
			IncludePrivate: authed,
		})
		if err != nil {
			return errContent("search failed: " + err.Error())
		}
		return jsonResult(map[string]any{"query": a.Query, "count": len(items), "items": items})

	case "get_item":
		var a struct {
			ID            int64 `json:"id"`
			IncludeImages bool  `json:"include_images"`
			MaxImages     int   `json:"max_images"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return nil, nil, false, fmt.Errorf("invalid arguments: %w", err)
		}
		if a.ID <= 0 {
			return nil, nil, false, errors.New("id must be a positive integer")
		}
		detail, rawItem, err := s.queryItemDetail(ctx, a.ID, authed)
		if err != nil {
			return errContent("load item failed: " + err.Error())
		}
		if detail == nil {
			return errContent(fmt.Sprintf("item %d not found", a.ID))
		}
		out := jsonText(detail)
		if a.IncludeImages && rawItem != nil {
			maxImgs := a.MaxImages
			if maxImgs <= 0 || maxImgs > mcpMaxInlineImgs {
				maxImgs = mcpMaxInlineImgs
			}
			out = append(out, s.mcpItemImageBlocks(ctx, *rawItem, maxImgs)...)
		}
		return out, detail, false, nil

	case "curatorial_walk":
		var a struct {
			StartID       int64  `json:"start_id"`
			Steps         int    `json:"steps"`
			Mode          string `json:"mode"`
			IncludeImages bool   `json:"include_images"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return nil, nil, false, fmt.Errorf("invalid arguments: %w", err)
		}
		walk, err := s.queryCuratorialWalk(ctx, a.StartID, a.Steps, a.Mode, authed)
		if err != nil {
			return errContent("curatorial walk failed: " + err.Error())
		}
		payload := map[string]any{
			"mode":  firstNonEmpty(a.Mode, "balanced"),
			"count": len(walk),
			"steps": walk,
		}
		out := jsonText(payload)
		if a.IncludeImages {
			attached := 0
			for _, st := range walk {
				if attached >= mcpMaxInlineImgs {
					break
				}
				if raw, err := s.store.GetItem(ctx, st.Item.ID, authed); err == nil && raw != nil {
					if blocks := s.mcpItemImageBlocks(ctx, *raw, 1); len(blocks) > 0 {
						out = append(out, mcpContent{Type: "text", Text: fmt.Sprintf("Step %d — Plate %03d (%s)", st.Step, st.Item.ID, st.Reason)})
						out = append(out, blocks...)
						attached++
					}
				}
			}
		}
		return out, payload, false, nil

	case "color_palette":
		var a struct {
			Hex      string  `json:"hex"`
			MaxDelta float64 `json:"max_delta"`
			Limit    int     `json:"limit"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return nil, nil, false, fmt.Errorf("invalid arguments: %w", err)
		}
		if hexColor := store.NormalizeHex(a.Hex); hexColor != "" {
			if a.Limit <= 0 {
				a.Limit = 15
			}
			if a.Limit > 50 {
				a.Limit = 50
			}
			items, err := s.queryItems(ctx, itemQueryParams{
				Hex:            hexColor,
				MaxDelta:       a.MaxDelta,
				Limit:          a.Limit,
				IncludePrivate: authed,
			})
			if err != nil {
				return errContent("palette search failed: " + err.Error())
			}
			bucket := store.ColorBucket(hexColor)
			return jsonResult(map[string]any{
				"query_hex":    hexColor,
				"bucket":       bucket,
				"bucket_label": store.BucketLabel(bucket),
				"count":        len(items),
				"items":        items,
			})
		}
		buckets, err := s.queryPaletteSummary(ctx, authed)
		if err != nil {
			return errContent("palette summary failed: " + err.Error())
		}
		return jsonResult(map[string]any{"buckets": buckets})

	case "serendipity":
		var a struct {
			Count          int    `json:"count"`
			Color          string `json:"color"`
			Kind           string `json:"kind"`
			Tag            string `json:"tag"`
			Category       string `json:"category"`
			MultiImageOnly bool   `json:"multi_image_only"`
			IncludeImages  bool   `json:"include_images"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return nil, nil, false, fmt.Errorf("invalid arguments: %w", err)
		}
		if a.Count <= 0 {
			a.Count = 3
		}
		if a.Count > 8 {
			a.Count = 8
		}
		items, err := s.queryRandomItems(ctx, itemQueryParams{
			Color:          a.Color,
			Kind:           a.Kind,
			Tag:            a.Tag,
			Category:       a.Category,
			MultiOnly:      a.MultiImageOnly,
			IncludePrivate: authed,
			IncludePlates:  true,
		}, a.Count)
		if err != nil {
			return errContent("serendipity failed: " + err.Error())
		}
		payload := map[string]any{"count": len(items), "items": items}
		out := jsonText(payload)
		if a.IncludeImages {
			attached := 0
			for _, it := range items {
				if attached >= mcpMaxInlineImgs {
					break
				}
				if raw, err := s.store.GetItem(ctx, it.ID, authed); err == nil && raw != nil {
					if blocks := s.mcpItemImageBlocks(ctx, *raw, 1); len(blocks) > 0 {
						out = append(out, mcpContent{Type: "text", Text: fmt.Sprintf("Plate %03d — %s", it.ID, firstNonEmpty(it.Title, "Untitled"))})
						out = append(out, blocks...)
						attached++
					}
				}
			}
		}
		return out, payload, false, nil

	case "archive_item":
		if !authed {
			return errContent("bearer token required to archive items")
		}
		if ok, _ := s.apiCreateTokenRL.allow(tokenHash); !ok {
			return errContent("rate limited (token budget exceeded)")
		}
		if ok, _ := s.apiCreateIPRL.allow(s.clientIP(r)); !ok {
			return errContent("rate limited (IP budget exceeded)")
		}
		var a struct {
			URL        string   `json:"url"`
			URLs       []string `json:"urls"`
			Source     string   `json:"source"`
			Title      string   `json:"title"`
			Note       string   `json:"note"`
			Category   string   `json:"category"`
			Tags       []string `json:"tags"`
			Visibility string   `json:"visibility"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return nil, nil, false, fmt.Errorf("invalid arguments: %w", err)
		}
		in := ingest.Input{
			URL:        strings.TrimSpace(a.URL),
			URLs:       a.URLs,
			Source:     strings.TrimSpace(a.Source),
			Title:      a.Title,
			Note:       a.Note,
			Category:   a.Category,
			Tags:       a.Tags,
			Visibility: a.Visibility,
		}
		dup, _ := s.ingest.CheckDuplicate(ctx, in)
		id, err := s.ingest.Create(ctx, in)
		if err != nil {
			return errContent("archive failed: " + err.Error())
		}
		s.invalidateSiteCache()
		resp := map[string]any{
			"id":           id,
			"url":          s.absURL(itemPath(id)),
			"markdown_url": s.absURL(itemPath(id) + ".md"),
		}
		if dup != nil && dup.ID != id {
			resp["duplicate_of"] = dup.ID
		}
		return jsonResult(resp)

	case "curate_item":
		if !authed {
			return errContent("bearer token required to curate items")
		}
		var a struct {
			ID         int64    `json:"id"`
			Title      *string  `json:"title"`
			Note       *string  `json:"note"`
			SourceURL  *string  `json:"source_url"`
			Category   *string  `json:"category"`
			Tags       []string `json:"tags"`
			AddTags    []string `json:"add_tags"`
			Visibility *string  `json:"visibility"`
			PlateAlts  []struct {
				ImageID  int64  `json:"image_id"`
				Position *int   `json:"position"`
				Alt      string `json:"alt"`
			} `json:"plate_alts"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return nil, nil, false, fmt.Errorf("invalid arguments: %w", err)
		}
		if a.ID <= 0 {
			return nil, nil, false, errors.New("id must be a positive integer")
		}
		it, err := s.store.GetItem(ctx, a.ID, true)
		if err != nil {
			return errContent("load item failed: " + err.Error())
		}
		if it == nil {
			return errContent(fmt.Sprintf("item %d not found", a.ID))
		}

		// Merge fields: omitted fields preserve existing values.
		edit := store.ItemEdit{
			Title:      it.Title,
			Note:       it.Note,
			SourceURL:  it.SourceURL,
			Category:   it.CategoryName,
			Visibility: it.Visibility,
		}
		for _, t := range it.Tags {
			edit.Tags = append(edit.Tags, t.Name)
		}
		if a.Title != nil {
			edit.Title = *a.Title
		}
		if a.Note != nil {
			edit.Note = *a.Note
		}
		if a.SourceURL != nil {
			src := strings.TrimSpace(*a.SourceURL)
			if src != "" && !isSafeHTTPURL(src) {
				return errContent("source_url must use http:// or https://")
			}
			edit.SourceURL = src
		}
		if a.Category != nil {
			edit.Category = *a.Category
		}
		if a.Visibility != nil {
			edit.Visibility = *a.Visibility
		}
		if a.Tags != nil {
			edit.Tags = a.Tags
		}
		if len(a.AddTags) > 0 {
			edit.Tags = append(edit.Tags, a.AddTags...)
		}
		if err := s.store.UpdateItem(ctx, a.ID, edit); err != nil {
			return errContent("update item failed: " + err.Error())
		}
		updatedPlates := 0
		for _, pa := range a.PlateAlts {
			imgID := pa.ImageID
			if imgID == 0 && pa.Position != nil {
				for _, img := range it.Images {
					if img.Position == *pa.Position {
						imgID = img.ID
						break
					}
				}
			}
			if imgID > 0 {
				if err := s.store.UpdateItemImageAlt(ctx, a.ID, imgID, pa.Alt); err == nil {
					updatedPlates++
				}
			}
		}
		s.invalidateSiteCache()
		detail, _, _ := s.queryItemDetail(ctx, a.ID, true)
		return jsonResult(map[string]any{
			"updated":        true,
			"updated_plates": updatedPlates,
			"item":           detail.Item,
		})

	case "list_unannotated":
		if !authed {
			return errContent("bearer token required for list_unannotated")
		}
		var a struct {
			Missing string `json:"missing"`
			Limit   int    `json:"limit"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return nil, nil, false, fmt.Errorf("invalid arguments: %w", err)
		}
		missing := strings.ToLower(strings.TrimSpace(a.Missing))
		if missing == "" {
			missing = "alt"
		}
		limit := a.Limit
		if limit <= 0 {
			limit = 20
		}
		if limit > 50 {
			limit = 50
		}
		all, err := s.store.ListItems(ctx, store.ItemFilter{IncludePrivate: true, IncludeTags: true})
		if err != nil {
			return errContent("scan failed: " + err.Error())
		}
		var out []apiItem
		for _, it := range all {
			needTitle := strings.TrimSpace(it.Title) == ""
			needTags := len(it.Tags) == 0
			needAlt := false
			if it.Kind == "image" || it.CoverKey != "" {
				full, err := s.store.GetItem(ctx, it.ID, true)
				if err == nil && full != nil {
					it = *full
					for _, img := range it.Images {
						if strings.TrimSpace(img.Alt) == "" {
							needAlt = true
							break
						}
					}
				}
			}
			match := false
			switch missing {
			case "alt":
				match = needAlt
			case "tags":
				match = needTags
			case "title":
				match = needTitle
			default:
				match = needAlt || needTags || needTitle
			}
			if match {
				out = append(out, s.toAPIItem(it, true))
				if len(out) >= limit {
					break
				}
			}
		}
		return jsonResult(map[string]any{"missing": missing, "count": len(out), "items": out})

	default:
		return nil, nil, false, fmt.Errorf("unknown tool %q", name)
	}
}

// mcpItemImageBlocks loads up to maxImgs lightweight JPEG thumbnails directly
// from media.Store and encodes them as MCP base64 image content blocks.
func (s *Server) mcpItemImageBlocks(ctx context.Context, it store.Item, maxImgs int) []mcpContent {
	var keys []string
	if len(it.Images) > 0 {
		for _, img := range it.Images {
			if len(keys) >= maxImgs {
				break
			}
			if k := firstNonEmpty(img.SmallKey, img.ThumbKey, img.CoverKey); k != "" {
				keys = append(keys, k)
			}
		}
	} else if k := firstNonEmpty(it.SmallKey, it.ThumbKey, it.CoverKey); k != "" {
		keys = append(keys, k)
	}
	var out []mcpContent
	for _, k := range keys {
		rc, err := s.media.Open(ctx, k)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(rc, mcpMaxImageBytes+1))
		_ = rc.Close()
		if err != nil || len(data) == 0 || len(data) > mcpMaxImageBytes {
			continue
		}
		ct := http.DetectContentType(data)
		if !strings.HasPrefix(ct, "image/") {
			ct = "image/jpeg"
		}
		out = append(out, mcpContent{
			Type:     "image",
			MimeType: ct,
			Data:     base64.StdEncoding.EncodeToString(data),
		})
	}
	return out
}

func (s *Server) readMCPResource(ctx context.Context, uri string, authed bool) ([]map[string]any, error) {
	switch {
	case uri == "dnttg://archive/overview":
		ov, err := s.queryOverview(ctx, authed)
		if err != nil {
			return nil, err
		}
		b, _ := json.MarshalIndent(ov, "", "  ")
		return []map[string]any{{"uri": uri, "mimeType": "application/json", "text": string(b)}}, nil

	case uri == "dnttg://archive/recent":
		items, err := s.queryItems(ctx, itemQueryParams{Limit: 25, IncludePrivate: authed})
		if err != nil {
			return nil, err
		}
		b, _ := json.MarshalIndent(map[string]any{"count": len(items), "items": items}, "", "  ")
		return []map[string]any{{"uri": uri, "mimeType": "application/json", "text": string(b)}}, nil

	case uri == "dnttg://archive/palette":
		pal, err := s.queryPaletteSummary(ctx, authed)
		if err != nil {
			return nil, err
		}
		b, _ := json.MarshalIndent(map[string]any{"buckets": pal}, "", "  ")
		return []map[string]any{{"uri": uri, "mimeType": "application/json", "text": string(b)}}, nil

	case strings.HasPrefix(uri, "dnttg://item/"):
		idStr := strings.TrimPrefix(uri, "dnttg://item/")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid item id in %q", uri)
		}
		detail, _, err := s.queryItemDetail(ctx, id, authed)
		if err != nil {
			return nil, err
		}
		if detail == nil {
			return nil, fmt.Errorf("item %d not found", id)
		}
		return []map[string]any{{"uri": uri, "mimeType": "text/markdown", "text": renderItemMarkdown(*detail)}}, nil

	case strings.HasPrefix(uri, "dnttg://tag/"):
		slug := strings.TrimPrefix(uri, "dnttg://tag/")
		items, err := s.queryItems(ctx, itemQueryParams{Tag: slug, Limit: 50, IncludePrivate: authed})
		if err != nil {
			return nil, err
		}
		b, _ := json.MarshalIndent(map[string]any{"tag": slug, "count": len(items), "items": items}, "", "  ")
		return []map[string]any{{"uri": uri, "mimeType": "application/json", "text": string(b)}}, nil

	case strings.HasPrefix(uri, "dnttg://color/"):
		bucket := strings.TrimPrefix(uri, "dnttg://color/")
		if !store.ValidColorBucket(bucket) {
			return nil, fmt.Errorf("unknown color bucket %q", bucket)
		}
		items, err := s.queryItems(ctx, itemQueryParams{Color: bucket, Limit: 50, IncludePrivate: authed})
		if err != nil {
			return nil, err
		}
		b, _ := json.MarshalIndent(map[string]any{"bucket": bucket, "label": store.BucketLabel(bucket), "count": len(items), "items": items}, "", "  ")
		return []map[string]any{{"uri": uri, "mimeType": "application/json", "text": string(b)}}, nil

	default:
		return nil, fmt.Errorf("resource %q not found", uri)
	}
}

func mcpPromptDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "curate-exhibition",
			"description": "Curate a museum-grade mini-exhibition of 5–8 plates from the archive around a theme, mood, or color, complete with exhibition title, wall text, and plate sequencing.",
			"arguments": []map[string]any{
				{"name": "theme", "description": "Exhibition concept, mood, era, or color motif", "required": true},
			},
		},
		{
			"name":        "visual-essay",
			"description": "Trace an associative visual essay through the archive using curatorial_walk and plate inspection.",
			"arguments": []map[string]any{
				{"name": "start_id", "description": "Optional starting item ID (leave blank for serendipitous seed)", "required": false},
			},
		},
		{
			"name":        "chromatic-study",
			"description": "Conduct a perceptual color study across archive plates matching a target hex or OKLCH palette family.",
			"arguments": []map[string]any{
				{"name": "color", "description": "Hex code (e.g. #4a6c8c) or bucket name (red, orange, yellow, green, blue, violet, light, neutral, dark)", "required": true},
			},
		},
		{
			"name":        "describe-plates",
			"description": "Visually inspect an archived item's plates (using get_item with include_images=true) and craft concise, museum-grade alt text and archival tags.",
			"arguments": []map[string]any{
				{"name": "item_id", "description": "Numeric item ID to inspect and annotate", "required": true},
			},
		},
	}
}

func (s *Server) getMCPPrompt(name string, args map[string]string) (map[string]any, error) {
	id := s.siteID()
	switch name {
	case "curate-exhibition":
		theme := strings.TrimSpace(args["theme"])
		if theme == "" {
			theme = "unexpected juxtapositions"
		}
		text := fmt.Sprintf(
			"Curate a 6-plate exhibition from %s (%s) around the theme %q. "+
				"First call `archive_overview`, `search_archive`, `browse_archive`, or `color_palette` to survey relevant plates, then inspect your finalists with `get_item(include_images=true)`. "+
				"Present an exhibition title, brief curatorial wall text, and each sequenced plate with its Plate ID, direct link, palette notes, and why it sits next to its neighbors.",
			id.Title, s.siteBaseURL(), theme,
		)
		return map[string]any{
			"description": "Curate a themed mini-exhibition from the archive",
			"messages":    []map[string]any{{"role": "user", "content": map[string]any{"type": "text", "text": text}}},
		}, nil

	case "visual-essay":
		seed := strings.TrimSpace(args["start_id"])
		seedInstr := "an unseeded `curatorial_walk(steps=6)`"
		if seed != "" {
			seedInstr = fmt.Sprintf("`curatorial_walk(start_id=%s, steps=6)`", seed)
		}
		text := fmt.Sprintf(
			"Write a short associative visual essay wandering through %s. Start by calling %s with `include_images=true` (or inspect individual plates with `get_item`). "+
				"Weave together the chromatic transitions, formal echoes, and historical/thematic bridges linking each plate, citing every plate ID and URL.",
			id.Title, seedInstr,
		)
		return map[string]any{
			"description": "Associative visual essay across connected plates",
			"messages":    []map[string]any{{"role": "user", "content": map[string]any{"type": "text", "text": text}}},
		}, nil

	case "chromatic-study":
		col := strings.TrimSpace(args["color"])
		if col == "" {
			col = "blue"
		}
		text := fmt.Sprintf(
			"Perform a chromatic study of %q across %s. Use `color_palette` (passing `hex` if it is a hex code) or `browse_archive(color=...)` to gather matching plates, "+
				"inspect key plates with `get_item(include_images=true)`, and analyze how pigment, material, light, and multi-image series palettes behave across the archive.",
			col, id.Title,
		)
		return map[string]any{
			"description": "Chromatic color study in OKLab space",
			"messages":    []map[string]any{{"role": "user", "content": map[string]any{"type": "text", "text": text}}},
		}, nil

	case "describe-plates":
		itemID := strings.TrimSpace(args["item_id"])
		if itemID == "" {
			return nil, errors.New("item_id argument is required")
		}
		text := fmt.Sprintf(
			"Call `get_item(id=%s, include_images=true)` to inspect all plates of item %s. "+
				"For each plate, draft crisp, objective, museum-grade alt text (1–2 sentences noting subject, medium, composition, and typography if present) and propose 3–6 archival tags. "+
				"If authenticated with write access, offer or apply the updates via `curate_item`.",
			itemID, itemID,
		)
		return map[string]any{
			"description": "Visual inspection and museum-grade alt-text/tagging workflow",
			"messages":    []map[string]any{{"role": "user", "content": map[string]any{"type": "text", "text": text}}},
		}, nil

	default:
		return nil, fmt.Errorf("unknown prompt %q", name)
	}
}
