// Command dnttg is the DO NOT TOUCH THE GLASS server.
//
// Subcommands:
//
//	dnttg serve                      run the HTTP server (default)
//	dnttg migrate                    apply migrations and exit
//	dnttg ready                      exit 0 if the DB is reachable
//	dnttg seed                       insert demo content if the archive is empty
//	dnttg reconcile                  push local-only refined variants up to R2 (backfill)
//	dnttg backfill-variants          generate the ~400px small variant for older images
//	dnttg localize-private-media     pull private media off R2 onto local disk only
//	dnttg backup                     snapshot the DB to the R2 backups bucket (+ prune old)
//	dnttg reset-content              delete all items/media/tags/categories (keeps password + tokens)
//	dnttg set-password [pw]          set/replace the admin login password (stdin if omitted)
//	dnttg token [name]               mint an API token (alias of token mint)
//	dnttg token mint [name]          mint an API token for the extension/bookmarklet
//	dnttg token list                 list API token names and timestamps
//	dnttg token revoke <id|name>     revoke an API token by id or exact name
package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

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
	case "ready":
		st := mustStore(cfg)
		defer st.Close()
		if err := st.Ping(context.Background()); err != nil {
			log.Fatal(err)
		}
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
	case "backfill-variants":
		st := mustStore(cfg)
		defer st.Close()
		if err := backfillVariants(context.Background(), st, mustMedia(cfg)); err != nil {
			log.Fatal(err)
		}
	case "localize-private-media":
		st := mustStore(cfg)
		defer st.Close()
		if err := localizePrivateMedia(context.Background(), st, mustMedia(cfg)); err != nil {
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
		pw, fromArg, err := readPasswordArg()
		if err != nil {
			log.Fatal(err)
		}
		if pw == "" {
			log.Fatal("password must not be empty")
		}
		if fromArg {
			log.Printf("warning: password on argv is less safe (shell history / process list); prefer stdin or a prompt")
		}
		st := mustStore(cfg)
		defer st.Close()
		hash, err := web.HashPassword(pw)
		if err != nil {
			log.Fatal(err)
		}
		if err := st.SetSetting(context.Background(), "password_hash", hash); err != nil {
			log.Fatal(err)
		}
		fmt.Println("password updated")
	case "token":
		runToken(cfg, os.Args[2:])
	default:
		log.Fatalf("unknown command %q (serve|migrate|ready|seed|reconcile|backfill-variants|localize-private-media|backup|reset-content|set-password|token)", cmd)
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

	// Cancelled on SIGINT/SIGTERM — drives graceful shutdown + background loops.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var bc web.BackupController
	if cfg.BackupsEnabled() {
		bp, err := newBackuper(cfg, st)
		if err != nil {
			log.Fatalf("backup: %v", err)
		}
		bp.Start(ctx)
		bc = bp
		log.Printf("backups: enabled (bucket %s, every %dh, keep %dd)",
			cfg.R2BackupBucket, cfg.BackupIntervalHours, cfg.BackupRetentionDays)
	}

	srv, err := web.New(cfg, st, ms, ingest.New(st, ms), bc)
	if err != nil {
		log.Fatal(err)
	}

	// Periodically purge expired sessions and pending share blobs.
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			if n, err := st.PurgeExpiredSessions(context.Background()); err != nil {
				log.Printf("session purge: %v", err)
			} else if n > 0 {
				log.Printf("purged %d expired sessions", n)
			}
			if expired, err := st.ListExpiredPendingShares(context.Background()); err != nil {
				log.Printf("pending share list: %v", err)
			} else {
				for _, p := range expired {
					if p.FileKey != "" {
						_ = os.Remove(filepath.Join(cfg.DataDir, "pending", p.FileKey))
					}
				}
				if n, err := st.PurgeExpiredPendingShares(context.Background()); err != nil {
					log.Printf("pending share purge: %v", err)
				} else if n > 0 {
					log.Printf("purged %d expired pending shares", n)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No Read/WriteTimeout on purpose: /media streams large files and admin
		// uploads can be slow. ReadHeaderTimeout (Slowloris) + the per-handler 30MB
		// cap + the fronting reverse proxy cover the slow-client risk.
	}

	go func() {
		log.Printf("DO NOT TOUCH THE GLASS — listening on %s (public %s)", cfg.Addr, cfg.BaseURL)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}


// readPasswordArg returns the password for set-password.
// Priority: argv form (less safe), else stdin — interactive terminal prompts without echo.
func readPasswordArg() (pw string, fromArg bool, err error) {
	if len(os.Args) >= 3 {
		return os.Args[2], true, nil
	}
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "New password: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", false, err
		}
		return string(b), false, nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", false, err
	}
	return strings.TrimRight(string(b), "\r\n"), false, nil
}

func runToken(cfg config.Config, args []string) {
	sub := "mint"
	if len(args) > 0 {
		switch args[0] {
		case "list", "revoke", "mint":
			sub = args[0]
			args = args[1:]
		default:
			// bare `token [name]` keeps mint behavior
			sub = "mint"
		}
	}
	st := mustStore(cfg)
	defer st.Close()
	ctx := context.Background()

	switch sub {
	case "list":
		toks, err := st.ListTokens(ctx)
		if err != nil {
			log.Fatal(err)
		}
		if len(toks) == 0 {
			fmt.Println("no API tokens")
			return
		}
		for _, t := range toks {
			last := "never"
			if t.LastUsedAt != nil {
				last = t.LastUsedAt.Format(time.RFC3339)
			}
			fmt.Printf("%d\t%s\tcreated=%s\tlast_used=%s\n",
				t.ID, t.Name, t.CreatedAt.Format(time.RFC3339), last)
		}
	case "revoke":
		if len(args) < 1 {
			log.Fatal("usage: dnttg token revoke <id|name>")
		}
		if err := st.RevokeToken(ctx, args[0]); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				log.Fatalf("no token matching %q", args[0])
			}
			log.Fatal(err)
		}
		fmt.Printf("revoked token %q\n", args[0])
	case "mint":
		name := "default"
		if len(args) > 0 {
			name = args[0]
		}
		tok := web.NewToken()
		if _, err := st.CreateToken(ctx, name, web.HashToken(tok)); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("API token (%s) — store it now, it will not be shown again:\n\n  %s\n\n", name, tok)
	}
}

// localizePrivateMedia copies private-item blobs off R2 onto local disk and
// clears on_r2 so private media is never served from a public CDN URL.
func localizePrivateMedia(ctx context.Context, st *store.Store, ms media.Store) error {
	rows, err := st.ListPrivateMediaOnR2(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no private media on R2")
		return nil
	}

	// Must have R2 so we can delete private objects after localizing.
	mirror, ok := ms.(*media.MirrorStore)
	if !ok || !mirror.HasR2() {
		return fmt.Errorf("R2 not configured — set R2_* env vars before localize-private-media")
	}
	type privatePutter interface {
		PutPrivate(ctx context.Context, key, contentType string, r io.Reader, size int64) error
	}
	pp, ok := ms.(privatePutter)
	if !ok {
		return fmt.Errorf("media store does not support PutPrivate")
	}

	var done, failed int
	for _, m := range rows {
		if err := localizeOne(ctx, st, ms, pp, mirror, m); err != nil {
			log.Printf("localize %s (id=%d): %v — leaving row unchanged", m.StorageKey, m.ID, err)
			failed++
			continue
		}
		done++
		fmt.Printf("localized %s\n", m.StorageKey)
	}
	fmt.Printf("localize-private-media: %d ok, %d failed, %d total\n", done, failed, len(rows))
	return nil
}

func localizeOne(ctx context.Context, st *store.Store, ms media.Store, pp interface {
	PutPrivate(ctx context.Context, key, contentType string, r io.Reader, size int64) error
}, mirror *media.MirrorStore, m store.Media) error {
	rc, err := ms.Open(m.StorageKey)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	ct := m.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	if err := pp.PutPrivate(ctx, m.StorageKey, ct, bytes.NewReader(data), int64(len(data))); err != nil {
		return fmt.Errorf("put local: %w", err)
	}
	if mirror != nil {
		if err := mirror.DeleteR2(ctx, m.StorageKey); err != nil {
			return fmt.Errorf("delete r2: %w", err)
		}
	}
	if err := st.MarkMediaLocalOnly(ctx, m.ID); err != nil {
		return fmt.Errorf("mark local: %w", err)
	}
	return nil
}
