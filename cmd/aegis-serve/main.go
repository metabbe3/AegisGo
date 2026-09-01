// Command aegis-serve runs the AegisGo hybrid agent as a long-lived HTTP
// service for other microservices to call (see internal/server): sync and
// async runs, health/readiness probes, trace IDs on every request.
// Designed to run as a systemd unit on a Linux server — single binary, no
// runtime deps, router-first so it stays useful even when the provider is
// down (AEGIS_LLM=off).
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

	"aegisgo/internal/app"
	"aegisgo/internal/config"
	"aegisgo/internal/server"
	"aegisgo/internal/store"
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

	// Graceful shutdown on SIGINT/SIGTERM (systemd stop).
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	a, cleanup, err := app.Build(ctx, cfg, tier, store.IFaceREST, logger)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: server.Handler(server.Deps{
			Engine: a.Engine, Answers: a.Store, Readines: a.Store,
			Webhook: a.Webhook, // nil unless Telegram runs in webhook mode
			Logger:  logger,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr)
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
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		// Give in-flight async runs a beat to persist their answers, then
		// close the store (cleanup drains queued audit writes).
		time.Sleep(500 * time.Millisecond)
		return nil
	}
}
