package workers

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/http/auth"
	"llmgate/internal/platform/relaytoken"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenEndpointPermissionBoundary(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	issuer, err := relaytoken.New(relaytoken.Config{Issuer: "test", Audience: "relaygate", KeyID: "test", PrivateKeyFile: path, GatewayEndpoint: "tls://localhost:27420", Profiles: map[string]relaytoken.Profile{"embedding": {Destination: "llmgate/embed", Version: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	h := New(issuer, telemetry.NopSink{}, "test", "test")
	body := `{"protocol_version":1,"profile":"embedding","profile_version":"1"}`
	for _, test := range []struct {
		name     string
		consumer auth.ConsumerInfo
		body     string
		code     int
	}{
		{"anonymous", auth.ConsumerInfo{}, body, 401},
		{"caller_only", auth.ConsumerInfo{Name: "caller", AllowedAliases: []string{"embed"}}, body, 403},
		{"publisher", auth.ConsumerInfo{Name: "worker", AllowedWorkerProfiles: []string{"embedding"}}, body, 200},
		{"cannot_choose_action", auth.ConsumerInfo{Name: "worker", AllowedWorkerProfiles: []string{"embedding"}}, strings.TrimSuffix(body, "}") + `,"action":"dial"}`, 400},
		{"trailing_json", auth.ConsumerInfo{Name: "worker", AllowedWorkerProfiles: []string{"embedding"}}, body + `{}`, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/workers/token", strings.NewReader(test.body))
			r = r.WithContext(auth.WithConsumer(r.Context(), &test.consumer))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.code {
				t.Fatalf("status %d want %d: %s", w.Code, test.code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
		})
	}
}
