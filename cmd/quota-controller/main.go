package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/poll"
	"github.com/gazon1/quota-router/internal/provider/minimax"
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

// pruneInterval is how often retention runs while the controller serves. This
// bounds what a long-running process can accumulate; a restart also prunes, so
// the interval only matters for uptime longer than one tick.
const pruneInterval = time.Hour

func run(ctx context.Context, log *slog.Logger, level *slog.LevelVar) int {
	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "error", err)
		return 1
	}
	level.Set(cfg.Observability.SlogLevel())
	// One signal-aware context covers the poller and the listener, so a
	// shutdown stops polling and serving together.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A configured alias the proxy does not serve cannot work at request time, so
	// that is fatal. A proxy that is merely not answering is not: the two
	// containers have no dependency edge and either may start first.
	if err := cfg.VerifyCatalogue(ctx, nil); err != nil {
		if errors.Is(err, config.ErrCatalogueUnavailable) {
			log.Warn("model catalogue not verified; routing may hit an unknown alias", "error", err)
		} else {
			log.Error("model catalogue error", "error", err)
			return 1
		}
	}

	st, err := store.Open(store.Config{Path: cfg.SQLite.Path, BusyTimeout: cfg.SQLite.BusyTimeout}, log)
	if err != nil {
		log.Error("storage unavailable", "error", err)
		return 1
	}
	defer st.Close()

	// Migrations finish before the listener starts: a database this build cannot
	// migrate must fail startup, not serve with a stale schema.
	if err := st.Migrate(ctx); err != nil {
		log.Error("database migration failed", "error", err)
		return 1
	}
	// A failed cleanup does not stop the controller: the schema is current and
	// the database readable, so it can route. Only a full database is fatal —
	// writes will fail anyway, and that is what cleanup exists to prevent.
	if err := prune(ctx, log, st, cfg.SQLite.Retention); err != nil {
		if retentionBlocksStartup(err) {
			log.Error("retention cleanup failed on a full database", "error", err)
			return 1
		}
		log.Warn("retention cleanup failed, keeping history until the next tick", "error", err)
	}
	defer startPruner(ctx, log, st, cfg.SQLite.Retention, pruneInterval)()

	targets, err := pollingTargets(cfg)
	if err != nil {
		log.Error("polling configuration error", "error", err)
		return 1
	}
	// The poller is stopped before the store closes: Run waits for the polls in
	// flight, so no write lands on a closed database.
	stopPolling := startPoller(ctx, cfg, log, st, targets)
	defer stopPolling()

	return serve(ctx, log, cfg, server.New(log, cfg, st).Handler())
}

// startPoller runs one independently supervised loop per configured provider
// until ctx is cancelled, and returns a function that waits for it to stop.
func startPoller(ctx context.Context, cfg config.Config, log *slog.Logger, st *store.Store, targets []poll.Target) func() {
	if len(targets) == 0 {
		log.Info("no quota providers configured, polling is idle")
		return func() {}
	}
	p := poll.New(cfg.Polling.Interval, staleAfter(cfg.Polling.Interval), log, st, targets...)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	log.Info("quota polling started", "providers", len(targets), "interval", cfg.Polling.Interval,
		"stale_after", staleAfter(cfg.Polling.Interval))
	return func() { <-done }
}

// staleAfter is the freshness bound of a persisted observation. Three intervals
// give a provider two failed polls of grace before its last good reading stops
// counting as current, which is the retry window this poller actually has.
func staleAfter(interval time.Duration) time.Duration { return 3 * interval }

// pollingTargets builds one adapter per configured provider. A provider this
// build has no adapter for is a configuration error rather than a silently
// unmonitored provider.
func pollingTargets(cfg config.Config) ([]poll.Target, error) {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic startup order and log output

	var targets []poll.Target
	for _, name := range names {
		p := cfg.Providers[name]
		if p.APIKey == "" {
			return nil, fmt.Errorf("providers.%s.api_key is not set", name)
		}
		switch name {
		case minimax.Provider:
			a, err := minimax.New(p, cfg.Routing.LowQuotaThreshold, cfg.Polling.RequestTimeout)
			if err != nil {
				return nil, err
			}
			targets = append(targets, poll.Target{Name: name, Source: a})
		default:
			return nil, fmt.Errorf("providers.%s: no quota adapter for this provider", name)
		}
	}
	return targets, nil
}

// retentionBlocksStartup reports whether a failed cleanup must stop startup.
// Only a full database justifies it: the write path cannot grow, so serving
// would fail on the first observation anyway.
func retentionBlocksStartup(err error) bool { return errors.Is(err, store.ErrFull) }

// startPruner runs cleanup every interval until the returned function is called,
// and waits for a tick in flight so the caller may close the store afterwards.
// A periodic failure is logged, never fatal: a transient lock or I/O error must
// not take down a controller that is otherwise serving.
func startPruner(ctx context.Context, log *slog.Logger, st *store.Store, r config.Retention, interval time.Duration) func() {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := prune(ctx, log, st, r); err != nil {
					log.Warn("retention cleanup failed", "error", err)
				}
			}
		}
	}()
	return func() {
		close(stop)
		wg.Wait()
	}
}

// prune applies the configured retention bounds and logs what it removed.
func prune(ctx context.Context, log *slog.Logger, st *store.Store, r config.Retention) error {
	deleted, err := st.Prune(ctx, store.Retention{
		QuotaSnapshots:   r.QuotaSnapshots,
		ProviderHealth:   r.ProviderHealth,
		UsageRecords:     r.UsageRecords,
		RoutingDecisions: r.RoutingDecisions,
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

// serve runs the HTTP server until ctx is cancelled or the listener fails. The
// caller owns signal handling (run does), so a shutdown cancels polling and
// serving through the same context.
func serve(ctx context.Context, log *slog.Logger, cfg config.Config, handler http.Handler) int {
	httpServer := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
	}

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
