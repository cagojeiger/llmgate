package openai

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"llmgate/internal/llmtypes"
)

func requireProviderError(t *testing.T, err error) *llmtypes.Error {
	t.Helper()

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

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }
