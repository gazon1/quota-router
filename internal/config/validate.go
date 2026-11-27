package config

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
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
	for alias, m := range c.LiteLLM.ModelAliases {
		where := "litellm.model_aliases." + alias
		if alias == "" {
			errs = append(errs, errors.New("litellm.model_aliases must not contain an empty alias"))
		} else if strings.ContainsAny(alias, "/ \t") {
			// The alias is also an environment-variable segment and a routing key.
			errs = append(errs, fmt.Errorf("litellm.model_aliases.%s must not contain a slash or whitespace", alias))
		}
		// The provider is the prefix of the model id, so one check covers both.
		if _, name, found := strings.Cut(m.Model, "/"); m.Provider() == "" || !found || name == "" {
			errs = append(errs, fmt.Errorf("%s.model must be <provider>/<model>, got %q", where, m.Model))
		}
		if m.ContextLimit < 0 {
			errs = append(errs, fmt.Errorf("%s.context_limit must be >= 0 (0 means unverified), got %d", where, m.ContextLimit))
		}
		for name, cap := range map[string]Capability{
			"streaming":         m.Capabilities.Streaming,
			"tool_calling":      m.Capabilities.ToolCalling,
			"structured_output": m.Capabilities.StructuredOutput,
		} {
			switch cap {
			case CapabilityUnknown, CapabilitySupported, CapabilityUnsupported:
			default:
				errs = append(errs, fmt.Errorf("%s.capabilities.%s must be %q, %q or absent, got %q",
					where, name, CapabilitySupported, CapabilityUnsupported, cap))
			}
		}
		for _, task := range m.Tasks {
			if strings.TrimSpace(task) == "" {
				errs = append(errs, fmt.Errorf("%s.tasks must not contain an empty task class", where))
			}
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
	errs = append(errs, c.validateRouting()...)

	check(c.SQLite.Path != "", "sqlite.path must not be empty")
	// The DSN is built from this value, so a SQLite URI form here would be
	// prefixed with another "file:" and open a different database than the one
	// named — or an in-memory one that loses everything on restart. Accept a
	// plain filesystem path only.
	lower := strings.ToLower(c.SQLite.Path)
	check(!strings.HasPrefix(c.SQLite.Path, ":"),
		"sqlite.path must be a file path, not a SQLite special name like %q (it would not persist)", c.SQLite.Path)
	check(!strings.HasPrefix(lower, "file:"),
		"sqlite.path must be a file path, not a SQLite URI like %q", c.SQLite.Path)
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

func (c Config) validateRouting() []error {
	if len(c.Routing.Providers) == 0 {
		return nil
	}

	var errs []error
	byProvider := map[string][]string{}
	for _, alias := range sortedAliases(c.LiteLLM.ModelAliases) {
		m := c.LiteLLM.ModelAliases[alias]
		if !m.IsEnabled() {
			continue
		}
		p := m.Provider()
		byProvider[p] = append(byProvider[p], alias)
	}

	seenPriority := map[int]string{}
	for _, provider := range sortedProviderRouting(c.Routing.Providers) {
		rp := c.Routing.Providers[provider]
		where := "routing.providers." + provider
		if provider == "" {
			errs = append(errs, errors.New("routing.providers must not contain an empty provider name"))
			continue
		}
		if rp.Priority <= 0 {
			errs = append(errs, fmt.Errorf("%s.priority must be a positive integer (lower means higher preference), got %d", where, rp.Priority))
		} else if prev, ok := seenPriority[rp.Priority]; ok {
			errs = append(errs, fmt.Errorf("%s.priority duplicates %s.priority: %d", where, "routing.providers."+prev, rp.Priority))
		} else {
			seenPriority[rp.Priority] = provider
		}

		aliases, ok := byProvider[provider]
		if !ok {
			errs = append(errs, fmt.Errorf("%s references provider %q, which has no enabled catalogue aliases", where, provider))
			continue
		}
		if len(rp.Models) == 0 {
			errs = append(errs, fmt.Errorf("%s.models must list every enabled catalogue alias for provider %q", where, provider))
			continue
		}

		seenModelPriority := map[int]string{}
		for _, alias := range sortedModelPriorities(rp.Models) {
			priority := rp.Models[alias]
			mWhere := where + ".models." + alias
			if alias == "" {
				errs = append(errs, fmt.Errorf("%s.models must not contain an empty alias", where))
				continue
			}
			m, exists := c.LiteLLM.ModelAliases[alias]
			if !exists {
				errs = append(errs, fmt.Errorf("%s references unknown catalogue alias %q", mWhere, alias))
				continue
			}
			if !m.IsEnabled() {
				errs = append(errs, fmt.Errorf("%s references disabled catalogue alias %q", mWhere, alias))
				continue
			}
			if m.Provider() != provider {
				errs = append(errs, fmt.Errorf("%s belongs to provider %q, not %q", mWhere, m.Provider(), provider))
				continue
			}
			if priority <= 0 {
				errs = append(errs, fmt.Errorf("%s must be a positive integer (lower means higher preference), got %d", mWhere, priority))
			} else if prev, ok := seenModelPriority[priority]; ok {
				errs = append(errs, fmt.Errorf("%s duplicates %s: %d", mWhere, where+".models."+prev, priority))
			} else {
				seenModelPriority[priority] = alias
			}
		}
		for _, alias := range aliases {
			if _, ok := rp.Models[alias]; !ok {
				errs = append(errs, fmt.Errorf("%s.models is missing enabled catalogue alias %q", where, alias))
			}
		}
	}

	for provider, aliases := range byProvider {
		if _, ok := c.Routing.Providers[provider]; ok {
			continue
		}
		errs = append(errs, fmt.Errorf("routing.providers is missing provider %q for enabled catalogue aliases %s", provider, strings.Join(aliases, ", ")))
	}
	return errs
}

func sortedProviderRouting(m map[string]ProviderRouting) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedModelPriorities(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
