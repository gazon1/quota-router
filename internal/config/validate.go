package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// validate reports every broken constraint at once. Messages name the setting
// and, for URLs, print the redacted form so embedded credentials stay out of
// logs and error output.
func (c Config) validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}

	check(c.Server.Addr != "", "server.addr must not be empty")
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"shutdown_timeout", c.Server.ShutdownTimeout},
		{"read_header_timeout", c.Server.ReadHeaderTimeout},
		{"read_timeout", c.Server.ReadTimeout},
		{"write_timeout", c.Server.WriteTimeout},
		{"idle_timeout", c.Server.IdleTimeout},
	} {
		check(d.v > 0, "server.%s must be > 0, got %s", d.name, d.v)
	}

	if err := checkURL(c.LiteLLM.BaseURL); err != nil {
		errs = append(errs, fmt.Errorf("litellm.base_url: %w", err))
	}
	for alias, model := range c.LiteLLM.ModelAliases {
		if alias == "" {
			errs = append(errs, errors.New("litellm.model_aliases must not contain an empty alias"))
		} else if p, m, found := strings.Cut(model, "/"); !found || p == "" || m == "" {
			errs = append(errs, fmt.Errorf("litellm.model_aliases.%s must be <provider>/<model>, got %q", alias, model))
		}
	}

	for name, p := range c.Providers {
		if name == "" {
			errs = append(errs, errors.New("providers must not contain an empty provider name"))
			continue
		}
		if err := checkURL(p.BaseURL); err != nil {
			errs = append(errs, fmt.Errorf("providers.%s.base_url: %w", name, err))
		}
	}

	check(c.Polling.Interval > 0, "polling.interval must be > 0, got %s", c.Polling.Interval)
	check(c.Polling.RequestTimeout > 0, "polling.request_timeout must be > 0, got %s", c.Polling.RequestTimeout)
	check(c.Polling.RequestTimeout <= c.Polling.Interval,
		"polling.request_timeout (%s) must not exceed polling.interval (%s), or polls overlap", c.Polling.RequestTimeout, c.Polling.Interval)

	check(c.Routing.LowQuotaThreshold > 0 && c.Routing.LowQuotaThreshold <= 1,
		"routing.low_quota_threshold must be in (0, 1], got %v", c.Routing.LowQuotaThreshold)

	check(c.SQLite.Path != "", "sqlite.path must not be empty")
	check(c.SQLite.BusyTimeout > 0, "sqlite.busy_timeout must be > 0, got %s", c.SQLite.BusyTimeout)
	// Zero disables pruning for that table; a negative value is a typo.
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"quota_snapshots", c.SQLite.Retention.QuotaSnapshots},
		{"provider_health", c.SQLite.Retention.ProviderHealth},
		{"usage_records", c.SQLite.Retention.UsageRecords},
	} {
		check(d.v >= 0, "sqlite.retention.%s must be >= 0 (0 keeps everything), got %s", d.name, d.v)
	}

	switch c.Observability.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("observability.level must be debug, info, warn or error, got %q", c.Observability.Level))
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return nil
}

// checkURL never echoes the value: a URL may carry user:password credentials.
// Credentials are rejected rather than accepted, because a base URL is a plain
// string that a log line or a JSON dump would print in full; callers set the
// credential on the matching Secret field instead.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an absolute http:// or https:// URL")
	}
	if u.User != nil {
		return errors.New("must not embed credentials in the URL; use the api_key or token setting instead")
	}
	return nil
}
