package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"donottouchtheglass/internal/ingest"
	"donottouchtheglass/internal/store"
)

const remoteFeedPage = 100
const remoteFeedMaxBytes = 1 << 20

type remoteFeedParsed struct {
	Update store.RemoteFeedUpdate
	Items  []store.RemoteFeedItem
}

// cleanFeedURL resolves relative refs against base, accepts only http/https,
// strips fragments, and returns "" for any other scheme or parse error.
func cleanFeedURL(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	abs := b.ResolveReference(r)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return ""
	}
	abs.Fragment = ""
	return abs.String()
}

func parseRemoteJSONFeed(feedURL string, body []byte, fetchedAt time.Time) (remoteFeedParsed, error) {
	var raw struct {
		Version     string          `json:"version"`
		Title       string          `json:"title"`
		HomePageURL string          `json:"home_page_url"`
		Description string          `json:"description"`
		Icon        string          `json:"icon"`
		Favicon     string          `json:"favicon"`
		Items       json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return remoteFeedParsed{}, fmt.Errorf("parse JSON Feed: %w", err)
	}
	if !strings.HasPrefix(raw.Version, "https://jsonfeed.org/version/") {
		return remoteFeedParsed{}, fmt.Errorf("remote feed is not JSON Feed")
	}

	title := strings.TrimSpace(raw.Title)
	if title == "" {
		if u, err := url.Parse(feedURL); err == nil && u.Hostname() != "" {
			title = u.Hostname()
		} else {
			title = feedURL
		}
	}
	icon := cleanFeedURL(feedURL, raw.Icon)
	if icon == "" {
		icon = cleanFeedURL(feedURL, raw.Favicon)
	}

	out := remoteFeedParsed{
		Update: store.RemoteFeedUpdate{
			Title:       title,
			SiteURL:     cleanFeedURL(feedURL, raw.HomePageURL),
			Description: strings.TrimSpace(raw.Description),
			IconURL:     icon,
		},
	}

	var itemRaws []json.RawMessage
	if len(raw.Items) > 0 {
		if err := json.Unmarshal(raw.Items, &itemRaws); err != nil {
			return remoteFeedParsed{}, fmt.Errorf("parse JSON Feed items: %w", err)
		}
	}
	if len(itemRaws) > 200 {
		itemRaws = itemRaws[:200]
	}

	for _, ir := range itemRaws {
		var it struct {
			ID            string `json:"id"`
			URL           string `json:"url"`
			ExternalURL   string `json:"external_url"`
			Title         string `json:"title"`
			ContentText   string `json:"content_text"`
			Image         string `json:"image"`
			BannerImage   string `json:"banner_image"`
			DatePublished string `json:"date_published"`
			DateModified  string `json:"date_modified"`
			Authors       []struct {
				Name string `json:"name"`
				URL  string `json:"url"`
			} `json:"authors"`
			Author *struct {
				Name string `json:"name"`
				URL  string `json:"url"`
			} `json:"author"`
			Attachments []struct {
				URL      string `json:"url"`
				MimeType string `json:"mime_type"`
			} `json:"attachments"`
		}
		if err := json.Unmarshal(ir, &it); err != nil {
			continue
		}
		remoteID := strings.TrimSpace(it.ID)
		if remoteID == "" {
			continue
		}

		imageURL := cleanFeedURL(feedURL, it.Image)
		if imageURL == "" {
			imageURL = cleanFeedURL(feedURL, it.BannerImage)
		}

		authorName, authorURL := "", ""
		if len(it.Authors) > 0 {
			authorName = strings.TrimSpace(it.Authors[0].Name)
			authorURL = cleanFeedURL(feedURL, it.Authors[0].URL)
		} else if it.Author != nil {
			authorName = strings.TrimSpace(it.Author.Name)
			authorURL = cleanFeedURL(feedURL, it.Author.URL)
		}

		attachURL, attachMime := "", ""
		for _, a := range it.Attachments {
			u := cleanFeedURL(feedURL, a.URL)
			if u != "" {
				attachURL = u
				attachMime = strings.TrimSpace(a.MimeType)
				break
			}
		}

		published := fetchedAt
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(it.DatePublished)); err == nil {
			published = t.UTC()
		} else if t, err := time.Parse(time.RFC3339, strings.TrimSpace(it.DateModified)); err == nil {
			published = t.UTC()
		}

		rawJSON := ""
		var obj any
		if err := json.Unmarshal(ir, &obj); err == nil {
			if b, err := json.Marshal(obj); err == nil {
				rawJSON = string(b)
			}
		}

		out.Items = append(out.Items, store.RemoteFeedItem{
			RemoteID:       remoteID,
			URL:            cleanFeedURL(feedURL, it.URL),
			ExternalURL:    cleanFeedURL(feedURL, it.ExternalURL),
			Title:          strings.TrimSpace(it.Title),
			ContentText:    strings.TrimSpace(it.ContentText),
			ImageURL:       imageURL,
			AttachmentURL:  attachURL,
			AttachmentMime: attachMime,
			AuthorName:     authorName,
			AuthorURL:      authorURL,
			PublishedAt:    published,
			FetchedAt:      fetchedAt,
			RawJSON:        rawJSON,
		})
	}
	return out, nil
}

func (s *Server) syncRemoteFeed(ctx context.Context, feedID int64) error {
	feed, err := s.store.GetRemoteFeed(ctx, feedID)
	if err != nil {
		return err
	}
	if feed == nil {
		return fmt.Errorf("remote feed %d not found", feedID)
	}
	if s.ingest == nil {
		return fmt.Errorf("ingest service unavailable")
	}

	now := time.Now().UTC()
	res, err := s.ingest.Fetch(ctx, feed.FeedURL, ingest.FetchOptions{
		Accept:       "application/feed+json, application/json;q=0.9",
		ETag:         feed.ETag,
		LastModified: feed.LastModified,
		MaxBytes:     remoteFeedMaxBytes,
	})
	if err != nil {
		_ = s.store.SaveRemoteFeedError(ctx, feedID, now, err.Error())
		return err
	}
	if res.NotModified {
		if err := s.store.MarkRemoteFeedChecked(ctx, feedID, now); err != nil {
			return err
		}
		return nil
	}

	parsed, err := parseRemoteJSONFeed(feed.FeedURL, res.Body, now)
	if err != nil {
		_ = s.store.SaveRemoteFeedError(ctx, feedID, now, err.Error())
		return err
	}
	parsed.Update.ETag = res.ETag
	parsed.Update.LastModified = res.LastModified
	parsed.Update.LastFetchedAt = now
	parsed.Update.LastSuccessAt = now
	parsed.Update.LastError = ""
	if _, err := s.store.SaveRemoteFeedFetch(ctx, feedID, parsed.Update, parsed.Items); err != nil {
		_ = s.store.SaveRemoteFeedError(ctx, feedID, now, err.Error())
		return err
	}
	return nil
}

func (s *Server) syncAllRemoteFeeds(ctx context.Context) {
	feeds, err := s.store.ListRemoteFeeds(ctx, true)
	if err != nil {
		log.Printf("list remote feeds: %v", err)
		return
	}
	for _, f := range feeds {
		if err := s.syncRemoteFeed(ctx, f.ID); err != nil {
			log.Printf("sync remote feed %d (%s): %v", f.ID, f.FeedURL, err)
		}
	}
}

func (s *Server) handleRemoteFeedPage(w http.ResponseWriter, r *http.Request) {
	f := store.RemoteFeedItemFilter{Limit: remoteFeedPage, ActiveOnly: true}
	if cur := r.URL.Query().Get("cursor"); cur != "" {
		if parts := strings.SplitN(cur, ":", 2); len(parts) == 2 {
			f.BeforePublished, _ = strconv.ParseInt(parts[0], 10, 64)
			f.BeforeID, _ = strconv.ParseInt(parts[1], 10, 64)
		}
	}
	items, err := s.store.ListRemoteFeedItems(r.Context(), f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	feeds, err := s.store.ListRemoteFeeds(r.Context(), false)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pd := s.page(r, "FEED")
	pd.RemoteFeeds = feeds
	pd.RemoteItems = items
	if n := len(items); n > 0 {
		last := items[n-1]
		pd.CursorCreated = last.PublishedAt.Unix()
		pd.CursorID = last.ID
	}
	pd.BoardDone = len(items) < remoteFeedPage
	s.render(w, "feed.html", pd)
}

func (s *Server) handleRemoteFeedAddSource(w http.ResponseWriter, r *http.Request) {
	feedURL := strings.TrimSpace(r.FormValue("feed_url"))
	feed, _, err := s.store.AddRemoteFeed(r.Context(), feedURL)
	if err != nil {
		feeds, _ := s.store.ListRemoteFeeds(r.Context(), false)
		items, _ := s.store.ListRemoteFeedItems(r.Context(), store.RemoteFeedItemFilter{
			Limit: remoteFeedPage, ActiveOnly: true,
		})
		pd := s.page(r, "FEED")
		pd.RemoteFeeds = feeds
		pd.RemoteItems = items
		pd.Error = err.Error()
		pd.BoardDone = true
		s.render(w, "feed.html", pd)
		return
	}
	// Best-effort immediate sync; errors surface via last_error on the source.
	_ = s.syncRemoteFeed(r.Context(), feed.ID)
	http.Redirect(w, r, "/feed", http.StatusSeeOther)
}

func (s *Server) handleRemoteFeedFetchSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.notFound(w, r)
		return
	}
	_ = s.syncRemoteFeed(r.Context(), id)
	http.Redirect(w, r, "/feed", http.StatusSeeOther)
}

func (s *Server) handleRemoteFeedUnfollowSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.notFound(w, r)
		return
	}
	if err := s.store.SetRemoteFeedActive(r.Context(), id, false); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/feed", http.StatusSeeOther)
}

func (s *Server) handleRemoteFeedSyncAll(w http.ResponseWriter, r *http.Request) {
	s.syncAllRemoteFeeds(r.Context())
	http.Redirect(w, r, "/feed", http.StatusSeeOther)
}

func (s *Server) handleRemoteFeedRepost(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.notFound(w, r)
		return
	}
	// Confirm the remote item exists before CreateRepost so missing rows 404.
	remote, err := s.store.GetRemoteFeedItem(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if remote == nil {
		s.notFound(w, r)
		return
	}
	localID, _, err := s.store.CreateRepost(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if localID > 0 {
		s.invalidateSiteCache()
	}
	http.Redirect(w, r, "/item/"+strconv.FormatInt(localID, 10), http.StatusSeeOther)
}
