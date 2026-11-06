package systemone

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	llmresultschema "llmgate/internal/domain/llmresult/schema"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	httpauth "llmgate/internal/platform/http/auth"
	"llmgate/internal/platform/http/requestid"
)

const (
	inputBody = `{"model":"jev-1.14.0","state":{"ticket":9007199254740993},"questions":{"billing":{"type":"noul","instructions":"Is this about billing?"}}}`
	answer    = `{"model":"jev-1.14.0","answers":{"billing":{"type":"noul","noul":0.98}},"usage":{"input_tokens":23,"output_tokens":3}}`
)

type eventCapture struct{ events []telemetry.Event }

func (c *eventCapture) Emit(_ context.Context, event telemetry.Event) {
	c.events = append(c.events, event)
}
func (*eventCapture) Close() error { return nil }

type resultCapture struct{ events []*llmresultschema.Event }

func (c *resultCapture) Emit(_ context.Context, event *llmresultschema.Event) {
	c.events = append(c.events, event)
}
func (*resultCapture) Close() error { return nil }

type closedResponseWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w closedResponseWriter) Write([]byte) (int, error) {
	w.cancel()
	return 0, io.ErrClosedPipe
}

func systemOneRequest(body string, consumer httpauth.ConsumerInfo) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	ctx := requestid.WithContext(r.Context(), "systemone-request")
	return r.WithContext(httpauth.WithConsumer(ctx, &consumer))
}

func TestSystemOneRecordsRequestAndResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-key" {
			t.Error("upstream did not receive its own bearer credential")
		}
		if r.Header.Get("TypeSafe-Danger-Zone-Images") != "" {
			t.Error("caller feature headers reached upstream")
		}
		var wire map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		if string(wire["model"]) != `"jev-1.14.0"` || string(wire["state"]) != `{"ticket":9007199254740993}` {
			t.Errorf("unexpected upstream model or state: %s, %s", wire["model"], wire["state"])
		}
		_, _ = io.WriteString(w, answer)
	}))
	defer upstream.Close()
	telemetrySink, resultSink := &eventCapture{}, &resultCapture{}
	h := NewHandler(nil, telemetrySink, HandlerConfig{
		APIKey: "upstream-key", Endpoint: upstream.URL + "/v1/systemone", ResultSink: resultSink,
	})
	// Canonicalization keeps the gateway and upstream on the same model when
	// the caller repeats the model field.
	r := systemOneRequest(`{"model":"jev-latest",`+inputBody[1:], httpauth.ConsumerInfo{
		Name: "consumer", KeyID: "key-id", AllowedAliases: []string{Model},
	})
	r.Header.Set("Authorization", "Bearer caller-key")
	r.Header.Set("TypeSafe-Danger-Zone-Images", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != answer {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	if len(telemetrySink.events) != 2 || len(resultSink.events) != 1 {
		t.Fatalf("got %d telemetry and %d result events", len(telemetrySink.events), len(resultSink.events))
	}
	audit := telemetrySink.events[0].(*telemetry.AuditEvent)
	call := telemetrySink.events[1].(*telemetry.CallEvent)
	result := resultSink.events[0]
	if audit.RequestID != "systemone-request" || audit.Operation != "decision.systemone" || audit.PolicyResult != telemetry.PolicyResultAllowed || audit.StatusCode != http.StatusOK {
		t.Fatalf("unexpected audit: %+v", audit)
	}
	if call.RequestID != audit.RequestID || call.ModelUsed != Model || call.Vendor != vendor || len(call.Attempts) != 1 || call.Usage.TotalTokens != 26 {
		t.Fatalf("unexpected call: %+v", call)
	}
	if result.RequestID != audit.RequestID || string(result.DecisionResponse) != answer || result.Usage.TotalTokens != 26 {
		t.Fatalf("unexpected result: %+v", result)
	}
	var recorded map[string]json.RawMessage
	if err := json.Unmarshal(result.DecisionRequest, &recorded); err != nil || string(recorded["state"]) != `{"ticket":9007199254740993}` || !strings.Contains(string(recorded["questions"]), "Is this about billing?") {
		t.Fatalf("unexpected recorded request: %s, %v", result.DecisionRequest, err)
	}
}

func TestSystemOneRecordsDecisionWhenClientWriteFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, answer)
	}))
	defer upstream.Close()
	telemetrySink, resultSink := &eventCapture{}, &resultCapture{}
	h := NewHandler(nil, telemetrySink, HandlerConfig{APIKey: "key", Endpoint: upstream.URL, ResultSink: resultSink})
	r := systemOneRequest(inputBody, httpauth.ConsumerInfo{Name: "consumer"})
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	w := closedResponseWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	h.ServeHTTP(w, r.WithContext(ctx))
	if len(telemetrySink.events) != 2 || len(resultSink.events) != 1 {
		t.Fatalf("got %d telemetry and %d result events", len(telemetrySink.events), len(resultSink.events))
	}
	audit := telemetrySink.events[0].(*telemetry.AuditEvent)
	call := telemetrySink.events[1].(*telemetry.CallEvent)
	result := resultSink.events[0]
	if audit.Kind != llmtypes.KindClientClosed || call.Kind != llmtypes.KindClientClosed || result.ErrorKind != llmtypes.KindClientClosed {
		t.Fatalf("client disconnect was not recorded: %q, %q, %q", audit.Kind, call.Kind, result.ErrorKind)
	}
	if len(result.DecisionRequest) == 0 || string(result.DecisionResponse) != answer || result.Usage == nil || result.Usage.TotalTokens != 26 {
		t.Fatalf("completed decision was not retained: %+v", result)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].StatusCode != http.StatusOK || result.Attempts[0].Kind != "" {
		t.Fatalf("upstream attempt should remain successful: %+v", result.Attempts)
	}
}

func TestSystemOnePreflightFailuresOnlyAudit(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	validConsumer := httpauth.ConsumerInfo{Name: "consumer"}
	for _, tc := range []struct {
		name     string
		consumer httpauth.ConsumerInfo
		body     string
		limit    int64
		apiKey   string
		status   int
	}{
		{"auth", httpauth.ConsumerInfo{AuthError: telemetry.AuthErrorUnknown}, inputBody, 0, "key", http.StatusUnauthorized},
		{"allowlist", httpauth.ConsumerInfo{Name: "consumer", AllowedAliases: []string{"chat"}}, inputBody, 0, "key", http.StatusForbidden},
		{"missing upstream key", validConsumer, inputBody, 0, "", http.StatusServiceUnavailable},
		{"other model", validConsumer, strings.Replace(inputBody, Model, "jev-latest", 1), 0, "key", http.StatusBadRequest},
		{"invalid state", validConsumer, strings.Replace(inputBody, `{"ticket":9007199254740993}`, "null", 1), 0, "key", http.StatusUnprocessableEntity},
		{"too large", validConsumer, inputBody, 8, "key", http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			telemetrySink, resultSink := &eventCapture{}, &resultCapture{}
			h := NewHandler(nil, telemetrySink, HandlerConfig{
				APIKey: tc.apiKey, Endpoint: upstream.URL, MaxRequestBodyBytes: tc.limit, ResultSink: resultSink,
			})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, systemOneRequest(tc.body, tc.consumer))
			if w.Code != tc.status || len(telemetrySink.events) != 1 || len(resultSink.events) != 0 {
				t.Fatalf("got status %d, %d telemetry events and %d results", w.Code, len(telemetrySink.events), len(resultSink.events))
			}
			if audit := telemetrySink.events[0].(*telemetry.AuditEvent); audit.StatusCode != tc.status {
				t.Fatalf("unexpected audit: %+v", audit)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("rejected requests reached upstream %d times", requests.Load())
	}
}

func TestSystemOneUpstreamFailuresRetainRequestOnly(t *testing.T) {
	const errorBody = `{"error":"private upstream detail"}`
	for _, tc := range []struct {
		name           string
		providerStatus int
		status         int
		kind           llmtypes.ErrorKind
		retryAfter     string
		body           string
	}{
		{"validation", http.StatusUnprocessableEntity, http.StatusUnprocessableEntity, llmtypes.KindBadRequest, "", errorBody},
		{"rate limit", http.StatusTooManyRequests, http.StatusTooManyRequests, llmtypes.KindRateLimit, "7", errorBody},
		{"overloaded", statusOverloaded, statusOverloaded, llmtypes.KindUpstream, "7", errorBody},
		{"provider auth", http.StatusUnauthorized, http.StatusBadGateway, llmtypes.KindUpstream, "", errorBody},
		{"redirect", http.StatusTemporaryRedirect, http.StatusBadGateway, llmtypes.KindUpstream, "", errorBody},
		{"malformed success", http.StatusOK, http.StatusBadGateway, llmtypes.KindUpstream, "", errorBody},
		{"mismatched answer type", http.StatusOK, http.StatusBadGateway, llmtypes.KindUpstream, "", `{"model":"jev-1.14.0","answers":{"billing":{"type":"choice","choice":"yes","probabilities":{"yes":0.98,"no":0.02},"confidence":0.9}},"usage":{"input_tokens":23,"output_tokens":3}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/" {
					t.Error("redirect was followed")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Location", "/redirected")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tc.providerStatus)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			telemetrySink, resultSink := &eventCapture{}, &resultCapture{}
			h := NewHandler(nil, telemetrySink, HandlerConfig{APIKey: "key", Endpoint: upstream.URL, ResultSink: resultSink})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, systemOneRequest(inputBody, httpauth.ConsumerInfo{Name: "consumer"}))
			if w.Code != tc.status || w.Header().Get("Retry-After") != tc.retryAfter || strings.Contains(w.Body.String(), "private upstream detail") {
				t.Fatalf("unexpected response: %d %v %s", w.Code, w.Header(), w.Body.String())
			}
			if requests.Load() != 1 || len(telemetrySink.events) != 2 || len(resultSink.events) != 1 {
				t.Fatalf("got %d upstream requests, %d telemetry events and %d results", requests.Load(), len(telemetrySink.events), len(resultSink.events))
			}
			audit := telemetrySink.events[0].(*telemetry.AuditEvent)
			result := resultSink.events[0]
			if audit.Kind != tc.kind || result.StatusCode != tc.status || len(result.DecisionRequest) == 0 || len(result.DecisionResponse) != 0 {
				t.Fatalf("unexpected audit or result: %+v, %+v", audit, result)
			}
			if len(result.Attempts) != 1 || result.Attempts[0].StatusCode != tc.providerStatus {
				t.Fatalf("unexpected attempts: %+v", result.Attempts)
			}
		})
	}
}
