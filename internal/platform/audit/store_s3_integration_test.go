package audit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestS3Store_Integration exercises the real minio-go client against a
// live MinIO. It is skipped unless AUDIT_S3_IT=1 so it never runs in
// `make test`/CI (which have no MinIO). Run it against the compose stack:
//
//	docker compose up -d minio minio-init
//	AUDIT_S3_IT=1 go test -run TestS3Store_Integration ./internal/platform/audit/
func TestS3Store_Integration(t *testing.T) {
	if os.Getenv("AUDIT_S3_IT") == "" {
		t.Skip("set AUDIT_S3_IT=1 with a live MinIO to run the S3 integration test")
	}
	cfg := S3Config{
		Endpoint:  envOr("AUDIT_S3_ENDPOINT", "localhost:9000"),
		Bucket:    envOr("AUDIT_S3_BUCKET", "llm-audit"),
		AccessKey: envOr("AUDIT_S3_ACCESS_KEY", "minioadmin"),
		SecretKey: envOr("AUDIT_S3_SECRET_KEY", "minioadmin"),
		UseSSL:    false,
		PathStyle: true,
	}
	store, err := NewS3Store(cfg)
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}

	// Write a sealed-style file and upload it under a real partition key.
	dir := t.TempDir()
	name := sealedName("it-instance", time.Now())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(`{"request_id":"it-1"}`+"\n"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	key := objectKey("audit", time.Now(), name)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := store.Put(ctx, key, path); err != nil {
		t.Fatalf("Put to MinIO failed: %v", err)
	}
	t.Logf("uploaded object: %s", key)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
