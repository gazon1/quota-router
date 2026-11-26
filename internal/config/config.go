package config

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// Config holds runtime configuration parsed from environment variables.
type Config struct {
	Addr            string
	ShutdownTimeout time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
}

const (
	defaultAddr            = ":8080"
	defaultShutdownTimeout = 10 * time.Second
	defaultReadTimeout     = 5 * time.Second
	defaultWriteTimeout    = 10 * time.Second
	defaultIdleTimeout     = 60 * time.Second
)

// Load reads configuration from environment variables. It fails fast with an
// actionable message when a value is present but invalid.
func Load() (Config, error) {
	cfg := Config{
		Addr:            envOrDefault("QUOTA_ROUTER_ADDR", defaultAddr),
		ShutdownTimeout: defaultShutdownTimeout,
		ReadTimeout:     defaultReadTimeout,
		WriteTimeout:    defaultWriteTimeout,
		IdleTimeout:     defaultIdleTimeout,
	}

	var errs []error
	var err error

	if cfg.ShutdownTimeout, err = durationEnv("QUOTA_ROUTER_SHUTDOWN_TIMEOUT", defaultShutdownTimeout); err != nil {
		errs = append(errs, err)
	}
	if cfg.ReadTimeout, err = durationEnv("QUOTA_ROUTER_READ_TIMEOUT", defaultReadTimeout); err != nil {
		errs = append(errs, err)
	}
	if cfg.WriteTimeout, err = durationEnv("QUOTA_ROUTER_WRITE_TIMEOUT", defaultWriteTimeout); err != nil {
		errs = append(errs, err)
	}
	if cfg.IdleTimeout, err = durationEnv("QUOTA_ROUTER_IDLE_TIMEOUT", defaultIdleTimeout); err != nil {
		errs = append(errs, err)
	}

	if cfg.Addr == "" {
		errs = append(errs, errors.New("QUOTA_ROUTER_ADDR must not be empty"))
	}
	if cfg.ShutdownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("QUOTA_ROUTER_SHUTDOWN_TIMEOUT must be > 0, got %s", cfg.ShutdownTimeout))
	}
	if cfg.ReadTimeout <= 0 {
		errs = append(errs, fmt.Errorf("QUOTA_ROUTER_READ_TIMEOUT must be > 0, got %s", cfg.ReadTimeout))
	}
	if cfg.WriteTimeout <= 0 {
		errs = append(errs, fmt.Errorf("QUOTA_ROUTER_WRITE_TIMEOUT must be > 0, got %s", cfg.WriteTimeout))
	}
	if cfg.IdleTimeout <= 0 {
		errs = append(errs, fmt.Errorf("QUOTA_ROUTER_IDLE_TIMEOUT must be > 0, got %s", cfg.IdleTimeout))
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func envOrDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid duration (examples: 5s, 1m): %w", key, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be > 0, got %s", key, d)
	}
	if d > 24*time.Hour {
		return 0, fmt.Errorf("%s must be <= 24h, got %s", key, d)
	}
	return d, nil
}
