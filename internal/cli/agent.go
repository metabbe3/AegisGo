package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"aegisgo/internal/app"
	"aegisgo/internal/config"
	"aegisgo/internal/engine"
	"aegisgo/internal/logx"
	"aegisgo/internal/store"
	"aegisgo/internal/trace"
)

// agentCmd parses agent flags (--tier) and delegates to runAgent. The
// prompt is everything after the flags, joined with spaces — flags after
// the first prompt word become prompt text, matching Go's flag semantics.
func agentCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("aegis agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tierFlag := fs.String("tier", "smart", "model tier: fast or smart (see AEGIS_MODEL_FAST / AEGIS_MODEL_SMART)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runAgent(ctx, *tierFlag, fs.Args(), stdout)
}

// runAgent boots the app and answers one prompt or drops into the REPL.
// stdout is injected so tests can capture the user-facing stream; logs stay
// on stderr.
func runAgent(ctx context.Context, tierFlag string, args []string, stdout io.Writer) error {
	tier, err := config.ParseTier(tierFlag)
	if err != nil {
		return err
	}
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	// Text on stderr keeps stdout clean for answers; SetDefault pulls the
	// package-global slog lines (audit "request") onto the same handler.
	logger := logx.New(cfg.LogLevel, os.Stderr, false)
	slog.SetDefault(logger)
	a, cleanup, err := app.Build(ctx, cfg, tier, store.IFaceCLI, logger)
	if err != nil {
		return err
	}
	defer cleanup()

	if len(args) > 0 {
		_, rctx := trace.New(ctx, "")
		res := a.Engine.Run(rctx, strings.Join(args, " "))
		fmt.Fprintln(stdout, res.Answer)
		return nil
	}
	return repl(ctx, a.Engine, os.Stdin, stdout)
}

// repl reads prompts line by line until blank line, "exit", "quit", or EOF.
// Router hits print with their rule id and latency; LLM answers show their
// decision source so cost behavior is visible while working. in/out are
// injected so tests can drive the loop without the process terminal.
func repl(ctx context.Context, eng *engine.Engine, in io.Reader, out io.Writer) error {
	fmt.Fprintln(out, "aegis agent interactive mode — empty line or Ctrl-D to exit")
	fmt.Fprintln(out, "router commands: /uptime /disk /memory /hostname /kernel /who /csv_summary <path> /csv_head <path> [n] /mkdir <path> /ls [path] /download <url> <path> /job <id>")
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for {
		fmt.Fprint(out, "\n> ")
		if !sc.Scan() {
			fmt.Fprintln(out)
			return nil
		}
		prompt := strings.TrimSpace(sc.Text())
		if prompt == "" || prompt == "exit" || prompt == "quit" {
			return nil
		}
		_, rctx := trace.New(ctx, "")
		res := eng.Run(rctx, prompt)
		fmt.Fprintln(out, res.Header(", "))
		fmt.Fprintln(out, res.Answer)
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
	}
}
