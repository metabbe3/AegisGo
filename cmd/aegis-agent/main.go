// Command aegis-agent runs the AegisGo hybrid agent from the command line:
// one-shot with a prompt argument, or interactive (no argument). The
// deterministic router answers matching commands instantly with zero LLM
// cost; everything else falls back to the configured provider. All
// configuration is environment-driven (see internal/config).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"aegisgo/internal/app"
	"aegisgo/internal/config"
	"aegisgo/internal/engine"
	"aegisgo/internal/store"
	"aegisgo/internal/trace"
)

func main() {
	tierFlag := flag.String("tier", "smart", "model tier: fast or smart (see AEGIS_MODEL_FAST / AEGIS_MODEL_SMART)")
	flag.Parse()

	if err := run(context.Background(), *tierFlag, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "aegis-agent:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, tierFlag string, args []string) error {
	tier, err := config.ParseTier(tierFlag)
	if err != nil {
		return err
	}
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	a, cleanup, err := app.Build(ctx, cfg, tier, store.IFaceCLI,
		slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		return err
	}
	defer cleanup()

	if len(args) > 0 {
		_, rctx := trace.New(ctx, "")
		res := a.Engine.Run(rctx, strings.Join(args, " "))
		fmt.Println(res.Answer)
		return nil
	}
	return repl(ctx, a.Engine)
}

// repl reads prompts line by line until blank line, "exit", "quit", or EOF.
// Router hits print with their rule id and latency; LLM answers show their
// decision source so cost behavior is visible while working.
func repl(ctx context.Context, eng *engine.Engine) error {
	fmt.Println("aegis-agent interactive mode — empty line or Ctrl-D to exit")
	fmt.Println("router commands: /uptime /disk /memory /hostname /kernel /who /csv_summary <path> /csv_head <path> [n]")
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for {
		fmt.Print("\n> ")
		if !sc.Scan() {
			fmt.Println()
			return nil
		}
		prompt := strings.TrimSpace(sc.Text())
		if prompt == "" || prompt == "exit" || prompt == "quit" {
			return nil
		}
		_, rctx := trace.New(ctx, "")
		res := eng.Run(rctx, prompt)
		if res.RuleID != "" {
			fmt.Printf("[%s via %s, %dms]\n", res.DecisionSource, res.RuleID, res.LatencyMS)
		} else {
			fmt.Printf("[%s, %dms]\n", res.DecisionSource, res.LatencyMS)
		}
		fmt.Println(res.Answer)
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
	}
}
