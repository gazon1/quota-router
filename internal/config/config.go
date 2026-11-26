package config

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// Config holds runtime configuration parsed from environment variables.
type Config struct {
	Addr              string
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

const (
	defaultAddr              = ":8080"
	defaultShutdownTimeout   = 10 * time.Second
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 5 * time.Second
	defaultWriteTimeout      = 10 * time.Second
	defaultIdleTimeout       = 60 * time.Second
)

// Load reads configuration from environment variables. It fails fast with an
// actionable message when a value is present but invalid.
func Load() (Config, error) {
	cfg := Config{
		Addr:              defaultAddr,
		ShutdownTimeout:   defaultShutdownTimeout,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}
	if v, ok := os.LookupEnv("QUOTA_ROUTER_ADDR"); ok {
		cfg.Addr = v
	}

	var errs []error
	var err error

	if cfg.ShutdownTimeout, err = durationEnv("QUOTA_ROUTER_SHUTDOWN_TIMEOUT", defaultShutdownTimeout); err != nil {
		errs = append(errs, err)
	}
	if cfg.ReadHeaderTimeout, err = durationEnv("QUOTA_ROUTER_READ_HEADER_TIMEOUT", defaultReadHeaderTimeout); err != nil {
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

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
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
	return d, nil
}
