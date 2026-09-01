// Command aegis-agent runs the AegisGo agent from the command line: one-shot
// with a prompt argument, or interactive (no argument). Configuration is
// entirely environment-driven (see internal/config).
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

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/tool"

	"aegisgo/internal/config"
	"aegisgo/internal/mcpclient"
	"aegisgo/internal/provider"
	"aegisgo/internal/tools"
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
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	builtin, err := tools.Builtin(cfg.WorkspaceDir())
	if err != nil {
		return err
	}
	mcpTools, release, err := mcpclient.Connect(ctx, cfg.MCPServers)
	if err != nil {
		return err
	}
	defer release()

	a, err := provider.New(cfg, tier, append(append([]tool.Tool{}, builtin...), mcpTools...), logger)
	if err != nil {
		return err
	}
	logger.Info("agent ready",
		"provider", cfg.Provider,
		"model", cfg.ModelFor(tier),
		"tier", tier,
		"workspace", cfg.WorkspaceDir(),
		"mcp_tools", len(mcpTools),
		"builtin_tools", len(builtin),
	)

	if len(args) > 0 {
		return oneShot(ctx, a, strings.Join(args, " "))
	}
	return repl(ctx, a)
}

func oneShot(ctx context.Context, a *agent.Agent, prompt string) error {
	resp, err := a.RunText(ctx, prompt).Collect()
	if err != nil {
		return err
	}
	fmt.Println(resp.String())
	return nil
}

// repl reads prompts line by line until blank line, "exit", "quit", or EOF.
// Each line is an independent run (no history carries over yet).
func repl(ctx context.Context, a *agent.Agent) error {
	fmt.Println("aegis-agent interactive mode — empty line or Ctrl-D to exit")
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
		resp, err := a.RunText(ctx, prompt).Collect()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}
		fmt.Println(resp.String())
	}
}
