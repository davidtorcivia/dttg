// Package backup creates rolling SQLite snapshots and stores them in a private
// R2 bucket, pruning snapshots older than the retention window.
package backup

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"donottouchtheglass/internal/store"
)

type Config struct {
	AccountID string
	Bucket    string // private backups bucket (NOT the public media bucket)
	AccessKey string
	SecretKey string
	Endpoint  string
	Retention time.Duration
	Interval  time.Duration
}

type Backuper struct {
	cfg     Config
	store   *store.Store
	client  *minio.Client
	dataDir string

	mu      sync.Mutex
	last    time.Time
	count   int
	lastErr string
}

func New(st *store.Store, dataDir string, cfg Config) (*Backuper, error) {
	if cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("backup: missing creds/bucket")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		if cfg.AccountID == "" {
			return nil, fmt.Errorf("backup: need R2_ACCOUNT_ID or R2_ENDPOINT")
		}
		endpoint = cfg.AccountID + ".r2.cloudflarestorage.com"
	}
	endpoint = strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"), "/")
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: true,
		Region: "auto",
	})
	if err != nil {
		return nil, err
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 14 * 24 * time.Hour
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 24 * time.Hour
	}
	// Best-effort: create the backups bucket if it doesn't exist yet (a common
	// reason backups silently never appear). If the token can't check/create it,
	// log and carry on — RunOnce will surface the real PutObject error.
	if exists, e := client.BucketExists(context.Background(), cfg.Bucket); e != nil {
		log.Printf("backup: could not verify bucket %q (token may lack access): %v", cfg.Bucket, e)
	} else if !exists {
		if me := client.MakeBucket(context.Background(), cfg.Bucket, minio.MakeBucketOptions{Region: "auto"}); me != nil {
			log.Printf("backup: bucket %q is missing and could not be created: %v", cfg.Bucket, me)
		} else {
			log.Printf("backup: created bucket %q", cfg.Bucket)
		}
	}
	return &Backuper{cfg: cfg, store: st, client: client, dataDir: dataDir}, nil
}

// RunOnce snapshots the DB, uploads it, prunes old snapshots, and records status.
func (b *Backuper) RunOnce(ctx context.Context) error {
	err := b.run(ctx)
	b.mu.Lock()
	if err != nil {
		b.lastErr = err.Error()
	} else {
		b.lastErr = ""
		b.last = time.Now()
		b.count++
	}
	b.mu.Unlock()
	return err
}

func (b *Backuper) run(ctx context.Context) error {
	tmp := filepath.Join(b.dataDir, fmt.Sprintf(".backup-%d.db", time.Now().UnixNano()))
	if err := b.store.BackupTo(ctx, tmp); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()

	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	key := "db/dnttg-" + time.Now().UTC().Format("20060102-150405") + ".db"
	if _, err := b.client.PutObject(ctx, b.cfg.Bucket, key, f, st.Size(),
		minio.PutObjectOptions{ContentType: "application/x-sqlite3"}); err != nil {
		return fmt.Errorf("upload to bucket %q: %w", b.cfg.Bucket, err)
	}
	log.Printf("backup: uploaded %s (%d bytes) to %q", key, st.Size(), b.cfg.Bucket)
	// A successful upload is a successful backup — a prune hiccup must not mask it.
	if err := b.prune(ctx); err != nil {
		log.Printf("backup: prune failed (snapshot is safe): %v", err)
	}
	return nil
}

func (b *Backuper) prune(ctx context.Context) error {
	cutoff := time.Now().Add(-b.cfg.Retention)
	for obj := range b.client.ListObjects(ctx, b.cfg.Bucket,
		minio.ListObjectsOptions{Prefix: "db/", Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if obj.LastModified.Before(cutoff) {
			if err := b.client.RemoveObject(ctx, b.cfg.Bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
				log.Printf("backup: prune %s: %v", obj.Key, err)
			}
		}
	}
	return nil
}

// LatestRemote reports the most recent backup time and the total number of
// snapshots currently in the R2 bucket. Unlike Status (in-memory, per-session)
// this reflects reality across restarts/redeploys.
func (b *Backuper) LatestRemote(ctx context.Context) (time.Time, int, error) {
	var latest time.Time
	count := 0
	for obj := range b.client.ListObjects(ctx, b.cfg.Bucket,
		minio.ListObjectsOptions{Prefix: "db/", Recursive: true}) {
		if obj.Err != nil {
			return latest, count, obj.Err
		}
		count++
		if obj.LastModified.After(latest) {
			latest = obj.LastModified
		}
	}
	return latest, count, nil
}

// Start launches the periodic backup loop until ctx is cancelled.
func (b *Backuper) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(b.cfg.Interval)
		defer t.Stop()
		// Back up ~1h after boot, then on the regular interval. The hour delay keeps
		// dev restarts/redeploys from churning out near-identical snapshots.
		first := time.NewTimer(time.Hour)
		defer first.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-first.C:
				if err := b.RunOnce(ctx); err != nil {
					log.Printf("backup: %v", err)
				}
			case <-t.C:
				if err := b.RunOnce(ctx); err != nil {
					log.Printf("backup: %v", err)
				}
			}
		}
	}()
}

// Status reports the last successful backup time, count, and last error.
func (b *Backuper) Status() (time.Time, int, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last, b.count, b.lastErr
}

// RetentionDays returns the configured retention window in whole days.
func (b *Backuper) RetentionDays() int { return int(b.cfg.Retention.Hours() / 24) }
