package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/server"
	"github.com/gazon1/quota-router/internal/store"
)

func main() {
	level := new(slog.LevelVar)
	os.Exit(run(context.Background(), newLogger(os.Stdout, level), level))
}

// newLogger builds the production logger: JSON, filtered by level. Tests use it
// too, so a change to this wiring is covered rather than duplicated.
func newLogger(w io.Writer, level *slog.LevelVar) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

func run(ctx context.Context, log *slog.Logger, level *slog.LevelVar) int {
	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "error", err)
		return 1
	}
	level.Set(cfg.Observability.SlogLevel())

	st, err := store.Open(store.Config{Path: cfg.SQLite.Path, BusyTimeout: cfg.SQLite.BusyTimeout}, log)
	if err != nil {
		log.Error("storage unavailable", "error", err)
		return 1
	}
	defer st.Close()

	// Migrations and cleanup finish before the listener starts: a database this
	// build cannot migrate must fail startup, not serve with a stale schema.
	if err := st.Migrate(ctx); err != nil {
		log.Error("database migration failed", "error", err)
		return 1
	}
	if err := prune(ctx, log, st, cfg.SQLite.Retention); err != nil {
		log.Error("retention cleanup failed", "error", err)
		return 1
	}

	return serve(ctx, log, cfg, server.New(log).Handler())
}

// prune applies the configured retention bounds. A failure is fatal at startup:
// a full disk is exactly what cleanup exists to avoid, and continuing would hit
// the write path with a database that cannot grow.
func prune(ctx context.Context, log *slog.Logger, st *store.Store, r config.Retention) error {
	deleted, err := st.Prune(ctx, store.Retention{
		QuotaSnapshots: r.QuotaSnapshots,
		ProviderHealth: r.ProviderHealth,
		UsageRecords:   r.UsageRecords,
	}, time.Now())
	if err != nil {
		return err
	}
	for table, n := range deleted {
		if n > 0 {
			log.Info("retention removed rows", "table", table, "rows", n)
		}
	}
	return nil
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
