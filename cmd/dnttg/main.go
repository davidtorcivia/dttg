// Command dnttg is the DO NOT TOUCH THE GLASS server.
//
// Subcommands:
//
//	dnttg serve                      run the HTTP server (default)
//	dnttg migrate                    apply migrations and exit
//	dnttg ready                      exit 0 if the DB is reachable
//	dnttg seed                       insert demo content if the archive is empty
//	dnttg reconcile                  move blobs to the tier their visibility calls for
//	                                 (public → R2, private → local only)
//	dnttg localize-private-media     alias of reconcile
//	dnttg backfill-variants          generate the ~400px small variant for older images
//	dnttg backup                     snapshot the DB to the R2 backups bucket (+ prune old)
//	dnttg reset-content              delete all items/media/tags/categories (keeps password + tokens)
//	dnttg set-password [pw]          set/replace the admin login password (stdin if omitted)
//	dnttg token [name]               mint an API token (alias of token mint)
//	dnttg token mint [name]          mint an API token for the extension/bookmarklet
//	dnttg token list                 list API token names and timestamps
//	dnttg token revoke <id|name>     revoke an API token by id or exact name
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	ctx := context.Background()

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	// run opens the store for a one-shot subcommand and exits non-zero on error.
	run := func(fn func(st *store.Store) error) {
		st := mustStore(cfg)
		defer st.Close()
		if err := fn(st); err != nil {
			log.Fatal(err)
		}
	}

	switch cmd {
	case "serve":
		serve(cfg)
	case "migrate":
		run(func(*store.Store) error { fmt.Println("migrations applied"); return nil })
	case "ready":
		run(func(st *store.Store) error { return st.Ping(ctx) })
	case "seed":
		run(func(st *store.Store) error { return seed(ctx, st, ingest.New(st, mustMedia(cfg))) })
	case "reconcile", "localize-private-media":
		run(func(st *store.Store) error { return reconcile(ctx, st, mustMedia(cfg)) })
	case "backfill-variants":
		run(func(st *store.Store) error { return backfillVariants(ctx, st, mustMedia(cfg)) })
	case "backup":
		if !cfg.BackupsEnabled() {
			log.Fatal("backups not configured (set R2_* and R2_BACKUP_BUCKET)")
		}
		run(func(st *store.Store) error {
			bp, err := newBackuper(cfg, st)
			if err == nil {
				err = bp.RunOnce(ctx)
			}
			return err
		})
		fmt.Println("backup complete")
	case "reset-content":
		run(func(st *store.Store) error { return st.ResetContent(ctx) })
		fmt.Println("archive content cleared (password + tokens kept)")
	case "set-password":
		run(func(st *store.Store) error { return setPassword(ctx, st) })
	case "token":
		run(func(st *store.Store) error { return runToken(ctx, st, os.Args[2:]) })
	default:
		log.Fatalf("unknown command %q (serve|migrate|ready|seed|reconcile|backfill-variants|backup|reset-content|set-password|token)", cmd)
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
	if !cfg.R2Enabled() {
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
	srv.Start(ctx)

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

// setPassword sets the admin password. Priority: argv (less safe: shell history
// / process list), else stdin — an interactive terminal prompts without echo.
func setPassword(ctx context.Context, st *store.Store) error {
	var pw string
	switch fd := int(os.Stdin.Fd()); {
	case len(os.Args) >= 3:
		pw = os.Args[2]
		log.Printf("warning: password on argv is less safe (shell history / process list); prefer stdin or a prompt")
	case term.IsTerminal(fd):
		fmt.Fprint(os.Stderr, "New password: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		pw = string(b)
	default:
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		pw = strings.TrimRight(string(b), "\r\n")
	}
	if pw == "" {
		return errors.New("password must not be empty")
	}
	hash, err := web.HashPassword(pw)
	if err != nil {
		return err
	}
	if err := st.SetSetting(ctx, "password_hash", hash); err != nil {
		return err
	}
	fmt.Println("password updated")
	return nil
}

func runToken(ctx context.Context, st *store.Store, args []string) error {
	sub := "mint" // bare `token [name]` mints
	if len(args) > 0 && (args[0] == "list" || args[0] == "revoke" || args[0] == "mint") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list":
		toks, err := st.ListTokens(ctx)
		if err != nil {
			return err
		}
		if len(toks) == 0 {
			fmt.Println("no API tokens")
		}
		for _, t := range toks {
			last := "never"
			if t.LastUsedAt != nil {
				last = t.LastUsedAt.Format(time.RFC3339)
			}
			fmt.Printf("%d\t%s\tcreated=%s\tlast_used=%s\n", t.ID, t.Name, t.CreatedAt.Format(time.RFC3339), last)
		}
	case "revoke":
		if len(args) < 1 {
			return errors.New("usage: dnttg token revoke <id|name>")
		}
		if err := st.RevokeToken(ctx, args[0]); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no token matching %q", args[0])
		} else if err != nil {
			return err
		}
		fmt.Printf("revoked token %q\n", args[0])
	case "mint":
		name := "default"
		if len(args) > 0 {
			name = args[0]
		}
		tok := web.NewToken()
		if _, err := st.CreateToken(ctx, name, web.HashToken(tok)); err != nil {
			return err
		}
		fmt.Printf("API token (%s) — store it now, it will not be shown again:\n\n  %s\n\n", name, tok)
	}
	return nil
}
