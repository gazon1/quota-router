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

// Outcome is how one attempt ended, as the reporter observed it. It is a
// closed set because the store must not guess between "the stream broke" and
// "the caller cancelled".
type Outcome string

const (
	// OutcomeSuccess means the attempt completed and the response was delivered.
	OutcomeSuccess Outcome = "SUCCESS"
	// OutcomeFailedBeforeFirstToken means the attempt produced no output. The
	// provider may still have charged the input.
	OutcomeFailedBeforeFirstToken Outcome = "FAILED_BEFORE_FIRST_TOKEN"
	// OutcomePartialStream means part of the response was delivered before the
	// attempt stopped.
	OutcomePartialStream Outcome = "PARTIAL_STREAM"
	// OutcomeCancelled means the caller abandoned the attempt.
	OutcomeCancelled Outcome = "CANCELLED"
	// OutcomeUnknown means the reporter did not say. It is not a failure and
	// not a success, and no usage is inferred from it.
	OutcomeUnknown Outcome = "UNKNOWN"
)

func (o Outcome) valid() bool {
	switch o {
	case OutcomeSuccess, OutcomeFailedBeforeFirstToken, OutcomePartialStream, OutcomeCancelled, OutcomeUnknown:
		return true
	}
	return false
}

// UsageRecord is one attempt of one request, as this installation observed it.
// It is accounting data, never a substitute for subscription quota: a reported
// token count describes what the provider reported for that request, not how
// much subscription quota it consumed.
type UsageRecord struct {
	// RequestID deduplicates retried or repeated reports; empty means the
	// source supplied none and duplicates cannot be detected.
	RequestID string
	// Attempt numbers the attempt within the request, starting at 1. A retry is
	// a new attempt, not a second copy of the usage it replaces. Zero means the
	// first attempt.
	Attempt  int
	Provider string
	Model    string
	// Nil means the number was not supplied; it is not estimated to zero.
	InputTokens,
	OutputTokens *int
	Provenance Confidence
	// Source names the channel that reported the row, for provenance.
	Source  string
	Outcome Outcome
	// DecisionID is the routing decision this attempt was routed by, when the
	// reporter knows it. Routing history owns that record; this only references
	// it, and an id that no decision claims is kept as reported.
	DecisionID string
	ObservedAt time.Time
}

// HealthRecord is one observation of provider reachability.
type HealthRecord struct {
	Provider      string
	State         HealthState
	ObservedAt    time.Time
	Error         string
	CooldownUntil *time.Time
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
var secretMarkers = []string{
	"authorization:", "bearer ", "api_key", "apikey", "x-api-key", "sk-",
	"password", "access_token", "access-token", "client_secret", "client-secret",
}

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
	if u.Attempt < 0 {
		errs = append(errs, fmt.Errorf("attempt must be >= 1 (or 0 for the first), got %d", u.Attempt))
	}
	if !u.Provenance.valid() {
		errs = append(errs, fmt.Errorf("provenance %q must be %s or %s", u.Provenance, Reported, Estimated))
	}
	if u.Source == "" {
		errs = append(errs, errors.New("source must not be empty"))
	}
	if !u.Outcome.valid() {
		errs = append(errs, fmt.Errorf("outcome %q must be SUCCESS, FAILED_BEFORE_FIRST_TOKEN, PARTIAL_STREAM, CANCELLED or UNKNOWN", u.Outcome))
	}
	// Negative counts are a broken reporter, and storing one would corrupt a
	// SUM later. Zero is a legitimate reported value.
	if u.InputTokens != nil && *u.InputTokens < 0 {
		errs = append(errs, fmt.Errorf("input_tokens must be >= 0, got %d", *u.InputTokens))
	}
	if u.OutputTokens != nil && *u.OutputTokens < 0 {
		errs = append(errs, fmt.Errorf("output_tokens must be >= 0, got %d", *u.OutputTokens))
	}
	if len(u.RequestID) > maxKey || len(u.DecisionID) > maxKey || len(u.Source) > maxKey {
		errs = append(errs, fmt.Errorf("request_id, decision_id and source must be at most %d bytes", maxKey))
	}
	return errors.Join(errs...)
}

// maxKey bounds the identifier columns. They are written verbatim from an HTTP
// request, and an unbounded one is a cheap way to fill the database.
const maxKey = 256

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

// RoutingDecisionRecord is one persisted routing decision.
type RoutingDecisionRecord struct {
	DecisionID       string
	PolicyVersion    string
	SessionID        string
	SelectedAlias    string
	SelectedProvider string
	Payload          string
	CreatedAt        time.Time
}

// Validate checks the routing decision record fields.
func (r RoutingDecisionRecord) Validate() error {
	var errs []error
	if r.DecisionID == "" {
		errs = append(errs, errors.New("decision_id must not be empty"))
	}
	if r.PolicyVersion == "" {
		errs = append(errs, errors.New("policy_version must not be empty"))
	}
	if r.SessionID == "" {
		errs = append(errs, errors.New("session_id must not be empty"))
	}
	if r.Payload == "" {
		errs = append(errs, errors.New("payload must not be empty"))
	}
	if r.CreatedAt.IsZero() {
		errs = append(errs, errors.New("created_at must be set"))
	}
	return errors.Join(errs...)
}
