package web

import (
	"context"
	"testing"
)

func TestSiteIdentityOverridesAndFallback(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	// Before any DB settings, identity comes from the env config.
	if got := s.siteTitle(); got != "TEST ARCHIVE" {
		t.Fatalf("default title = %q", got)
	}
	if got := s.siteBaseURL(); got != "http://localhost:8080" {
		t.Fatalf("default base = %q", got)
	}

	// Admin-set values override, and are normalized (trailing slash trimmed).
	for k, v := range map[string]string{
		"site_title":       "My Gallery",
		"base_url":         "https://gallery.example.com/",
		"site_description": "curated things",
	} {
		if err := s.store.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	s.loadSite(ctx)

	if got := s.siteTitle(); got != "My Gallery" {
		t.Errorf("title = %q", got)
	}
	if got := s.siteBaseURL(); got != "https://gallery.example.com" {
		t.Errorf("base = %q", got)
	}
	if got := s.siteHost(); got != "gallery.example.com" {
		t.Errorf("host = %q", got)
	}
	if got := s.metaDescription(); got != "My Gallery — curated things" {
		t.Errorf("description = %q", got)
	}
	if got := s.absURL("/feed.json"); got != "https://gallery.example.com/feed.json" {
		t.Errorf("absURL = %q", got)
	}

	// Clearing a setting reverts to the env default.
	if err := s.store.SetSetting(ctx, "site_title", ""); err != nil {
		t.Fatal(err)
	}
	s.loadSite(ctx)
	if got := s.siteTitle(); got != "TEST ARCHIVE" {
		t.Errorf("cleared title should revert to default, got %q", got)
	}
}
