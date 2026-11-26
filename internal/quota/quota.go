// Package quota defines the provider-neutral domain model of subscription
// quota, local usage and provider health.
//
// The three signals are separate types on purpose and nothing converts one into
// another: subscription quota comes only from a documented provider quota
// source, local usage is what this installation observed of its own requests,
// and provider health describes reachability. A successful request, a healthy
// provider, a billing number or locally counted tokens never imply remaining
// subscription quota.
//
// An unusable observation is QuotaUnknown with a reason, never zero remaining
// and never Available. Values are in the provider's own unit and are only
// comparable inside one provider and one window.
package quota

import (
	"math"
	"time"
)

// clockSkewTolerance is how far an observation may be dated in the future
// before its timestamp is treated as untrustworthy rather than as fresh quota.
const clockSkewTolerance = time.Minute

// Window is one subscription quota window as the provider reports it, for
// example a rolling 5-hour window and a weekly window. A nil field was not
// reported; it must not be read as zero.
type Window struct {
	Name      string    // provider window name, for example "5h" or "weekly"
	Limit     *float64  // quota granted for the window
	Used      *float64  // quota consumed in the window
	Remaining *float64  // quota left in the window, as reported
	ResetsAt  time.Time // UTC reset of the window; zero when not reported
}

// QuotaState is the subscription quota state derived from reported windows.
type QuotaState string

const (
	// Available means every window reports remaining quota above the low fraction.
	Available QuotaState = "AVAILABLE"
	// LowQuota means at least one window is at or below the low fraction.
	LowQuota QuotaState = "LOW_QUOTA"
	// Exhausted means at least one window reports no remaining quota.
	Exhausted QuotaState = "EXHAUSTED"
	// QuotaUnknown means quota cannot be established; Unknown says why.
	QuotaUnknown QuotaState = "UNKNOWN"
)

// UnknownReason names why subscription quota is QuotaUnknown. It is the only
// detail that leaves the adapter: raw provider errors can carry credentials.
type UnknownReason string

const (
	// UnsupportedNoQuotaAPI means the provider documents no permitted quota source.
	UnsupportedNoQuotaAPI UnknownReason = "UNSUPPORTED_NO_DOCUMENTED_QUOTA_API"
	// TransientFetchFailure means this attempt failed: timeout, 5xx, network.
	TransientFetchFailure UnknownReason = "TRANSIENT_FETCH_FAILURE"
	// StaleObservation means the observation is too old, retained after a failed
	// refresh, or dated in the future beyond clockSkewTolerance.
	StaleObservation UnknownReason = "STALE_OBSERVATION"
	// MalformedResponse means fields are missing, inconsistent or unparseable.
	MalformedResponse UnknownReason = "MALFORMED_RESPONSE"
	// AuthFailure means authentication or permission was rejected.
	AuthFailure UnknownReason = "AUTHENTICATION_OR_PERMISSION_FAILURE"
	// NotYetObserved means no fetch has completed yet.
	NotYetObserved UnknownReason = "NOT_YET_OBSERVED"
)

// Confidence is how much of a snapshot came from documented provider fields.
type Confidence string

const (
	ConfidenceHigh Confidence = "HIGH" // complete documented response
	ConfidenceLow  Confidence = "LOW"  // usable, but some fields were absent
	ConfidenceNone Confidence = "NONE" // nothing usable was parsed
)

// Snapshot is one observation of subscription quota for one provider.
//
// Windows is the only source of truth for the quota state: State derives from
// it on every read, so a snapshot cannot claim a state its windows do not
// support. The remaining fields are set by Observed and Failed, never by a
// caller, which is why they are unexported.
type Snapshot struct {
	Provider   string
	Source     string // documented provider quota source, never an inference endpoint
	ObservedAt time.Time
	Windows    []Window
	Confidence Confidence
	// Provenance maps a normalized field to the raw provider field it came from,
	// for example "remaining" -> "weekly.remaining_credits".
	Provenance map[string]string
	// Stale marks a last-known-good copy kept for diagnostics after a later
	// observation failed. Consumers must not route on it.
	Stale bool

	lowFraction float64       // threshold State derives with
	unknown     UnknownReason // why this snapshot carries no quota at all
}

// State derives the quota state from the windows this snapshot carries.
func (s Snapshot) State() (QuotaState, UnknownReason) {
	if s.unknown != "" {
		return QuotaUnknown, s.unknown
	}
	return Derive(s.Windows, s.lowFraction)
}

// Observed builds the snapshot of one successful observation. It records only
// what the provider reported; State derives from those windows on every read,
// so the two cannot disagree. lowFraction is the remaining-quota fraction at or
// below which a window counts as scarce; zero or less disables the
// classification.
func Observed(provider, source string, at time.Time, windows []Window, lowFraction float64) Snapshot {
	windows = cloneWindows(windows)
	confidence := ConfidenceHigh
	switch state, _ := Derive(windows, lowFraction); {
	case state == QuotaUnknown:
		confidence = ConfidenceNone
	case partial(windows):
		confidence = ConfidenceLow
	}
	return Snapshot{
		Provider:    provider,
		Source:      source,
		ObservedAt:  at,
		Windows:     windows,
		Confidence:  confidence,
		lowFraction: lowFraction,
	}
}

// cloneWindows owns the values the caller passed, so mutating its windows or
// their numbers afterwards cannot leave a snapshot whose State disagrees with
// its Windows. Reset times become UTC, which the model promises.
func cloneWindows(windows []Window) []Window {
	out := make([]Window, len(windows))
	for i, w := range windows {
		out[i] = Window{Name: w.Name, Limit: cloneFloat(w.Limit), Used: cloneFloat(w.Used), Remaining: cloneFloat(w.Remaining)}
		if !w.ResetsAt.IsZero() {
			out[i].ResetsAt = w.ResetsAt.UTC()
		}
	}
	return out
}

func cloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// Failed builds the snapshot of an attempt that produced no usable quota. It
// carries no windows, so nothing downstream can read zero remaining out of it.
func Failed(provider, source string, at time.Time, reason UnknownReason) Snapshot {
	return Snapshot{
		Provider:   provider,
		Source:     source,
		ObservedAt: at,
		Confidence: ConfidenceNone,
		unknown:    reason,
	}
}

// Derive computes the state of a set of windows. A window whose quota cannot be
// established makes the snapshot UNKNOWN even when another window is exhausted,
// because the requirements say an unparseable value must never become a quota
// claim. Among established windows the worst wins, so one exhausted window
// outweighs a low or available one.
func Derive(windows []Window, lowFraction float64) (QuotaState, UnknownReason) {
	if len(windows) == 0 {
		return QuotaUnknown, MalformedResponse
	}
	want := Available
	for _, w := range windows {
		s, r := w.state(lowFraction)
		if s == QuotaUnknown {
			// Any unusable window makes the whole snapshot unestablished: the
			// issue requires missing/unparseable quota to become UNKNOWN, and a
			// consumer cannot tell a fully known snapshot from a partial one.
			return QuotaUnknown, r
		}
		if rank(s) > rank(want) {
			want = s
		}
	}
	return want, ""
}

// state derives one window. A reported remaining of zero or less is exhausted
// even when the limit is absent, because that is the safe reading; a positive
// remaining without a limit cannot be classified and stays unknown.
func (w Window) state(lowFraction float64) (QuotaState, UnknownReason) {
	switch {
	case w.Remaining == nil:
		return QuotaUnknown, MalformedResponse
	case !finite(*w.Remaining):
		// NaN compares false against everything, so without this check an
		// unparseable number falls through to Available.
		return QuotaUnknown, MalformedResponse
	case *w.Remaining <= 0:
		return Exhausted, ""
	case w.Limit == nil || *w.Limit <= 0 || !finite(*w.Limit):
		return QuotaUnknown, MalformedResponse
	case lowFraction > 0 && *w.Remaining <= *w.Limit*lowFraction:
		return LowQuota, ""
	}
	return Available, ""
}

// finite rejects NaN and infinities, which a provider response can carry via an
// unparseable or out-of-range numeric field.
func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// partial reports a usable window that is missing a reported-when-available
// field, which lowers confidence without changing the state. A non-finite Used
// counts as missing: it cannot be reported as complete accounting. Used never
// establishes quota, so a broken one lowers confidence instead of the state.
func partial(windows []Window) bool {
	for _, w := range windows {
		if w.Used == nil || !finite(*w.Used) || w.ResetsAt.IsZero() {
			return true
		}
	}
	return false
}

func rank(s QuotaState) int {
	switch s {
	case Exhausted:
		return 3
	case LowQuota:
		return 2
	case QuotaUnknown:
		return 1
	default:
		return 0
	}
}

// StateAt reports the state a consumer may act on at now. The state always
// comes from the windows this snapshot carries, never from a stored copy, so a
// snapshot cannot claim more than its windows show. A snapshot dated in the
// future beyond clockSkewTolerance, older than maxAge, marked stale, or never
// observed is UNKNOWN. A non-positive maxAge is UNKNOWN too: a freshness bound
// that nobody set must not accept a day-old observation as available quota.
func (s Snapshot) StateAt(now time.Time, maxAge time.Duration) (QuotaState, UnknownReason) {
	switch {
	case maxAge <= 0:
		return QuotaUnknown, StaleObservation
	case s.unknown != "":
		// An explicit failure keeps its reason even when it was recorded with a
		// zero timestamp: NOT_YET_OBSERVED would hide what actually went wrong.
		return QuotaUnknown, s.unknown
	case s.ObservedAt.IsZero():
		return QuotaUnknown, NotYetObserved
	case s.Stale, untrusted(s.ObservedAt, now, maxAge):
		return QuotaUnknown, StaleObservation
	}
	return s.State()
}

// untrusted reports an observation whose timestamp cannot be believed: dated in
// the future, or older than maxAge. Either way this is a clock or freshness
// problem, not fresh quota.
func untrusted(observedAt, now time.Time, maxAge time.Duration) bool {
	return observedAt.After(now.Add(clockSkewTolerance)) || now.Sub(observedAt) > maxAge
}
