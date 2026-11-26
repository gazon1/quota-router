package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/config"
)

func TestRunConfigError(t *testing.T) {
	t.Setenv("QUOTA_ROUTER_READ_TIMEOUT", "bad")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	code := run(context.Background(), log)
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
}

func TestRunListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	t.Setenv("QUOTA_ROUTER_ADDR", ln.Addr().String())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	code := run(context.Background(), log)
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
}

func TestRunGracefulShutdownOnCancel(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("QUOTA_ROUTER_ADDR", addr)
	t.Setenv("QUOTA_ROUTER_SHUTDOWN_TIMEOUT", "2s")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, log)
	}()

	waitForHTTP(t, "http://"+addr+"/health", 2*time.Second)
	cancel()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run() = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not exit after cancellation")
	}
}

func TestRunShutdownTimeout(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("QUOTA_ROUTER_ADDR", addr)
	t.Setenv("QUOTA_ROUTER_SHUTDOWN_TIMEOUT", "50ms")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	blocked := make(chan struct{}, 1)
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block" {
			select {
			case blocked <- struct{}{}:
			default:
			}
			<-release
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	done := make(chan int, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		done <- runWithHandler(ctx, log, handler)
	}()

	waitForTCP(t, addr, 2*time.Second)

	client := &http.Client{Timeout: 2 * time.Second}
	respCh := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + addr + "/block")
		if err == nil {
			resp.Body.Close()
		}
		respCh <- err
	}()

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not receive /block request")
	}

	cancel()

	select {
	case code := <-done:
		close(release)
		if code != 1 {
			t.Fatalf("run() = %d, want 1 on shutdown timeout", code)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("run() did not exit after shutdown timeout")
	}

	select {
	case <-respCh:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked request did not complete")
	}
}

func runWithHandler(ctx context.Context, log *slog.Logger, handler http.Handler) int {
	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "error", err)
		return 1
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			log.Error("http server failed", "error", err)
			return 1
		}
		return 0
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
		return 1
	}
	return 0
}

var _ = config.Load // keep config import used in build tag variations.

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

func waitForHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become ready within %s", url, timeout)
}

func waitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s did not accept connections within %s", addr, timeout)
}
