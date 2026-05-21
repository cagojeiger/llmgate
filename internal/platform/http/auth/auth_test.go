package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgate/internal/domain/telemetry"
)

func TestClassify_NilStore_DoesNotPanic(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer some-key")

	got := Classify(req, nil)

	if got.AuthError != telemetry.AuthErrorUnknown {
		t.Fatalf("AuthError = %q, want %q", got.AuthError, telemetry.AuthErrorUnknown)
	}
	if got.Name != "" || got.KeyID != "" {
		t.Errorf("identity leaked through nil-store classify: %+v", got)
	}
}

func TestClassify_NilStore_MissingHeader_KeepsMissingError(t *testing.T) {
	// Nil-store guard runs after the header-presence checks so the
	// auth_error continues to differentiate "no header" from "header
	// present but no backing store".
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	got := Classify(req, nil)

	if got.AuthError != telemetry.AuthErrorMissing {
		t.Fatalf("AuthError = %q, want %q", got.AuthError, telemetry.AuthErrorMissing)
	}
}
