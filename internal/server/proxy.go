package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"llmgate/internal/config"
)

const gatewayUserAgent = "llmgate/0.1"

type Forwarder struct {
	apiKey string
	log    *slog.Logger
	base   *url.URL
	chat   *httputil.ReverseProxy
}

func NewForwarder(cfg *config.Provider, log *slog.Logger) (*Forwarder, error) {
	if cfg == nil {
		return nil, fmt.Errorf("server: provider config is nil")
	}
	if log == nil {
		log = slog.Default()
	}

	base, err := url.Parse(cfg.OpenCodeBaseURL)
	if err != nil {
		return nil, fmt.Errorf("server: parse opencode base URL: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("server: opencode base URL must be absolute, got %q", cfg.OpenCodeBaseURL)
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// No response-header timeout: LLM first byte can take minutes; cancellation flows via context.
		ResponseHeaderTimeout: 0,
	}

	f := &Forwarder{
		apiKey: cfg.OpenCodeAPIKey,
		log:    log,
		base:   base,
	}
	f.chat = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = f.base.Scheme
			req.URL.Host = f.base.Host
			req.URL.Path = singleJoiningSlash(f.base.Path, "/chat/completions")
			req.URL.RawPath = ""
			req.Host = f.base.Host
			req.Header.Del("Host")
			applyUpstreamHeaders(req, f.apiKey)
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			f.log.LogAttrs(r.Context(), slog.LevelError, "upstream unavailable",
				slog.String("path", r.URL.Path),
				slog.String("err", err.Error()),
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("{\"error\":{\"message\":\"upstream unavailable\",\"type\":\"upstream_error\"}}\n"))
		},
	}

	return f, nil
}

func applyUpstreamHeaders(req *http.Request, apiKey string) {
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.Header.Del("Anthropic-Api-Key")
	req.Header.Del("Proxy-Authorization")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", gatewayUserAgent)
	}
}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.ServeChat(w, r)
}

// DO NOT read r.Body. Pure pass-through.
func (f *Forwarder) ServeChat(w http.ResponseWriter, r *http.Request) {
	f.chat.ServeHTTP(w, r)
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
