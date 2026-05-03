package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"llmgate/internal/audit"
	"llmgate/internal/provider"
	"llmgate/internal/router"
)

const maxChatRequestBytes = 1 << 20

// ChatRouter is the upstream contract Handler needs.
type ChatRouter interface {
	Complete(ctx context.Context, req *provider.Request) (*router.RouteResult, error)
	CompleteStream(ctx context.Context, req *provider.Request) (*router.RouteResult, error)
}

type Handler struct {
	router            ChatRouter
	log               *slog.Logger
	recorder          audit.Recorder
	requestTimeout    time.Duration
	streamIdleTimeout time.Duration
}

type HandlerConfig struct {
	RequestTimeout    time.Duration
	StreamIdleTimeout time.Duration
}

func NewHandler(router ChatRouter, log *slog.Logger, recorder audit.Recorder) *Handler {
	return NewHandlerWithConfig(router, log, recorder, HandlerConfig{})
}

func NewHandlerWithConfig(router ChatRouter, log *slog.Logger, recorder audit.Recorder, cfg HandlerConfig) *Handler {
	if log == nil {
		log = slog.Default()
	}
	if recorder == nil {
		recorder = audit.Nop{}
	}
	return &Handler{
		router:            router,
		log:               log,
		recorder:          recorder,
		requestTimeout:    cfg.RequestTimeout,
		streamIdleTimeout: cfg.StreamIdleTimeout,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()
	if h.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}

	rec := &audit.Record{
		Timestamp: start,
		RequestID: RequestIDFromContext(ctx),
		Method:    "chat.completions",
	}
	defer func() {
		rec.DurationMS = time.Since(start).Milliseconds()
		h.recorder.Record(ctx, rec)
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBytes))
	if err != nil {
		perr := &provider.Error{Kind: provider.KindBadRequest, Message: "read request body: " + err.Error()}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}
	rec.RequestBytes = int64(len(body))

	req := &provider.Request{}
	if err := json.Unmarshal(body, req); err != nil {
		perr := &provider.Error{Kind: provider.KindBadRequest, Message: "decode request: " + err.Error()}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}
	rec.ModelRequested = req.Model

	if req.Stream != nil && *req.Stream {
		rec.Method = "chat.completions.stream"
		h.serveStream(w, r, req, rec)
		return
	}
	h.serveComplete(w, r, req, rec)
}

// adoptRoute copies routing metadata onto rec.
func adoptRoute(rec *audit.Record, result *router.RouteResult) {
	if result == nil {
		return
	}
	rec.Attempts = result.Attempts
	rec.Vendor = result.Vendor
	rec.ModelUsed = result.ModelUsed
}

// adoptError populates rec.ErrorKind and rec.StatusCode from err.
func adoptError(rec *audit.Record, err error) {
	var perr *provider.Error
	if errors.As(err, &perr) {
		rec.ErrorKind = perr.Kind
	}
	rec.StatusCode = errStatus(err)
}

// adoptStreamSummary finalizes stream audit fields after the stream ends.
func adoptStreamSummary(rec *audit.Record, sum *provider.Summary, now time.Time) {
	if len(rec.Attempts) > 0 {
		last := &rec.Attempts[len(rec.Attempts)-1]
		last.DurationMS = now.Sub(last.StartedAt).Milliseconds()
		if last.ErrorKind == "" && rec.ErrorKind != "" {
			last.ErrorKind = rec.ErrorKind
		}
		if sum != nil {
			if sum.Usage != nil {
				last.Usage = sum.Usage
			}
			if sum.VendorCost != "" {
				last.VendorCost = sum.VendorCost
			}
		}
	}
	if sum == nil {
		return
	}
	if sum.Usage != nil {
		rec.Usage = sum.Usage
	}
	if sum.VendorCost != "" {
		rec.VendorCost = sum.VendorCost
	}
}

func (h *Handler) serveComplete(w http.ResponseWriter, r *http.Request, req *provider.Request, rec *audit.Record) {
	result, err := h.router.Complete(r.Context(), req)
	adoptRoute(rec, result)
	if err != nil {
		adoptError(rec, err)
		writeError(w, err)
		return
	}

	out, err := json.Marshal(result.Response)
	if err != nil {
		perr := &provider.Error{Kind: provider.KindUnknown, Message: "encode response: " + err.Error(), Cause: err}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}

	rec.StatusCode = http.StatusOK
	if result.Response != nil {
		rec.Usage = result.Response.Usage
		if cost, ok := result.Response.Extra["cost"]; ok && len(cost) > 0 {
			rec.VendorCost = string(cost)
		}
	}
	rec.ResponseBytes = int64(len(out))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, req *provider.Request, rec *audit.Record) {
	result, err := h.router.CompleteStream(r.Context(), req)
	adoptRoute(rec, result)
	if err != nil {
		adoptError(rec, err)
		writeError(w, err)
		return
	}
	stream := result.Stream
	defer stream.Close()
	defer func() { adoptStreamSummary(rec, stream.Summary(), time.Now()) }()

	flusher, ok := w.(http.Flusher)
	if !ok {
		perr := &provider.Error{Kind: provider.KindUnknown, Message: "streaming unsupported"}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}

	sink := newSSEWriter(w, flusher)
	defer func() { rec.ResponseBytes = sink.Bytes() }()
	sink.WriteHeaders()
	rec.StatusCode = http.StatusOK

	if result.FirstEvent != nil {
		payload, err := json.Marshal(result.FirstEvent)
		if err != nil {
			perr := &provider.Error{Kind: provider.KindUnknown, Message: "encode stream event: " + err.Error(), Cause: err}
			rec.ErrorKind = perr.Kind
			sink.SendError(perr)
			sink.SendDone()
			return
		}
		sink.Send(payload)
	}

	for {
		event, err := recvWithIdleTimeout(r.Context(), stream, h.streamIdleTimeout)
		if errors.Is(err, io.EOF) {
			sink.SendDone()
			return
		}
		if err != nil {
			var perr *provider.Error
			if errors.As(err, &perr) {
				rec.ErrorKind = perr.Kind
			}
			h.log.LogAttrs(r.Context(), slog.LevelWarn, "stream receive failed",
				slog.String("vendor", rec.Vendor),
				slog.String("err", err.Error()),
			)
			sink.SendError(err)
			sink.SendDone()
			return
		}

		payload, err := json.Marshal(event)
		if err != nil {
			perr := &provider.Error{Kind: provider.KindUnknown, Message: "encode stream event: " + err.Error(), Cause: err}
			rec.ErrorKind = perr.Kind
			sink.SendError(perr)
			sink.SendDone()
			return
		}
		sink.Send(payload)
	}
}

type recvResult struct {
	event *provider.Event
	err   error
}

func recvWithIdleTimeout(ctx context.Context, stream provider.Stream, timeout time.Duration) (*provider.Event, error) {
	ch := make(chan recvResult, 1)
	go func() {
		event, err := stream.Recv()
		ch <- recvResult{event: event, err: err}
	}()

	var timeoutC <-chan time.Time
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}

	select {
	case got := <-ch:
		return got.event, got.err
	case <-timeoutC:
		_ = stream.Close()
		<-ch
		return nil, &provider.Error{Kind: provider.KindTimeout, Message: "stream idle timeout"}
	case <-ctx.Done():
		_ = stream.Close()
		<-ch
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &provider.Error{Kind: provider.KindTimeout, Message: ctx.Err().Error(), Cause: ctx.Err()}
		}
		return nil, ctx.Err()
	}
}
