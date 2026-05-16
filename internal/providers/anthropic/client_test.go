package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"llmgate/internal/llmtypes"
)

func writeAnthropicResponse(t *testing.T, w http.ResponseWriter, stopReason string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_, err := w.Write([]byte(`{
		"id": "msg-1",
		"type": "message",
		"role": "assistant",
		"model": "minimax-m2.5",
		"content": [{"type": "text", "text": "pong"}],
		"stop_reason": "` + stopReason + `",
		"usage": {"input_tokens": 2, "output_tokens": 1}
	}`))
	if err != nil {
		t.Fatalf("write response: %v", err)
	}
}

func writeSSEEvent(t *testing.T, w http.ResponseWriter, event, payload string) {
	t.Helper()
	payload = compactJSONPayload(t, payload)
	_, err := w.Write([]byte("event: " + event + "\n" + "data: " + payload + "\n\n"))
	if err != nil {
		t.Fatalf("write SSE event: %v", err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func compactJSONPayload(t *testing.T, payload string) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, []byte(payload)); err != nil {
		t.Fatalf("compact SSE payload: %v", err)
	}
	return out.String()
}

func requireProviderError(t *testing.T, err error) *llmtypes.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var perr *llmtypes.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err type = %T, want *llmtypes.Error", err)
	}
	return perr
}

func mustNew(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

type localServer struct {
	*httptest.Server
	Client *http.Client
}

func newLocalServer(handler http.Handler) *localServer {
	listener := newPipeListener()
	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: handler},
	}
	server.Start()

	transport := &http.Transport{DialContext: listener.DialContext}
	return &localServer{
		Server: server,
		Client: &http.Client{Transport: transport},
	}
}

func (s *localServer) Close() {
	if transport, ok := s.Client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	s.Server.Close()
}

type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
	})
	return nil
}

func (l *pipeListener) Addr() net.Addr {
	return pipeAddr("pipe")
}

func (l *pipeListener) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	clientConn, serverConn := net.Pipe()
	select {
	case l.conns <- serverConn:
		return clientConn, nil
	case <-ctx.Done():
		_ = clientConn.Close()
		_ = serverConn.Close()
		return nil, ctx.Err()
	case <-l.closed:
		_ = clientConn.Close()
		_ = serverConn.Close()
		return nil, net.ErrClosed
	}
}

type pipeAddr string

func (a pipeAddr) Network() string { return string(a) }
func (a pipeAddr) String() string  { return string(a) }
