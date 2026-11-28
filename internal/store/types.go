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
	// OutcomeAmbiguous means the attempt may or may not have been served: a
	// timeout after dispatch, for example. It is neither a success nor a
	// failure, and it never implies that nothing was charged.
	OutcomeAmbiguous Outcome = "AMBIGUOUS"
	// OutcomeUnknown means the reporter did not say. It is not a failure and
	// not a success, and no usage is inferred from it.
	OutcomeUnknown Outcome = "UNKNOWN"
)

func (o Outcome) valid() bool {
	switch o {
	case OutcomeSuccess, OutcomeFailedBeforeFirstToken, OutcomePartialStream, OutcomeCancelled, OutcomeAmbiguous, OutcomeUnknown:
		return true
	}
	return false
}

// TokenProvenance says which channel produced a token count. Every count
// carries the provenance of the observation that produced it: an estimate is
// never promoted to a reported count, and counts from different sources are
// never merged into one number.
type TokenProvenance string

const (
	// TokenProviderReported is the provider's own accounting.
	TokenProviderReported TokenProvenance = "provider_reported"
	// TokenProxyReported is a count the proxy (LiteLLM) reported.
	TokenProxyReported TokenProvenance = "proxy_reported"
	// TokenAgentReported is a count the agent reported.
	TokenAgentReported TokenProvenance = "agent_reported"
	// TokenEstimated is a count this installation derived, for example by
	// counting tokens itself.
	TokenEstimated TokenProvenance = "estimated"
	// TokenReportedLegacy is what rows written before per-count provenance
	// carry. The exact channel was not recorded, so it stays ambiguous.
	TokenReportedLegacy TokenProvenance = "reported"
)

func (p TokenProvenance) valid() bool {
	switch p {
	case TokenProviderReported, TokenProxyReported, TokenAgentReported, TokenEstimated, TokenReportedLegacy:
		return true
	}
	return false
}

// Valid reports whether the provenance is one of the documented channels. The
// wire boundary uses it to reject an unknown channel by name instead of
// storing a value nothing can aggregate on.
func (p TokenProvenance) Valid() bool { return p.valid() }

// ErrorClass is the normalized reason an attempt failed. It is a closed set:
// the class is what an operator aggregates on, never a provider message.
type ErrorClass string

const (
	ErrorTimeout        ErrorClass = "timeout"
	ErrorRateLimited    ErrorClass = "rate_limited"
	ErrorAuth           ErrorClass = "auth"
	ErrorInvalidRequest ErrorClass = "invalid_request"
	ErrorProvider       ErrorClass = "provider_error"
	ErrorNetwork        ErrorClass = "network"
	ErrorUnknown        ErrorClass = "unknown"
)

func (e ErrorClass) valid() bool {
	switch e {
	case ErrorTimeout, ErrorRateLimited, ErrorAuth, ErrorInvalidRequest, ErrorProvider, ErrorNetwork, ErrorUnknown:
		return true
	}
	return false
}

// StreamState is how far the response stream got. It is separate from the
// outcome: a completed non-streaming response and a completed stream are both
// successes, and only the stream state says which one it was.
type StreamState string

const (
	// StreamNotStreamed means the response was not streamed.
	StreamNotStreamed StreamState = "not_streamed"
	// StreamComplete means the stream ran to its end.
	StreamComplete StreamState = "complete"
	// StreamInterrupted means the stream stopped before its end.
	StreamInterrupted StreamState = "interrupted"
)

func (s StreamState) valid() bool {
	switch s {
	case StreamNotStreamed, StreamComplete, StreamInterrupted:
		return true
	}
	return false
}

// RouteSource says where the route an attempt used came from. A manual route
// is the operator's explicit stand-in used when the controller could not
// answer; it never has a routing decision behind it.
type RouteSource string

const (
	// RouteController means the controller selected the route.
	RouteController RouteSource = "controller"
	// RouteManual means the agent used its manual fallback.
	RouteManual RouteSource = "manual"
)

func (r RouteSource) valid() bool {
	switch r {
	case RouteController, RouteManual:
		return true
	}
	return false
}

// UsageRecord is one attempt of one request, as this installation observed it:
// the canonical request/outcome event (issue #23).
//
// It is accounting data, never a substitute for subscription quota: a reported
// token count describes what the provider reported for that request, not how
// much subscription quota it consumed.
//
// Every optional field is absent, not zero, when the reporter did not supply
// it. Nothing here is guessed: an outcome that was not reported is UNKNOWN, a
// latency that was not measured is absent, and a count that was not observed
// has no value and no provenance.
type UsageRecord struct {
	// RequestID deduplicates retried or repeated reports; empty means the
	// source supplied none and duplicates cannot be detected.
	RequestID string
	// Attempt numbers the attempt within the request, starting at 1. A retry is
	// a new attempt, not a second copy of the usage it replaces. Zero means the
	// first attempt.
	Attempt int
	// EventID identifies this event. Empty means the store derives a stable one
	// from (request_id, attempt); it is never a metric label.
	EventID string
	// SchemaVersion versions the event contract. Zero means the current one.
	SchemaVersion int
	Provider      string
	Model         string
	// TaskClass is the bounded class of work the request described, when the
	// reporter named one. The vocabulary belongs to the routing request
	// contract, so an unknown value is rejected before it is stored.
	TaskClass string
	// RouteSource says whether the controller selected the route or the agent
	// used its manual fallback.
	RouteSource RouteSource
	// Nil means the number was not supplied; it is not estimated to zero.
	InputTokens,
	OutputTokens *int
	// InputProvenance and OutputProvenance say which channel produced the
	// matching count. A count without provenance, or a provenance without a
	// count, is not stored: the two travel together.
	InputProvenance,
	OutputProvenance TokenProvenance
	// Source names the channel that reported the row, for provenance.
	Source  string
	Outcome Outcome
	// ErrorClass is the normalized failure reason, when the attempt failed.
	ErrorClass ErrorClass
	// StreamState says whether the response was streamed and how far it got.
	StreamState StreamState
	// TTFTMS and LatencyMS are the measured time to the first token and the
	// total time, in milliseconds. Nil means the boundary did not measure
	// them; it never means zero.
	TTFTMS,
	LatencyMS *int64
	// DecisionID is the routing decision this attempt was routed by, when the
	// reporter knows it. Routing history owns that record; this only references
	// it, and an id that no decision claims is kept as reported.
	DecisionID string
	// ObservedAt is when the source observed the attempt, in UTC.
	ObservedAt time.Time
	// ObservedAtReported records whether the reporter supplied the observation
	// timestamp. A redelivery that omitted it carries server-stamped arrival
	// bookkeeping, not event content, so it must not conflict on that alone.
	ObservedAtReported bool
	// ReceivedAt is when this installation stored the report. The gap to
	// ObservedAt is the report delay; the two clocks are never reconciled.
	ReceivedAt time.Time
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
	if u.Source == "" {
		errs = append(errs, errors.New("source must not be empty"))
	}
	if !u.Outcome.valid() {
		errs = append(errs, fmt.Errorf("outcome %q must be SUCCESS, FAILED_BEFORE_FIRST_TOKEN, PARTIAL_STREAM, CANCELLED, AMBIGUOUS or UNKNOWN", u.Outcome))
	}
	// Negative counts are a broken reporter, and storing one would corrupt a
	// SUM later. Zero is a legitimate reported value.
	if u.InputTokens != nil && *u.InputTokens < 0 {
		errs = append(errs, fmt.Errorf("input_tokens must be >= 0, got %d", *u.InputTokens))
	}
	if u.OutputTokens != nil && *u.OutputTokens < 0 {
		errs = append(errs, fmt.Errorf("output_tokens must be >= 0, got %d", *u.OutputTokens))
	}
	// A count and its provenance travel together: a count with no source is an
	// unsourced number, and a source with no count describes nothing.
	if err := checkProvenance("input", u.InputTokens, u.InputProvenance); err != nil {
		errs = append(errs, err)
	}
	if err := checkProvenance("output", u.OutputTokens, u.OutputProvenance); err != nil {
		errs = append(errs, err)
	}
	if u.ErrorClass != "" && !u.ErrorClass.valid() {
		errs = append(errs, fmt.Errorf("error_class %q is not one of the documented classes", u.ErrorClass))
	}
	if u.StreamState != "" && !u.StreamState.valid() {
		errs = append(errs, fmt.Errorf("stream_state %q must be not_streamed, complete or interrupted", u.StreamState))
	}
	if u.RouteSource != "" && !u.RouteSource.valid() {
		errs = append(errs, fmt.Errorf("route_source %q must be controller or manual", u.RouteSource))
	}
	if u.TTFTMS != nil && *u.TTFTMS < 0 {
		errs = append(errs, fmt.Errorf("ttft_ms must be >= 0, got %d", *u.TTFTMS))
	}
	if u.LatencyMS != nil && *u.LatencyMS < 0 {
		errs = append(errs, fmt.Errorf("latency_ms must be >= 0, got %d", *u.LatencyMS))
	}
	if len(u.TaskClass) > maxKey {
		errs = append(errs, fmt.Errorf("task_class must be at most %d bytes", maxKey))
	}
	if len(u.RequestID) > maxKey || len(u.DecisionID) > maxKey || len(u.Source) > maxKey || len(u.EventID) > maxKey {
		errs = append(errs, fmt.Errorf("request_id, decision_id, source and event_id must be at most %d bytes", maxKey))
	}
	return errors.Join(errs...)
}

// checkProvenance keeps a token count and the channel that produced it
// together: one without the other is either an unsourced number or a claim
// about a count that was never observed.
func checkProvenance(name string, count *int, p TokenProvenance) error {
	switch {
	case count != nil && p == "":
		return fmt.Errorf("%s_tokens must state their provenance", name)
	case count == nil && p != "":
		return fmt.Errorf("%s_provenance must not be set without %s_tokens", name, name)
	case p != "" && !p.valid():
		return fmt.Errorf("%s_provenance %q is not a documented provenance", name, p)
	}
	return nil
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

// RoutingDecisionRecord is one persisted routing decision: the route telemetry
// side of the event contract (issue #23).
type RoutingDecisionRecord struct {
	DecisionID       string
	PolicyVersion    string
	SessionID        string
	SelectedAlias    string
	SelectedProvider string
	// TaskClass is the class of work the decision was made for, when the
	// request named one.
	TaskClass string
	// Explicit says the selected route was an explicit manual override rather
	// than the result of normal selection.
	Explicit  bool
	Payload   string
	CreatedAt time.Time
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
