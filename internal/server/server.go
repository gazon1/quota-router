package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/quota"
	"github.com/gazon1/quota-router/internal/routing"
	"github.com/gazon1/quota-router/internal/store"
)

// DecisionStore persists routing decisions for audit.
type DecisionStore interface {
	SaveRoutingDecision(ctx context.Context, r store.RoutingDecisionRecord) error
	LatestRoutingDecisions(ctx context.Context, limit int) ([]store.RoutingDecisionRecord, error)
}

// StateStore reads the latest quota and provider health observations.
type StateStore interface {
	LatestQuotaSnapshot(ctx context.Context) ([]store.QuotaSnapshot, error)
	LatestProviderHealth(ctx context.Context) ([]store.HealthRecord, error)
}

// Server wires HTTP routes and handlers.
type Server struct {
	log       *slog.Logger
	mux       *http.ServeMux
	cfg       config.Config
	selector  routing.Selector
	store     StateStore
	decisions DecisionStore
	usage     UsageSink
	// token is the bearer token the usage route requires. Empty disables it.
	token string
}

// Options carries what the routes read. A nil Usage or an empty Token leaves
// the usage route registered but refusing every report. A nil Store leaves the
// routing routes registered but returning 503.
type Options struct {
	Usage     UsageSink
	AuthToken string
	Config    config.Config
	Store     *store.Store
}

// New creates a server with registered routes.
func New(log *slog.Logger, opts Options) *Server {
	if log == nil {
		log = slog.Default()
	}
	cfg := opts.Config
	if opts.Config.Polling.Interval == 0 && len(opts.Config.LiteLLM.ModelAliases) == 0 && len(opts.Config.Routing.Providers) == 0 {
		cfg = config.Default()
	}
	s := &Server{
		log:      log,
		mux:      http.NewServeMux(),
		cfg:      cfg,
		selector: routing.Selector{Config: cfg},
		usage:    opts.Usage,
		token:    opts.AuthToken,
	}
	if opts.Store != nil {
		s.store = opts.Store
		s.decisions = opts.Store
	}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /v1/routing/select", s.handleSelect)
	s.mux.HandleFunc("GET /v1/routing/decisions", s.handleDecisions)
	s.mux.HandleFunc("POST /v1/usage", s.handleUsage)
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

type healthResponse struct {
	Status string `json:"status"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Error("encode response", "error", err)
	}
}

func (s *Server) writeJSONError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

type selectResponse struct {
	Decision routing.Decision `json:"decision"`
}

func (s *Server) handleSelect(w http.ResponseWriter, r *http.Request) {
	var req routing.RouteRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	state, err := s.loadSelectState(r.Context())
	if err != nil {
		s.log.ErrorContext(r.Context(), "load routing state", "error", err)
		s.writeJSONError(w, http.StatusServiceUnavailable, "routing state unavailable")
		return
	}

	decision := s.selector.Select(req, state)

	if err := s.persistDecision(r.Context(), decision); err != nil {
		s.log.ErrorContext(r.Context(), "persist routing decision", "error", err, "decision_id", decision.DecisionID)
		s.writeJSONError(w, http.StatusServiceUnavailable, "routing decision audit persistence failed")
		return
	}

	status := http.StatusOK
	if decision.SelectedAlias == "" {
		status = http.StatusConflict
	}
	s.writeJSON(w, status, selectResponse{Decision: decision})
}

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	if s.decisions == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "decision store unavailable")
		return
	}
	records, err := s.decisions.LatestRoutingDecisions(r.Context(), 50)
	if err != nil {
		s.log.ErrorContext(r.Context(), "list routing decisions", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to read decisions")
		return
	}
	type item struct {
		DecisionID       string          `json:"decision_id"`
		PolicyVersion    string          `json:"policy_version"`
		SessionID        string          `json:"session_id"`
		SelectedAlias    string          `json:"selected_alias,omitempty"`
		SelectedProvider string          `json:"selected_provider,omitempty"`
		CreatedAt        time.Time       `json:"created_at"`
		Decision         json.RawMessage `json:"decision"`
	}
	out := make([]item, 0, len(records))
	for _, r := range records {
		out = append(out, item{
			DecisionID:       r.DecisionID,
			PolicyVersion:    r.PolicyVersion,
			SessionID:        r.SessionID,
			SelectedAlias:    r.SelectedAlias,
			SelectedProvider: r.SelectedProvider,
			CreatedAt:        r.CreatedAt,
			Decision:         json.RawMessage(r.Payload),
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"decisions": out})
}

func (s *Server) loadSelectState(ctx context.Context) (routing.SelectState, error) {
	now := time.Now().UTC()
	freshness := staleAfter(s.cfg.Polling.Interval)
	// HTTP integration currently loads quota and health observations only.
	// Session affinity and performance ranking are supported by the selector,
	// but their state providers/persistence are not wired into this endpoint yet;
	// until then, HTTP selection does not apply affinity or performance tie-breaks.
	state := routing.SelectState{
		Now:          now,
		Quota:        map[string]quota.Snapshot{},
		Health:       map[string]quota.ProviderState{},
		QuotaMaxAge:  freshness,
		HealthMaxAge: freshness,
	}
	if s.store == nil {
		return routing.SelectState{}, fmt.Errorf("routing state store unavailable")
	}
	snaps, err := s.store.LatestQuotaSnapshot(ctx)
	if err != nil {
		return routing.SelectState{}, err
	}
	for _, snap := range snaps {
		state.Quota[snap.Provider] = quotaSnapshotFromStore(snap, s.cfg.Routing.LowQuotaThreshold)
	}
	health, err := s.store.LatestProviderHealth(ctx)
	if err != nil {
		return routing.SelectState{}, err
	}
	for _, h := range health {
		state.Health[h.Provider] = providerStateFromStore(h)
	}
	return state, nil
}

func staleAfter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 90 * time.Second
	}
	return 3 * interval
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
		// Keep windows for diagnostics? Failed() carries no windows by design;
		// StateAt will return unknown with the same reason, which is correct.
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
		// Stored error may be a legacy free-form description; keep it out of the
		// typed reason field.
		reason = quota.ReasonServerError
	}
	st := quota.NewProviderState(h.Provider, state, reason, h.ObservedAt)
	if h.CooldownUntil != nil {
		st.CooldownUntil = *h.CooldownUntil
	}
	return st
}

func (s *Server) persistDecision(ctx context.Context, d routing.Decision) error {
	if s.decisions == nil {
		return nil
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
