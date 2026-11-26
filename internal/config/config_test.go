package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

var configEnvKeys = []string{
	"QUOTA_ROUTER_ADDR",
	"QUOTA_ROUTER_SHUTDOWN_TIMEOUT",
	"QUOTA_ROUTER_READ_HEADER_TIMEOUT",
	"QUOTA_ROUTER_READ_TIMEOUT",
	"QUOTA_ROUTER_WRITE_TIMEOUT",
	"QUOTA_ROUTER_IDLE_TIMEOUT",
}

func TestLoadDefaults(t *testing.T) {
	unsetConfigEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Fatalf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want 10s", cfg.ShutdownTimeout)
	}
	if cfg.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s, want 5s", cfg.ReadHeaderTimeout)
	}
	if cfg.ReadTimeout != 5*time.Second {
		t.Fatalf("ReadTimeout = %s, want 5s", cfg.ReadTimeout)
	}
	if cfg.WriteTimeout != 10*time.Second {
		t.Fatalf("WriteTimeout = %s, want 10s", cfg.WriteTimeout)
	}
	if cfg.IdleTimeout != 60*time.Second {
		t.Fatalf("IdleTimeout = %s, want 60s", cfg.IdleTimeout)
	}
}

func TestLoadInvalidDuration(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "shutdown", key: "QUOTA_ROUTER_SHUTDOWN_TIMEOUT"},
		{name: "read header", key: "QUOTA_ROUTER_READ_HEADER_TIMEOUT"},
		{name: "read", key: "QUOTA_ROUTER_READ_TIMEOUT"},
		{name: "write", key: "QUOTA_ROUTER_WRITE_TIMEOUT"},
		{name: "idle", key: "QUOTA_ROUTER_IDLE_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetConfigEnv(t)
			t.Setenv(tt.key, "nope")

			_, err := Load()
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.key) || !strings.Contains(msg, "not a valid duration") {
				t.Fatalf("error %q missing actionable detail for %s", msg, tt.key)
			}
		})
	}
}

func TestLoadRejectsNonPositiveDuration(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "shutdown zero", key: "QUOTA_ROUTER_SHUTDOWN_TIMEOUT", value: "0s"},
		{name: "shutdown negative", key: "QUOTA_ROUTER_SHUTDOWN_TIMEOUT", value: "-1s"},
		{name: "read header zero", key: "QUOTA_ROUTER_READ_HEADER_TIMEOUT", value: "0s"},
		{name: "read header negative", key: "QUOTA_ROUTER_READ_HEADER_TIMEOUT", value: "-1s"},
		{name: "read zero", key: "QUOTA_ROUTER_READ_TIMEOUT", value: "0s"},
		{name: "read negative", key: "QUOTA_ROUTER_READ_TIMEOUT", value: "-1s"},
		{name: "write zero", key: "QUOTA_ROUTER_WRITE_TIMEOUT", value: "0s"},
		{name: "write negative", key: "QUOTA_ROUTER_WRITE_TIMEOUT", value: "-1s"},
		{name: "idle zero", key: "QUOTA_ROUTER_IDLE_TIMEOUT", value: "0s"},
		{name: "idle negative", key: "QUOTA_ROUTER_IDLE_TIMEOUT", value: "-1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetConfigEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := Load()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.key) || !strings.Contains(err.Error(), "must be > 0") {
				t.Fatalf("error %q missing actionable detail for %s", err, tt.key)
			}
		})
	}
}

func TestLoadRejectsEmptyAddr(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("QUOTA_ROUTER_ADDR", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "QUOTA_ROUTER_ADDR must not be empty") {
		t.Fatalf("error %q missing actionable detail", err)
	}
}

func TestLoadEmptyDurationUsesDefault(t *testing.T) {
	for _, key := range []string{
		"QUOTA_ROUTER_SHUTDOWN_TIMEOUT",
		"QUOTA_ROUTER_READ_HEADER_TIMEOUT",
		"QUOTA_ROUTER_READ_TIMEOUT",
		"QUOTA_ROUTER_WRITE_TIMEOUT",
		"QUOTA_ROUTER_IDLE_TIMEOUT",
	} {
		t.Run(key, func(t *testing.T) {
			unsetConfigEnv(t)
			t.Setenv(key, "")

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v, want default for empty %s", err, key)
			}
			want := map[string]time.Duration{
				"QUOTA_ROUTER_SHUTDOWN_TIMEOUT":    10 * time.Second,
				"QUOTA_ROUTER_READ_HEADER_TIMEOUT": 5 * time.Second,
				"QUOTA_ROUTER_READ_TIMEOUT":        5 * time.Second,
				"QUOTA_ROUTER_WRITE_TIMEOUT":       10 * time.Second,
				"QUOTA_ROUTER_IDLE_TIMEOUT":        60 * time.Second,
			}[key]
			got := map[string]time.Duration{
				"QUOTA_ROUTER_SHUTDOWN_TIMEOUT":    cfg.ShutdownTimeout,
				"QUOTA_ROUTER_READ_HEADER_TIMEOUT": cfg.ReadHeaderTimeout,
				"QUOTA_ROUTER_READ_TIMEOUT":        cfg.ReadTimeout,
				"QUOTA_ROUTER_WRITE_TIMEOUT":       cfg.WriteTimeout,
				"QUOTA_ROUTER_IDLE_TIMEOUT":        cfg.IdleTimeout,
			}[key]
			if got != want {
				t.Fatalf("%s = %s, want default %s", key, got, want)
			}
		})
	}
}

func TestLoadReportsMultipleErrors(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("QUOTA_ROUTER_ADDR", "")
	t.Setenv("QUOTA_ROUTER_READ_TIMEOUT", "bad")
	t.Setenv("QUOTA_ROUTER_WRITE_TIMEOUT", "-1s")
	t.Setenv("QUOTA_ROUTER_SHUTDOWN_TIMEOUT", "0s")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{
		"QUOTA_ROUTER_ADDR must not be empty",
		"QUOTA_ROUTER_READ_TIMEOUT",
		"not a valid duration",
		"QUOTA_ROUTER_WRITE_TIMEOUT",
		"must be > 0",
		"QUOTA_ROUTER_SHUTDOWN_TIMEOUT",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

func unsetConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range configEnvKeys {
		orig, ok := os.LookupEnv(key)
		if ok {
			k := key
			v := orig
			t.Cleanup(func() { _ = os.Setenv(k, v) })
		} else {
			k := key
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
		_ = os.Unsetenv(key)
	}
}
