package requestid

import (
	"context"
	"strings"
	"testing"
)

func TestContextRoundTrip(t *testing.T) {
	ctx := WithContext(context.Background(), "req-123")

	if got := FromContext(ctx); got != "req-123" {
		t.Fatalf("FromContext() = %q, want req-123", got)
	}
}

func TestFromContext_Missing(t *testing.T) {
	if got := FromContext(context.Background()); got != "" {
		t.Fatalf("FromContext() = %q, want empty", got)
	}
}

func TestValid(t *testing.T) {
	cases := map[string]bool{
		"":                       false,
		"req-123":                true,
		strings.Repeat("a", 128): true,
		strings.Repeat("a", 129): false,
		"line\nbreak":            false,
		"tab\tbreak":             false,
	}
	for id, want := range cases {
		if got := Valid(id); got != want {
			t.Fatalf("Valid(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestMustNew(t *testing.T) {
	if got := MustNew(); !Valid(got) {
		t.Fatalf("MustNew() = %q, want valid request id", got)
	}
}
