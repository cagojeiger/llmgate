package relaytoken

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublishGrantSignatureAndScope(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "issuer.pem")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	issuer, err := New(Config{Issuer: "test", Audience: "relaygate", KeyID: "test", PrivateKeyFile: path, GatewayEndpoint: "tls://localhost:27420", Profiles: map[string]Profile{"embedding": {Destination: "llmgate/embedding-v1", Version: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000000000, 0)
	grant, err := issuer.Issue("embedding", "1", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(grant.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatal("not JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatal("not JOSE ES256 signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&key.PublicKey, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("invalid signature")
	}
	claimsBytes, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Exp         int64 `json:"exp"`
		Permissions []struct {
			Action, Namespace string
			Scope             struct{ Kind, Name string }
		}
	}
	if err = json.Unmarshal(claimsBytes, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Exp != now.Unix()+300 || len(claims.Permissions) != 1 {
		t.Fatalf("unexpected lifetime or permission count: %+v", claims)
	}
	p := claims.Permissions[0]
	if p.Action != "publish" || p.Namespace != "llmgate" || p.Scope.Kind != "exact" || p.Scope.Name != "embedding-v1" {
		t.Fatalf("overbroad permission: %+v", p)
	}
	if _, err = issuer.Issue("embedding", "2", now); err == nil {
		t.Fatal("unknown version accepted")
	}
	if _, err = issuer.Issue("other", "1", now); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestCallerAddressDefaultsAndValidation(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Issuer: "test", Audience: "relaygate", KeyID: "test", PrivateKeyFile: path, GatewayEndpoint: "tls://localhost:443", Profiles: map[string]Profile{"embedding": {Destination: "llmgate/e", Version: "1"}}}
	issuer, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if issuer.Profiles()["embedding"].CallerAddress != "127.0.0.1:18081" {
		t.Fatal("missing default")
	}
	if cfg.Profiles["embedding"].CallerAddress != "" {
		t.Fatal("mutated caller config")
	}
	for _, address := range []string{"0.0.0.0:18081", "localhost:18081", "127.0.0.1:0"} {
		cfg.Profiles["embedding"] = Profile{Destination: "llmgate/e", Version: "1", CallerAddress: address}
		if _, err := New(cfg); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
}
