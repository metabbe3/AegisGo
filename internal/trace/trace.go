// Package trace mints and propagates request trace IDs. Every interface
// (REST now; gRPC and Telegram in later phases) calls trace.New on entry;
// every audit row, slog line, and async answer is joined by the same ID.
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type key struct{}

// New returns a trace ID and a context carrying it, accepting a caller
// supplied ID (e.g. X-Trace-Id) when present.
func New(ctx context.Context, supplied string) (string, context.Context) {
	id := supplied
	if id == "" || len(id) > 64 {
		id = mint()
	}
	return id, context.WithValue(ctx, key{}, id)
}

// From extracts the trace ID, if any.
func From(ctx context.Context) string {
	if v, ok := ctx.Value(key{}).(string); ok {
		return v
	}
	return ""
}

func mint() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "trace-unknown"
	}
	return hex.EncodeToString(b[:])
}
