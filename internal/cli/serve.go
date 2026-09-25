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
	"aegisgo/internal/logx"
	"aegisgo/internal/loop"
	"aegisgo/internal/server"
	"aegisgo/internal/store"
	"aegisgo/internal/task"
	"aegisgo/internal/version"
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

	// JSON on stdout for journald/log shippers; SetDefault routes the
	// package-global slog lines (audit "request", engine lifecycle) into
	// the same stream instead of the stderr-text default. AEGIS_LOG_LEVEL
	// finally means something here.
	var logSink io.Writer = os.Stdout
	if cfg.LogFile != "" {
		// Size-rotated file logging: a daemon writing JSON lines forever
		// would otherwise grow one unbounded file (launchd has no logrotate).
		rw := &logx.RotatingWriter{Path: cfg.LogFile, MaxBytes: 10 << 20, KeepFiles: 5}
		defer rw.Close()
		logSink = rw
	}
	logger := logx.New(cfg.LogLevel, logSink, true)
	slog.SetDefault(logger)

	a, cleanup, err := app.Build(ctx, cfg, tier, store.IFaceREST, logger)
	if err != nil {
		return err
	}
	defer cleanup()

	// Sweep expired async answers every 5 minutes: lazy delete-on-read only
	// fires on traffic, so abandoned traces would otherwise sit forever.
	defer loop.Periodic(ctx, 5*time.Minute, time.Minute, func(sctx context.Context) {
		if err := a.Store.DeleteExpiredAnswers(sctx); err != nil {
			logger.Warn("sweeping expired answers", "error", err)
		}
	})()

	// One task group for every detached run (REST + gRPC async answers) so
	// shutdown can join them for real instead of guessing with a sleep.
	tasks := &task.Group{}
	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: server.Handler(server.Deps{
			Engine: a.Engine, Answers: a.Store, Readiness: a.Store, Stats: a.Store,
			Webhook:   a.Webhook, // nil unless Telegram runs in webhook mode
			Approvals: a.Store,   // HITL REST (ADR-0008)
			Tasks:     tasks,
			Logger:    logger,
			AuthToken: cfg.HTTPToken, // AEGIS_HTTP_TOKEN: empty = LAN-open, set = Bearer on /v1/*
			Jobs:      a.Jobs,        // GET /v1/jobs: live download-job snapshots
			Dashboard: &server.DashboardDeps{ // mini status page at GET /
				Stats: a.Store, StartedAt: time.Now(), Commit: version.Commit,
			},
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
		grpcapi.Register(grpcSrv, grpcapi.New(a.Engine, a.Store, a.Store, tasks, logger))
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
		// Join in-flight async runs (bounded): their answers must persist
		// before the store closes (cleanup drains queued audit writes). A
		// missed join would 404 answers that were already acked with 202.
		if !tasks.Wait(10 * time.Second) {
			logger.Warn("shutdown: async runs still in flight after 10s; closing anyway")
		}
		return nil
	}
}
