package embeddings

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgate/internal/domain/routing"
	"llmgate/internal/domain/telemetry"
	httpauth "llmgate/internal/platform/http/auth"
	"llmgate/internal/platform/providers/openai"
)

type events struct {
	audits []*telemetry.AuditEvent
	calls  []*telemetry.CallEvent
}

func (e *events) Emit(_ context.Context, event telemetry.Event) {
	switch v := event.(type) {
	case *telemetry.AuditEvent:
		e.audits = append(e.audits, v)
	case *telemetry.CallEvent:
		e.calls = append(e.calls, v)
	}
}
func (*events) Close() error { return nil }

// Exercise the real handler, routing, and provider together, including the
// connection boundary that causes a new RelayGate pool selection per request.
func TestEmbeddingRequestBoundary(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/embeddings" || !r.Close {
			t.Errorf("path/close = %s/%v", r.URL.Path, r.Close)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","model":"qwen","data":[{"object":"embedding","index":0,"embedding":[0.6,0.8]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	provider, err := openai.NewEmbedding(openai.EmbeddingConfig{BaseURL: upstream.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := routing.NewService(nil, routing.Aliases{"embed": {"qwen"}}, routing.FallbackPolicy{CompleteTimeout: time.Second}, nil, routing.WithEmbeddings(routing.EmbeddingModels{"qwen": provider}))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, body string
		consumer   httpauth.ConsumerInfo
		status     int
		upstream   bool
	}{
		{"success", `{"model":"embed","input":"안녕하세요"}`, httpauth.ConsumerInfo{}, 200, true},
		{"auth", `{"model":"embed","input":"hello"}`, httpauth.ConsumerInfo{AuthError: telemetry.AuthErrorMissing}, 401, false},
		{"policy", `{"model":"embed","input":"hello"}`, httpauth.ConsumerInfo{AllowedAliases: []string{"chat"}}, 403, false},
		{"tokens unsupported", `{"model":"embed","input":[1,2]}`, httpauth.ConsumerInfo{}, 400, false},
		{"null item", `{"model":"embed","input":["hi",null]}`, httpauth.ConsumerInfo{}, 400, false},
		{"invalid JSON", `{`, httpauth.ConsumerInfo{}, 400, false},
		{"unknown model", `{"model":"other","input":"hi"}`, httpauth.ConsumerInfo{}, 400, false},
		{"body limit", `{"model":"embed","input":"` + strings.Repeat("x", 256) + `"}`, httpauth.ConsumerInfo{}, 400, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &events{}
			h := NewHandler(svc, nil, sink, HandlerConfig{RequestTimeout: time.Second, MaxRequestBytes: 256})
			req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(tc.body))
			req = req.WithContext(httpauth.WithConsumer(req.Context(), &tc.consumer))
			rec := httptest.NewRecorder()
			before := calls
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if len(sink.audits) != 1 || sink.audits[0].StatusCode != tc.status {
				t.Fatalf("audit=%+v", sink.audits)
			}
			if (calls > before) != tc.upstream || (len(sink.calls) > 0) != tc.upstream {
				t.Fatalf("calls=%d events=%d", calls-before, len(sink.calls))
			}
		})
	}
}
