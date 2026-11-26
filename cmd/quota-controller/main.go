package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/server"
)

func main() {
	level := new(slog.LevelVar)
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	os.Exit(run(context.Background(), log, level))
}

func run(ctx context.Context, log *slog.Logger, level *slog.LevelVar) int {
	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "error", err)
		return 1
	}
	level.Set(cfg.Observability.SlogLevel())

	return serve(ctx, log, cfg, server.New(log).Handler())
}

// serve runs the HTTP server until ctx is cancelled or the listener fails.
func serve(ctx context.Context, log *slog.Logger, cfg config.Config, handler http.Handler) int {
	httpServer := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server starting", "addr", cfg.Server.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			log.Error("http server failed", "error", err)
			return 1
		}
		return 0
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
		return 1
	}

	log.Info("shutdown complete")
	return 0
}
