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
	// OutcomeTimeout means the attempt exceeded its deadline. The error class
	// says whether the deadline was the caller's or the provider's.
	OutcomeTimeout Outcome = "TIMEOUT"
	// OutcomeUnknown means the reporter did not say. It is not a failure and
	// not a success, and no usage is inferred from it.
	OutcomeUnknown Outcome = "UNKNOWN"
)

func (o Outcome) valid() bool {
	switch o {
	case OutcomeSuccess, OutcomeFailedBeforeFirstToken, OutcomePartialStream, OutcomeCancelled, OutcomeTimeout, OutcomeUnknown:
		return true
	}
	return false
}

// ErrorClass is the normalized class of a failed attempt. It is a closed set
// for the same reason the health reasons are: a raw provider error can carry a
// credential in a URL or a header, so only the class is stored and the text
// stays in the reporter's own logs.
//
// The vocabulary matches the provider-health reasons, so a consumer reads one
// set of codes across the two domains.
type ErrorClass string

const (
	// ErrorClassTimeout means the attempt exceeded its deadline.
	ErrorClassTimeout ErrorClass = "TIMEOUT"
	// ErrorClassRateLimited means the provider returned a rate or usage limit.
	ErrorClassRateLimited ErrorClass = "RATE_LIMITED"
	// ErrorClassAuthentication means the provider rejected authentication or
	// permission.
	ErrorClassAuthentication ErrorClass = "AUTHENTICATION_OR_PERMISSION_FAILURE"
	// ErrorClassProviderError means the provider answered 5xx or an unusable body.
	ErrorClassProviderError ErrorClass = "PROVIDER_ERROR"
	// ErrorClassUnreachable means the connection failed or was refused.
	ErrorClassUnreachable ErrorClass = "UNREACHABLE"
	// ErrorClassClientError means the request itself was rejected.
	ErrorClassClientError ErrorClass = "CLIENT_ERROR"
	// ErrorClassUnknown means the failure could not be classified.
	ErrorClassUnknown ErrorClass = "UNKNOWN"
)

func (e ErrorClass) valid() bool {
	switch e {
	case ErrorClassTimeout, ErrorClassRateLimited, ErrorClassAuthentication,
		ErrorClassProviderError, ErrorClassUnreachable, ErrorClassClientError, ErrorClassUnknown:
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
	// LatencyMS is how long the attempt took in milliseconds. Nil means
	// nobody measured it; it is never estimated to zero.
	LatencyMS *int
	// ErrorClass is the normalized class of a failed attempt. Empty means the
	// attempt did not fail or nobody classified it. Never raw error text.
	ErrorClass ErrorClass
	ObservedAt time.Time
	// ObservedAtReported records whether the reporter supplied the observation
	// timestamp. A redelivery that omitted it carries server-stamped arrival
	// bookkeeping, not event content, so it must not conflict on that alone.
	ObservedAtReported bool
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
		errs = append(errs, fmt.Errorf("outcome %q must be SUCCESS, FAILED_BEFORE_FIRST_TOKEN, PARTIAL_STREAM, CANCELLED, TIMEOUT or UNKNOWN", u.Outcome))
	}
	// Negative counts are a broken reporter, and storing one would corrupt a
	// SUM later. Zero is a legitimate reported value.
	if u.InputTokens != nil && *u.InputTokens < 0 {
		errs = append(errs, fmt.Errorf("input_tokens must be >= 0, got %d", *u.InputTokens))
	}
	if u.OutputTokens != nil && *u.OutputTokens < 0 {
		errs = append(errs, fmt.Errorf("output_tokens must be >= 0, got %d", *u.OutputTokens))
	}
	if u.LatencyMS != nil && *u.LatencyMS < 0 {
		errs = append(errs, fmt.Errorf("latency_ms must be >= 0, got %d", *u.LatencyMS))
	}
	// An error class outside the declared set is refused rather than stored: it
	// is the one place a raw provider message could enter the database.
	if u.ErrorClass != "" && !u.ErrorClass.valid() {
		errs = append(errs, fmt.Errorf("error_class %q is not one of the documented classes", u.ErrorClass))
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
	DecisionID    string
	PolicyVersion string
	SessionID     string
	// TaskClass is the agent-supplied task class, empty when the request
	// carried none.
	TaskClass string
	// RequestedAlias is the alias the request asked for — an override or a
	// preference — and RequestedProvider the provider it named. Both are empty
	// when the request named neither.
	RequestedAlias    string
	RequestedProvider string
	SelectedAlias     string
	SelectedProvider  string
	// SelectedReasonCode is the stable code of the policy result that chose
	// the route; SelectedReason is the prose beside it.
	SelectedReasonCode string
	SelectedReason     string
	// AffinityExpiresAt is when the session's route pin expires, when the
	// decision was made under a pin. Nil when the session was not pinned.
	AffinityExpiresAt *time.Time
	// Payload is the decision JSON, the immutable snapshot of the decision.
	Payload string
	// Candidates is the evaluated candidate set, in evaluation order. It is
	// written in the same transaction as the decision: a decision without its
	// candidates is not valid history.
	Candidates []RoutingDecisionCandidate
	CreatedAt  time.Time
}

// RoutingDecisionCandidate is one evaluated candidate of a decision: what it
// was, whether it was eligible and whether it was chosen, the stable reason
// code of that outcome, and the quota/health values the hard checks read.
//
// The values are copies of what the decision acted on, not references to
// observation rows: a pruned observation must not make a decision unreadable.
// A value that was not observed stays empty.
type RoutingDecisionCandidate struct {
	// Ordinal is the evaluation order, 1-based.
	Ordinal int
	Alias   string
	// Provider is the provider the alias belongs to.
	Provider string
	// Selected reports whether the decision chose this candidate.
	Selected bool
	// Eligible reports whether the candidate satisfied every hard constraint.
	Eligible bool
	// EliminatedStage is the stage that eliminated the candidate, empty when
	// it was not eliminated.
	EliminatedStage string
	// ReasonCode is the stable code of the decision about this candidate.
	ReasonCode string
	// Reason is the prose beside the code. It is a short description, never a
	// raw provider message.
	Reason string
	// Transient marks an elimination that says nothing about the route itself.
	Transient bool
	// The quota values the decision acted on.
	QuotaState         string
	QuotaUnknownReason string
	QuotaObservedAt    *time.Time
	QuotaResetAt       *time.Time
	// The health values the decision acted on.
	HealthState      string
	HealthReason     string
	HealthObservedAt *time.Time
	CooldownUntil    *time.Time
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
	if len(r.DecisionID) > maxKey || len(r.SessionID) > maxKey {
		errs = append(errs, fmt.Errorf("decision_id and session_id must be at most %d bytes", maxKey))
	}
	for i, c := range r.Candidates {
		if err := c.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("candidate %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

// Validate checks one candidate row. The uniqueness of the ordinal and of the
// alias inside one decision is a storage invariant, not a field check: the
// database enforces it, so an injected duplicate fails the write and rolls the
// decision back.
func (c RoutingDecisionCandidate) Validate() error {
	var errs []error
	if c.Ordinal <= 0 {
		errs = append(errs, fmt.Errorf("ordinal must be >= 1, got %d", c.Ordinal))
	}
	if c.Alias == "" {
		errs = append(errs, errors.New("alias must not be empty"))
	}
	if c.Provider == "" {
		errs = append(errs, errors.New("provider must not be empty"))
	}
	if c.ReasonCode == "" {
		errs = append(errs, errors.New("reason_code must not be empty"))
	}
	if len(c.Alias) > maxKey || len(c.Provider) > maxKey || len(c.ReasonCode) > maxKey {
		errs = append(errs, fmt.Errorf("alias, provider and reason_code must be at most %d bytes", maxKey))
	}
	// The reason is written into a column an operator reads and a backup
	// carries, so it must be a short description like every other error text.
	if err := checkSafeError(c.Reason); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// SessionAffinity is the route one agent session is currently pinned to. It is
// routing state, not history: the row stops being affinity at ExpiresAt, and a
// reader must not pin a session past it on its own authority.
//
// CreatedAt survives a re-pin so an operator can see when the pin started;
// UpdatedAt is what a re-pin moves. Both are set by the store, never by the
// caller, so a restart cannot rewind them.
type SessionAffinity struct {
	SessionID string
	Alias     string
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt time.Time
}

// Validate checks the affinity row fields. A pin without a future expiry is not
// affinity: an unbounded pin would outlive the route it names.
func (a SessionAffinity) Validate() error {
	var errs []error
	if a.SessionID == "" {
		errs = append(errs, errors.New("session_id must not be empty"))
	}
	if a.Alias == "" {
		errs = append(errs, errors.New("alias must not be empty"))
	}
	if a.CreatedAt.IsZero() {
		errs = append(errs, errors.New("created_at must be set"))
	}
	if a.UpdatedAt.IsZero() {
		errs = append(errs, errors.New("updated_at must be set"))
	}
	if a.ExpiresAt.IsZero() {
		errs = append(errs, errors.New("expires_at must be set"))
	}
	if !a.ExpiresAt.After(a.UpdatedAt) {
		errs = append(errs, fmt.Errorf("expires_at %s must be after updated_at %s", a.ExpiresAt, a.UpdatedAt))
	}
	if a.ExpiresAt.Before(a.CreatedAt) {
		errs = append(errs, fmt.Errorf("expires_at %s must not precede created_at %s", a.ExpiresAt, a.CreatedAt))
	}
	return errors.Join(errs...)
}
