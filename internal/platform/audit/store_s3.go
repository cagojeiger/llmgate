package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// bucketCheckTimeout bounds the one-shot existence probe at startup so a
// slow/unreachable MinIO cannot stall boot.
const bucketCheckTimeout = 5 * time.Second

// S3Config addresses an S3-compatible endpoint. It mirrors what the
// cluster's Loki already uses against the in-cluster MinIO (path-style,
// plain http), so audit reuses the same storage with only a new bucket.
type S3Config struct {
	Endpoint  string // host:port, no scheme
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	// UseSSL toggles https. In-cluster MinIO is typically plain http.
	UseSSL bool
	// PathStyle forces bucket-in-path addressing, which MinIO requires
	// and AWS S3 also accepts.
	PathStyle bool
}

type s3Store struct {
	client *minio.Client
	bucket string
}

// NewS3Store builds an ObjectStore backed by minio-go. minio-go is a
// single-purpose S3 client with a lighter dependency tree than the full
// AWS SDK, and it is the native client for the MinIO backend in use.
//
// The bucket must already exist: this service never creates it (buckets
// are provisioned by ops, like the cluster's Loki buckets). A one-shot
// existence probe encodes that contract — a definitively-missing bucket
// fails startup with an actionable error, while an unreachable MinIO at
// boot only warns so a transient outage never blocks the gateway
// (uploads retry once it recovers).
func NewS3Store(cfg S3Config, log *slog.Logger) (ObjectStore, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("audit s3: Endpoint and Bucket are required")
	}
	if log == nil {
		log = slog.Default()
	}
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	}
	if cfg.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	client, err := minio.New(cfg.Endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("audit s3: new client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), bucketCheckTimeout)
	defer cancel()
	switch exists, err := client.BucketExists(ctx, cfg.Bucket); {
	case err != nil:
		log.Warn("audit s3: could not verify bucket at startup; uploads will retry",
			slog.String("bucket", cfg.Bucket), slog.String("err", err.Error()))
	case !exists:
		return nil, fmt.Errorf("audit s3: bucket %q does not exist; provision it first (this service does not create buckets)", cfg.Bucket)
	}
	return &s3Store{client: client, bucket: cfg.Bucket}, nil
}

// Put uploads filePath to key. FPutObject streams the file directly and
// picks single-part vs multipart by size, so a large sealed file never
// loads fully into memory.
func (s *s3Store) Put(ctx context.Context, key, filePath string) error {
	if _, err := s.client.FPutObject(ctx, s.bucket, key, filePath, minio.PutObjectOptions{
		ContentType: "application/x-ndjson",
	}); err != nil {
		return fmt.Errorf("audit s3 put %s: %w", key, err)
	}
	return nil
}
