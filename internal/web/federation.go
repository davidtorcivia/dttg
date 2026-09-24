package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
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
			RawJSON:        string(ir),
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
	now := time.Now().UTC()
	res, err := s.ingest.Fetch(ctx, feed.FeedURL, ingest.FetchOptions{
		Accept:       "application/feed+json, application/json;q=0.9",
		ETag:         feed.ETag,
		LastModified: feed.LastModified,
		MaxBytes:     remoteFeedMaxBytes,
	})
	if err == nil && res.NotModified {
		return s.store.MarkRemoteFeedChecked(ctx, feedID, now)
	}
	var parsed remoteFeedParsed
	if err == nil {
		parsed, err = parseRemoteJSONFeed(feed.FeedURL, res.Body, now)
	}
	if err == nil {
		upd := parsed.Update
		upd.ETag, upd.LastModified, upd.LastFetchedAt, upd.LastSuccessAt = res.ETag, res.LastModified, now, now
		_, err = s.store.SaveRemoteFeedFetch(ctx, feedID, upd, parsed.Items)
	}
	if err != nil {
		_ = s.store.SaveRemoteFeedError(ctx, feedID, now, err.Error())
	}
	return err
}

func (s *Server) handleRemoteFeedPage(w http.ResponseWriter, r *http.Request) {
	s.renderFeedPage(w, r, http.StatusOK, "")
}

func (s *Server) renderFeedPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	f := store.RemoteFeedItemFilter{Limit: remoteFeedPage, ActiveOnly: true}
	f.BeforePublished, f.BeforeID = parseCursor(r)
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
	pd.RemoteFeeds, pd.RemoteItems, pd.Error = feeds, items, msg
	if n := len(items); n > 0 {
		pd.CursorCreated, pd.CursorID = items[n-1].PublishedAt.Unix(), items[n-1].ID
	}
	pd.BoardDone = len(items) < remoteFeedPage
	s.render(w, status, "feed.html", pd)
}

func (s *Server) handleRemoteFeedAddSource(w http.ResponseWriter, r *http.Request) {
	feedURL := strings.TrimSpace(r.FormValue("feed_url"))
	if feedURL = cleanFeedURL(feedURL, feedURL); feedURL == "" {
		s.renderFeedPage(w, r, http.StatusBadRequest, "Feed URL must be an absolute http(s) URL")
		return
	}
	feed, _, err := s.store.AddRemoteFeed(r.Context(), feedURL)
	if err != nil {
		s.renderFeedPage(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// Best-effort immediate sync; errors surface via last_error on the source.
	_ = s.syncRemoteFeed(r.Context(), feed.ID)
	http.Redirect(w, r, "/feed", http.StatusSeeOther)
}

func (s *Server) handleRemoteFeedFetchSource(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(r); ok {
		_ = s.syncRemoteFeed(r.Context(), id)
	}
	http.Redirect(w, r, "/feed", http.StatusSeeOther)
}

func (s *Server) handleRemoteFeedUnfollowSource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
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
	feeds, err := s.store.ListRemoteFeeds(r.Context(), true)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, f := range feeds {
		if err := s.syncRemoteFeed(r.Context(), f.ID); err != nil {
			log.Printf("sync remote feed %d (%s): %v", f.ID, f.FeedURL, err)
		}
	}
	http.Redirect(w, r, "/feed", http.StatusSeeOther)
}

func (s *Server) handleRemoteFeedRepost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	// Confirm the remote item exists before CreateRepost so missing rows 404.
	if remote, err := s.store.GetRemoteFeedItem(r.Context(), id); err != nil || remote == nil {
		if err != nil {
			s.serverError(w, r, err)
		} else {
			s.notFound(w, r)
		}
		return
	}
	localID, _, err := s.store.CreateRepost(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.invalidateSiteCache()
	http.Redirect(w, r, itemPath(localID), http.StatusSeeOther)
}
