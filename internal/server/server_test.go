package server

import (
	"testing"
	"time"

	"llmgate/internal/config"
)

func TestNew_ReadTimeoutFollowsRequestTimeout(t *testing.T) {
	cfg := &config.Server{
		Addr:           ":0",
		RequestTimeout: 2 * time.Second,
	}

	srv := New(cfg, nil, &Handler{}, nil)

	if srv.ReadTimeout != cfg.RequestTimeout {
		t.Fatalf("ReadTimeout = %v, want %v", srv.ReadTimeout, cfg.RequestTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want 0 so SSE responses are not cut off", srv.WriteTimeout)
	}
}
