package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"aegisgo/internal/app"
	"aegisgo/internal/config"
	"aegisgo/internal/grpcapi"
	"aegisgo/internal/server"
	"aegisgo/internal/store"
)

// serveCmd parses serve flags (--tier) and delegates to serve.
func serveCmd(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("aegis serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tierFlag := fs.String("tier", "smart", "model tier: fast or smart (see AEGIS_MODEL_FAST / AEGIS_MODEL_SMART)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return serve(ctx, *tierFlag)
}

// serve runs the hybrid agent as a long-lived HTTP service for other
// microservices to call (see internal/server): sync and async runs,
// health/readiness probes, trace IDs on every request. Designed to run as
// a systemd unit on a Linux server — single binary, no runtime deps,
// router-first so it stays useful even when the provider is down
// (AEGIS_LLM=off).
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
			Engine: a.Engine, Answers: a.Store, Readines: a.Store, Stats: a.Store,
			Webhook: a.Webhook, // nil unless Telegram runs in webhook mode
			Logger:  logger,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("listening", "addr", cfg.Addr, "proto", "http")
		errCh <- srv.ListenAndServe()
	}()

	// gRPC interface (same engine) for cross-language callers; disable
	// with AEGIS_GRPC_ADDR=none.
	var grpcSrv *grpc.Server
	if cfg.GRPCEnabled() {
		lis, err := net.Listen("tcp", cfg.GRPCAddr)
		if err != nil {
			return err
		}
		grpcSrv = grpc.NewServer()
		grpcapi.Register(grpcSrv, grpcapi.New(a.Engine, a.Store, a.Store, logger))
		go func() {
			logger.Info("listening", "addr", cfg.GRPCAddr, "proto", "grpc")
			errCh <- grpcSrv.Serve(lis)
		}()
	} else {
		logger.Info("grpc disabled — set AEGIS_GRPC_ADDR (e.g. :8081) to enable")
	}

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, grpc.ErrServerStopped) {
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
		if grpcSrv != nil {
			grpcSrv.GracefulStop()
		}
		// Give in-flight async runs a beat to persist their answers, then
		// close the store (cleanup drains queued audit writes).
		time.Sleep(500 * time.Millisecond)
		return nil
	}
}
