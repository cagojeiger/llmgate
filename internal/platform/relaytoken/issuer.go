// Package relaytoken signs publish-only RelayGate grants approved by the application.
// It never authenticates callers or grants dial access to workers.
package relaytoken

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type Profile struct {
	Destination string `json:"destination"`
	Version     string `json:"version"`
}
type Config struct {
	Issuer          string             `json:"issuer"`
	Audience        string             `json:"audience"`
	KeyID           string             `json:"key_id"`
	PrivateKeyFile  string             `json:"private_key_file"`
	GatewayEndpoint string             `json:"gateway_endpoint"`
	TTLSeconds      int                `json:"ttl_seconds"`
	Profiles        map[string]Profile `json:"profiles"`
}
type Issuer struct {
	config Config
	key    *ecdsa.PrivateKey
}
type Grant struct {
	ProtocolVersion int    `json:"protocol_version"`
	Profile         string `json:"profile"`
	ProfileVersion  string `json:"profile_version"`
	Destination     string `json:"destination"`
	GatewayEndpoint string `json:"gateway_endpoint"`
	AccessToken     string `json:"access_token"`
	ExpiresAt       int64  `json:"expires_at"`
}

func LoadFromEnv() (*Issuer, error) {
	path := os.Getenv("LLMGATE_WORKERS_CONFIG")
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path) // #nosec G703 -- path is operator-owned process configuration, never request input.
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<10 {
		return nil, errors.New("workers config too large")
	}
	var cfg Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&cfg); err != nil {
		return nil, err
	}
	if err = d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing workers config data")
	}
	return New(cfg)
}

var namespace = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var segment = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

func New(cfg Config) (*Issuer, error) {
	if cfg.Issuer == "" || cfg.Audience == "" || cfg.KeyID == "" || len(cfg.Profiles) == 0 {
		return nil, errors.New("worker issuer identity and profiles are required")
	}
	if cfg.TTLSeconds == 0 {
		cfg.TTLSeconds = 300
	}
	if cfg.TTLSeconds < 60 || cfg.TTLSeconds > 900 {
		return nil, errors.New("worker token TTL must be 60..900 seconds")
	}
	u, err := url.Parse(cfg.GatewayEndpoint)
	if err != nil || u.Scheme != "tls" || u.Hostname() == "" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, errors.New("worker Gateway must be tls://host:port")
	}
	for name, p := range cfg.Profiles {
		parts := strings.SplitN(p.Destination, "/", 2)
		if name == "" || p.Version != "1" || len(parts) != 2 || !namespace.MatchString(parts[0]) || len(p.Destination) > 1024 {
			return nil, fmt.Errorf("invalid worker profile %q", name)
		}
		for _, s := range strings.Split(parts[1], "/") {
			if !segment.MatchString(s) || s == "." || s == ".." {
				return nil, fmt.Errorf("invalid worker destination for %q", name)
			}
		}
	}
	b, err := os.ReadFile(cfg.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read worker signing key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("worker signing key must be PEM")
	}
	var key *ecdsa.PrivateKey
	if block.Type == "EC PRIVATE KEY" {
		key, err = x509.ParseECPrivateKey(block.Bytes)
	} else {
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			key, _ = parsed.(*ecdsa.PrivateKey)
		}
	}
	if err != nil || key == nil || key.Curve != elliptic.P256() {
		return nil, errors.New("worker signing key must be ES256 P-256")
	}
	return &Issuer{config: cfg, key: key}, nil
}

func (i *Issuer) Issue(profile, version string, now time.Time) (*Grant, error) {
	p, ok := i.config.Profiles[profile]
	if !ok || p.Version != version {
		return nil, errors.New("unsupported worker profile/version")
	}
	parts := strings.SplitN(p.Destination, "/", 2)
	expires := now.Add(time.Duration(i.config.TTLSeconds) * time.Second).Unix()
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": i.config.KeyID, "typ": "relaygate-operation+jwt"})
	claims, _ := json.Marshal(map[string]any{"iss": i.config.Issuer, "aud": i.config.Audience, "nbf": now.Unix() - 5, "exp": expires, "permissions": []any{map[string]any{"action": "publish", "namespace": parts[0], "scope": map[string]string{"kind": "exact", "name": parts[1]}}}})
	b64 := base64.RawURLEncoding.EncodeToString
	message := b64(header) + "." + b64(claims)
	digest := sha256.Sum256([]byte(message))
	r, s, err := ecdsa.Sign(rand.Reader, i.key, digest[:])
	if err != nil {
		return nil, errors.New("worker token signing failed")
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return &Grant{ProtocolVersion: 1, Profile: profile, ProfileVersion: p.Version, Destination: p.Destination, GatewayEndpoint: i.config.GatewayEndpoint, AccessToken: message + "." + b64(signature), ExpiresAt: expires}, nil
}
