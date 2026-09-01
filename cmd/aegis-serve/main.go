// Command aegis-serve runs the AegisGo agent as a long-lived HTTP service
// for other microservices to call (see internal/server). Designed to run as
// a systemd unit on a Linux server — single binary, no runtime deps.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/microsoft/agent-framework-go/tool"

	"aegisgo/internal/config"
	"aegisgo/internal/mcpclient"
	"aegisgo/internal/provider"
	"aegisgo/internal/server"
	"aegisgo/internal/tools"
)

func main() {
	tierFlag := flag.String("tier", "smart", "model tier: fast or smart (see AEGIS_MODEL_FAST / AEGIS_MODEL_SMART)")
	flag.Parse()

	if err := serve(context.Background(), *tierFlag); err != nil {
		fmt.Fprintln(os.Stderr, "aegis-serve:", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, tierFlag string) error {
	tier, err := config.ParseTier(tierFlag)
	if err != nil {
		return err
	}
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		return err
	}

	// Graceful shutdown on SIGINT/SIGTERM (systemd stop).
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

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

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.Handler(a, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr, "provider", cfg.Provider, "model", cfg.ModelFor(tier), "tier", tier)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
