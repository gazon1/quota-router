// Package store persists quota snapshots, local usage, provider health and
// controller state in one local SQLite database.
//
// It owns the schema (versioned migrations under migrations/), the SQLite
// operational settings, retention and the backup path. Callers write records and
// read the latest state back; they never see SQL.
//
// The types here mirror the provider-neutral model of issue #9. They live in
// this package because nothing else consumes them yet; move them out when the
// model package lands rather than keeping two copies.
package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// QuotaState is the subscription-quota state. UNKNOWN is never zero remaining:
// a missing, stale or unparsable observation is UNKNOWN, never AVAILABLE.
type QuotaState string

const (
	QuotaAvailable QuotaState = "AVAILABLE"
	QuotaLow       QuotaState = "LOW_QUOTA"
	QuotaExhausted QuotaState = "EXHAUSTED"
	QuotaUnknown   QuotaState = "UNKNOWN"
)

// UnknownReason says why a subscription quota is QuotaUnknown.
type UnknownReason string

const (
	// ReasonUnsupported: the provider documents no quota API.
	ReasonUnsupported UnknownReason = "unsupported"
	// ReasonFetchFailed: a transient fetch failure.
	ReasonFetchFailed UnknownReason = "fetch_failed"
	// ReasonStale: the last observation is older than the freshness bound.
	ReasonStale UnknownReason = "stale"
	// ReasonMalformed: the response was malformed or incomplete.
	ReasonMalformed UnknownReason = "malformed"
	// ReasonAuth: authentication or permission failure.
	ReasonAuth UnknownReason = "auth"
	// ReasonNotObserved: not polled yet.
	ReasonNotObserved UnknownReason = "not_observed"
)

// Confidence marks whether a value came from the provider or is a local
// estimate. Local usage is estimated; a subscription quota is reported.
type Confidence string

const (
	Reported  Confidence = "reported"
	Estimated Confidence = "estimated"
)

// HealthState is provider reachability, independent of quota state: HTTP
// success does not prove subscription quota is available.
type HealthState string

const (
	Healthy     HealthState = "HEALTHY"
	Degraded    HealthState = "DEGRADED"
	Unavailable HealthState = "UNAVAILABLE"
	Unknown     HealthState = "UNKNOWN"
)

// QuotaWindow is one quota window a provider reports, for example a rolling
// five-hour window. A nil value means the provider did not report that number,
// which stays distinct from a reported zero.
type QuotaWindow struct {
	Window string
	Unit   string // "tokens", "requests", "percent", ...
	Limit,
	Used,
	Remaining *float64
	ResetAt *time.Time
}

// QuotaSnapshot is one observation of a provider's subscription quota.
type QuotaSnapshot struct {
	Provider string
	State    QuotaState
	// UnknownReason explains State == QuotaUnknown; nil otherwise.
	UnknownReason *UnknownReason
	// Source names the documented provider endpoint or feed the values came
	// from, for provenance.
	Source     string
	Confidence Confidence
	ObservedAt time.Time
	// Error is a short, safe description of a failed fetch. Never a raw
	// provider response: those can carry credentials. Validate enforces that.
	Error   string
	Windows []QuotaWindow
}

// UsageRecord is one locally observed request. It is accounting data, never a
// substitute for subscription quota.
type UsageRecord struct {
	// RequestID deduplicates retried or repeated reports; empty means the
	// source supplied none and duplicates cannot be detected.
	RequestID string
	Provider  string
	Model     string
	// Nil means the number was not supplied; it is not estimated to zero.
	InputTokens,
	OutputTokens *int
	Provenance Confidence
	ObservedAt time.Time
}

// HealthRecord is one observation of provider reachability.
type HealthRecord struct {
	Provider   string
	State      HealthState
	ObservedAt time.Time
	Error      string
}

// Validate checks the values the schema constrains, so a bad enum reports the
// field instead of a bare constraint failure.
func (s QuotaSnapshot) Validate() error {
	var errs []error
	if s.Provider == "" {
		errs = append(errs, errors.New("provider must not be empty"))
	}
	if s.ObservedAt.IsZero() {
		errs = append(errs, errors.New("observed_at must be set"))
	}
	if s.Source == "" {
		errs = append(errs, errors.New("source must not be empty"))
	}
	if !s.Confidence.valid() {
		errs = append(errs, fmt.Errorf("confidence %q must be reported or estimated", s.Confidence))
	}
	switch s.State {
	case QuotaAvailable, QuotaLow, QuotaExhausted:
		if s.UnknownReason != nil {
			errs = append(errs, fmt.Errorf("unknown_reason %q is only valid for %s", *s.UnknownReason, QuotaUnknown))
		}
	case QuotaUnknown:
		if s.UnknownReason == nil {
			errs = append(errs, fmt.Errorf("%s needs an unknown_reason", QuotaUnknown))
		} else {
			switch *s.UnknownReason {
			case ReasonUnsupported, ReasonFetchFailed, ReasonStale, ReasonMalformed, ReasonAuth, ReasonNotObserved:
			default:
				errs = append(errs, fmt.Errorf("unknown_reason %q is not one of the documented reasons", *s.UnknownReason))
			}
		}
	default:
		errs = append(errs, fmt.Errorf("state %q must be AVAILABLE, LOW_QUOTA, EXHAUSTED or UNKNOWN", s.State))
	}
	if err := checkSafeError(s.Error); err != nil {
		errs = append(errs, err)
	}
	for i, w := range s.Windows {
		if w.Window == "" {
			errs = append(errs, fmt.Errorf("window %d has no name", i))
		}
	}
	return errors.Join(errs...)
}

func (c Confidence) valid() bool { return c == Reported || c == Estimated }

// maxError bounds a stored error description. A provider message long enough to
// carry a full response body is a raw response, not a description.
const maxError = 512

// secretMarkers are shapes that identify a credential rather than a failure.
// Not exhaustive by design: the rule is "describe the failure", and this stops
// the obvious mistakes from reaching a database that gets backed up.
var secretMarkers = []string{"authorization:", "bearer ", "api_key", "apikey", "x-api-key", "password", "sk-"}

// checkSafeError rejects an error string that is not a short, credential-free
// description. It is a trust boundary: whatever a provider adapter passes here
// is persisted, printed in logs and copied into every backup.
func checkSafeError(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > maxError {
		return fmt.Errorf("error must be a short description, got %d bytes (raw provider responses carry credentials)", len(v))
	}
	for _, m := range secretMarkers {
		if strings.Contains(strings.ToLower(v), m) {
			return fmt.Errorf("error looks like it contains a credential (%q); store a short reason instead", m)
		}
	}
	return nil
}

// Validate checks the fields the usage schema constrains.
func (u UsageRecord) Validate() error {
	var errs []error
	if u.Provider == "" {
		errs = append(errs, errors.New("provider must not be empty"))
	}
	if u.ObservedAt.IsZero() {
		errs = append(errs, errors.New("observed_at must be set"))
	}
	if !u.Provenance.valid() {
		errs = append(errs, fmt.Errorf("provenance %q must be %s or %s", u.Provenance, Reported, Estimated))
	}
	return errors.Join(errs...)
}

// Validate checks the fields the health schema constrains.
func (h HealthRecord) Validate() error {
	var errs []error
	if h.Provider == "" {
		errs = append(errs, errors.New("provider must not be empty"))
	}
	if h.ObservedAt.IsZero() {
		errs = append(errs, errors.New("observed_at must be set"))
	}
	switch h.State {
	case Healthy, Degraded, Unavailable, Unknown:
	default:
		errs = append(errs, fmt.Errorf("state %q must be HEALTHY, DEGRADED, UNAVAILABLE or UNKNOWN", h.State))
	}
	if err := checkSafeError(h.Error); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
