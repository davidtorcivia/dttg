// Command dnttg is the DO NOT TOUCH THE GLASS server.
//
// Subcommands:
//
//	dnttg serve                 run the HTTP server (default)
//	dnttg migrate               apply migrations and exit
//	dnttg seed                  insert demo content if the archive is empty
//	dnttg reconcile             push local-only refined variants up to R2 (backfill)
//	dnttg backup                snapshot the DB to the R2 backups bucket (+ prune old)
//	dnttg reset-content         delete all items/media/tags/categories (keeps password + tokens)
//	dnttg set-password <pw>     set/replace the admin login password
//	dnttg token [name]          mint an API token for the extension/bookmarklet
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"donottouchtheglass/internal/backup"
	"donottouchtheglass/internal/config"
	"donottouchtheglass/internal/ingest"
	"donottouchtheglass/internal/media"
	"donottouchtheglass/internal/store"
	"donottouchtheglass/internal/web"
)

func main() {
	log.SetFlags(log.Ltime)
	cfg := config.Load()

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "serve":
		serve(cfg)
	case "migrate":
		st := mustStore(cfg)
		defer st.Close()
		fmt.Println("migrations applied")
	case "seed":
		st := mustStore(cfg)
		defer st.Close()
		ms := mustMedia(cfg)
		if err := seed(context.Background(), st, ingest.New(st, ms)); err != nil {
			log.Fatal(err)
		}
	case "reconcile":
		st := mustStore(cfg)
		defer st.Close()
		if err := reconcile(context.Background(), st, mustMedia(cfg)); err != nil {
			log.Fatal(err)
		}
	case "backup":
		if !cfg.BackupsEnabled() {
			log.Fatal("backups not configured (set R2_* and R2_BACKUP_BUCKET)")
		}
		st := mustStore(cfg)
		defer st.Close()
		bp, err := newBackuper(cfg, st)
		if err != nil {
			log.Fatal(err)
		}
		if err := bp.RunOnce(context.Background()); err != nil {
			log.Fatal(err)
		}
		fmt.Println("backup complete")
	case "reset-content":
		st := mustStore(cfg)
		defer st.Close()
		if err := st.ResetContent(context.Background()); err != nil {
			log.Fatal(err)
		}
		fmt.Println("archive content cleared (password + tokens kept)")
	case "set-password":
		if len(os.Args) < 3 {
			log.Fatal("usage: dnttg set-password <password>")
		}
		st := mustStore(cfg)
		defer st.Close()
		hash, err := web.HashPassword(os.Args[2])
		if err != nil {
			log.Fatal(err)
		}
		if err := st.SetSetting(context.Background(), "password_hash", hash); err != nil {
			log.Fatal(err)
		}
		fmt.Println("password updated")
	case "token":
		name := "default"
		if len(os.Args) > 2 {
			name = os.Args[2]
		}
		st := mustStore(cfg)
		defer st.Close()
		tok := web.NewToken()
		if _, err := st.CreateToken(context.Background(), name, web.HashToken(tok)); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("API token (%s) — store it now, it will not be shown again:\n\n  %s\n\n", name, tok)
	default:
		log.Fatalf("unknown command %q (serve|migrate|seed|reconcile|backup|reset-content|set-password|token)", cmd)
	}
}

func mustStore(cfg config.Config) *store.Store {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	return st
}

// mustMedia builds the media store: a local archive, wrapped in an R2 mirror
// when R2 credentials are configured.
func mustMedia(cfg config.Config) media.Store {
	local, err := media.NewLocalStore(cfg.MediaDir, "/media")
	if err != nil {
		log.Fatal(err)
	}
	if cfg.R2AccessKey == "" || cfg.R2SecretKey == "" || cfg.R2Bucket == "" {
		return local
	}
	r2, err := media.NewR2Store(media.R2Config{
		AccountID:  cfg.R2AccountID,
		Bucket:     cfg.R2Bucket,
		AccessKey:  cfg.R2AccessKey,
		SecretKey:  cfg.R2SecretKey,
		Endpoint:   cfg.R2Endpoint,
		PublicBase: cfg.MediaBaseURL,
	})
	if err != nil {
		log.Fatalf("r2: %v", err)
	}
	log.Printf("media: local archive + R2 mirror (%s)", cfg.R2Bucket)
	return media.NewMirrorStore(local, r2)
}

func newBackuper(cfg config.Config, st *store.Store) (*backup.Backuper, error) {
	return backup.New(st, cfg.DataDir, backup.Config{
		AccountID: cfg.R2AccountID,
		Bucket:    cfg.R2BackupBucket,
		AccessKey: cfg.R2AccessKey,
		SecretKey: cfg.R2SecretKey,
		Endpoint:  cfg.R2Endpoint,
		Retention: time.Duration(cfg.BackupRetentionDays) * 24 * time.Hour,
		Interval:  time.Duration(cfg.BackupIntervalHours) * time.Hour,
	})
}

func serve(cfg config.Config) {
	st := mustStore(cfg)
	defer st.Close()

	ms := mustMedia(cfg)

	var bc web.BackupController
	if cfg.BackupsEnabled() {
		bp, err := newBackuper(cfg, st)
		if err != nil {
			log.Fatalf("backup: %v", err)
		}
		bp.Start(context.Background())
		bc = bp
		log.Printf("backups: enabled (bucket %s, every %dh, keep %dd)",
			cfg.R2BackupBucket, cfg.BackupIntervalHours, cfg.BackupRetentionDays)
	}

	srv, err := web.New(cfg, st, ms, ingest.New(st, ms), bc)
	if err != nil {
		log.Fatal(err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("DO NOT TOUCH THE GLASS — listening on %s (public %s)", cfg.Addr, cfg.BaseURL)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
