package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"llmgate/internal/audit"
	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
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
		Operation:     "chat.completions",
		ConsumerName:  consumer.Name,
		ConsumerKeyID: consumer.KeyID,
	}
	defer func() {
		rec.DurationMS = time.Since(start).Milliseconds()
		h.recorder.Record(ctx, rec)
	}()
	// Self-contained panic guard. Stamps rec.Kind = KindPanic on the
	// audit record so the audit-always invariant (ADR 003) holds even
	// when handler logic panics — without this, the deferred Record
	// above would still fire but with rec.Kind / rec.StatusCode at
	// their last-assigned values (often empty / 0), making panic
	// spikes invisible in the audit stream and indistinguishable from
	// other failures during forensics.
	//
	// Registered AFTER the audit defer so it runs FIRST (LIFO):
	// stamp first → audit defer reads the stamped record afterward.
	//
	// Best-effort 500 response. If the response has already started
	// (mid-stream SSE), the underlying ResponseWriter ignores further
	// headers/body — the audit row is still stamped, which is the
	// invariant. The wire message is intentionally generic so panic
	// internals don't reach the caller; the panic value + full stack
	// trace go to slog (ERROR) for operator visibility.
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		// http.ErrAbortHandler is net/http's documented sentinel: the
		// handler intentionally panicked to abort the response without
		// writing anything further (chi.Recoverer also lets this one
		// through). Re-panic so the standard abort path runs as
		// intended — and do NOT stamp it as KindPanic in the audit
		// stream, since it is an intentional protocol signal rather
		// than a fault. The audit defer above still fires (audit-always
		// invariant), but with the kind left at whatever the request
		// reached before the abort.
		if p == http.ErrAbortHandler {
			panic(p)
		}
		rec.Kind = llmtypes.KindPanic
		if rec.StatusCode == 0 {
			rec.StatusCode = http.StatusInternalServerError
		}
		h.log.LogAttrs(ctx, slog.LevelError, "handler panic",
			slog.String("request_id", rec.RequestID),
			slog.Any("panic", p),
			slog.String("stack", string(debug.Stack())),
		)
		// If the response has already started — the typical case for
		// a streaming panic mid-Run, when SSE headers (200, text/
		// event-stream) are already flushed — net/http silently drops
		// a second WriteHeader but still sends raw body bytes. Writing
		// our JSON error envelope on top of an in-flight SSE stream
		// would corrupt the framing the client is decoding. Skip the
		// body write in that case; the audit row is still stamped,
		// which is the invariant. The slog ERROR above carries the
		// full diagnostic for operators.
		if cw, ok := w.(*countingWriter); ok && cw.wroteHeader {
			return
		}
		writeError(w, &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "internal server error"})
	}()

	if consumer.AuthError != "" {
		// Auth middleware ran but rejected; emit the audit record
		// (audit-always — ADR 003) and return 401. The specific
		// audit.AuthError stays out of the wire response — callers see
		// only "unauthorized" — but is stamped on rec.AuthError so
		// audit/access-log surfaces show "missing" vs "format" vs
		// "unknown" for operators.
		rec.AuthError = consumer.AuthError
		perr := &llmtypes.Error{Kind: llmtypes.KindAuth, Message: "unauthorized"}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBytes))
	if err != nil {
		perr := &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "read request body: " + err.Error()}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}
	rec.RequestBytes = int64(len(body))

	req := &llmtypes.Request{}
	if err := json.Unmarshal(body, req); err != nil {
		perr := &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "decode request: " + err.Error()}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}
	rec.ModelRequested = req.Model

	if req.Stream != nil && *req.Stream {
		rec.Operation = "chat.completions.stream"
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

// adoptError populates rec.Kind and rec.StatusCode from err.
func adoptError(rec *audit.Record, err error) {
	rec.Kind = llmtypes.ErrorKindOf(err)
	rec.StatusCode = errStatus(err)
}

// adoptStreamSummary finalizes stream audit fields after the stream ends.
func adoptStreamSummary(rec *audit.Record, sum *llmtypes.Summary, now time.Time) {
	if len(rec.Attempts) > 0 {
		last := &rec.Attempts[len(rec.Attempts)-1]
		last.DurationMS = now.Sub(last.StartedAt).Milliseconds()
		if last.Kind == "" && rec.Kind != "" {
			last.Kind = rec.Kind
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
		perr := &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "encode response: " + err.Error(), Cause: err}
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
		rec.Kind = llmtypes.KindClientClosed
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
