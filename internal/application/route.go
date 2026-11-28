package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	log       *slog.Logger
	cfg       config.Config
	selector  routing.Selector
	state     StateReader
	affinity  AffinityStore
	decisions DecisionWriter
	sessions  sessionLocks
}

// NewRouteService creates a route use-case service.
func NewRouteService(log *slog.Logger, cfg config.Config, selector routing.Selector, state StateReader, affinity AffinityStore, decisions DecisionWriter) *RouteService {
	if log == nil {
		log = slog.Default()
	}
	return &RouteService{
		log:       log,
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
	if err := s.persistDecision(ctx, decision); err != nil {
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
// pin is dropped here as well as in the selector, so the decision explanation
// and the stored state agree on what the session is pinned to. Performance
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

func (s *RouteService) persistDecision(ctx context.Context, d routing.Decision) error {
	if s.decisions == nil {
		return fmt.Errorf("decision store unavailable")
	}
	payload, err := json.Marshal(d)
	if err != nil {
		return err
	}
	rec := store.RoutingDecisionRecord{
		DecisionID:       d.DecisionID,
		PolicyVersion:    d.PolicyVersion,
		SessionID:        d.Request.SessionID,
		SelectedAlias:    d.SelectedAlias,
		SelectedProvider: d.SelectedProvider,
		Payload:          string(payload),
		CreatedAt:        time.Now().UTC(),
	}
	return s.decisions.SaveRoutingDecision(ctx, rec)
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
	windows := make([]quota.Window, 0, len(s.Windows))
	for _, w := range s.Windows {
		win := quota.Window{Name: w.Window, Limit: w.Limit, Used: w.Used, Remaining: w.Remaining}
		if w.ResetAt != nil {
			win.ResetsAt = *w.ResetAt
		}
		windows = append(windows, win)
	}
	if s.State == store.QuotaUnknown {
		reason := quotaUnknownReasonFromStore(s.UnknownReason)
		snap := quota.Failed(s.Provider, s.Source, s.ObservedAt, reason)
		_ = windows
		return snap
	}
	snap := quota.Observed(s.Provider, s.Source, s.ObservedAt, windows, lowFraction)
	return snap
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
