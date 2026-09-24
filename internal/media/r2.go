package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// R2Config configures a Cloudflare R2 (S3-compatible) bucket.
type R2Config struct {
	AccountID  string
	Bucket     string
	AccessKey  string
	SecretKey  string
	Endpoint   string // optional override; default <account>.r2.cloudflarestorage.com
	PublicBase string // bucket custom domain, e.g. https://media.example.com
}

// NewR2Client builds an S3 client for R2 (shared with the backup bucket).
func NewR2Client(cfg R2Config) (*minio.Client, error) {
	if cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("r2: missing access key / secret / bucket")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		if cfg.AccountID == "" {
			return nil, fmt.Errorf("r2: need R2_ACCOUNT_ID or R2_ENDPOINT")
		}
		endpoint = cfg.AccountID + ".r2.cloudflarestorage.com"
	}
	endpoint = strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"), "/")
	return minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: true,
		Region: "auto",
	})
}

// R2Store puts/reads objects in an R2 bucket and resolves public URLs via the
// bucket's custom domain. It is only used behind MirrorStore.
type R2Store struct {
	client     *minio.Client
	bucket     string
	publicBase string
}

func NewR2Store(cfg R2Config) (*R2Store, error) {
	client, err := NewR2Client(cfg)
	if err != nil {
		return nil, err
	}
	return &R2Store{client: client, bucket: cfg.Bucket, publicBase: strings.TrimRight(cfg.PublicBase, "/")}, nil
}

func (r *R2Store) put(ctx context.Context, key, contentType string, data []byte) error {
	_, err := r.client.PutObject(ctx, r.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType:  contentType,
		CacheControl: "public, max-age=31536000, immutable",
	})
	return err
}

// open fetches key. GetObject is lazy, so Stat forces the request here: a
// missing object must fail now (letting MirrorStore fall back to local), not on
// the first Read.
func (r *R2Store) open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := r.client.GetObject(ctx, r.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, err
	}
	return obj, nil
}

func (r *R2Store) url(key string) string { return r.publicBase + "/" + strings.TrimLeft(key, "/") }

func (r *R2Store) delete(ctx context.Context, key string) error {
	return r.client.RemoveObject(ctx, r.bucket, key, minio.RemoveObjectOptions{})
}

func (r *R2Store) list(ctx context.Context) ([]ObjectInfo, error) {
	var out []ObjectInfo
	for obj := range r.client.ListObjects(ctx, r.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return out, obj.Err
		}
		out = append(out, ObjectInfo{Key: obj.Key, Size: obj.Size})
	}
	return out, nil
}
