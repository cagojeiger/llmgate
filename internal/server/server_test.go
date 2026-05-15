package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llmgate/internal/config"
)

func TestNew_ReadTimeoutFollowsRequestTimeout(t *testing.T) {
	cfg := &config.Server{
		Addr:           ":0",
		RequestTimeout: 2 * time.Second,
	}

	srv := New(cfg, nil, &Handler{}, nil, nil)

	if srv.ReadTimeout != cfg.RequestTimeout {
		t.Fatalf("ReadTimeout = %v, want %v", srv.ReadTimeout, cfg.RequestTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want 0 so SSE responses are not cut off", srv.WriteTimeout)
	}
}

func TestNewWithOptions_MetricsMountedOutsideMiddleware(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("metrics-ok"))
	})
	srv := NewWithOptions(ServerOptions{
		Config:         &config.Server{Addr: ":0"},
		Handler:        &Handler{},
		MetricsHandler: metrics,
	})
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("Get /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "metrics-ok" {
		t.Fatalf("body = %q, want metrics-ok", string(body))
	}
	if got := resp.Header.Get("X-Request-Id"); got != "" {
		t.Fatalf("X-Request-Id = %q, want empty because metrics bypasses middleware", got)
	}
}
