package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{
		"QUOTA_ROUTER_ADDR",
		"QUOTA_ROUTER_SHUTDOWN_TIMEOUT",
		"QUOTA_ROUTER_READ_TIMEOUT",
		"QUOTA_ROUTER_WRITE_TIMEOUT",
		"QUOTA_ROUTER_IDLE_TIMEOUT",
	} {
		unsetEnv(t, k)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Fatalf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("ShutdownTimeout = %s", cfg.ShutdownTimeout)
	}
	if cfg.ReadTimeout != 5*time.Second {
		t.Fatalf("ReadTimeout = %s", cfg.ReadTimeout)
	}
	if cfg.WriteTimeout != 10*time.Second {
		t.Fatalf("WriteTimeout = %s", cfg.WriteTimeout)
	}
	if cfg.IdleTimeout != 60*time.Second {
		t.Fatalf("IdleTimeout = %s", cfg.IdleTimeout)
	}
}

func TestLoadInvalidDuration(t *testing.T) {
	t.Setenv("QUOTA_ROUTER_READ_TIMEOUT", "nope")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "QUOTA_ROUTER_READ_TIMEOUT") || !strings.Contains(msg, "not a valid duration") {
		t.Fatalf("error %q missing actionable detail", msg)
	}
}

func TestLoadRejectsNonPositiveDuration(t *testing.T) {
	t.Setenv("QUOTA_ROUTER_SHUTDOWN_TIMEOUT", "-1s")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "QUOTA_ROUTER_SHUTDOWN_TIMEOUT") {
		t.Fatalf("error %q missing key", err)
	}
}

func TestLoadRejectsEmptyAddr(t *testing.T) {
	t.Setenv("QUOTA_ROUTER_ADDR", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "QUOTA_ROUTER_ADDR") {
		t.Fatalf("error %q missing key", err)
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	orig, ok := os.LookupEnv(key)
	if ok {
		t.Cleanup(func() { _ = os.Setenv(key, orig) })
	} else {
		t.Cleanup(func() { _ = os.Unsetenv(key) })
	}
	_ = os.Unsetenv(key)
}
