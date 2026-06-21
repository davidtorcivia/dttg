// Package config loads runtime configuration from the environment.
//
// Everything is env-driven so the same binary runs locally, in Docker, and
// behind a Cloudflare tunnel without recompiling. Media URLs are resolved from
// MediaBaseURL (the R2 custom domain) when set, otherwise served locally.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Addr     string // listen address, e.g. ":8080"
	DataDir  string // root for db + media (mounted volume in Docker)
	DBPath   string // sqlite file path
	MediaDir string // local media archive root (source of truth)

	BaseURL      string // canonical public origin, e.g. https://archive.example.com
	MediaBaseURL string // public media origin (R2 custom domain), e.g. https://media.example.com; empty => serve /media locally

	Dev bool // relaxes cookie Secure flag for http://localhost

	TrustProxy bool // trust X-Forwarded-For / X-Real-IP (set when running behind a reverse proxy)

	SiteTitle   string // shown center of the header bar
	SiteTagline string // shown left of the header bar (e.g. "INDEX")

	// R2 (used by the ingest mirror in M2; read here so config stays in one place).
	R2AccountID string
	R2Bucket    string
	R2AccessKey string
	R2SecretKey string
	R2Endpoint  string // override; default derived from account id

	// R2 rolling DB backups (separate, private bucket).
	R2BackupBucket      string
	BackupRetentionDays int
	BackupIntervalHours int
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func Load() Config {
	dataDir := env("DNTTG_DATA_DIR", "data")
	c := Config{
		Addr:         env("DNTTG_ADDR", ":8080"),
		DataDir:      dataDir,
		DBPath:       env("DNTTG_DB_PATH", filepath.Join(dataDir, "dnttg.db")),
		MediaDir:     env("DNTTG_MEDIA_DIR", filepath.Join(dataDir, "media")),
		BaseURL:      strings.TrimRight(env("DNTTG_BASE_URL", "http://localhost:8080"), "/"),
		MediaBaseURL: strings.TrimRight(env("DNTTG_MEDIA_BASE_URL", ""), "/"),
		Dev:          env("DNTTG_DEV", "") != "",
		TrustProxy:   env("DNTTG_TRUST_PROXY", "") != "",
		SiteTitle:    env("DNTTG_SITE_TITLE", "DO NOT TOUCH THE GLASS"),
		SiteTagline:  env("DNTTG_SITE_TAGLINE", "INDEX"),
		R2AccountID:  env("R2_ACCOUNT_ID", ""),
		R2Bucket:     env("R2_BUCKET", ""),
		R2AccessKey:  env("R2_ACCESS_KEY_ID", ""),
		R2SecretKey:  env("R2_SECRET_ACCESS_KEY", ""),
		R2Endpoint:   env("R2_ENDPOINT", ""),

		R2BackupBucket:      env("R2_BACKUP_BUCKET", ""),
		BackupRetentionDays: atoiDefault(env("DNTTG_BACKUP_RETENTION_DAYS", ""), 14),
		BackupIntervalHours: atoiDefault(env("DNTTG_BACKUP_INTERVAL_HOURS", ""), 24),
	}
	return c
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return def
}

// HTTPSBase reports whether the public origin is https (used for the Secure cookie flag).
func (c Config) HTTPSBase() bool { return strings.HasPrefix(c.BaseURL, "https://") && !c.Dev }

// R2Enabled reports whether the media R2 mirror is configured.
func (c Config) R2Enabled() bool {
	return c.R2AccessKey != "" && c.R2SecretKey != "" && c.R2Bucket != ""
}

// BackupsEnabled reports whether R2 rolling DB backups are configured.
func (c Config) BackupsEnabled() bool {
	return c.R2AccessKey != "" && c.R2SecretKey != "" && c.R2BackupBucket != ""
}
