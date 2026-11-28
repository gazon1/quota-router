package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/quota"
	"github.com/gazon1/quota-router/internal/routing"
	"github.com/gazon1/quota-router/internal/store"
)

// StateReader reads quota and health observations the route use case needs.
type StateReader interface {
	LatestQuotaSnapshot(ctx context.Context) ([]store.QuotaSnapshot, error)
	LatestProviderHealth(ctx context.Context) ([]store.HealthRecord, error)
}

// AffinityStore reads and writes session route pins.
type AffinityStore interface {
	SessionAffinity(ctx context.Context, sessionID string) (store.SessionAffinity, bool, error)
	SaveSessionAffinity(ctx context.Context, a store.SessionAffinity) error
	DeleteSessionAffinity(ctx context.Context, sessionID string) error
}

// DecisionWriter persists routing decisions for audit.
type DecisionWriter interface {
	SaveRoutingDecision(ctx context.Context, r store.RoutingDecisionRecord) error
}

// RouteService owns the route use case: loading state, selecting a route,
// auditing the decision, and pinning session affinity. It is constructible with
// interfaces so tests can exercise it without HTTP or SQLite.
type RouteService struct {
	cfg       config.Config
	selector  routing.Selector
	state     StateReader
	affinity  AffinityStore
	decisions DecisionWriter
	sessions  sessionLocks
}

// NewRouteService creates a route use-case service.
func NewRouteService(cfg config.Config, selector routing.Selector, state StateReader, affinity AffinityStore, decisions DecisionWriter) *RouteService {
	return &RouteService{
		cfg:       cfg,
		selector:  selector,
		state:     state,
		affinity:  affinity,
		decisions: decisions,
	}
}

// sessionAffinityTTL is how long a pin outlives its last use. Long enough that a
// working session keeps its model across a pause, short enough that an abandoned
// session stops being pinned without anyone releasing it. It is deliberately not
// configurable yet: nothing has measured a better value.
const sessionAffinityTTL = 24 * time.Hour

// Route performs the whole session-routing operation under the session's lock:
// read state, decide, audit the decision, pin the route. It returns the
// decision and a user-safe error message on failure.
func (s *RouteService) Route(ctx context.Context, req routing.RouteRequest) (routing.Decision, error) {
	unlock := s.sessions.lock(req.SessionID)
	defer unlock()

	state, err := s.loadSelectState(ctx, req.SessionID)
	if err != nil {
		return routing.Decision{}, errors.New("routing state unavailable")
	}
	decision := s.selector.Select(req, state)
	if err := s.persistDecision(ctx, decision, state); err != nil {
		return routing.Decision{}, errors.New("routing decision audit persistence failed")
	}
	// The pin is written after the decision is audited: a decision that cannot be
	// stored is not returned as success, and a route that was never recorded must
	// not persist as the session's. This is a deliberate two-write sequence, not
	// one transaction: the audit log records what was decided, the pin records
	// what will be used, and a failure here leaves a decision that was reported as
	// failed. Every decision with a route writes the pin, including one that just
	// kept it, so the TTL is measured from the last use and an active session does
	// not lose its route while it is still being used.
	if err := s.pinAffinity(ctx, req.SessionID, decision, state.Now); err != nil {
		return routing.Decision{}, errors.New("session affinity could not be stored")
	}
	return decision, nil
}

// Release ends one session's route use: the pin is deleted, so the next decision
// starts from the normal candidate set. Deleting an absent pin is not an error.
func (s *RouteService) Release(ctx context.Context, sessionID string) (store.SessionAffinity, bool, error) {
	if s.affinity == nil {
		return store.SessionAffinity{}, false, errors.New("session affinity store unavailable")
	}
	unlock := s.sessions.lock(sessionID)
	defer unlock()

	pin, ok, err := s.affinity.SessionAffinity(ctx, sessionID)
	if err != nil {
		return store.SessionAffinity{}, false, err
	}
	if err := s.affinity.DeleteSessionAffinity(ctx, sessionID); err != nil {
		return store.SessionAffinity{}, false, err
	}
	return pin, ok, nil
}

// loadSelectState reads what selection needs for one session: quota and health
// observations plus that session's route pin, if it has a live one. An expired
// pin is ignored here as well as in the selector, so the decision explanation
// and the stored state agree on what the session is pinned to. Expired rows are
// left for TTL/storage cleanup rather than deleted on the read path. Performance
// tie-breaks are implemented in the selector but no observation store feeds them
// yet; they stay unused rather than being loaded from nothing.
func (s *RouteService) loadSelectState(ctx context.Context, sessionID string) (routing.SelectState, error) {
	now := time.Now().UTC()
	freshness := staleAfter(s.cfg.Polling.Interval)
	state := routing.SelectState{
		Now:          now,
		Quota:        map[string]quota.Snapshot{},
		Health:       map[string]quota.ProviderState{},
		QuotaMaxAge:  freshness,
		HealthMaxAge: freshness,
	}
	if s.state == nil {
		return routing.SelectState{}, fmt.Errorf("routing state store unavailable")
	}
	snaps, err := s.state.LatestQuotaSnapshot(ctx)
	if err != nil {
		return routing.SelectState{}, err
	}
	for _, snap := range snaps {
		state.Quota[snap.Provider] = quotaSnapshotFromStore(snap, s.cfg.Routing.LowQuotaThreshold)
	}
	health, err := s.state.LatestProviderHealth(ctx)
	if err != nil {
		return routing.SelectState{}, err
	}
	for _, h := range health {
		state.Health[h.Provider] = providerStateFromStore(h)
	}
	if s.affinity == nil {
		return routing.SelectState{}, fmt.Errorf("routing state store unavailable")
	}
	pin, ok, err := s.affinity.SessionAffinity(ctx, sessionID)
	if err != nil {
		return routing.SelectState{}, err
	}
	if ok && pin.ExpiresAt.After(now) {
		state.Affinity = &routing.Affinity{SessionID: pin.SessionID, Alias: pin.Alias, ExpiresAt: pin.ExpiresAt}
	}
	return state, nil
}

// pinAffinity records the route the session is now on. The write is one upsert
// and the store keeps an existing created_at, so an active session shows when its
// pin started while updated_at and the TTL move with each decision. A decision
// with no route pins nothing: there is no route to keep.
func (s *RouteService) pinAffinity(ctx context.Context, sessionID string, d routing.Decision, now time.Time) error {
	if s.affinity == nil {
		return fmt.Errorf("routing state store unavailable")
	}
	if d.SelectedAlias == "" {
		return nil
	}
	return s.affinity.SaveSessionAffinity(ctx, store.SessionAffinity{
		SessionID: sessionID,
		Alias:     d.SelectedAlias,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(sessionAffinityTTL),
	})
}

// persistDecision writes the decision and the candidate set it was made from.
// The store writes both in one transaction, so a decision that could not be
// stored completely is not reported as stored.
func (s *RouteService) persistDecision(ctx context.Context, d routing.Decision, state routing.SelectState) error {
	if s.decisions == nil {
		return fmt.Errorf("decision store unavailable")
	}
	payload, err := json.Marshal(d)
	if err != nil {
		return err
	}
	rec := store.RoutingDecisionRecord{
		DecisionID:         d.DecisionID,
		PolicyVersion:      d.PolicyVersion,
		SessionID:          d.Request.SessionID,
		TaskClass:          string(d.Request.TaskClass),
		RequestedAlias:     requestedAlias(d.Request),
		RequestedProvider:  requestedProvider(d.Request),
		SelectedAlias:      d.SelectedAlias,
		SelectedProvider:   d.SelectedProvider,
		SelectedReasonCode: selectedReasonCode(d),
		SelectedReason:     d.PrimaryReason(),
		Payload:            string(payload),
		CreatedAt:          time.Now().UTC(),
		Candidates:         decisionCandidates(d.Candidates),
	}
	// The pin's expiry is the route lease the decision was made under: it says
	// how long the chosen route stays the session's route.
	if state.Affinity != nil && !state.Affinity.ExpiresAt.IsZero() {
		expires := state.Affinity.ExpiresAt.UTC()
		rec.AffinityExpiresAt = &expires
	}
	return s.decisions.SaveRoutingDecision(ctx, rec)
}

// requestedAlias is the alias the request asked for: an override first, then a
// request preference. Empty when the request named neither.
func requestedAlias(req routing.RouteRequest) string {
	if req.Override != nil && strings.TrimSpace(req.Override.ModelAlias) != "" {
		return strings.TrimSpace(req.Override.ModelAlias)
	}
	if req.Preference != nil {
		return strings.TrimSpace(req.Preference.ModelAlias)
	}
	return ""
}

// requestedProvider is the provider the request asked for, empty when it named
// none.
func requestedProvider(req routing.RouteRequest) string {
	if req.Preference == nil {
		return ""
	}
	return strings.TrimSpace(req.Preference.Provider)
}

// selectedReasonCode is the stable code of the policy result that chose the
// route. It is taken from the selected candidate, so the decision row and the
// candidate row cannot disagree about why the route was chosen.
func selectedReasonCode(d routing.Decision) string {
	for _, c := range d.Candidates {
		if c.Selected {
			return string(c.ReasonCode)
		}
	}
	return ""
}

// decisionCandidates maps the evaluated candidate set onto the stored rows.
func decisionCandidates(cands []routing.CandidateEvaluation) []store.RoutingDecisionCandidate {
	if len(cands) == 0 {
		return nil
	}
	out := make([]store.RoutingDecisionCandidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, store.RoutingDecisionCandidate{
			Ordinal:            c.Ordinal,
			Alias:              c.Alias,
			Provider:           c.Provider,
			Selected:           c.Selected,
			Eligible:           c.Eligible,
			EliminatedStage:    c.EliminatedStage,
			ReasonCode:         string(c.ReasonCode),
			Reason:             c.Reason,
			Transient:          c.Transient,
			QuotaState:         c.QuotaState,
			QuotaUnknownReason: c.QuotaUnknownReason,
			QuotaObservedAt:    c.QuotaObservedAt,
			QuotaResetAt:       c.QuotaResetAt,
			HealthState:        c.HealthState,
			HealthReason:       c.HealthReason,
			HealthObservedAt:   c.HealthObservedAt,
			CooldownUntil:      c.CooldownUntil,
		})
	}
	return out
}

func staleAfter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 90 * time.Second
	}
	return 3 * interval
}

// QuotaSnapshotFromStore converts a stored quota snapshot for routing/status use.
func QuotaSnapshotFromStore(s store.QuotaSnapshot, lowFraction float64) quota.Snapshot {
	return quotaSnapshotFromStore(s, lowFraction)
}

func quotaSnapshotFromStore(s store.QuotaSnapshot, lowFraction float64) quota.Snapshot {
	if s.State == store.QuotaUnknown {
		reason := quotaUnknownReasonFromStore(s.UnknownReason)
		return quota.Failed(s.Provider, s.Source, s.ObservedAt, reason)
	}
	windows := make([]quota.Window, 0, len(s.Windows))
	for _, w := range s.Windows {
		win := quota.Window{Name: w.Window, Limit: w.Limit, Used: w.Used, Remaining: w.Remaining}
		if w.ResetAt != nil {
			win.ResetsAt = *w.ResetAt
		}
		windows = append(windows, win)
	}
	return quota.Observed(s.Provider, s.Source, s.ObservedAt, windows, lowFraction)
}

func quotaUnknownReasonFromStore(r *store.UnknownReason) quota.UnknownReason {
	if r == nil {
		return quota.MalformedResponse
	}
	switch *r {
	case store.ReasonUnsupported:
		return quota.UnsupportedNoQuotaAPI
	case store.ReasonFetchFailed:
		return quota.TransientFetchFailure
	case store.ReasonStale:
		return quota.StaleObservation
	case store.ReasonMalformed:
		return quota.MalformedResponse
	case store.ReasonAuth:
		return quota.AuthFailure
	case store.ReasonNotObserved:
		return quota.NotYetObserved
	default:
		return quota.MalformedResponse
	}
}

// ProviderStateFromStore converts a stored health record for routing/status use.
func ProviderStateFromStore(h store.HealthRecord) quota.ProviderState {
	return providerStateFromStore(h)
}

func providerStateFromStore(h store.HealthRecord) quota.ProviderState {
	state := quota.Health(strings.ToUpper(string(h.State)))
	reason := quota.ReasonNone
	switch strings.ToUpper(h.Error) {
	case string(quota.ReasonAuthFailed):
		reason = quota.ReasonAuthFailed
	case string(quota.ReasonTimeout):
		reason = quota.ReasonTimeout
	case string(quota.ReasonRateLimited):
		reason = quota.ReasonRateLimited
	case string(quota.ReasonServerError):
		reason = quota.ReasonServerError
	case string(quota.ReasonUnreachable):
		reason = quota.ReasonUnreachable
	case string(quota.ReasonNotProbed):
		reason = quota.ReasonNotProbed
	case "":
		reason = quota.ReasonNone
	default:
		reason = quota.ReasonServerError
	}
	st := quota.NewProviderState(h.Provider, state, reason, h.ObservedAt)
	if h.CooldownUntil != nil {
		st.CooldownUntil = *h.CooldownUntil
	}
	return st
}
