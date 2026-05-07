package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"llmgate/internal/audit"
	"llmgate/internal/llmtypes"
	"llmgate/internal/llmrouter"
)

const maxChatRequestBytes = 1 << 20

// ChatService is the upstream contract Handler needs.
type ChatService interface {
	Complete(ctx context.Context, req *llmtypes.Request) (*llmrouter.RouteResult, error)
	CompleteStream(ctx context.Context, req *llmtypes.Request) (*llmrouter.RouteResult, error)
}

type Handler struct {
	service        ChatService
	log            *slog.Logger
	recorder       audit.Recorder
	requestTimeout time.Duration
	stream         *streamRelay
}

type HandlerConfig struct {
	RequestTimeout    time.Duration
	StreamIdleTimeout time.Duration
}

func NewHandler(service ChatService, log *slog.Logger, recorder audit.Recorder, cfg HandlerConfig) *Handler {
	if log == nil {
		log = slog.Default()
	}
	if recorder == nil {
		recorder = audit.Nop{}
	}
	return &Handler{
		service:        service,
		log:            log,
		recorder:       recorder,
		requestTimeout: cfg.RequestTimeout,
		stream:         newStreamRelay(log, cfg.StreamIdleTimeout),
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

	consumer := ConsumerFromContext(ctx)
	rec := &audit.Record{
		Timestamp:     start,
		RequestID:     RequestIDFromContext(ctx),
		Method:        "chat.completions",
		ConsumerName:  consumer.Name,
		ConsumerKeyID: consumer.KeyID,
	}
	defer func() {
		rec.DurationMS = time.Since(start).Milliseconds()
		h.recorder.Record(ctx, rec)
	}()

	if consumer.AuthError != "" {
		// Auth middleware ran but rejected; emit the audit record
		// (audit-always — ADR 008) and return 401. The specific
		// AuthErrorKind stays out of the wire response — callers see
		// only "unauthorized" — but is stamped on rec.AuthError so
		// audit/access-log surfaces show "missing" vs "format" vs
		// "unknown" for operators.
		rec.AuthError = string(consumer.AuthError)
		perr := &llmtypes.Error{ErrorKind: llmtypes.KindAuth, Message: "unauthorized"}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBytes))
	if err != nil {
		perr := &llmtypes.Error{ErrorKind: llmtypes.KindBadRequest, Message: "read request body: " + err.Error()}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}
	rec.RequestBytes = int64(len(body))

	req := &llmtypes.Request{}
	if err := json.Unmarshal(body, req); err != nil {
		perr := &llmtypes.Error{ErrorKind: llmtypes.KindBadRequest, Message: "decode request: " + err.Error()}
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
func adoptRoute(rec *audit.Record, result *llmrouter.RouteResult) {
	if result == nil {
		return
	}
	rec.Attempts = result.Attempts
	rec.Vendor = result.Vendor
	rec.ModelUsed = result.ModelUsed
}

// adoptError populates rec.ErrorKind and rec.StatusCode from err.
func adoptError(rec *audit.Record, err error) {
	rec.ErrorKind = llmtypes.ErrorKindOf(err)
	rec.StatusCode = errStatus(err)
}

// adoptStreamSummary finalizes stream audit fields after the stream ends.
func adoptStreamSummary(rec *audit.Record, sum *llmtypes.Summary, now time.Time) {
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

func (h *Handler) serveComplete(w http.ResponseWriter, r *http.Request, req *llmtypes.Request, rec *audit.Record) {
	result, err := h.service.Complete(r.Context(), req)
	adoptRoute(rec, result)
	if err != nil {
		adoptError(rec, err)
		writeError(w, err)
		return
	}

	out, err := json.Marshal(result.Response)
	if err != nil {
		perr := &llmtypes.Error{ErrorKind: llmtypes.KindUnknown, Message: "encode response: " + err.Error(), Cause: err}
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
	if _, werr := w.Write(out); werr != nil {
		rec.ErrorKind = llmtypes.KindClientClosed
		h.log.LogAttrs(r.Context(), slog.LevelInfo, "client write failed",
			slog.String("vendor", rec.Vendor),
			slog.String("err", werr.Error()),
		)
	}
}

func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, req *llmtypes.Request, rec *audit.Record) {
	result, err := h.service.CompleteStream(r.Context(), req)
	adoptRoute(rec, result)
	if err != nil {
		adoptError(rec, err)
		writeError(w, err)
		return
	}
	stream := result.Stream
	defer stream.Close()
	defer func() { adoptStreamSummary(rec, stream.Summary(), time.Now()) }()

	h.stream.Run(r.Context(), w, stream, rec)
}
