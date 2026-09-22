// Package logx is the one place AegisGo loggers are built. Binary edges
// call New (serve: JSON on stdout; agent: text on stderr) and set the
// result as the slog default, so package-global slog calls — the audit
// "request" line and engine lifecycle logs — land in the same stream and
// format as the injected loggers instead of the stderr-text default. Or
// is the nil-guard shared by every constructor taking an optional logger.
package logx

import (
	"io"
	"log/slog"
	"strings"
)

// New builds a logger at level (debug|info|warn|error — anything else
// means info, matching AEGIS_LOG_LEVEL's documented set) writing to w as
// JSON or text.
func New(level string, w io.Writer, json bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	if json {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// Or returns l, or the slog default when l is nil.
func Or(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
