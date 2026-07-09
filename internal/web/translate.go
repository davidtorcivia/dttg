package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	translateMaxRunes = 5000
	translateCacheTTL = time.Hour
)

type translateCacheEntry struct {
	text   string
	source string
	exp    time.Time
}

// translateCache is a tiny in-memory translation result cache keyed by
// sha256(target + "\x00" + text). Entries expire after translateCacheTTL.
type translateCache struct {
	mu   sync.Mutex
	data map[string]translateCacheEntry
}

func newTranslateCache() *translateCache {
	return &translateCache{data: map[string]translateCacheEntry{}}
}

func translateCacheKey(target, text string) string {
	sum := sha256.Sum256([]byte(target + "\x00" + text))
	return hex.EncodeToString(sum[:])
}

func (c *translateCache) get(key string) (text, source string, ok bool) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	e, ok := c.data[key]
	if !ok || now.After(e.exp) {
		return "", "", false
	}
	return e.text, e.source, true
}

func (c *translateCache) set(key, text, source string) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	c.data[key] = translateCacheEntry{text: text, source: source, exp: now.Add(translateCacheTTL)}
}

func (c *translateCache) prune(now time.Time) {
	for k, e := range c.data {
		if now.After(e.exp) {
			delete(c.data, k)
		}
	}
}

// handleTranslate translates a block of text (auto-detecting the source language)
// to the requested target (default "en"). Public, length-capped, rate-limited.
func (s *Server) handleTranslate(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.translateRL != nil {
		if ok, retry := s.translateRL.allow(ip); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			writeJSONError(w, http.StatusTooManyRequests, "rate limited")
			return
		}
	}

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
	target := strings.TrimSpace(body.Target)
	if target == "" {
		target = "en"
	}

	cacheKey := translateCacheKey(target, text)
	if s.translateCache != nil {
		if translated, source, ok := s.translateCache.get(cacheKey); ok {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"text": translated, "source": source})
			return
		}
	}

	translated, source, err := translateText(r.Context(), text, target)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "translation unavailable")
		return
	}
	if s.translateCache != nil {
		s.translateCache.set(cacheKey, translated, source)
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
