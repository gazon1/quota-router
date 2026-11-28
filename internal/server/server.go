package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/quota"
	"github.com/gazon1/quota-router/internal/routing"
	"github.com/gazon1/quota-router/internal/settings"
	"github.com/gazon1/quota-router/internal/store"
)

// DecisionStore persists routing decisions for audit.
type DecisionStore interface {
	SaveRoutingDecision(ctx context.Context, r store.RoutingDecisionRecord) error
	LatestRoutingDecisions(ctx context.Context, limit int) ([]store.RoutingDecisionRecord, error)
}

// StateStore reads the latest quota and provider health observations and the
// session-to-route pins. Affinity lives here rather than behind a second seam
// because it is routing state the selection path reads and writes, like quota.
type StateStore interface {
	LatestQuotaSnapshot(ctx context.Context) ([]store.QuotaSnapshot, error)
	LatestProviderHealth(ctx context.Context) ([]store.HealthRecord, error)
	SessionAffinity(ctx context.Context, sessionID string) (store.SessionAffinity, bool, error)
	SaveSessionAffinity(ctx context.Context, a store.SessionAffinity) error
	DeleteSessionAffinity(ctx context.Context, sessionID string) error
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
	// sessions serializes the read-decide-write of one session's route pin
	// against a release of the same session. Per session, so unrelated sessions
	// do not contend.
	sessions sessionLocks
	// token is the bearer token the API routes require. Empty disables them:
	// the health route stays open, every other route refuses.
	token string
}

// Options carries what the routes read. A nil Usage or an empty Token leaves
// the usage route registered but refusing every report. A nil Store leaves the
// routing, status and session release routes registered but returning 503:
// affinity is routing state, so the store that holds it also ends route use.
type Options struct {
	Usage     UsageSink
	AuthToken string
	Config    config.Config
	Store     *store.Store
}


func settingsCatalogue(cfg config.Config) settings.Catalogue {
	return settings.Catalogue{ModelAliases: cfg.LiteLLM.ModelAliases, Routing: cfg.Routing}
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
		selector: routing.Selector{Catalogue: settingsCatalogue(cfg)},
		usage:    opts.Usage,
		token:    opts.AuthToken,
	}
	if opts.Store != nil {
		s.store = opts.Store
		s.decisions = opts.Store
	}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /v1/status", s.handleStatus)
	s.mux.HandleFunc("POST /v1/route", s.handleSelect)
	s.mux.HandleFunc("POST /v1/routing/select", s.handleSelect)
	s.mux.HandleFunc("GET /v1/routing/decisions", s.handleDecisions)
	s.mux.HandleFunc("POST /v1/usage", s.handleUsage)
	s.mux.HandleFunc("POST /v1/sessions/{id}/release", s.handleRelease)
	// Catch-all answers requests no registered route owns, so the mux's own
	// not-found and method-not-allowed replies flow through the same error
	// envelope as every handler-level error.
	s.mux.HandleFunc("/", s.handleFallback)
	return s
}

// Handler returns the root HTTP handler. It wraps the mux with the per-request
// correlation id so every response can name the request that produced it.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if len(id) > maxRequestID {
			id = newRequestID()
		}
		if id == "" {
			id = newRequestID()
		}
		s.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, id)))
	})
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

// Error codes of the one stable error envelope. Handlers pick the code from the
// condition they detected and the status from the mapping, so a client can branch
// on either without parsing prose.
const (
	errorCodeInvalidRequest   = "invalid_request"
	errorCodeValidation       = "validation_failed"
	errorCodeUnauthorized     = "unauthorized"
	errorCodeNotFound         = "not_found"
	errorCodeConflict         = "conflict"
	errorCodeRateLimited      = "rate_limited"
	errorCodeUnavailable      = "unavailable"
	errorCodeInternal         = "internal_error"
	errorCodePayloadTooLarge  = "payload_too_large"
	errorCodeMethodNotAllowed = "method_not_allowed"
)

// errorDetail is one stable error object: machine-readable code, safe message,
// per-request correlation id and a retryable flag where the condition allows a
// well-behaved retry. No credential, raw provider body or internal detail rides
// along — the message is written by the handler, never echoed from an error that
// could carry provider content.
type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

// errorResponse is the single error shape every controller endpoint returns.
type errorResponse struct {
	Error errorDetail `json:"error"`
}

// writeError writes one stable error response. The status follows the documented
// mapping; the code names the condition and Retryable says whether the caller
// may retry the same request safely.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string, retryable bool) {
	s.writeJSON(w, status, errorResponse{Error: errorDetail{
		Code:      code,
		Message:   msg,
		RequestID: requestID(r.Context()),
		Retryable: retryable,
	}})
}

// The correlation id of a request: the bounded caller-supplied value when a
// trusted proxy forwarded one, otherwise a fresh per-request id.
type requestIDContextKey struct{}

const (
	requestIDHeader = "X-Request-Id"
	maxRequestID    = 128
)

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func requestID(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDContextKey{}).(string); ok {
		return v
	}
	return ""
}

const maxSelectBody = 64 << 10

// maxSessionIDLength bounds a session identifier before it becomes a storage key
// and an error message.
const maxSessionIDLength = 256

// sessionAffinityTTL is how long a pin outlives its last use. Long enough that a
// working session keeps its model across a pause, short enough that an abandoned
// session stops being pinned without anyone releasing it. It is deliberately not
// configurable yet: nothing has measured a better value.
const sessionAffinityTTL = 24 * time.Hour

type selectResponse struct {
	Decision routing.Decision `json:"decision"`
}

func (s *Server) handleSelect(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeError(w, r, http.StatusUnauthorized, errorCodeUnauthorized, "invalid or missing bearer token", false)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSelectBody)
	var req routing.RouteRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if isMaxBytesError(err) {
			s.writeError(w, r, http.StatusRequestEntityTooLarge, errorCodePayloadTooLarge, "routing request body too large", false)
			return
		}
		s.writeError(w, r, http.StatusBadRequest, errorCodeInvalidRequest, "invalid request body: "+err.Error(), false)
		return
	}
	// The decoded value must be the whole body: a second JSON value or trailing
	// content is an invalid request, not content to silently skip.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if isMaxBytesError(err) {
			s.writeError(w, r, http.StatusRequestEntityTooLarge, errorCodePayloadTooLarge, "routing request body too large", false)
			return
		}
		s.writeError(w, r, http.StatusBadRequest, errorCodeInvalidRequest, "invalid request body: unexpected content after JSON object", false)
		return
	}
	if err := req.Validate(); err != nil {
		s.writeError(w, r, http.StatusBadRequest, errorCodeValidation, "invalid routing request: "+err.Error(), false)
		return
	}

	// One session, one route decision at a time. The read of the pin, the decision
	// and the write of the pin are one logical operation: a release that lands
	// between them would be undone by the write, and two concurrent decisions for
	// one session could each pin a different route. The lock is per session, so it
	// costs nothing to unrelated traffic.
	decision, err := s.routeSession(r.Context(), req)
	if err != nil {
		s.log.ErrorContext(r.Context(), "route session", "error", err, "session_id", req.SessionID)
		s.writeError(w, r, http.StatusServiceUnavailable, errorCodeUnavailable, err.Error(), true)
		return
	}

	status := http.StatusOK
	if decision.SelectedAlias == "" {
		status = http.StatusConflict
	}
	s.writeJSON(w, status, selectResponse{Decision: decision})
}

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeError(w, r, http.StatusUnauthorized, errorCodeUnauthorized, "invalid or missing bearer token", false)
		return
	}
	if s.decisions == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, errorCodeUnavailable, "decision store unavailable", true)
		return
	}
	records, err := s.decisions.LatestRoutingDecisions(r.Context(), 50)
	if err != nil {
		s.log.ErrorContext(r.Context(), "list routing decisions", "error", err)
		s.writeError(w, r, http.StatusInternalServerError, errorCodeInternal, "failed to read decisions", false)
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

// Status reads the latest quota and provider health observations. It is the
// read-side companion of the selector: the same freshness bound decides what a
// consumer may act on, so status never disagrees with a routing decision.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeError(w, r, http.StatusUnauthorized, errorCodeUnauthorized, "invalid or missing bearer token", false)
		return
	}
	if s.store == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, errorCodeUnavailable, "status state unavailable", true)
		return
	}
	snaps, err := s.store.LatestQuotaSnapshot(r.Context())
	if err != nil {
		s.log.ErrorContext(r.Context(), "read status quota", "error", err)
		s.writeError(w, r, http.StatusServiceUnavailable, errorCodeUnavailable, "status state unavailable", true)
		return
	}
	healths, err := s.store.LatestProviderHealth(r.Context())
	if err != nil {
		s.log.ErrorContext(r.Context(), "read status health", "error", err)
		s.writeError(w, r, http.StatusServiceUnavailable, errorCodeUnavailable, "status state unavailable", true)
		return
	}
	s.writeJSON(w, http.StatusOK, statusResponse{Providers: s.statusProviders(snaps, healths)})
}

// statusResponse is the GET /v1/status payload: one entry per provider the
// controller knows about, with the quota and health state a consumer may act on.
type statusResponse struct {
	Providers []statusProvider `json:"providers"`
}

// statusProvider is the status of one provider: its subscription quota and its
// health, each omitted when no observation exists yet.
type statusProvider struct {
	Provider string        `json:"provider"`
	Quota    *statusQuota  `json:"quota,omitempty"`
	Health   *statusHealth `json:"health,omitempty"`
}

// statusQuota is the actionable quota state: the derived state plus the reason
// when it is unknown, the source and freshness of the observation, and the
// windows it was derived from.
type statusQuota struct {
	State         string         `json:"state"`
	UnknownReason string         `json:"unknown_reason,omitempty"`
	Source        string         `json:"source"`
	ObservedAt    string         `json:"observed_at"`
	Confidence    string         `json:"confidence"`
	Windows       []statusWindow `json:"windows,omitempty"`
}

// statusWindow is one quota window in the status payload. The field names follow
// the wire contract, not the internal Go names.
type statusWindow struct {
	Window    string   `json:"window"`
	Limit     *float64 `json:"limit,omitempty"`
	Used      *float64 `json:"used,omitempty"`
	Remaining *float64 `json:"remaining,omitempty"`
	ResetAt   string   `json:"reset_at,omitempty"`
}

// statusHealth is the provider reachability state and its closed reason.
type statusHealth struct {
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	CooldownUntil string `json:"cooldown_until,omitempty"`
	ObservedAt    string `json:"observed_at"`
}

// statusProviders builds the status rows. Observed providers come first in the
// map iteration order of the reads; the union with configured catalogue providers
// keeps a freshly started controller from reporting an empty status before its
// first poll.
func (s *Server) statusProviders(snaps []store.QuotaSnapshot, healths []store.HealthRecord) []statusProvider {
	byQuota := make(map[string]store.QuotaSnapshot, len(snaps))
	for _, sn := range snaps {
		byQuota[sn.Provider] = sn
	}
	byHealth := make(map[string]store.HealthRecord, len(healths))
	for _, h := range healths {
		byHealth[h.Provider] = h
	}
	names := make([]string, 0, len(byQuota)+len(byHealth))
	seen := make(map[string]bool, len(byQuota)+len(byHealth))
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		names = append(names, n)
	}
	for n := range byQuota {
		add(n)
	}
	for n := range byHealth {
		add(n)
	}
	for _, m := range s.cfg.LiteLLM.ModelAliases {
		add(m.Provider())
	}
	sort.Strings(names)

	out := make([]statusProvider, 0, len(names))
	now := time.Now().UTC()
	for _, n := range names {
		p := statusProvider{Provider: n}
		if snap, ok := byQuota[n]; ok {
			q := quotaSnapshotFromStore(snap, s.cfg.Routing.LowQuotaThreshold)
			state, reason := q.StateAt(now, staleAfter(s.cfg.Polling.Interval))
			p.Quota = &statusQuota{
				State:         string(state),
				UnknownReason: string(reason),
				Source:        q.Source,
				ObservedAt:    q.ObservedAt.UTC().Format(time.RFC3339Nano),
				Confidence:    string(q.Confidence),
				Windows:       statusWindows(q.Windows),
			}
		}
		if h, ok := byHealth[n]; ok {
			ps := providerStateFromStore(h)
			health := ps.Health
			// Status reads apply the same freshness bound as the selector: an
			// observation that is absent or older than the bound is not
			// actionable, so the provider must not be reported healthy.
			if ps.ObservedAt.IsZero() || now.Sub(ps.ObservedAt) > staleAfter(s.cfg.Polling.Interval) {
				health = quota.HealthUnknown
			}
			cooldown := ""
			if !ps.CooldownUntil.IsZero() {
				cooldown = ps.CooldownUntil.UTC().Format(time.RFC3339Nano)
			}
			p.Health = &statusHealth{
				State:         string(health),
				Reason:        string(ps.Reason()),
				CooldownUntil: cooldown,
				ObservedAt:    ps.ObservedAt.UTC().Format(time.RFC3339Nano),
			}
		}
		out = append(out, p)
	}
	return out
}

func statusWindows(ws []quota.Window) []statusWindow {
	out := make([]statusWindow, 0, len(ws))
	for _, w := range ws {
		sw := statusWindow{Window: w.Name}
		if w.Limit != nil {
			sw.Limit = w.Limit
		}
		if w.Used != nil {
			sw.Used = w.Used
		}
		if w.Remaining != nil {
			sw.Remaining = w.Remaining
		}
		if !w.ResetsAt.IsZero() {
			sw.ResetAt = w.ResetsAt.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, sw)
	}
	return out
}

// Session release ends one agent session's route use: the pin is deleted, so the
// next decision for that session starts from the normal candidate set. Deleting
// an absent pin is not an error, which is what makes a repeat release safe. It
// never touches quota or health state, and it reports whether a pin was actually
// there, because "released" and "nothing to release" are different answers.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeError(w, r, http.StatusUnauthorized, errorCodeUnauthorized, "invalid or missing bearer token", false)
		return
	}
	if s.store == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, errorCodeUnavailable, "session release store unavailable", true)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.writeError(w, r, http.StatusBadRequest, errorCodeValidation, "session_id must not be empty", false)
		return
	}
	if len(id) > maxSessionIDLength {
		s.writeError(w, r, http.StatusBadRequest, errorCodeInvalidRequest, "session_id must be at most 256 bytes", false)
		return
	}
	// The same lock selection takes: without it a decision that read the pin just
	// before this delete could write it back and silently undo the release.
	unlock := s.sessions.lock(id)
	defer unlock()

	pin, released, err := s.deleteAffinity(r.Context(), id)
	if err != nil {
		s.log.ErrorContext(r.Context(), "release session", "error", err, "session_id", id)
		s.writeError(w, r, http.StatusServiceUnavailable, errorCodeUnavailable, "session release could not be stored", true)
		return
	}
	resp := map[string]string{"status": "released"}
	if released {
		// Naming the route that was released is what lets a caller tell which of
		// its sessions actually moved; it is not a credential or a prompt.
		resp["released_alias"] = pin.Alias
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleFallback answers requests that no registered route owns. An unknown
// path is not found; a known path called with the wrong method is a method
// mismatch. Both go through the one error envelope so the documented contract
// holds for every response, including the ones the mux would answer itself.
func (s *Server) handleFallback(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/health" || r.URL.Path == "/v1/status" || r.URL.Path == "/v1/route" || r.URL.Path == "/v1/routing/select" || r.URL.Path == "/v1/routing/decisions" || r.URL.Path == "/v1/usage":
		s.writeError(w, r, http.StatusMethodNotAllowed, errorCodeMethodNotAllowed, "method not allowed", false)
	case isSessionReleasePath(r.URL.Path):
		s.writeError(w, r, http.StatusMethodNotAllowed, errorCodeMethodNotAllowed, "method not allowed", false)
	default:
		s.writeError(w, r, http.StatusNotFound, errorCodeNotFound, "not found", false)
	}
}

// isSessionReleasePath reports whether path names a session release resource
// of the form /v1/sessions/<id>/release.
func isSessionReleasePath(path string) bool {
	const prefix = "/v1/sessions/"
	const suffix = "/release"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	return id != "" && !strings.Contains(id, "/")
}

// isMaxBytesError reports whether err is the size-limit refusal the
// net/http package returns once a body exceeds http.MaxBytesReader.
func isMaxBytesError(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// deleteAffinity removes a session's pin and reports the route that was there, so
// a caller learns which session actually moved. An absent pin is not an error.
func (s *Server) deleteAffinity(ctx context.Context, sessionID string) (store.SessionAffinity, bool, error) {
	pin, ok, err := s.store.SessionAffinity(ctx, sessionID)
	if err != nil {
		return store.SessionAffinity{}, false, err
	}
	if err := s.store.DeleteSessionAffinity(ctx, sessionID); err != nil {
		return store.SessionAffinity{}, false, err
	}
	return pin, ok, nil
}

// routeSession performs the whole session-routing operation under the session's
// lock: read state, decide, audit the decision, pin the route. It returns the
// error the caller should report, so each failure keeps its own message.
func (s *Server) routeSession(ctx context.Context, req routing.RouteRequest) (routing.Decision, error) {
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

// loadSelectState reads what selection needs for one session: quota and health
// observations plus that session's route pin, if it has a live one. An expired
// pin is dropped here as well as in the selector, so the decision explanation
// and the stored state agree on what the session is pinned to. Performance
// tie-breaks are implemented in the selector but no observation store feeds them
// yet; they stay unused rather than being loaded from nothing.
func (s *Server) loadSelectState(ctx context.Context, sessionID string) (routing.SelectState, error) {
	now := time.Now().UTC()
	freshness := staleAfter(s.cfg.Polling.Interval)
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
	pin, ok, err := s.store.SessionAffinity(ctx, sessionID)
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
func (s *Server) pinAffinity(ctx context.Context, sessionID string, d routing.Decision, now time.Time) error {
	if s.store == nil {
		return fmt.Errorf("routing state store unavailable")
	}
	if d.SelectedAlias == "" {
		return nil
	}
	return s.store.SaveSessionAffinity(ctx, store.SessionAffinity{
		SessionID: sessionID,
		Alias:     d.SelectedAlias,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(sessionAffinityTTL),
	})
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
