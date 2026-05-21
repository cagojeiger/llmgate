package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

type contextKey struct{}

func WithContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}

func Valid(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// MustNew mints a fresh request id and never returns "". The name
// encodes the contract callers rely on: a 128-bit random hex value,
// or — if the OS RNG fails — a "ts-<nanos>" fallback so structured
// log fields always have something to correlate on.
func MustNew() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	n := time.Now().UnixNano()
	if n == 0 {
		n = 1
	}
	return fmt.Sprintf("ts-%016x", n)
}
