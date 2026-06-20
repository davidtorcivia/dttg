package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const translateMaxRunes = 5000

// handleTranslate translates a block of text (auto-detecting the source language)
// to the requested target (default "en"). Public, length-capped.
func (s *Server) handleTranslate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text   string `json:"text"`
		Target string `json:"target"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeJSONError(w, http.StatusBadRequest, "empty text")
		return
	}
	if rs := []rune(text); len(rs) > translateMaxRunes {
		text = string(rs[:translateMaxRunes])
	}
	translated, source, err := translateText(r.Context(), text, body.Target)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "translation unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"text": translated, "source": source})
}

// translateText uses Google's public (keyless) translate endpoint. It auto-detects
// the source language and returns (translation, detectedSource).
func translateText(ctx context.Context, text, target string) (string, string, error) {
	if target == "" {
		target = "en"
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	endpoint := "https://translate.googleapis.com/translate_a/single?client=gtx&sl=auto&tl=" +
		url.QueryEscape(target) + "&dt=t&q=" + url.QueryEscape(text)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; dttg/1.0)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("translate status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return "", "", err
	}

	// Response shape: [[["translated","original",...],...], null, "detectedLang", ...]
	var top []json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || len(top) == 0 {
		return "", "", fmt.Errorf("translate parse")
	}
	var segments [][]json.RawMessage
	_ = json.Unmarshal(top[0], &segments)
	var b strings.Builder
	for _, seg := range segments {
		if len(seg) > 0 {
			var chunk string
			if json.Unmarshal(seg[0], &chunk) == nil {
				b.WriteString(chunk)
			}
		}
	}
	source := ""
	if len(top) > 2 {
		_ = json.Unmarshal(top[2], &source)
	}
	if b.Len() == 0 {
		return "", "", fmt.Errorf("translate empty")
	}
	return b.String(), source, nil
}
