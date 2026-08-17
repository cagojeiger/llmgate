package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
)

// completedType is the upstream frame that closes one transcription turn. Its
// `transcript` field is the finalized text the gateway accumulates for audit.
const completedType = "conversation.item.input_audio_transcription.completed"

// broker dials the upstream realtime endpoint and relays frames in both
// directions until either side closes. It owns the client connection's
// lifetime: on return the client WS is closed. It returns the accumulated
// completed-turn count and transcript(s) and stamps rec with the session
// outcome.
// maxRealtimeMessageBytes caps a single WS message on both the client and
// upstream conns. coder/websocket defaults to 32 KiB, which rejects audio
// append frames (base64 PCM16 for even a few seconds exceeds it) with a 1009
// close. 16 MiB is generous for realtime audio chunks while still bounding
// per-message memory against a hostile peer.
const maxRealtimeMessageBytes = 16 << 20

func (h *Handler) broker(clientConn *websocket.Conn, endpoint string, rec *telemetry.AuditEvent) (int, []string) {
	// The relay outlives the request: after the WS upgrade the request ctx is
	// canceled (the conn is hijacked), so a fresh session ctx — detached from
	// the request — governs both relay goroutines. Canceling it is how one
	// direction's close unblocks the other's pending Read.
	//nolint:contextcheck // WS relay is session-scoped and must detach from the hijacked request ctx, mirroring the async audit sink's intentional detach
	sessionCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// coder/websocket owns the handshake *http.Response body — the caller never
	// closes it (nil on success, drained by Dial on failure), so bodyclose is
	// suppressed rather than double-closing it.
	upstreamConn, _, err := websocket.Dial(sessionCtx, endpoint, nil) //nolint:bodyclose // handshake response body is managed by coder/websocket
	if err != nil {
		// Surface the failure to the client as a protocol `error` frame before
		// closing, so an SDK sees why the session never started.
		writeErrorFrame(sessionCtx, clientConn, "upstream_unavailable", err.Error())
		_ = clientConn.Close(websocket.StatusBadGateway, "upstream dial failed")
		rec.Kind = llmtypes.KindUpstream
		rec.StatusCode = http.StatusBadGateway
		h.log.LogAttrs(sessionCtx, slog.LevelWarn, "realtime upstream dial failed",
			slog.String("request_id", rec.RequestID),
			slog.String("endpoint", endpoint),
			slog.String("err", err.Error()),
		)
		return 0, nil
	}
	defer func() { _ = upstreamConn.CloseNow() }()
	upstreamConn.SetReadLimit(maxRealtimeMessageBytes)

	var (
		turns       int
		transcripts []string
		wg          sync.WaitGroup
	)
	wg.Add(2)

	// client → upstream: session.update / input_audio_buffer.append|commit,
	// relayed verbatim. No sniffing — the gateway does not reinterpret client
	// intent.
	go func() {
		defer wg.Done()
		defer cancel()
		relay(sessionCtx, clientConn, upstreamConn, nil)
	}()

	// upstream → client: session.* / transcription deltas + completed + error,
	// relayed verbatim while accumulating completed transcripts for audit. Only
	// this goroutine mutates turns/transcripts, and wg.Wait() below
	// happens-before the read, so no synchronization is needed on them.
	go func() {
		defer wg.Done()
		defer cancel()
		relay(sessionCtx, upstreamConn, clientConn, func(data []byte) {
			if t, ok := sniffCompleted(data); ok {
				turns++
				if t != "" {
					transcripts = append(transcripts, t)
				}
			}
		})
	}()

	wg.Wait()
	// A relayed session that reached this point completed its lifecycle; record
	// it as a success. Frame-level upstream `error` events are the client's to
	// interpret and do not fail the broker.
	_ = clientConn.Close(websocket.StatusNormalClosure, "")
	rec.StatusCode = 200
	return turns, transcripts
}

// relay copies frames from src to dst until either side errors or closes,
// preserving the message type. onFrame, when non-nil, observes each frame's
// payload before it is forwarded (used to sniff completed transcripts).
func relay(ctx context.Context, src, dst *websocket.Conn, onFrame func([]byte)) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return
		}
		if onFrame != nil {
			onFrame(data)
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

// sniffCompleted reports whether data is a completed-transcription frame and,
// if so, returns its transcript. Non-JSON frames (e.g. binary audio, should the
// upstream ever echo any) and other event types are ignored.
func sniffCompleted(data []byte) (string, bool) {
	var frame struct {
		Type       string `json:"type"`
		Transcript string `json:"transcript"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return "", false
	}
	if frame.Type != completedType {
		return "", false
	}
	return frame.Transcript, true
}

// writeErrorFrame sends an OpenAI-shaped `error` event to the client. Best
// effort — a write failure here means the client is already gone.
func writeErrorFrame(ctx context.Context, conn *websocket.Conn, code, message string) {
	payload, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    code,
			"message": message,
		},
	})
	if err != nil {
		return
	}
	_ = conn.Write(ctx, websocket.MessageText, payload)
}
