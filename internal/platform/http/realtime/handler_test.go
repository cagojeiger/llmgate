package realtime

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"llmgate/internal/domain/catalog"
	llmresult "llmgate/internal/domain/llmresult/schema"
	"llmgate/internal/domain/telemetry"
	httpauth "llmgate/internal/platform/http/auth"
)

// --- fakes ---

type captureResultSink struct {
	mu      sync.Mutex
	records []*llmresult.Event
	got     chan struct{}
}

func newCaptureResultSink() *captureResultSink {
	return &captureResultSink{got: make(chan struct{}, 1)}
}

func (c *captureResultSink) Emit(_ context.Context, event *llmresult.Event) {
	c.mu.Lock()
	c.records = append(c.records, event)
	c.mu.Unlock()
	select {
	case c.got <- struct{}{}:
	default:
	}
}
func (c *captureResultSink) Close() error { return nil }

func (c *captureResultSink) last(t *testing.T) *llmresult.Event {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.records) == 0 {
		t.Fatal("no result events captured")
	}
	return c.records[len(c.records)-1]
}

// realtimeCatalog builds an in-memory catalog whose stt-realtime alias points at
// the given ws:// upstream endpoint. Constructed directly (not via LoadDir) so
// the test controls the base_url.
func realtimeCatalog(endpoint string) *catalog.Catalog {
	return &catalog.Catalog{
		Models: map[string]*catalog.Model{
			"qwen-asr-realtime": {
				ID:       "qwen-asr-realtime",
				Vendor:   "qwen",
				API:      catalog.APIRealtime,
				Protocol: "openai",
				BaseURL:  endpoint,
			},
		},
		Aliases: map[string]*catalog.Alias{
			"stt-realtime": {Alias: "stt-realtime", Chain: []string{"qwen-asr-realtime"}},
		},
	}
}

// upstreamRealtimeServer stands in for the registered realtime STT server. It
// records the frames it receives (to prove the client→upstream leg relayed),
// then emits session.created, a delta, and a completed frame carrying the
// transcript before closing.
func upstreamRealtimeServer(t *testing.T, received chan<- []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx := r.Context()

		var got []string
		for range 3 {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			got = append(got, string(data))
		}
		received <- got

		for _, frame := range []string{
			`{"type":"session.created"}`,
			`{"type":"conversation.item.input_audio_transcription.delta","delta":"hel"}`,
			`{"type":"conversation.item.input_audio_transcription.completed","transcript":"hello world"}`,
		} {
			if err := c.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
				return
			}
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
}

// gatewayServer wraps the realtime Handler with a stub that injects a
// successfully-authenticated consumer, mirroring what the chi auth middleware
// does on the upgrade GET in production.
func gatewayServer(t *testing.T, h *Handler) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := httpauth.WithConsumer(r.Context(), &httpauth.ConsumerInfo{Name: "tester", KeyID: "key0"})
		h.ServeHTTP(w, r.WithContext(ctx))
	}))
}

func wsURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

func TestHandler_BrokersSessionAndAuditsTranscript(t *testing.T) {
	received := make(chan []string, 1)
	upstream := upstreamRealtimeServer(t, received)
	defer upstream.Close()

	sink := newCaptureResultSink()
	events := &countingEventSink{}
	h := NewHandler(realtimeCatalog(wsURL(upstream.URL)), slog.Default(), events, HandlerConfig{
		ServiceVersion: "test",
		Environment:    "test",
		ResultSink:     sink,
	})
	gw := gatewayServer(t, h)
	defer gw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, _, err := websocket.Dial(ctx, wsURL(gw.URL)+"/v1/realtime?model=stt-realtime", nil)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer func() { _ = client.CloseNow() }()

	// client → upstream leg
	for _, frame := range []string{
		`{"type":"session.update","session":{"type":"transcription"}}`,
		`{"type":"input_audio_buffer.append","audio":"AAAA"}`,
		`{"type":"input_audio_buffer.commit"}`,
	} {
		if err := client.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			t.Fatalf("client write: %v", err)
		}
	}

	// upstream → client leg
	clientGot := make([]string, 0, 3)
	for range 3 {
		_, data, err := client.Read(ctx)
		if err != nil {
			t.Fatalf("client read: %v", err)
		}
		clientGot = append(clientGot, string(data))
	}
	// Next read observes the upstream-initiated close relayed by the gateway.
	if _, _, err := client.Read(ctx); err == nil {
		t.Fatal("expected close after transcript, got a frame")
	}

	// Prove the client→upstream leg relayed verbatim.
	select {
	case got := <-received:
		if len(got) != 3 || !strings.Contains(got[0], "session.update") || !strings.Contains(got[1], "input_audio_buffer.append") {
			t.Fatalf("upstream received unexpected frames: %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received client frames")
	}

	// Prove the upstream→client leg relayed verbatim.
	if len(clientGot) != 3 || !strings.Contains(clientGot[0], "session.created") ||
		!strings.Contains(clientGot[2], `"transcript":"hello world"`) {
		t.Fatalf("client received unexpected frames: %v", clientGot)
	}

	// Wait for the session-close audit record.
	select {
	case <-sink.got:
	case <-time.After(2 * time.Second):
		t.Fatal("no result event emitted at session close")
	}

	ev := sink.last(t)
	if ev.Operation != "audio.realtime.transcription" {
		t.Errorf("Operation = %q, want audio.realtime.transcription", ev.Operation)
	}
	if ev.ModelRequested != "stt-realtime" {
		t.Errorf("ModelRequested = %q, want stt-realtime", ev.ModelRequested)
	}
	if ev.ModelUsed != "qwen-asr-realtime" {
		t.Errorf("ModelUsed = %q, want qwen-asr-realtime", ev.ModelUsed)
	}
	if ev.RealtimeTurns != 1 {
		t.Errorf("RealtimeTurns = %d, want 1", ev.RealtimeTurns)
	}
	if len(ev.RealtimeTranscripts) != 1 || ev.RealtimeTranscripts[0] != "hello world" {
		t.Errorf("RealtimeTranscripts = %v, want [hello world]", ev.RealtimeTranscripts)
	}
	if ev.RealtimeEndpoint == "" {
		t.Error("RealtimeEndpoint is empty")
	}
	if ev.ConsumerName != "tester" {
		t.Errorf("ConsumerName = %q, want tester", ev.ConsumerName)
	}
	if ev.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", ev.StatusCode)
	}
}

func TestHandler_Resolve(t *testing.T) {
	h := NewHandler(realtimeCatalog("ws://127.0.0.1:8770"), slog.Default(), nil, HandlerConfig{})

	used, base, err := h.resolve("stt-realtime")
	if err != nil {
		t.Fatalf("resolve alias: %v", err)
	}
	if used != "qwen-asr-realtime" || base != "ws://127.0.0.1:8770" {
		t.Fatalf("resolve alias = (%q,%q)", used, base)
	}

	if _, _, err := h.resolve("nope"); err == nil {
		t.Fatal("resolve unknown model: want error, got nil")
	}
}

func TestHandler_AuthFailureRejectsUpgrade(t *testing.T) {
	sink := newCaptureResultSink()
	h := NewHandler(realtimeCatalog("ws://127.0.0.1:8770"), slog.Default(), &countingEventSink{}, HandlerConfig{ResultSink: sink})
	// No consumer injected → the handler sees the missing-auth default and must
	// reject before upgrading.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := httpauth.WithConsumer(r.Context(), &httpauth.ConsumerInfo{AuthError: telemetry.AuthErrorMissing})
		h.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(srv.URL)+"/v1/realtime", nil)
	if err == nil {
		t.Fatal("expected dial to fail on auth rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
	if got := sink.last(t); got.Operation != "audio.realtime.transcription" || got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("audit record = %+v, want unauthorized realtime audit", got)
	}
}

// countingEventSink is a minimal telemetry.EventSink for tests that only need
// the operational audit path to not panic.
type countingEventSink struct {
	mu     sync.Mutex
	audits []*telemetry.AuditEvent
}

func (c *countingEventSink) Emit(_ context.Context, event telemetry.Event) {
	if ev, ok := event.(*telemetry.AuditEvent); ok {
		c.mu.Lock()
		c.audits = append(c.audits, ev)
		c.mu.Unlock()
	}
}
func (c *countingEventSink) Close() error { return nil }
