package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"llmgate/internal/domain/consumers"
	llmresultschema "llmgate/internal/domain/llmresult/schema"
	"llmgate/internal/platform/config"
	"llmgate/internal/platform/http/server"
	"llmgate/internal/platform/http/systemone"
	slogtelemetry "llmgate/internal/platform/telemetry/slog"
)

func TestSystemOne_WritesAuditAndDecisionResult(t *testing.T) {
	const (
		consumerKey = "test-consumer-bearer"
		providerKey = "test-provider-bearer"
		requestID   = "jev-success"
		deniedID    = "jev-denied"
		request     = `{"model":"jev-1.14.0","state":{"text":"Please cancel my plan"},"questions":{"cancel":{"type":"noul","instructions":"Is this a cancellation?"}}}`
		response    = `{"model":"jev-1.14.0","answers":{"cancel":{"type":"noul","noul":0.98}},"usage":{"input_tokens":15,"output_tokens":2}}`
	)
	type forwardedRequest struct {
		method, path, authorization string
		body                        []byte
	}
	forwarded := make(chan forwardedRequest, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		forwarded <- forwardedRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	defer upstream.Close()

	consumerDir := t.TempDir()
	consumer := fmt.Sprintf("name: alpha\nkey_hashes:\n  - sha256:%x\nallowed_aliases:\n  - %s\n", sha256.Sum256([]byte(consumerKey)), systemone.Model)
	if err := os.WriteFile(filepath.Join(consumerDir, "alpha.yaml"), []byte(consumer), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := consumers.LoadDir(consumerDir)
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	cfg := &config.Server{AuditDir: t.TempDir(), AuditCompression: "none", RequestTimeout: 5 * time.Second}
	results, record, err := buildResultSink(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = results.Close() })
	decision := systemone.NewHandler(logger, slogtelemetry.NewSink(logger, logger), systemone.HandlerConfig{
		APIKey:         providerKey,
		Endpoint:       upstream.URL + "/v1/systemone",
		RequestTimeout: cfg.RequestTimeout,
		ResultSink:     results,
		RecordResult:   record,
	})
	gateway := server.NewWithOptions(server.ServerOptions{
		Config:           cfg,
		Log:              logger,
		Handler:          http.NotFoundHandler(),
		SystemOneHandler: decision,
		Consumers:        store,
	})

	for _, tc := range []struct {
		id, authorization string
		status            int
	}{
		{requestID, "Bearer " + consumerKey, http.StatusOK},
		{deniedID, "", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(request))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", tc.id)
		if tc.authorization != "" {
			req.Header.Set("Authorization", tc.authorization)
		}
		out := httptest.NewRecorder()
		gateway.Handler.ServeHTTP(out, req)
		if out.Code != tc.status || out.Header().Get("X-Request-Id") != tc.id {
			t.Fatalf("request %q returned %d with id %q and body %s", tc.id, out.Code, out.Header().Get("X-Request-Id"), out.Body)
		}
		if tc.status == http.StatusOK {
			assertSystemOneJSON(t, out.Body.Bytes(), response)
		}
	}
	if len(forwarded) != 1 {
		t.Fatalf("upstream got %d requests, want 1", len(forwarded))
	}
	sent := <-forwarded
	if sent.method != http.MethodPost || sent.path != "/v1/systemone" || sent.authorization != "Bearer "+providerKey {
		t.Errorf("unexpected upstream request: %s %s with %q", sent.method, sent.path, sent.authorization)
	}
	assertSystemOneJSON(t, sent.body, request)

	if err := results.Close(); err != nil {
		t.Fatalf("flush audit results: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(cfg.AuditDir, "compressed", "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("result files = %v, error = %v, want one JSONL file", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var result llmresultschema.Event
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("expected one finalized result: %v\n%s", err, data)
	}
	if result.EventType != llmresultschema.EventType || result.RequestID != requestID || result.Operation != "decision.systemone" || result.StatusCode != http.StatusOK {
		t.Errorf("unexpected finalized event: %+v", result)
	}
	if result.ConsumerName != "alpha" || result.ModelRequested != systemone.Model || result.ModelUsed != systemone.Model || result.Vendor != "typesafe" {
		t.Errorf("unexpected decision metadata: %+v", result)
	}
	if result.Usage == nil || result.Usage.TotalTokens != 17 || len(result.Attempts) != 1 {
		t.Errorf("unexpected usage or attempts: %+v", result)
	}
	assertSystemOneJSON(t, result.DecisionRequest, request)
	assertSystemOneJSON(t, result.DecisionResponse, response)
	assertSystemOneAudit(t, logs.Bytes(), requestID, deniedID)
	for _, secret := range []string{consumerKey, providerKey, "Please cancel my plan", `"noul":0.98`} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("operational logs contain request, answer, or credential data: %q", secret)
		}
	}
}

func assertSystemOneAudit(t *testing.T, logs []byte, requestID, deniedID string) {
	t.Helper()
	statuses := map[string]int{requestID: http.StatusOK, deniedID: http.StatusUnauthorized}
	counts := map[string]int{}
	calls := 0
	for _, line := range bytes.Split(bytes.TrimSpace(logs), []byte("\n")) {
		var entry struct {
			Type      string `json:"event_type"`
			RequestID string `json:"request_id"`
			Operation string `json:"operation"`
			Status    int    `json:"status"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode gateway log: %v", err)
		}
		if entry.Type == "audit" {
			if entry.Operation != "decision.systemone" || entry.Status != statuses[entry.RequestID] {
				t.Errorf("unexpected audit event: %s", line)
			}
			counts[entry.RequestID]++
		}
		if entry.Type == "call" {
			if entry.RequestID != requestID {
				t.Errorf("unexpected provider call event: %s", line)
			}
			calls++
		}
	}
	if len(counts) != 2 || counts[requestID] != 1 || counts[deniedID] != 1 {
		t.Errorf("audit counts = %v, want one per request", counts)
	}
	if calls != 1 {
		t.Errorf("provider call log count = %d, want 1", calls)
	}
}

func assertSystemOneJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var actual, expected any
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatalf("decode decision JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatalf("decode expected decision JSON: %v", err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("decision JSON = %s, want %s", got, want)
	}
}
