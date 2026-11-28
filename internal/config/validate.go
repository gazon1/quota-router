package config

import (
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
)

// validate reports every broken constraint at once. Messages name the setting
// and never leak secrets or URL credentials.
func (c Config) validate() error {
	errs := make([]error, 0, 8)
	collectFieldErrors(getValidator().Struct(c), &errs)
	errs = append(errs, validateRoutingCatalogue(c)...)
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return nil
}

func collectFieldErrors(err error, errs *[]error) {
	if err == nil {
		return
	}
	var ve validator.ValidationErrors
	if !errors.As(err, &ve) {
		*errs = append(*errs, err)
		return
	}
	for _, fe := range ve {
		*errs = append(*errs, formatValidationError(fe))
	}
}

func formatValidationError(fe validator.FieldError) error {
	path := fieldPath(fe)
	switch fe.Tag() {
	case "required":
		if path == "litellm.model_aliases" {
			return fmt.Errorf("litellm.model_aliases must not contain an empty alias")
		}
		if strings.HasSuffix(path, ".days") {
			return fmt.Errorf("%s must name at least one weekday (monday..sunday)", path)
		}
		if strings.HasSuffix(path, ".timezone") {
			return fmt.Errorf("%s must name an IANA time zone when windows are configured", path)
		}
		if strings.Contains(path, ".tasks.") {
			base := path[:strings.LastIndex(path, ".tasks.")+len(".tasks")]
			return fmt.Errorf("%s must not contain an empty task class", base)
		}
		return fmt.Errorf("%s must not be empty", path)
	case "gt":
		if fe.Kind() == reflect.Int || fe.Kind() == reflect.Int64 {
			if strings.HasSuffix(path, ".priority") || strings.Contains(path, ".models.") {
				return fmt.Errorf("%s must be a positive integer (lower means higher preference), got %v", path, fe.Value())
			}
			return fmt.Errorf("%s must be > 0, got %v", path, fe.Value())
		}
		return fmt.Errorf("%s must be > 0, got %v", path, fe.Value())
	case "gte":
		if fe.Field() == "context_limit" {
			return fmt.Errorf("%s must be >= 0 (0 means unverified), got %v", path, fe.Value())
		}
		if strings.HasPrefix(path, "sqlite.retention.") {
			return fmt.Errorf("%s must be >= 0 (0 keeps everything), got %v", path, fe.Value())
		}
		if path == "routing.reserve_fraction" {
			return fmt.Errorf("%s must be in [0, 1], got %v", path, fe.Value())
		}
		return fmt.Errorf("%s must be >= %s, got %v", path, fe.Param(), fe.Value())
	case "lte":
		if path == "routing.low_quota_threshold" || path == "routing.reserve_fraction" {
			if path == "routing.low_quota_threshold" {
				return fmt.Errorf("%s must be in (0, 1], got %v", path, fe.Value())
			}
			return fmt.Errorf("%s must be in [0, 1], got %v", path, fe.Value())
		}
		return fmt.Errorf("%s must be <= %s, got %v", path, fe.Param(), fe.Value())
	case "oneof":
		if strings.HasPrefix(path, "observability.level") {
			return fmt.Errorf("%s must be debug, info, warn or error, got %q", path, fe.Value())
		}
		if strings.Contains(path, ".capabilities.") {
			return fmt.Errorf("%s must be %q, %q or absent, got %q", path, CapabilitySupported, CapabilityUnsupported, fe.Value())
		}
		return fmt.Errorf("%s must be one of [%s], got %v", path, fe.Param(), fe.Value())
	case "http_url":
		return httpURLValidationError(path, fmt.Sprint(fe.Value()))
	case "sqlite_path":
		p := fmt.Sprint(fe.Value())
		lower := strings.ToLower(p)
		if strings.HasPrefix(p, ":") {
			return fmt.Errorf("%s must be a file path, not a SQLite special name like %q (it would not persist)", path, p)
		}
		if strings.HasPrefix(lower, "file:") {
			return fmt.Errorf("%s must be a file path, not a SQLite URI like %q", path, p)
		}
		return fmt.Errorf("%s must be a file path", path)
	case "model_id":
		return fmt.Errorf("%s must be <provider>/<model>, got %q", path, fe.Value())
	case "alias_name":
		if strings.TrimSpace(fmt.Sprint(fe.Value())) == "" {
			if strings.HasPrefix(path, "litellm.model_aliases") {
				return fmt.Errorf("litellm.model_aliases must not contain an empty alias")
			}
			return fmt.Errorf("%s must not be empty", path)
		}
		return fmt.Errorf("%s must not contain a slash or whitespace", path)
	case "iana_timezone":
		if strings.TrimSpace(fmt.Sprint(fe.Value())) == "" {
			return fmt.Errorf("%s must name an IANA time zone when windows are configured", path)
		}
		return fmt.Errorf("%s must name an IANA time zone", path)
	case "weekday":
		// Report on the days field, not the individual element, to preserve
		// the existing config validation message shape.
		if idx := strings.LastIndex(path, ".days."); idx >= 0 {
			path = path[:idx+len(".days")]
		}
		return fmt.Errorf("%s must name weekdays (monday..sunday)", path)
	case "clock_hhmm":
		if strings.HasSuffix(path, ".start") {
			return fmt.Errorf("%s must be a 24-hour HH:MM time", path)
		}
		if strings.HasSuffix(path, ".end") {
			return fmt.Errorf("%s must be a 24-hour HH:MM time", path)
		}
		return fmt.Errorf("%s must be a 24-hour HH:MM time", path)
	case "nonempty_task":
		return fmt.Errorf("%s must not contain an empty task class", path)
	case "unique_provider_priority":
		return fmt.Errorf("%s duplicates routing.providers.%s.priority: %v", path, fe.Param(), fe.Value())
	case "unique_model_priority":
		providerPath := strings.Split(path, ".models.")[0]
		return fmt.Errorf("%s duplicates %s.models.%s: %v", path, providerPath, fe.Param(), fe.Value())
	case "window_bounds_differ":
		base := path
		if idx := strings.LastIndex(base, "."); idx >= 0 {
			base = base[:idx]
		}
		return fmt.Errorf("%s.start and end must differ", base)
	case "poll_timeout_lte_interval":
		// param carries interval, value is timeout
		return fmt.Errorf("polling.request_timeout (%v) must not exceed polling.interval (%s), or polls overlap", fe.Value(), fe.Param())
	case "reserve_gte_low_quota":
		return fmt.Errorf("routing.reserve_fraction (%v) must be 0 or >= routing.low_quota_threshold (%s)", fe.Value(), fe.Param())
	default:
		return fmt.Errorf("%s is invalid", path)
	}
}

func httpURLValidationError(path, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s: must be an absolute http:// or https:// URL", path)
	}
	if u.User != nil {
		return fmt.Errorf("%s: must not embed credentials in the URL; use the api_key or token setting instead", path)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("%s: must not contain a query string", path)
	}
	if u.Fragment != "" || strings.HasSuffix(raw, "#") {
		return fmt.Errorf("%s: must not contain a fragment", path)
	}
	return fmt.Errorf("%s: must be an absolute http:// or https:// URL", path)
}

func fieldPath(fe validator.FieldError) string {
	ns := fe.Namespace()
	ns = strings.TrimPrefix(ns, "Config.")
	// map keys in validator namespaces are wrapped as [key]
	ns = strings.ReplaceAll(ns, "[", ".")
	ns = strings.ReplaceAll(ns, "]", "")
	ns = strings.ReplaceAll(ns, "..", ".")
	return strings.Trim(ns, ".")
}

func validateConfigStruct(sl validator.StructLevel) {
	c := sl.Current().Interface().(Config)
	if c.Polling.RequestTimeout > 0 && c.Polling.Interval > 0 && c.Polling.RequestTimeout > c.Polling.Interval {
		sl.ReportError(c.Polling.RequestTimeout, "request_timeout", "request_timeout", "poll_timeout_lte_interval", c.Polling.Interval.String())
	}
	if c.Routing.ReserveFraction > 0 && c.Routing.LowQuotaThreshold > 0 && c.Routing.ReserveFraction < c.Routing.LowQuotaThreshold {
		sl.ReportError(c.Routing.ReserveFraction, "reserve_fraction", "reserve_fraction", "reserve_gte_low_quota", fmt.Sprint(c.Routing.LowQuotaThreshold))
	}
}

func validateRoutingStruct(sl validator.StructLevel) {
	r := sl.Current().Interface().(Routing)
	seenPriority := map[int]string{}
	for _, provider := range sortedProviderRouting(r.Providers) {
		rp := r.Providers[provider]
		if rp.Priority <= 0 {
			continue
		}
		if prev, ok := seenPriority[rp.Priority]; ok {
			sl.ReportError(rp.Priority, fmt.Sprintf("providers[%s].priority", provider), "priority", "unique_provider_priority", prev)
		} else {
			seenPriority[rp.Priority] = provider
		}
	}
}

func validateProviderRoutingStruct(sl validator.StructLevel) {
	rp := sl.Current().Interface().(ProviderRouting)
	seen := map[int]string{}
	for _, alias := range sortedModelPriorities(rp.Models) {
		prio := rp.Models[alias]
		if prio <= 0 {
			continue
		}
		if prev, ok := seen[prio]; ok {
			sl.ReportError(prio, fmt.Sprintf("models[%s]", alias), alias, "unique_model_priority", prev)
		} else {
			seen[prio] = alias
		}
	}
}

func validateModelStruct(sl validator.StructLevel) {
	m := sl.Current().Interface().(Model)
	for i, task := range m.Tasks {
		if strings.TrimSpace(task) == "" {
			sl.ReportError(task, fmt.Sprintf("tasks[%d]", i), "tasks", "nonempty_task", "")
		}
	}
	// unavailable window sanity for equal boundaries is handled in window struct validation;
	// cross-field presence is handled by Unavailable struct validation.
	_ = m
}

func validateUnavailableStruct(sl validator.StructLevel) {
	u := sl.Current().Interface().(Unavailable)
	if len(u.Windows) == 0 {
		return
	}
	if strings.TrimSpace(u.Timezone) == "" {
		sl.ReportError(u.Timezone, "timezone", "timezone", "required", "")
		return
	}
	if u.Timezone == "Local" {
		sl.ReportError(u.Timezone, "timezone", "timezone", "iana_timezone", "")
		return
	}
	if _, err := time.LoadLocation(u.Timezone); err != nil {
		sl.ReportError(u.Timezone, "timezone", "timezone", "iana_timezone", "")
	}
}

func validateUnavailableWindowStruct(sl validator.StructLevel) {
	w := sl.Current().Interface().(UnavailableWindow)
	start, err1 := timeParseHHMM(w.Start)
	end, err2 := timeParseHHMM(w.End)
	if err1 == nil && err2 == nil && start == end {
		sl.ReportError(w.End, "end", "end", "window_bounds_differ", "")
	}
}

func timeParseHHMM(v string) (int, error) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

func validateRoutingCatalogue(c Config) []error {
	if len(c.Routing.Providers) == 0 {
		return nil
	}
	var errs []error
	cat := c.Catalogue()
	byProvider := map[string][]string{}
	for _, alias := range sortedAliases(c.LiteLLM.ModelAliases) {
		m := cat.ModelAliases[alias]
		if !m.IsEnabled() {
			continue
		}
		byProvider[m.Provider()] = append(byProvider[m.Provider()], alias)
	}

	for _, provider := range sortedProviderRouting(c.Routing.Providers) {
		rp := c.Routing.Providers[provider]
		where := "routing.providers." + provider
		if provider == "" {
			errs = append(errs, errors.New("routing.providers must not contain an empty provider name"))
			continue
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

		for _, alias := range sortedModelPriorities(rp.Models) {
			mWhere := where + ".models." + alias
			m, exists := cat.ModelAliases[alias]
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
