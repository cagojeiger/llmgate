package soniox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"llmgate/internal/domain/llmtypes"
)

const (
	maxSessionDuration = time.Hour
	startTimeout       = 15 * time.Second
	dialTimeout        = 10 * time.Second
	writeTimeout       = 5 * time.Second
	drainTimeout       = 10 * time.Second
	maxStartBytes      = 64 << 10
	maxMessageBytes    = 1 << 20
	statusClientClosed = 499
)

type outcome struct {
	status    int
	kind      llmtypes.ErrorKind
	reason    string
	message   string
	closeCode websocket.StatusCode
}

type sessionResult struct {
	outcome
	model     string
	captureID string
	complete  bool
}

type recording struct {
	Capture
	finalized bool
	complete  bool
}

func (r *recording) finish(complete bool, reason string) error {
	r.finalized = true
	err := r.Finish(complete, reason)
	r.complete = complete && err == nil
	return err
}

func (h *Handler) session(ctx context.Context, client *websocket.Conn, requestID string) (res sessionResult) {
	client.SetReadLimit(maxStartBytes)
	startCtx, cancelStart := context.WithTimeout(ctx, startTimeout)
	kind, payload, err := client.Read(startCtx)
	cancelStart()
	if err != nil {
		switch {
		case ctx.Err() != nil:
			res.outcome = canceledOutcome(ctx.Err())
		case errors.Is(err, context.DeadlineExceeded):
			res.outcome = outcome{status: http.StatusRequestTimeout, kind: llmtypes.KindTimeout, reason: "start_timeout"}
		default:
			res.outcome = readFailure(err, false)
		}
		return res
	}
	start := message{kind: kind, payload: payload}
	if !validStart(start) {
		res.outcome = badMessage("send a Soniox stt-rt-v6 start object without an API key")
		endClient(ctx, client, res.outcome)
		return res
	}

	capture, err := h.cfg.BeginCapture(requestID, Model)
	if err != nil || capture == nil {
		res.outcome = outcome{http.StatusServiceUnavailable, llmtypes.KindUpstream, "capture_begin_failed", "Soniox session recording is unavailable", websocket.StatusInternalError}
		endClient(ctx, client, res.outcome)
		return res
	}
	record := &recording{Capture: capture}
	res.captureID = record.ID()
	defer func() {
		if !record.finalized {
			reason := res.reason
			if reason == "" {
				reason = "server_shutdown"
			}
			if record.finish(false, reason) != nil {
				h.log.WarnContext(context.WithoutCancel(ctx), "Soniox incomplete capture could not be finalized", "request_id", requestID)
			}
		}
		res.complete = record.complete
	}()

	upstream, err := h.dial(ctx)
	if err != nil {
		res.outcome = upstreamFailure("upstream_dial_failed", "Soniox connection failed")
		if ctx.Err() != nil {
			res.outcome = canceledOutcome(ctx.Err())
		}
		endClient(ctx, client, res.outcome)
		return res
	}
	defer func() { _ = upstream.CloseNow() }()
	res.model = Model
	client.SetReadLimit(maxMessageBytes)
	upstream.SetReadLimit(maxMessageBytes)
	if res.outcome = relayRequest(ctx, upstream, record, start); res.reason != "" {
		endClient(ctx, client, res.outcome)
		return res
	}

	// Keep the reader alive until after endClient so its cancellation cannot
	// close the socket before a final response or a terminal error is sent.
	readerCtx, stopReaders := context.WithCancel(ctx)
	var readers sync.WaitGroup
	defer func() {
		stopReaders()
		readers.Wait()
	}()
	fromClient, fromUpstream := make(chan message), make(chan message)
	readers.Go(func() { readMessages(readerCtx, client, fromClient) })
	readers.Go(func() { readMessages(readerCtx, upstream, fromUpstream) })
	res.outcome = relay(ctx, client, upstream, record, fromClient, fromUpstream)
	endClient(ctx, client, res.outcome)
	return res
}

func (h *Handler) dial(ctx context.Context) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+h.cfg.APIKey)
	conn, _, err := websocket.Dial(ctx, h.cfg.Endpoint, &websocket.DialOptions{ //nolint:bodyclose // Dial owns the handshake body.
		HTTPClient: h.client,
		HTTPHeader: headers,
	})
	return conn, err
}

func relay(ctx context.Context, client, upstream *websocket.Conn, record *recording, fromClient, fromUpstream <-chan message) outcome {
	var drain <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return canceledOutcome(ctx.Err())
		case <-drain:
			return outcome{http.StatusGatewayTimeout, llmtypes.KindTimeout, "drain_timeout", "Soniox did not finish in time", websocket.StatusInternalError}
		case msg := <-fromClient:
			if msg.err != nil {
				if ctx.Err() != nil {
					return canceledOutcome(ctx.Err())
				}
				if drain != nil && !errors.Is(msg.err, websocket.ErrMessageTooBig) {
					// The client may disconnect after sending EOF. Finish the
					// upstream drain and recording even if it cannot receive them.
					fromClient = nil
					continue
				}
				return readFailure(msg.err, false)
			}
			if drain != nil || !validClientMessage(msg) {
				return badMessage("invalid Soniox audio or control message")
			}
			if result := relayRequest(ctx, upstream, record, msg); result.reason != "" {
				return result
			}
			if msg.endOfInput() {
				timer := time.NewTimer(drainTimeout)
				defer timer.Stop()
				drain = timer.C
			}
		case msg := <-fromUpstream:
			if msg.err != nil {
				if ctx.Err() != nil {
					return canceledOutcome(ctx.Err())
				}
				return readFailure(msg.err, true)
			}
			if record.Append("response", msg.captureKind(), msg.payload) != nil {
				return captureFailure("capture_write_failed")
			}
			if result, terminal := checkResponse(record, msg, drain != nil); terminal {
				if (result.reason == "finished" || result.reason == "upstream_error") && fromClient != nil {
					if err := writeMessage(ctx, client, msg); err != nil && result.reason == "finished" {
						return readFailure(err, false)
					}
				}
				return result
			}
			if fromClient != nil && writeMessage(ctx, client, msg) != nil {
				if drain == nil {
					return readFailure(nil, false)
				}
				fromClient = nil
			}
		}
	}
}

func relayRequest(ctx context.Context, upstream *websocket.Conn, record *recording, msg message) outcome {
	// Write to the archive first. A recording failure must not let the session
	// continue, and an upstream write failure leaves the archive incomplete.
	if record.Append("request", msg.captureKind(), msg.payload) != nil {
		return captureFailure("capture_write_failed")
	}
	if writeMessage(ctx, upstream, msg) != nil {
		if ctx.Err() != nil {
			return canceledOutcome(ctx.Err())
		}
		return upstreamFailure("upstream_write_failed", "Soniox connection write failed")
	}
	return outcome{}
}

func checkResponse(record *recording, msg message, inputEnded bool) (outcome, bool) {
	var result *providerResult
	if msg.kind != websocket.MessageText || json.Unmarshal(msg.payload, &result) != nil || result == nil {
		return upstreamFailure("upstream_protocol_error", "invalid Soniox response"), true
	}
	if result.failed() {
		status := result.ErrorCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		kind := llmtypes.KindUpstream
		switch status {
		case http.StatusTooManyRequests:
			kind = llmtypes.KindRateLimit
		case http.StatusRequestTimeout, http.StatusGatewayTimeout:
			kind = llmtypes.KindTimeout
		}
		return outcome{status: status, kind: kind, reason: "upstream_error", closeCode: websocket.StatusNormalClosure}, true
	}
	if !result.Finished {
		return outcome{}, false
	}
	if !inputEnded {
		return upstreamFailure("upstream_protocol_error", "Soniox finished before input ended"), true
	}
	// The client must never receive Soniox's completion marker before the exact
	// response and the archive's complete marker are locally committed.
	if record.finish(true, "finished") != nil {
		return captureFailure("capture_finalize_failed"), true
	}
	return outcome{status: http.StatusOK, reason: "finished", closeCode: websocket.StatusNormalClosure}, true
}

func badMessage(detail string) outcome {
	return outcome{http.StatusBadRequest, llmtypes.KindBadRequest, "client_protocol_error", detail, websocket.StatusPolicyViolation}
}

func captureFailure(reason string) outcome {
	return outcome{http.StatusServiceUnavailable, llmtypes.KindUpstream, reason, "Soniox session recording failed", websocket.StatusInternalError}
}

func upstreamFailure(reason, detail string) outcome {
	return outcome{http.StatusBadGateway, llmtypes.KindUpstream, reason, detail, websocket.StatusBadGateway}
}

func readFailure(err error, upstream bool) outcome {
	if errors.Is(err, websocket.ErrMessageTooBig) {
		if upstream {
			return upstreamFailure("upstream_message_too_large", "Soniox response exceeds message limit")
		}
		return outcome{http.StatusRequestEntityTooLarge, llmtypes.KindBadRequest, "client_message_too_large", "Soniox request exceeds message limit", websocket.StatusMessageTooBig}
	}
	if upstream {
		return upstreamFailure("upstream_disconnected", "Soniox closed without completing the session")
	}
	return outcome{status: statusClientClosed, kind: llmtypes.KindClientClosed, reason: "client_disconnected"}
}

func canceledOutcome(err error) outcome {
	if errors.Is(err, context.DeadlineExceeded) {
		return outcome{status: http.StatusGatewayTimeout, kind: llmtypes.KindTimeout, reason: "session_timeout"}
	}
	return outcome{status: http.StatusServiceUnavailable, kind: llmtypes.KindUpstream, reason: "server_shutdown"}
}

func endClient(ctx context.Context, client *websocket.Conn, result outcome) {
	if ctx.Err() != nil || result.closeCode == 0 {
		return
	}
	if result.message != "" {
		writeError(ctx, client, result.status, result.reason, result.message)
	}
	_ = client.Close(result.closeCode, "")
}
