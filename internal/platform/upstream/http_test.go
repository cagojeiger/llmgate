package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"llmgate/internal/llmtypes"
)

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{"0", 0},
		{"-1", 0},
		{"-9999", 0},
		{"abc", 0},
		{"Wed, 21 Oct 1970 07:28:00 GMT", 0}, // past date → clamp
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			if got := ParseRetryAfter(tc.header); got != tc.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestParseRetryAfter_LargeValueCapsAt24h(t *testing.T) {
	// Without the cap, time.Duration(99999999999) * time.Second would
	// overflow int64 nanoseconds and silently produce a negative duration.
	got := ParseRetryAfter("99999999999")
	if got != maxRetryAfter {
		t.Errorf("ParseRetryAfter(huge) = %v, want %v (capped)", got, maxRetryAfter)
	}
}

func TestParseRetryAfter_OverflowingPositiveCaps(t *testing.T) {
	// 999...9 vastly exceeds int64; ParseInt returns ErrRange. The cap
	// path should still kick in instead of falling through to date
	// parsing (which would yield 0 = no backoff).
	got := ParseRetryAfter("999999999999999999999999")
	if got != maxRetryAfter {
		t.Errorf("ParseRetryAfter(overflow positive) = %v, want %v (capped)", got, maxRetryAfter)
	}
}

func TestParseRetryAfter_OverflowingNegativeIsZero(t *testing.T) {
	got := ParseRetryAfter("-999999999999999999999999")
	if got != 0 {
		t.Errorf("ParseRetryAfter(overflow negative) = %v, want 0", got)
	}
}

func TestParseRetryAfter_HTTPDate_FarFutureCaps(t *testing.T) {
	// HTTP-date 100 years out should also clamp, not return ~100y.
	far := time.Now().Add(100 * 365 * 24 * time.Hour).UTC().Format(http.TimeFormat)
	got := ParseRetryAfter(far)
	if got != maxRetryAfter {
		t.Errorf("ParseRetryAfter(100y future) = %v, want %v (capped)", got, maxRetryAfter)
	}
}

func TestParseRetryAfter_HTTPDate_FuturePositive(t *testing.T) {
	// HTTP-date in the future should yield a positive, bounded duration.
	// Must use http.TimeFormat (RFC 7231 IMF-fixdate with literal "GMT");
	// time.RFC1123 emits "UTC" which http.ParseTime rejects.
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	got := ParseRetryAfter(future)
	if got <= 0 || got > 5*time.Second {
		t.Errorf("ParseRetryAfter(%q) = %v, want roughly 2s", future, got)
	}
}

func TestFirstBytes_TruncatesAndCopies(t *testing.T) {
	src := bytes.Repeat([]byte("x"), 512)
	out := FirstBytes(src)
	if len(out) != 256 {
		t.Errorf("len(out) = %d, want 256 (truncated)", len(out))
	}
	// Mutating src must not mutate out — proves copy, not aliasing.
	src[0] = 'y'
	if out[0] != 'x' {
		t.Errorf("FirstBytes returned aliased slice (src mutation leaked)")
	}
}

func TestFirstBytes_PreservesShortInput(t *testing.T) {
	src := []byte("hello")
	out := FirstBytes(src)
	if string(out) != "hello" {
		t.Errorf("out = %q, want hello", out)
	}
	src[0] = 'Y'
	if out[0] != 'h' {
		t.Errorf("FirstBytes did not copy short input")
	}
}

func TestFirstBytes_NilSafe(t *testing.T) {
	out := FirstBytes(nil)
	if out == nil || len(out) != 0 {
		t.Errorf("FirstBytes(nil) = %v, want empty non-nil slice", out)
	}
}

func TestCopyHeaders_DeepCopy(t *testing.T) {
	in := map[string]string{"X-A": "1", "X-B": "2"}
	out := CopyHeaders(in)
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d", len(out), len(in))
	}
	out["X-A"] = "mutated"
	if in["X-A"] != "1" {
		t.Errorf("CopyHeaders did not deep-copy: in[X-A] = %q", in["X-A"])
	}
}

func TestCopyHeaders_EmptyReturnsNil(t *testing.T) {
	if got := CopyHeaders(nil); got != nil {
		t.Errorf("CopyHeaders(nil) = %v, want nil", got)
	}
	if got := CopyHeaders(map[string]string{}); got != nil {
		t.Errorf("CopyHeaders({}) = %v, want nil", got)
	}
}

func TestDefaultClient_TuningPreservedAcrossCalls(t *testing.T) {
	c := DefaultClient()
	if c == nil {
		t.Fatal("DefaultClient() = nil")
	}
	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (cancellation via context)", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("Transport = %T, want non-nil *http.Transport", c.Transport)
	}
	if tr.MaxIdleConns != 100 {
		t.Errorf("MaxIdleConns = %d, want 100", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != 50 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 50", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", tr.IdleConnTimeout)
	}
	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 10s", tr.TLSHandshakeTimeout)
	}
	if tr.ExpectContinueTimeout != time.Second {
		t.Errorf("ExpectContinueTimeout = %v, want 1s", tr.ExpectContinueTimeout)
	}
}

func TestLowLevelError_DeadlineExceededBecomesTimeout(t *testing.T) {
	perr := LowLevelError("opencode", "send request", context.DeadlineExceeded)
	if perr.Kind != llmtypes.KindTimeout {
		t.Errorf("Kind = %q, want timeout", perr.Kind)
	}
	if perr.Provider != "opencode" {
		t.Errorf("Provider = %q, want opencode", perr.Provider)
	}
	if !errors.Is(perr, context.DeadlineExceeded) {
		t.Errorf("perr does not wrap context.DeadlineExceeded — Cause chain broken")
	}
}

// timeoutNetErr satisfies net.Error with Timeout()=true to exercise the
// non-DeadlineExceeded timeout-detection branch.
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

var _ net.Error = timeoutNetErr{}

func TestLowLevelError_NetTimeoutBecomesTimeout(t *testing.T) {
	perr := LowLevelError("opencode", "dial", timeoutNetErr{})
	if perr.Kind != llmtypes.KindTimeout {
		t.Errorf("Kind = %q, want timeout (from net.Error.Timeout())", perr.Kind)
	}
}

func TestLowLevelError_GenericFailureIsNetwork(t *testing.T) {
	perr := LowLevelError("opencode", "read response", errors.New("connection reset"))
	if perr.Kind != llmtypes.KindNetwork {
		t.Errorf("Kind = %q, want network", perr.Kind)
	}
}

func TestBadRequest_AssemblesMessageAndRaw(t *testing.T) {
	cause := errors.New("invalid json")
	raw := []byte(`{"bad":`)
	perr := BadRequest("opencode", "decode body", cause, raw)
	if perr.Kind != llmtypes.KindBadRequest {
		t.Errorf("Kind = %q, want bad_request", perr.Kind)
	}
	if perr.Provider != "opencode" {
		t.Errorf("Provider = %q, want opencode", perr.Provider)
	}
	wantMsg := fmt.Sprintf("decode body: %s", cause.Error())
	if perr.Message != wantMsg {
		t.Errorf("Message = %q, want %q", perr.Message, wantMsg)
	}
	if string(perr.Raw) != string(raw) {
		t.Errorf("Raw = %q, want %q", perr.Raw, raw)
	}
}

func TestBadRequest_TrimsLongRawTo256(t *testing.T) {
	huge := bytes.Repeat([]byte("z"), 1024)
	perr := BadRequest("opencode", "marshal", errors.New("too big"), huge)
	if len(perr.Raw) != 256 {
		t.Errorf("len(Raw) = %d, want 256 (FirstBytes trim)", len(perr.Raw))
	}
	huge[0] = 'Y'
	if perr.Raw[0] == 'Y' {
		t.Errorf("Raw aliased the input slice (caller mutation leaked)")
	}
}
