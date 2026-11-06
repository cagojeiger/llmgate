package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/audit"
	httpsoniox "llmgate/internal/platform/http/soniox"
)

func buildSonioxHandler(in RuntimeInput, events telemetry.EventSink, results llmresultsink.Sink) (http.Handler, func() error, error) {
	cfg := in.Config
	if !cfg.SonioxRealtimeEnabled {
		return nil, nil, nil
	}
	if cfg.AuditDir == "" {
		return nil, nil, errors.New("soniox realtime requires LLMGATE_AUDIT_DIR")
	}
	key := strings.TrimSpace(os.Getenv("LLMGATE_SONIOX_API_KEY"))
	if key == "" {
		return nil, nil, errors.New("soniox realtime requires LLMGATE_SONIOX_API_KEY")
	}

	store, err := buildAuditStore(cfg, in.Logger)
	if err != nil {
		return nil, nil, err
	}
	captureConfig := auditConfig(cfg)
	captureConfig.DiskCap = cfg.SonioxArchiveDiskCap
	archive, err := audit.NewSonioxArchive(captureConfig, store, cfg.AuditS3Prefix, in.Logger)
	if err != nil {
		return nil, nil, fmt.Errorf("build soniox realtime archive: %w", err)
	}
	handler := httpsoniox.NewHandler(in.Logger, events, httpsoniox.Config{
		APIKey:         key,
		ServiceVersion: in.Version,
		Environment:    cfg.Environment,
		ResultSink:     results,
		BeginCapture: func(requestID, model string) (httpsoniox.Capture, error) {
			return archive.Begin(requestID, model)
		},
	})
	return handler, func() error { return errors.Join(handler.Close(), archive.Close()) }, nil
}
