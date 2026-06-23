package web

import (
	"context"
	"net/url"
	"strings"
)

// siteIdentity holds the deployment-specific branding that the admin Settings
// page can edit at runtime. Each field defaults to its config (env) value and is
// overridden by a DB setting when one is present. It is published behind an
// atomic pointer so request handlers read it lock-free; the admin save path
// rebuilds it via loadSite.
type siteIdentity struct {
	Title       string
	Tagline     string
	BaseURL     string // canonical origin, no trailing slash
	Description string // tagline appended after the title in share/SEO text
}

// loadSite reads the editable site settings from the store (falling back to the
// env config) and publishes them for handlers to read.
func (s *Server) loadSite(ctx context.Context) {
	get := func(key, def string) string {
		if v, err := s.store.GetSetting(ctx, key); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}
	s.site.Store(&siteIdentity{
		Title:       get("site_title", s.cfg.SiteTitle),
		Tagline:     get("site_tagline", s.cfg.SiteTagline),
		BaseURL:     strings.TrimRight(get("base_url", s.cfg.BaseURL), "/"),
		Description: get("site_description", s.cfg.SiteDescription),
	})
}

// siteID returns the current identity, falling back to raw config if loadSite
// hasn't run yet (e.g. very early in startup or in lightweight tests).
func (s *Server) siteID() siteIdentity {
	if id := s.site.Load(); id != nil {
		return *id
	}
	return siteIdentity{
		Title:       s.cfg.SiteTitle,
		Tagline:     s.cfg.SiteTagline,
		BaseURL:     strings.TrimRight(s.cfg.BaseURL, "/"),
		Description: s.cfg.SiteDescription,
	}
}

func (s *Server) siteTitle() string   { return s.siteID().Title }
func (s *Server) siteTagline() string { return s.siteID().Tagline }
func (s *Server) siteBaseURL() string { return s.siteID().BaseURL }

// metaDescription composes the default page/feed description: "<title> — <tagline>".
func (s *Server) metaDescription() string {
	id := s.siteID()
	if strings.TrimSpace(id.Description) == "" {
		return id.Title
	}
	return id.Title + " — " + id.Description
}

// siteHost is the bare host of the base URL, used as the OG card's URL kicker.
func (s *Server) siteHost() string {
	base := s.siteBaseURL()
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}
