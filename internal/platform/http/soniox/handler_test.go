package soniox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	llmresultschema "llmgate/internal/domain/llmresult/schema"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	httpauth "llmgate/internal/platform/http/auth"
)

const testStart = ` {"model":"stt-rt-v6", "audio_format":"pcm_s16le","sample_rate":16000,"num_channels":1, "enable_agent_text_input":true} `

type recorded struct {
	direction, kind, payload string
}

type mockCapture struct {
	mu         sync.Mutex
	frames     []recorded
	complete   bool
	reason     string
	finishes   int
	appendFail int
	finishFail bool
}

func (*mockCapture) ID() string { return "capture-1" }

func (m *mockCapture) Append(direction, kind string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appendFail > 0 && len(m.frames)+1 == m.appendFail {
		return errors.New("disk full")
	}
	m.frames = append(m.frames, recorded{direction, kind, string(payload)})
	return nil
}

func (m *mockCapture) Finish(complete bool, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finishes++
	m.reason = reason
	m.complete = complete && !m.finishFail
	if m.finishFail {
		return errors.New("seal failed")
	}
	return nil
}

func (m *mockCapture) check(t *testing.T, complete bool, reason string, want ...recorded) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.complete != complete || m.reason != reason || m.finishes != 1 || !slices.Equal(m.frames, want) {
		t.Fatalf("capture = %t %q %d %+v, want %t %q one finish and %+v", m.complete, m.reason, m.finishes, m.frames, complete, reason, want)
	}
}

type auditSink chan *telemetry.AuditEvent

func (s auditSink) Emit(_ context.Context, event telemetry.Event) { s <- event.(*telemetry.AuditEvent) }
func (s auditSink) Close() error                                  { return nil }

type summarySink chan *llmresultschema.Event

func (s summarySink) Emit(_ context.Context, event *llmresultschema.Event) { s <- event }
func (s summarySink) Close() error                                         { return nil }

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func provider(t *testing.T, script func(*websocket.Conn) error) (string, <-chan error) {
	t.Helper()
	result := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vendor-private-key" || r.Header.Get("Sec-WebSocket-Protocol") != "" {
			result <- errors.New("missing vendor authorization or unexpected subprotocol")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			result <- err
			return
		}
		defer func() { _ = c.CloseNow() }()
		result <- script(c)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), result
}

func gateway(t *testing.T, endpoint string, capture *mockCapture) (*Handler, string, auditSink, summarySink) {
	t.Helper()
	audits, summaries := make(auditSink, 2), make(summarySink, 2)
	h := NewHandler(nil, audits, Config{
		APIKey: "vendor-private-key", Endpoint: endpoint, ResultSink: summaries,
		BeginCapture: func(_, model string) (Capture, error) {
			if model != Model {
				return nil, errors.New("wrong model")
			}
			return capture, nil
		},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &httpauth.ConsumerInfo{AuthError: telemetry.AuthErrorMissing}
		if r.Header.Get("Authorization") == "Bearer gateway-client-key" {
			info = &httpauth.ConsumerInfo{KeyID: "test-key-id", AllowedAliases: []string{Model}}
		}
		h.ServeHTTP(w, r.WithContext(httpauth.WithConsumer(r.Context(), info)))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = h.Close() })
	return h, "ws" + strings.TrimPrefix(srv.URL, "http"), audits, summaries
}

func dialTest(t *testing.T, ctx context.Context, endpoint string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{ //nolint:bodyclose // Dial owns the handshake body.
		HTTPHeader: http.Header{"Authorization": []string{"Bearer gateway-client-key"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func send(t *testing.T, ctx context.Context, c *websocket.Conn, typ websocket.MessageType, data string) {
	t.Helper()
	if err := c.Write(ctx, typ, []byte(data)); err != nil {
		t.Fatal(err)
	}
}

func readMatch(ctx context.Context, c *websocket.Conn, wantType websocket.MessageType, want string) error {
	typ, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if typ != wantType || !bytes.Equal(data, []byte(want)) {
		return fmt.Errorf("got %v %q, want %v %q", typ, data, wantType, want)
	}
	return nil
}

func checkAudit(t *testing.T, ctx context.Context, audits auditSink, summaries summarySink, status int, kind llmtypes.ErrorKind, complete *bool) {
	t.Helper()
	select {
	case audit := <-audits:
		if audit.StatusCode != status || audit.Kind != kind || audit.Operation != operation {
			t.Fatalf("unexpected audit %+v", audit)
		}
	case <-ctx.Done():
		t.Fatal("audit was not emitted")
	}
	select {
	case summary := <-summaries:
		if summary.StatusCode != status || summary.Vendor != "soniox" || summary.ModelRequested != Model {
			t.Fatalf("unexpected summary %+v", summary)
		}
		if complete == nil {
			if summary.SonioxCaptureComplete != nil || summary.SonioxCaptureID != "" {
				t.Fatalf("unexpected capture summary %+v", summary)
			}
		} else if summary.SonioxCaptureComplete == nil || *summary.SonioxCaptureComplete != *complete || summary.SonioxCaptureID != "capture-1" {
			t.Fatalf("unexpected capture summary %+v", summary)
		}
	case <-ctx.Done():
		t.Fatal("summary was not emitted")
	}
}

func TestRelayPreservesNativeMessagesAndCommitsBeforeCompletion(t *testing.T) {
	ctx := testContext(t)
	const partial = `{ "tokens": [{ "text": "안녕", "is_final": false }], "total_audio_proc_ms": 45 }`
	const finals = `{ "tokens": [{ "text": "안녕하세요", "is_final": true }, { "text": "<fin>", "is_final": true }, { "text": "<end>", "is_final": true }] }`
	const finish = `{ "tokens": [], "finished": true, "final_audio_proc_ms": 234 }`
	requests := []struct {
		kind websocket.MessageType
		data string
	}{
		{websocket.MessageText, testStart},
		{websocket.MessageBinary, "\x00\xff\x01\x00"},
		{websocket.MessageBinary, ""},
		{websocket.MessageText, "AQI="},
		{websocket.MessageText, `{"type": "keepalive"}`},
		{websocket.MessageText, `{"type":"agent_text","text":" 같이 ","end_of_segment":true}`},
		{websocket.MessageText, `{ "type": "finalize" }`},
	}
	endpoint, vendor := provider(t, func(c *websocket.Conn) error {
		for _, req := range requests {
			if err := readMatch(ctx, c, req.kind, req.data); err != nil {
				return err
			}
		}
		for _, payload := range []string{partial, finals} {
			if err := c.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
				return err
			}
		}
		if err := readMatch(ctx, c, websocket.MessageText, ""); err != nil {
			return err
		}
		return c.Write(ctx, websocket.MessageText, []byte(finish))
	})
	capture := &mockCapture{}
	_, endpoint, audits, summaries := gateway(t, endpoint, capture)
	client := dialTest(t, ctx, endpoint)
	want := make([]recorded, 0, len(requests)+4)
	for _, req := range requests {
		send(t, ctx, client, req.kind, req.data)
		want = append(want, recorded{"request", (message{kind: req.kind}).captureKind(), req.data})
	}
	for _, payload := range []string{partial, finals} {
		if err := readMatch(ctx, client, websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
		want = append(want, recorded{"response", "text", payload})
	}
	send(t, ctx, client, websocket.MessageText, "")
	if err := readMatch(ctx, client, websocket.MessageText, finish); err != nil {
		t.Fatal(err)
	}
	want = append(want, recorded{"request", "text", ""}, recorded{"response", "text", finish})
	capture.check(t, true, "finished", want...)
	if _, _, err := client.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("expected normal close, got %v", err)
	}
	if err := <-vendor; err != nil {
		t.Fatal(err)
	}
	complete := true
	checkAudit(t, ctx, audits, summaries, http.StatusOK, "", &complete)
}

func TestCredentialsInStartOrControlsAreRejected(t *testing.T) {
	for _, tc := range []struct {
		name, blocked string
		start         bool
	}{
		{"start", `{"model":"stt-rt-v6","audio_format":"auto","\u0041PI_KEY":"client-secret"}`, true},
		{"control", `{"type":"keepalive","nested":{"api_key":"client-secret"},"nested":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testContext(t)
			endpoint, vendor := provider(t, func(c *websocket.Conn) error {
				if tc.start {
					return errors.New("vendor dialed for invalid start")
				}
				if err := readMatch(ctx, c, websocket.MessageText, testStart); err != nil {
					return err
				}
				if _, data, err := c.Read(ctx); err == nil {
					return fmt.Errorf("vendor saw forbidden control: %q", data)
				}
				return nil
			})
			capture := &mockCapture{}
			_, endpoint, audits, summaries := gateway(t, endpoint, capture)
			client := dialTest(t, ctx, endpoint)
			if !tc.start {
				send(t, ctx, client, websocket.MessageText, testStart)
			}
			send(t, ctx, client, websocket.MessageText, tc.blocked)
			_, data, err := client.Read(ctx)
			if err != nil || !bytes.Contains(data, []byte("client_protocol_error")) || bytes.Contains(data, []byte("client-secret")) {
				t.Fatalf("unexpected error frame %q, %v", data, err)
			}
			_, _, _ = client.Read(ctx)
			if tc.start {
				checkAudit(t, ctx, audits, summaries, http.StatusBadRequest, llmtypes.KindBadRequest, nil)
				select {
				case err := <-vendor:
					t.Fatal(err)
				default:
				}
			} else {
				complete := false
				checkAudit(t, ctx, audits, summaries, http.StatusBadRequest, llmtypes.KindBadRequest, &complete)
				capture.check(t, false, "client_protocol_error", recorded{"request", "text", testStart})
				if err := <-vendor; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestTerminalErrorsLeaveCaptureIncomplete(t *testing.T) {
	const finish = `{"tokens":[],"finished":true}`
	const rejection = `{ "tokens":[], "error_code":401, "error_type":"unauthenticated", "request_id":"vendor-id" }`
	for _, tc := range []struct {
		name, response, reason, finishReason string
		appendFail                           int
		finishFail, skipEOF                  bool
		status                               int
	}{
		{name: "vendor error with normal close", response: rejection, reason: "upstream_error", status: 401, skipEOF: true},
		{name: "normal close without finished", reason: "upstream_disconnected", status: 502},
		{name: "finish before EOF", response: finish, reason: "upstream_protocol_error", status: 502, skipEOF: true},
		{name: "archive append fails", response: finish, reason: "capture_write_failed", status: 503, appendFail: 3},
		{name: "archive seal fails", response: finish, reason: "capture_finalize_failed", finishReason: "finished", status: 503, finishFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testContext(t)
			endpoint, vendor := provider(t, func(c *websocket.Conn) error {
				if err := readMatch(ctx, c, websocket.MessageText, testStart); err != nil {
					return err
				}
				if !tc.skipEOF {
					if err := readMatch(ctx, c, websocket.MessageText, ""); err != nil {
						return err
					}
				}
				if tc.response != "" {
					if err := c.Write(ctx, websocket.MessageText, []byte(tc.response)); err != nil {
						return err
					}
				}
				_ = c.Close(websocket.StatusNormalClosure, "")
				return nil
			})
			capture := &mockCapture{appendFail: tc.appendFail, finishFail: tc.finishFail}
			_, endpoint, audits, summaries := gateway(t, endpoint, capture)
			client := dialTest(t, ctx, endpoint)
			send(t, ctx, client, websocket.MessageText, testStart)
			want := []recorded{{"request", "text", testStart}}
			if !tc.skipEOF {
				send(t, ctx, client, websocket.MessageText, "")
				want = append(want, recorded{"request", "text", ""})
			}
			if tc.response != "" && tc.appendFail == 0 {
				want = append(want, recorded{"response", "text", tc.response})
			}
			_, data, err := client.Read(ctx)
			if err != nil || bytes.Contains(data, []byte(`"finished":true`)) {
				t.Fatalf("unexpected terminal frame %q, %v", data, err)
			}
			if tc.reason == "upstream_error" {
				if string(data) != rejection {
					t.Fatalf("provider error changed: %q", data)
				}
			} else if !bytes.Contains(data, []byte(tc.reason)) {
				t.Fatalf("wrong error frame %q", data)
			}
			_, _, _ = client.Read(ctx)
			complete := false
			checkAudit(t, ctx, audits, summaries, tc.status, llmtypes.KindUpstream, &complete)
			reason := tc.finishReason
			if reason == "" {
				reason = tc.reason
			}
			capture.check(t, false, reason, want...)
			if err := <-vendor; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShutdownSealsActiveSession(t *testing.T) {
	ctx := testContext(t)
	const ready = `{"tokens":[]}`
	endpoint, vendor := provider(t, func(c *websocket.Conn) error {
		if err := readMatch(ctx, c, websocket.MessageText, testStart); err != nil {
			return err
		}
		if err := c.Write(ctx, websocket.MessageText, []byte(ready)); err != nil {
			return err
		}
		_, _, err := c.Read(ctx)
		if err == nil {
			return errors.New("upstream socket did not close")
		}
		return nil
	})
	capture := &mockCapture{}
	h, endpoint, audits, summaries := gateway(t, endpoint, capture)
	client := dialTest(t, ctx, endpoint)
	send(t, ctx, client, websocket.MessageText, testStart)
	if err := readMatch(ctx, client, websocket.MessageText, ready); err != nil {
		t.Fatal(err)
	}
	if err := h.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	complete := false
	checkAudit(t, ctx, audits, summaries, http.StatusServiceUnavailable, llmtypes.KindUpstream, &complete)
	capture.check(t, false, "server_shutdown", recorded{"request", "text", testStart}, recorded{"response", "text", ready})
	if err := <-vendor; err != nil {
		t.Fatal(err)
	}
}
