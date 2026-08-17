package audit

import (
	"context"
	"errors"
	"fmt"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

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
func NewS3Store(cfg S3Config) (ObjectStore, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("audit s3: Endpoint and Bucket are required")
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
