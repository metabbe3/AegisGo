// Package cli implements the aegis binary's subcommand surface: agent
// (one-shot prompt or interactive REPL), serve (HTTP + gRPC daemon), and
// ctl (offline admin against the store). All logic lives here — cmd/aegis
// is a thin shim — and every entry point takes injected writers and a
// context so tests drive exactly the code the binary runs.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

// Version is stamped at release time via
// -ldflags "-X aegisgo/internal/cli.Version=vX.Y.Z"; "dev" for source builds.
var Version = "dev"

// usageText is printed for `aegis help` (stdout) and for usage errors
// (stderr). Keep the commands list in sync with Main's switch.
const usageText = `usage: aegis <command> [args]

commands:
  agent [--tier fast|smart] [prompt...]   one-shot prompt, or interactive REPL
  serve [--tier fast|smart]               HTTP + gRPC daemon (AEGIS_* env config)
  ctl <rules|stats|replay> ...            offline admin tool (AEGIS_DB_PATH)
  version                                 print the build version
`

// Main dispatches args (os.Args[1:]) to one subcommand and returns the
// process exit code: 0 on success (including -h help), 1 on any error or
// usage failure. Subcommands parse their own flags (flag.NewFlagSet with
// ContinueOnError) so nothing touches the global flag.CommandLine.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 1
	}
	var err error
	switch args[0] {
	case "agent":
		err = agentCmd(ctx, args[1:], stdout, stderr)
	case "serve":
		err = serveCmd(ctx, args[1:], stderr)
	case "ctl":
		err = runCtl(args[1:], stdout)
	case "version":
		fmt.Fprintf(stdout, "aegis %s\n", Version)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
	default:
		fmt.Fprintf(stderr, "aegis: unknown command %q\n\n%s", args[0], usageText)
		return 1
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) { // -h on a subcommand's FlagSet
			return 0
		}
		fmt.Fprintf(stderr, "aegis: %v\n", err)
		return 1
	}
	return 0
}
