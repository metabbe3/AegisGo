// Command aegis is the single AegisGo binary: `aegis agent` (one-shot
// prompt or interactive REPL), `aegis serve` (HTTP + gRPC daemon), and
// `aegis ctl` (offline admin). All logic lives in internal/cli; this file
// only wires the process to it.
package main

import (
	"context"
	"os"

	"aegisgo/internal/cli"
)

func main() {
	os.Exit(cli.Main(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
