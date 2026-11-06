package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"llmgate/internal/domain/consumers"
	llmresultschema "llmgate/internal/domain/llmresult/schema"
	"llmgate/internal/platform/audit"
	"llmgate/internal/platform/config"
	"llmgate/internal/platform/http/server"
	httpsoniox "llmgate/internal/platform/http/soniox"
	slogtelemetry "llmgate/internal/platform/telemetry/slog"
)

type sonioxIntegrationFrame struct {
	typ     websocket.MessageType
	payload []byte
}

type sonioxFixtureObject struct {
	key     string
	payload []byte
}

type sonioxFixtureStore chan sonioxFixtureObject

func (store sonioxFixtureStore) Put(ctx context.Context, key, filePath string) error {
	payload, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	select {
	case store <- sonioxFixtureObject{key: key, payload: payload}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSonioxRealtime_ArchivesExactSession(t *testing.T) {
	const (
		consumerKey = "soniox-test-consumer-bearer"
		providerKey = "soniox-test-provider-bearer"
		requestID   = "soniox-complete-session"
		model       = "stt-rt-v6"
	)
	inputs := []sonioxIntegrationFrame{
		{websocket.MessageText, []byte(`{ "model": "stt-rt-v6", "audio_format": "pcm_s16le", "sample_rate": 16000, "num_channels": 1, "language_hints": ["ko"] }`)},
		{websocket.MessageBinary, []byte{0x00, 0xff, 0x04, 0x00, 0x7f, 0x00, 0x80, 0xff}},
		{websocket.MessageText, []byte(`{ "type" : "finalize" }`)},
		{websocket.MessageText, nil},
	}
	outputs := []sonioxIntegrationFrame{
		{websocket.MessageText, []byte(`{ "tokens" : [{ "text": "합성", "is_final": true }, {"text":"<fin>","is_final":true}], "final_audio_proc_ms": 3 }`)},
		{websocket.MessageText, []byte(`{ "tokens": [], "finished" : true, "total_audio_proc_ms" : 3 }`)},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	providerDone := make(chan error, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerDone <- serveSonioxFixture(ctx, w, r, providerKey, inputs, outputs)
	}))
	defer provider.Close()

	consumerDir := t.TempDir()
	consumer := fmt.Sprintf("name: test-soniox\nkey_hashes:\n  - sha256:%x\nallowed_aliases:\n  - %s\n", sha256.Sum256([]byte(consumerKey)), model)
	if err := os.WriteFile(filepath.Join(consumerDir, "test-soniox.yaml"), []byte(consumer), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := consumers.LoadDir(consumerDir)
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	cfg := &config.Server{AuditDir: t.TempDir(), AuditCompression: audit.CompressionNone, RequestTimeout: 5 * time.Second}
	results, err := buildResultSink(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = results.Close() })
	uploaded := make(sonioxFixtureStore, 2)
	archive, err := audit.NewSonioxArchive(auditConfig(cfg), uploaded, "test-prefix", logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	h := httpsoniox.NewHandler(logger, slogtelemetry.NewSink(logger, logger), httpsoniox.Config{
		APIKey:         providerKey,
		Endpoint:       "ws" + strings.TrimPrefix(provider.URL, "http") + "/transcribe-websocket",
		ServiceVersion: "test",
		Environment:    "test",
		ResultSink:     results,
		BeginCapture: func(requestID, model string) (httpsoniox.Capture, error) {
			return archive.Begin(requestID, model)
		},
	})
	t.Cleanup(func() { _ = h.Close() })
	gateway := server.NewWithOptions(server.ServerOptions{
		Config:        cfg,
		Log:           logger,
		Handler:       http.NotFoundHandler(),
		SonioxHandler: h,
		Consumers:     store,
	})
	gatewayDone := make(chan struct{}, 2)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gateway.Handler.ServeHTTP(w, r)
		gatewayDone <- struct{}{}
	}))
	defer proxy.Close()

	endpoint := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/v1/soniox/realtime"
	denied, response, err := websocket.Dial(ctx, endpoint, nil)
	if err == nil {
		_ = denied.CloseNow()
		t.Fatal("unauthenticated client upgraded")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upgrade response = %v, want 401", response)
	}
	waitSonioxGateway(t, ctx, gatewayDone)
	if len(providerDone) != 0 {
		t.Fatal("unauthenticated client reached the provider")
	}

	client, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization": {"Bearer " + consumerKey},
		"X-Request-Id":  {requestID},
	}})
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer func() { _ = client.CloseNow() }()
	if response.Header.Get("X-Request-Id") != requestID {
		t.Fatalf("upgrade request ID = %q, want %q", response.Header.Get("X-Request-Id"), requestID)
	}
	for i, frame := range inputs {
		if err := client.Write(ctx, frame.typ, frame.payload); err != nil {
			t.Fatalf("write client frame %d: %v", i, err)
		}
		switch i {
		case 2, 3:
			if err := readSonioxFixtureFrame(ctx, client, outputs[i-2]); err != nil {
				t.Fatalf("read client output %d: %v", i-2, err)
			}
		}
	}
	if _, _, err := client.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("client close = %v, want normal close after finished", err)
	}
	select {
	case err := <-providerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("provider fixture did not finish")
	}
	waitSonioxGateway(t, ctx, gatewayDone)

	if err := archive.Close(); err != nil {
		t.Fatalf("flush Soniox archive: %v", err)
	}
	if err := results.Close(); err != nil {
		t.Fatalf("flush result records: %v", err)
	}
	if len(uploaded) != 1 {
		t.Fatalf("uploaded %d Soniox captures, want 1", len(uploaded))
	}
	object := <-uploaded
	captureID := checkSonioxSessionArchive(t, object, requestID, model, inputs, outputs)
	checkSonioxResultLink(t, cfg.AuditDir, requestID, model, captureID)
	for _, secret := range []string{consumerKey, providerKey} {
		if strings.Contains(logs.String(), secret) || bytes.Contains(object.payload, []byte(secret)) {
			t.Fatal("operational logs or the capture contain a bearer token")
		}
	}
	if strings.Contains(logs.String(), "합성") || strings.Contains(logs.String(), "language_hints") {
		t.Fatal("operational logs contain frame payload data")
	}
}

func waitSonioxGateway(t *testing.T, ctx context.Context, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("gateway did not finish writing the session result")
	}
}

func serveSonioxFixture(ctx context.Context, w http.ResponseWriter, r *http.Request, key string, inputs, outputs []sonioxIntegrationFrame) error {
	if r.URL.Path != "/transcribe-websocket" || r.Header.Get("Authorization") != "Bearer "+key {
		http.Error(w, "unexpected fixture upgrade", http.StatusBadRequest)
		return fmt.Errorf("unexpected provider path or authentication")
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return fmt.Errorf("upgrade provider fixture: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	for i, frame := range inputs {
		if err := readSonioxFixtureFrame(ctx, conn, frame); err != nil {
			return fmt.Errorf("read provider input %d: %w", i, err)
		}
		switch i {
		case 2, 3:
			out := outputs[i-2]
			if err := conn.Write(ctx, out.typ, out.payload); err != nil {
				return fmt.Errorf("write provider output %d: %w", i-2, err)
			}
		}
	}
	return nil
}

func readSonioxFixtureFrame(ctx context.Context, conn *websocket.Conn, want sonioxIntegrationFrame) error {
	typ, payload, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	if typ != want.typ || !bytes.Equal(payload, want.payload) {
		return fmt.Errorf("frame = (%v, %q), want (%v, %q)", typ, payload, want.typ, want.payload)
	}
	return nil
}

func checkSonioxSessionArchive(t *testing.T, object sonioxFixtureObject, requestID, model string, inputs, outputs []sonioxIntegrationFrame) string {
	t.Helper()
	expected := []struct {
		direction string
		frame     sonioxIntegrationFrame
	}{
		{"request", inputs[0]}, {"request", inputs[1]}, {"request", inputs[2]},
		{"response", outputs[0]}, {"request", inputs[3]}, {"response", outputs[1]},
	}
	lines := bytes.SplitAfter(object.payload, []byte("\n"))
	footerIndex := len(expected) + 1
	if len(lines) != footerIndex+2 || len(lines[footerIndex+1]) != 0 {
		t.Fatalf("capture should contain one header, six frames and one footer: %s", object.payload)
	}
	var header struct {
		Type      string `json:"type"`
		Version   int    `json:"version"`
		CaptureID string `json:"capture_id"`
		RequestID string `json:"request_id"`
		Model     string `json:"model"`
	}
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatal(err)
	}
	if header.Type != "header" || header.Version != 1 || header.RequestID != requestID || header.Model != model || header.CaptureID == "" {
		t.Fatalf("unexpected capture header: %s", lines[0])
	}
	if !strings.HasPrefix(object.key, "test-prefix/soniox-realtime/dt=") || !strings.HasSuffix(object.key, "-"+header.CaptureID+".jsonl") {
		t.Fatalf("uploaded object key = %q, want partitioned key with capture ID %q", object.key, header.CaptureID)
	}

	var requestBytes, responseBytes, lastOffset int64
	for i, want := range expected {
		var frame struct {
			Type      string `json:"type"`
			Sequence  int    `json:"sequence"`
			OffsetNS  int64  `json:"offset_ns"`
			Direction string `json:"direction"`
			Kind      string `json:"kind"`
			Payload   []byte `json:"payload"`
		}
		if err := json.Unmarshal(lines[i+1], &frame); err != nil {
			t.Fatal(err)
		}
		kind := "text"
		if want.frame.typ == websocket.MessageBinary {
			kind = "binary"
		}
		if frame.Type != "frame" || frame.Sequence != i+1 || frame.OffsetNS < lastOffset ||
			frame.Direction != want.direction || frame.Kind != kind || frame.Payload == nil ||
			!bytes.Equal(frame.Payload, want.frame.payload) {
			t.Fatalf("unexpected capture frame %d: %s", i+1, lines[i+1])
		}
		lastOffset = frame.OffsetNS
		if want.direction == "request" {
			requestBytes += int64(len(frame.Payload))
		} else {
			responseBytes += int64(len(frame.Payload))
		}
	}
	var footer struct {
		Type          string `json:"type"`
		Complete      bool   `json:"complete"`
		Reason        string `json:"reason"`
		Frames        int    `json:"frames"`
		RequestBytes  int64  `json:"request_bytes"`
		ResponseBytes int64  `json:"response_bytes"`
		SHA256        string `json:"sha256"`
	}
	if err := json.Unmarshal(lines[footerIndex], &footer); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(bytes.Join(lines[:footerIndex], nil)))
	if footer.Type != "footer" || !footer.Complete || footer.Reason != "finished" ||
		footer.Frames != len(expected) || footer.RequestBytes != requestBytes ||
		footer.ResponseBytes != responseBytes || footer.SHA256 != digest {
		t.Fatalf("unexpected capture footer: %s", lines[footerIndex])
	}
	return header.CaptureID
}

func checkSonioxResultLink(t *testing.T, auditDir, requestID, model, captureID string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(auditDir, "compressed", "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("result files = %v, error = %v, want local JSONL", files, err)
	}
	matches := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			var result llmresultschema.Event
			if err := json.Unmarshal(line, &result); err != nil {
				t.Fatal(err)
			}
			if result.RequestID != requestID {
				continue
			}
			matches++
			if result.StatusCode != http.StatusOK || result.ModelUsed != model || result.ConsumerName != "test-soniox" ||
				result.SonioxCaptureID != captureID || result.SonioxCaptureComplete == nil || !*result.SonioxCaptureComplete {
				t.Fatalf("result does not identify the complete capture: %s", line)
			}
		}
	}
	if matches != 1 {
		t.Fatalf("found %d finalized results for request %q, want 1", matches, requestID)
	}
}
