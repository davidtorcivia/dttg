package media

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// R2Config configures the Cloudflare R2 (S3-compatible) backend.
type R2Config struct {
	AccountID  string
	Bucket     string
	AccessKey  string
	SecretKey  string
	Endpoint   string // optional override; default <account>.r2.cloudflarestorage.com
	PublicBase string // bucket custom domain, e.g. https://media.example.com
}

// R2Store puts/reads objects in an R2 bucket and resolves public URLs via the
// bucket's custom domain.
type R2Store struct {
	client     *minio.Client
	bucket     string
	publicBase string
}

func NewR2Store(cfg R2Config) (*R2Store, error) {
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
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: true,
		Region: "auto",
	})
	if err != nil {
		return nil, err
	}
	return &R2Store{
		client:     client,
		bucket:     cfg.Bucket,
		publicBase: strings.TrimRight(cfg.PublicBase, "/"),
	}, nil
}

func (r *R2Store) Put(ctx context.Context, key, contentType string, rd io.Reader) error {
	_, err := r.client.PutObject(ctx, r.bucket, key, rd, -1, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (r *R2Store) Open(key string) (io.ReadCloser, error) {
	obj, err := r.client.GetObject(context.Background(), r.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func (r *R2Store) URL(key string) string {
	return r.publicBase + "/" + strings.TrimLeft(key, "/")
}

func (r *R2Store) Delete(ctx context.Context, key string) error {
	return r.client.RemoveObject(ctx, r.bucket, key, minio.RemoveObjectOptions{})
}

func (r *R2Store) Mirrors(string) bool { return true }

func (r *R2Store) List(ctx context.Context) ([]ObjectInfo, error) {
	var out []ObjectInfo
	for obj := range r.client.ListObjects(ctx, r.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return out, obj.Err
		}
		out = append(out, ObjectInfo{Key: obj.Key, Size: obj.Size})
	}
	return out, nil
}
