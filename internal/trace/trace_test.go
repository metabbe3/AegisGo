package trace

import (
	"context"
	"strings"
	"testing"
)

// The "trace-unknown" fallback inside mint fires only when crypto/rand.Read
// fails, which these tests cannot provoke without fault injection (the
// package reads the global rand directly). The branch is deliberately left
// uncovered rather than faked; every other path in the package is exercised
// below.

// isMintedID reports whether id has the shape mint() produces: exactly 24
// lowercase hex characters (12 random bytes, hex-encoded).
func isMintedID(id string) bool {
	if len(id) != 24 {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func TestNewMintsID(t *testing.T) {
	id, ctx := New(context.Background(), "")
	if !isMintedID(id) {
		t.Fatalf(`New(ctx, "") = %q, want a fresh 24-char lowercase-hex ID`, id)
	}
	if got := From(ctx); got != id {
		t.Errorf("From(ctx) = %q, want round-trip of %q", got, id)
	}
}

func TestNewAcceptsSuppliedID(t *testing.T) {
	// X-Trace-Id shape: caller-chosen, so not necessarily hex.
	const supplied = "t-42"
	id, ctx := New(context.Background(), supplied)
	if id != supplied {
		t.Errorf("New(ctx, %q) = %q, want supplied ID used verbatim", supplied, id)
	}
	if got := From(ctx); got != supplied {
		t.Errorf("From(ctx) = %q, want %q propagated into the context", got, supplied)
	}
}

func TestNewReplacesOversizedID(t *testing.T) {
	cases := []struct {
		name     string
		supplied string
		wantKept bool
	}{
		{"64 chars is the largest length kept", strings.Repeat("a", 64), true},
		{"65 chars is replaced by a mint", strings.Repeat("a", 65), false},
		{"far oversized is replaced by a mint", strings.Repeat("x", 200), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ctx := New(context.Background(), tc.supplied)
			if tc.wantKept && id != tc.supplied {
				t.Errorf("New(ctx, 64-char ID) = %q (len %d), want it kept verbatim", id, len(id))
			}
			if !tc.wantKept && !isMintedID(id) {
				t.Errorf("New(ctx, %d-char ID) = %q, want a fresh 24-char lowercase-hex mint", len(tc.supplied), id)
			}
			// Whichever ID wins must still be the one carried in the context.
			if got := From(ctx); got != id {
				t.Errorf("From(ctx) = %q, want round-trip of %q", got, id)
			}
		})
	}
}

func TestFromMissing(t *testing.T) {
	if got := From(context.Background()); got != "" {
		t.Errorf("From(plain context) = %q, want empty string", got)
	}
}

func TestNewUniqueness(t *testing.T) {
	a, _ := New(context.Background(), "")
	b, _ := New(context.Background(), "")
	if a == b {
		t.Errorf("two consecutive New calls minted the same ID %q", a)
	}
}
