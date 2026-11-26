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

import "time"

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
type Snapshot struct {
	Provider   string
	Source     string // documented provider quota source, never an inference endpoint
	ObservedAt time.Time
	Windows    []Window
	State      QuotaState
	Unknown    UnknownReason // set exactly when State is QuotaUnknown
	Confidence Confidence
	// Provenance maps a normalized field to the raw provider field it came from,
	// for example "remaining" -> "weekly.remaining_credits".
	Provenance map[string]string
	// Stale marks a last-known-good copy kept for diagnostics after a later
	// observation failed. Consumers must not route on it.
	Stale bool
}

// Observed builds the snapshot of one successful observation and derives State
// from the reported windows, so the recorded state cannot disagree with them.
// lowFraction is the remaining-quota fraction at or below which a window counts
// as scarce; zero or less disables the classification.
func Observed(provider, source string, at time.Time, windows []Window, lowFraction float64) Snapshot {
	state, reason := Derive(windows, lowFraction)
	confidence := ConfidenceHigh
	switch {
	case state == QuotaUnknown:
		confidence = ConfidenceNone
	case partial(windows):
		confidence = ConfidenceLow
	}
	return Snapshot{
		Provider:   provider,
		Source:     source,
		ObservedAt: at,
		Windows:    windows,
		State:      state,
		Unknown:    reason,
		Confidence: confidence,
	}
}

// Failed builds the snapshot of an attempt that produced no usable quota. It
// carries no windows, so nothing downstream can read zero remaining out of it.
func Failed(provider, source string, at time.Time, reason UnknownReason) Snapshot {
	return Snapshot{
		Provider:   provider,
		Source:     source,
		ObservedAt: at,
		State:      QuotaUnknown,
		Unknown:    reason,
		Confidence: ConfidenceNone,
	}
}

// Derive computes the state of a set of windows. The worst window wins, so one
// exhausted window outweighs an available one, and a window whose quota cannot
// be established makes the snapshot UNKNOWN rather than Available.
func Derive(windows []Window, lowFraction float64) (QuotaState, UnknownReason) {
	if len(windows) == 0 {
		return QuotaUnknown, MalformedResponse
	}
	state, reason := Available, UnknownReason("")
	for _, w := range windows {
		s, r := w.state(lowFraction)
		if rank(s) > rank(state) {
			state = s
		}
		if s == QuotaUnknown && reason == "" {
			reason = r
		}
	}
	if state == QuotaUnknown {
		return state, reason
	}
	return state, ""
}

// state derives one window. A reported remaining of zero or less is exhausted
// even when the limit is absent, because that is the safe reading; a positive
// remaining without a limit cannot be classified and stays unknown.
func (w Window) state(lowFraction float64) (QuotaState, UnknownReason) {
	switch {
	case w.Remaining == nil:
		return QuotaUnknown, MalformedResponse
	case *w.Remaining <= 0:
		return Exhausted, ""
	case w.Limit == nil || *w.Limit <= 0:
		return QuotaUnknown, MalformedResponse
	case lowFraction > 0 && *w.Remaining <= *w.Limit*lowFraction:
		return LowQuota, ""
	}
	return Available, ""
}

// partial reports a usable window that is missing a reported-when-available
// field, which lowers confidence without changing the state.
func partial(windows []Window) bool {
	for _, w := range windows {
		if w.Used == nil || w.ResetsAt.IsZero() {
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

// StateAt reports the state a consumer may act on at now. A snapshot dated in
// the future beyond clockSkewTolerance, older than maxAge (zero disables the
// check), marked stale, or never observed is UNKNOWN, whatever State says.
func (s Snapshot) StateAt(now time.Time, maxAge time.Duration) (QuotaState, UnknownReason) {
	switch {
	case s.ObservedAt.IsZero():
		return QuotaUnknown, NotYetObserved
	case s.Stale, untrusted(s.ObservedAt, now, maxAge):
		return QuotaUnknown, StaleObservation
	case s.State == QuotaUnknown || s.State == "":
		if s.Unknown == "" {
			return QuotaUnknown, MalformedResponse
		}
		return QuotaUnknown, s.Unknown
	}
	return s.State, ""
}

// untrusted reports an observation whose timestamp cannot be believed: dated in
// the future, or older than maxAge. Either way this is a clock or freshness
// problem, not fresh quota.
func untrusted(observedAt, now time.Time, maxAge time.Duration) bool {
	if observedAt.After(now.Add(clockSkewTolerance)) {
		return true
	}
	return maxAge > 0 && now.Sub(observedAt) > maxAge
}
