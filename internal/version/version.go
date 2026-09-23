// Package version carries the build identity injected via ldflags so any
// package (CLI banner, Telegram /status) can report it without import
// cycles. Same -X target as internal/cli.Version.
package version

// Commit is set at build time: -ldflags "-X aegisgo/internal/version.Commit=$(git rev-parse --short HEAD)".
var Commit = "dev"
