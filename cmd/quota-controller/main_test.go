package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/config"
)

func TestRunConfigError(t *testing.T) {
	t.Setenv("QUOTA_ROUTER_SERVER__READ_TIMEOUT", "bad")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	code := run(context.Background(), log, new(slog.LevelVar))
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
}

func TestRunAppliesConfiguredLogLevel(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", addr)
	t.Setenv("QUOTA_ROUTER_OBSERVABILITY__LEVEL", "error")

	// Wire the handler to the LevelVar exactly as main does, so a debug record
	// is dropped before the configuration applies and emitted after it.
	level := new(slog.LevelVar)
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
	log.Debug("before")
	if buf.Len() != 0 {
		t.Fatal("debug record was emitted before the configured level applied")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, log, level)
	}()

	// The level is set right after config.Load, before the listener starts.
	waitForTCP(t, addr, 2*time.Second)
	if level.Level() != slog.LevelError {
		t.Fatalf("level = %s, want error from the configuration", level.Level())
	}
	log.Debug("still dropped")
	if strings.Contains(buf.String(), "still dropped") {
		t.Fatal("the handler ignored the configured level")
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
}

func TestRunListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", ln.Addr().String())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	code := run(context.Background(), log, new(slog.LevelVar))
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
}

func TestRunGracefulShutdownOnCancel(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", addr)
	t.Setenv("QUOTA_ROUTER_SERVER__SHUTDOWN_TIMEOUT", "2s")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, log, new(slog.LevelVar))
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
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", addr)
	t.Setenv("QUOTA_ROUTER_SERVER__SHUTDOWN_TIMEOUT", "50ms")

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

// runWithHandler serves with a test handler instead of the /health-only one.
func runWithHandler(ctx context.Context, log *slog.Logger, handler http.Handler) int {
	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "error", err)
		return 1
	}
	return serve(ctx, log, cfg, handler)
}

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
