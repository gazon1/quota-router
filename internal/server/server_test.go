package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/routing"
	"github.com/gazon1/quota-router/internal/store"
)

func TestHealth(t *testing.T) {
	srv := New(nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"status":"ok"}` {
		t.Fatalf("body = %q, want %q", got, `{"status":"ok"}`)
	}
}

func TestHealthMethodNotAllowed(t *testing.T) {
	srv := New(nil, Options{})
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestUnknownRouteReturnsErrorEnvelope(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken})
	req := httptest.NewRequest(http.MethodGet, "/v1/not-a-route", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != errorCodeNotFound {
		t.Fatalf("code = %q, want %q", body.Error.Code, errorCodeNotFound)
	}
	if body.Error.Retryable {
		t.Fatal("retryable = true, want false for a not-found condition")
	}
}

func TestWrongMethodReturnsMethodNotAllowed(t *testing.T) {
	cfg := testRoutingConfig()
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	req := httptest.NewRequest(http.MethodPost, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != errorCodeMethodNotAllowed {
		t.Fatalf("code = %q, want %q", body.Error.Code, errorCodeMethodNotAllowed)
	}
}

type fakeStore struct {
	mu          sync.Mutex
	snaps       []store.QuotaSnapshot
	health      []store.HealthRecord
	decisions   []store.RoutingDecisionRecord
	saveErr     error
	affinity    map[string]store.SessionAffinity
	affinityErr error
	// lastAttempt is the per-provider freshness anchor the status and selection
	// reads compare the persisted observation against.
	lastAttempt map[string]time.Time
	// pollErr fails the batch cycle read the status and selection paths use.
	pollErr error
}

func (f *fakeStore) SessionAffinity(_ context.Context, sessionID string) (store.SessionAffinity, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.affinityErr != nil {
		return store.SessionAffinity{}, false, f.affinityErr
	}
	a, ok := f.affinity[sessionID]
	return a, ok, nil
}

func (f *fakeStore) SaveSessionAffinity(_ context.Context, a store.SessionAffinity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.affinityErr != nil {
		return f.affinityErr
	}
	if f.affinity == nil {
		f.affinity = map[string]store.SessionAffinity{}
	}
	f.affinity[a.SessionID] = a
	return nil
}

func (f *fakeStore) DeleteSessionAffinity(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.affinityErr != nil {
		return f.affinityErr
	}
	delete(f.affinity, sessionID)
	return nil
}

// PollCycles maps the fake store's snapshot, health and last-attempt rows onto
// the batch cycle read the status and selection paths use. A provider without a
// snapshot row is absent from the map, like an absent poll cycle: nothing may be
// fresh without one.
func (f *fakeStore) PollCycles(_ context.Context) (map[string]store.PollCycle, error) {
	if f.pollErr != nil {
		return nil, f.pollErr
	}
	out := map[string]store.PollCycle{}
	for _, sn := range f.snaps {
		c := out[sn.Provider]
		c.Snapshot = sn
		c.AttemptAt = f.lastAttempt[sn.Provider]
		out[sn.Provider] = c
	}
	for _, h := range f.health {
		c := out[h.Provider]
		c.Health = h
		out[h.Provider] = c
	}
	return out, nil
}
func (f *fakeStore) SaveRoutingDecision(_ context.Context, r store.RoutingDecisionRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.decisions = append(f.decisions, r)
	return nil
}
func (f *fakeStore) LatestRoutingDecisions(_ context.Context, limit int) ([]store.RoutingDecisionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit <= 0 || limit > len(f.decisions) {
		limit = len(f.decisions)
	}
	out := make([]store.RoutingDecisionRecord, 0, limit)
	for i := len(f.decisions) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, f.decisions[i])
	}
	return out, nil
}

func testRoutingConfig() config.Config {
	cfg := config.Default()
	on := true
	cfg.LiteLLM.ModelAliases = map[string]config.Model{
		"cc-coding": {Model: "commandcode/glyph:free", Enabled: &on, Capabilities: config.Capabilities{Streaming: config.CapabilitySupported, ToolCalling: config.CapabilitySupported}, Tasks: []string{"coding"}},
		"mm-fast":   {Model: "minimax/MiniMax-M2.7", Enabled: &on, Capabilities: config.Capabilities{Streaming: config.CapabilitySupported}},
	}
	cfg.Routing.Providers = map[string]config.ProviderRouting{
		"commandcode": {Priority: 1, Models: map[string]int{"cc-coding": 1}},
		"minimax":     {Priority: 2, Models: map[string]int{"mm-fast": 1}},
	}
	return cfg
}

func quotaSnap(provider string, state store.QuotaState, rem float64) store.QuotaSnapshot {
	limit := 100.0
	used := limit - rem
	reset := time.Now().Add(time.Hour).UTC()
	return store.QuotaSnapshot{Provider: provider, State: state, Source: "test", Confidence: store.Reported, ObservedAt: time.Now().UTC(), Windows: []store.QuotaWindow{{Window: "5h", Limit: &limit, Used: &used, Remaining: &rem, ResetAt: &reset}}}
}

func healthRec(provider string, state store.HealthState) store.HealthRecord {
	return store.HealthRecord{Provider: provider, State: state, ObservedAt: time.Now().UTC()}
}

const (
	testRoutingToken   = "secret-token"
	sessionAffinityTTL = 24 * time.Hour
)

func postSelect(t *testing.T, srv *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/routing/select", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func postRoute(t *testing.T, srv *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/route", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func postRelease(t *testing.T, srv *Server, token, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+id+"/release", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestSelectEndpoint(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		lastAttempt: map[string]time.Time{
			"commandcode": time.Now().UTC(),
			"minimax":     time.Now().UTC(),
		},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	body := `{"version":1,"session_id":"s1","capabilities":{"tool_calling":true},"task_class":"coding"}`
	w := postSelect(t, srv, testRoutingToken, body)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Decision routing.Decision `json:"decision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Decision.SelectedAlias != "cc-coding" {
		t.Fatalf("selected=%q", resp.Decision.SelectedAlias)
	}
	if resp.Decision.DecisionID == "" || resp.Decision.PolicyVersion != routing.PolicyVersion {
		t.Fatalf("decision metadata missing: %+v", resp.Decision)
	}
	if len(st.decisions) != 1 {
		t.Fatalf("persisted decisions=%d", len(st.decisions))
	}
	if st.decisions[0].DecisionID != resp.Decision.DecisionID {
		t.Fatalf("persisted id=%q want %q", st.decisions[0].DecisionID, resp.Decision.DecisionID)
	}
}

func TestSelectEndpointNoEligible(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaExhausted, 0), quotaSnap("minimax", store.QuotaExhausted, 0)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	body := `{"version":1,"session_id":"s1","capabilities":{}}`
	w := postSelect(t, srv, testRoutingToken, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Decision routing.Decision `json:"decision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Decision.SelectedAlias != "" {
		t.Fatalf("selected=%q", resp.Decision.SelectedAlias)
	}
	if len(resp.Decision.Rejected) == 0 {
		t.Fatal("expected rejections")
	}
}

// A quota row persisted before polling stopped or the process crashed must not
// be routed on: the freshness bound is anchored to the last poll attempt, and a
// stale row is rejected before a candidate can be selected.
func TestSelectEndpointDoesNotRouteOnStalePollStateQuota(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:       []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80)},
		health:      []store.HealthRecord{healthRec("commandcode", store.Healthy)},
		lastAttempt: map[string]time.Time{"commandcode": time.Now().UTC().Add(-2 * time.Hour)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	body := `{"version":1,"session_id":"s1","capabilities":{"tool_calling":true},"task_class":"coding"}`
	w := postSelect(t, srv, testRoutingToken, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409 when a persisted poll is stale", w.Code, w.Body.String())
	}
	var resp struct {
		Decision routing.Decision `json:"decision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Decision.SelectedAlias != "" {
		t.Fatalf("selected=%q, want none for a stale quota row", resp.Decision.SelectedAlias)
	}
	if len(resp.Decision.Rejected) == 0 {
		t.Fatal("expected the stale quota row to be rejected")
	}
}

func TestSelectEndpointInvalidVersion(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})
	w := postSelect(t, srv, testRoutingToken, `{"version":2,"session_id":"s1","capabilities":{}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != errorCodeValidation {
		t.Fatalf("code = %q, want %q", body.Error.Code, errorCodeValidation)
	}
	if !strings.Contains(body.Error.Message, "version must be 1") {
		t.Fatalf("message = %q, want it to name the version error", body.Error.Message)
	}
	if body.Error.RequestID == "" {
		t.Fatal("error response must carry the request id")
	}
	if body.Error.Retryable {
		t.Fatal("retryable = true, want false for a validation failure")
	}
}

func TestSelectEndpointPersistenceFailure(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:   []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health:  []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		saveErr: context.DeadlineExceeded,
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	body := `{"version":1,"session_id":"s1","capabilities":{}}`
	w := postSelect(t, srv, testRoutingToken, body)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDecisionsEndpoint(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		lastAttempt: map[string]time.Time{
			"commandcode": time.Now().UTC(),
			"minimax":     time.Now().UTC(),
		},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	body := `{"version":1,"session_id":"s1","capabilities":{}}`
	w := postSelect(t, srv, testRoutingToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("select status=%d body=%s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/routing/decisions", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req)
	if w2.Code != http.StatusOK {
		t.Fatalf("decisions status=%d body=%s", w2.Code, w2.Body.String())
	}
	var resp struct {
		Decisions []map[string]any `json:"decisions"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Decisions) != 1 {
		t.Fatalf("decisions=%d", len(resp.Decisions))
	}
}

func TestSelectEndpointStateLoadError(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{pollErr: context.DeadlineExceeded}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"s1","capabilities":{}}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// A failed read must not leave a decision behind: nothing was decided, so
	// nothing may be persisted as the session's successful choice.
	if len(st.decisions) != 0 {
		t.Fatalf("decisions=%d, want none when the selection state cannot be read", len(st.decisions))
	}
}

func TestSelectEndpointInvalidJSON(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken})
	w := postSelect(t, srv, testRoutingToken, `{`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestSelectEndpointRejectsUnknownField(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken})
	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"s1","capabilities":{},"unknown":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestSelectEndpointRequiresAuth(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy)},
		lastAttempt: map[string]time.Time{
			"commandcode": time.Now().UTC(),
		},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})
	body := `{"version":1,"session_id":"s1","capabilities":{}}`

	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"missing token", "", http.StatusUnauthorized},
		{"wrong token", "wrong", http.StatusUnauthorized},
		{"valid token", testRoutingToken, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postSelect(t, srv, tc.token, body)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDecisionsEndpointRequiresAuth(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"missing token", "", http.StatusUnauthorized},
		{"wrong token", "wrong", http.StatusUnauthorized},
		{"valid token", testRoutingToken, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/routing/decisions", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSelectEndpointBodyLimit(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken, Store: &fakeStore{}, Decisions: &fakeStore{}})
	huge := `{"version":1,"session_id":"s1","capabilities":{},"task_class":"` + strings.Repeat("x", maxSelectBody) + `"}`
	w := postSelect(t, srv, testRoutingToken, huge)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != errorCodePayloadTooLarge {
		t.Fatalf("code = %q, want %q", body.Error.Code, errorCodePayloadTooLarge)
	}
	if body.Error.Retryable {
		t.Fatal("retryable = true, want false for a payload size failure")
	}
}

// The size bound covers the whole body, including content appended after a
// valid JSON object: the limit refuses the request rather than silently
// decoding only the first value.
func TestSelectEndpointBodyLimitAfterValidJSON(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken, Store: &fakeStore{}, Decisions: &fakeStore{}})
	body := `{"version":1,"session_id":"s1","capabilities":{}}` + `{"pad":"` + strings.Repeat("x", maxSelectBody) + `"}`
	w := postSelect(t, srv, testRoutingToken, body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestSelectEndpointRejectsTrailingJSON(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken})
	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"s1","capabilities":{}} {"unexpected":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != errorCodeInvalidRequest {
		t.Fatalf("code = %q, want %q", body.Error.Code, errorCodeInvalidRequest)
	}
}

// The status endpoint reports the actionable quota and health state per provider
// through the same freshness bound the selector uses.
func TestStatusEndpoint(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("minimax", store.Healthy)},
		lastAttempt: map[string]time.Time{
			"minimax": time.Now().UTC(),
		},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st})

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Providers) == 0 {
		t.Fatal("status returned no providers")
	}
	var found *statusProvider
	for i := range resp.Providers {
		if resp.Providers[i].Provider == "minimax" {
			found = &resp.Providers[i]
		}
	}
	if found == nil {
		t.Fatalf("providers = %+v", resp.Providers)
	}
	if found.Quota == nil {
		t.Fatal("quota state missing")
	}
	if found.Quota.State != "AVAILABLE" {
		t.Fatalf("quota state = %q, want AVAILABLE", found.Quota.State)
	}
	if found.Health == nil {
		t.Fatal("health state missing")
	}
	if found.Health.State != "HEALTHY" {
		t.Fatalf("health state = %q, want HEALTHY", found.Health.State)
	}
}

// A persisted quota row from before a process crash or a polling stop reads as
// stale at the status boundary: the freshness bound is anchored to the last poll
// attempt, not only to the observation it left behind.
func TestStatusEndpointStaleQuotaAfterPollingStopped(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:       []store.QuotaSnapshot{quotaSnap("minimax", store.QuotaAvailable, 80)},
		health:      []store.HealthRecord{healthRec("minimax", store.Healthy)},
		lastAttempt: map[string]time.Time{"minimax": time.Now().UTC().Add(-2 * time.Hour)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st})

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found *statusProvider
	for i := range resp.Providers {
		if resp.Providers[i].Provider == "minimax" {
			found = &resp.Providers[i]
		}
	}
	if found == nil || found.Quota == nil {
		t.Fatalf("providers=%+v quota=%+v", resp.Providers, found)
	}
	if found.Quota.State != "UNKNOWN" {
		t.Fatalf("quota state = %q, want UNKNOWN for a stale post-stop row", found.Quota.State)
	}
	if found.Quota.UnknownReason != "STALE_OBSERVATION" {
		t.Fatalf("unknown reason = %q, want STALE_OBSERVATION", found.Quota.UnknownReason)
	}
	if got := len(found.Quota.Windows); got != 1 {
		t.Fatalf("windows = %d, want the stale last-known window retained for diagnostics", got)
	}
}

// A persisted quota row whose poll state is missing is treated like a stale
// one: nothing durably anchored the observation, so it must not be routed on
// and status must not read it as current quota.
func TestSelectEndpointDoesNotRouteOnMissingPollState(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	body := `{"version":1,"session_id":"s1","capabilities":{"tool_calling":true},"task_class":"coding"}`
	w := postSelect(t, srv, testRoutingToken, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409 when poll state is missing for a persisted quota row", w.Code, w.Body.String())
	}
	var resp struct {
		Decision routing.Decision `json:"decision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Decision.SelectedAlias != "" {
		t.Fatalf("selected=%q, want none when poll state is missing", resp.Decision.SelectedAlias)
	}
	if len(resp.Decision.Rejected) == 0 {
		t.Fatal("expected the missing poll state to reject the persisted quota row")
	}
}

// The status endpoint must not read a persisted quota row as current when no
// poll ever anchored it: the row is stale, with its last-known window kept for
// diagnostics.
func TestStatusEndpointStaleQuotaWhenPollStateMissing(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("minimax", store.Healthy)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st})

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found *statusProvider
	for i := range resp.Providers {
		if resp.Providers[i].Provider == "minimax" {
			found = &resp.Providers[i]
		}
	}
	if found == nil || found.Quota == nil {
		t.Fatalf("providers=%+v quota=%+v", resp.Providers, found)
	}
	if found.Quota.State != "UNKNOWN" {
		t.Fatalf("quota state = %q, want UNKNOWN for a row without poll state", found.Quota.State)
	}
	if found.Quota.UnknownReason != "STALE_OBSERVATION" {
		t.Fatalf("unknown reason = %q, want STALE_OBSERVATION", found.Quota.UnknownReason)
	}
	if got := len(found.Quota.Windows); got != 1 {
		t.Fatalf("windows = %d, want the last-known window retained for diagnostics", got)
	}
}

// The status endpoint applies the same freshness bound as the selector: a
// provider whose health observation is older than the bound must not be
// reported healthy.
func TestStatusEndpointStaleHealthNotHealthy(t *testing.T) {
	cfg := testRoutingConfig()
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: &fakeStore{
		snaps: []store.QuotaSnapshot{quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{{
			Provider:   "minimax",
			State:      store.Healthy,
			ObservedAt: time.Now().UTC().Add(-2 * time.Hour),
		}},
	}})

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found *statusProvider
	for i := range resp.Providers {
		if resp.Providers[i].Provider == "minimax" {
			found = &resp.Providers[i]
		}
	}
	if found == nil || found.Health == nil {
		t.Fatalf("providers = %+v", resp.Providers)
	}
	if found.Health.State == "HEALTHY" {
		t.Fatalf("stale health reported as HEALTHY, want a non-healthy actionable state, got %q", found.Health.State)
	}
}

// The freshness policy is applied in two steps: the read marks the row stale
// when the poll anchor is missing or past the bound, and StateAt then derives the
// actionable state from the observation itself, honouring an observation that is
// older than the bound. Status and the selector must agree on every combination
// of anchor and observation age, so the pair is pinned here.
func TestFreshnessBoundCombinesAnchorAndObservation(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig()})
	now := time.Now().UTC()
	bound := staleAfter(srv.cfg.Polling.Interval)
	limit := 100.0
	used := 20.0
	remaining := 80.0
	reset := now.Add(time.Hour)
	snap := store.QuotaSnapshot{
		Provider:   "minimax",
		State:      store.QuotaAvailable,
		Source:     "test",
		Confidence: store.Reported,
		ObservedAt: now,
		Windows: []store.QuotaWindow{{
			Window: "5h", Limit: &limit, Used: &used, Remaining: &remaining, ResetAt: &reset,
		}},
	}
	snapWith := func(observedAt time.Time) store.QuotaSnapshot {
		snap.ObservedAt = observedAt
		return snap
	}

	cases := []struct {
		name       string
		anchor     time.Time
		observedAt time.Time
		wantStale  bool
		wantState  store.QuotaState
		wantReason string
	}{
		{"fresh anchor and fresh observation", now, now, false, store.QuotaAvailable, ""},
		{"stale anchor marks the row stale", now.Add(-2 * time.Hour), now.Add(-2 * time.Hour), true, store.QuotaUnknown, "STALE_OBSERVATION"},
		{"missing anchor marks the row stale", time.Time{}, now, true, store.QuotaUnknown, "STALE_OBSERVATION"},
		{"stale observation is stale even with a fresh anchor", now, now.Add(-2 * time.Hour), false, store.QuotaUnknown, "STALE_OBSERVATION"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := srv.quotaAt(snapWith(tc.observedAt), tc.anchor, now, bound)
			state, reason := q.StateAt(now, bound)
			if q.Stale != tc.wantStale {
				t.Fatalf("quotaAt stale = %t, want %t", q.Stale, tc.wantStale)
			}
			if got := string(state); got != string(tc.wantState) {
				t.Fatalf("state = %s, want %s", got, tc.wantState)
			}
			if got := string(reason); got != tc.wantReason {
				t.Fatalf("unknown reason = %q, want %q", got, tc.wantReason)
			}
		})
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken})
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != errorCodeUnauthorized {
		t.Fatalf("code = %q, want %q", body.Error.Code, errorCodeUnauthorized)
	}
}

func TestStatusUnavailableWithoutStore(t *testing.T) {
	srv := New(nil, Options{AuthToken: testRoutingToken})
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStatusUnavailableOnStoreError(t *testing.T) {
	cfg := testRoutingConfig()
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: &fakeStore{pollErr: context.DeadlineExceeded}})
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// The issue names the route-decision endpoint /v1/route; the repository also
// serves the contract name /v1/routing/select, so both must behave identically.
func TestRouteAliasEndpoint(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("minimax", store.Healthy)},
		lastAttempt: map[string]time.Time{
			"minimax": time.Now().UTC(),
		},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	w := postRoute(t, srv, testRoutingToken, `{"version":1,"session_id":"s1","capabilities":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Decision routing.Decision `json:"decision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Decision.DecisionID == "" {
		t.Fatal("decision id missing")
	}
}

// Session release ends a session's route use and must be safely repeatable: the
// pin is deleted, and releasing an unpinned session succeeds too.
func TestReleaseEndpoint(t *testing.T) {
	st := &fakeStore{affinity: map[string]store.SessionAffinity{
		"sess-1": pin("sess-1", "cc-coding", time.Now().UTC()),
	}}
	srv := New(nil, Options{AuthToken: testToken, Store: st})

	w := postRelease(t, srv, testToken, "sess-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	type releaseBody struct {
		Status        string `json:"status"`
		ReleasedAlias string `json:"released_alias"`
	}
	var body releaseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "released" {
		t.Fatalf("status = %q, want released", body.Status)
	}
	if body.ReleasedAlias != "cc-coding" {
		t.Fatalf("released_alias = %q, want the route that was let go", body.ReleasedAlias)
	}
	if _, ok := st.affinity["sess-1"]; ok {
		t.Fatal("pin survived release")
	}

	// A repeat release has nothing left to delete and must still succeed.
	w = postRelease(t, srv, testToken, "sess-1")
	if w.Code != http.StatusOK {
		t.Fatalf("repeat release status=%d body=%s", w.Code, w.Body.String())
	}
	var repeat releaseBody
	if err := json.Unmarshal(w.Body.Bytes(), &repeat); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if repeat.ReleasedAlias != "" {
		t.Fatalf("released_alias = %q on a repeat release, want empty", repeat.ReleasedAlias)
	}
}

func TestReleaseEndpointValidation(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken, Store: &fakeStore{}})

	if w := postRelease(t, srv, testToken, strings.Repeat("s", maxSessionIDLength+1)); w.Code != http.StatusBadRequest {
		t.Fatalf("long session id status=%d body=%s", w.Code, w.Body.String())
	}
	if w := postRelease(t, srv, "", "sess-1"); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated release status=%d body=%s", w.Code, w.Body.String())
	}
	if w := postRelease(t, srv, testToken, "sess-1"); w.Code != http.StatusOK {
		t.Fatalf("release status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestReleaseEndpointUnavailableWithoutStore(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken})
	if w := postRelease(t, srv, testToken, "sess-1"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("release status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestReleaseEndpointUnavailableOnStoreError(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken, Store: &fakeStore{affinityErr: context.DeadlineExceeded}})
	if w := postRelease(t, srv, testToken, "sess-1"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("release status=%d body=%s", w.Code, w.Body.String())
	}
}

// A released session must not keep its retained affinity: release deletes the
// pin, so the next selection starts from the full candidate set.
func TestSelectEndpointHonorsSessionRelease(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:    []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health:   []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		affinity: map[string]store.SessionAffinity{"sess-1": pin("sess-1", "mm-fast", time.Now().UTC())},
		lastAttempt: map[string]time.Time{
			"commandcode": time.Now().UTC(),
			"minimax":     time.Now().UTC(),
		},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	// Pinned to mm-fast, which the configured priority would not pick.
	if got := selectAlias(t, srv, "sess-1"); got != "mm-fast" {
		t.Fatalf("pinned alias = %q, want mm-fast", got)
	}

	if w := postRelease(t, srv, testRoutingToken, "sess-1"); w.Code != http.StatusOK {
		t.Fatalf("release status=%d body=%s", w.Code, w.Body.String())
	}
	if got := selectAlias(t, srv, "sess-1"); got != "cc-coding" {
		t.Fatalf("alias after release = %q, want the normally selected cc-coding", got)
	}
}

// pin builds a stored affinity row the way the selection path writes one.
func pin(sessionID, alias string, now time.Time) store.SessionAffinity {
	return store.SessionAffinity{SessionID: sessionID, Alias: alias, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(sessionAffinityTTL)}
}

// selectAlias runs one route decision and returns the selected alias.
func selectAlias(t *testing.T, srv *Server, sessionID string) string {
	t.Helper()
	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"`+sessionID+`","capabilities":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("select status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Decision routing.Decision `json:"decision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Decision.SelectedAlias
}

func affinityServer(t *testing.T, st *fakeStore) *Server {
	t.Helper()
	cfg := testRoutingConfig()
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})
	return srv
}

// The first decision pins the session; every later one keeps that route even
// when the configured priority would pick another, which is the whole point of
// affinity.
func TestSelectPinsAndKeepsTheRoute(t *testing.T) {
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		lastAttempt: map[string]time.Time{
			"commandcode": time.Now().UTC(),
			"minimax":     time.Now().UTC(),
		},
	}
	srv := affinityServer(t, st)

	if got := selectAlias(t, srv, "sess-1"); got != "cc-coding" {
		t.Fatalf("first selection = %q, want the ranked cc-coding", got)
	}
	stored, ok := st.affinity["sess-1"]
	if !ok {
		t.Fatal("first selection did not persist a pin")
	}
	if stored.Alias != "cc-coding" {
		t.Fatalf("stored alias = %q, want cc-coding", stored.Alias)
	}
	if !stored.ExpiresAt.After(stored.UpdatedAt) {
		t.Fatalf("stored pin expires %s, not after its update %s", stored.ExpiresAt, stored.UpdatedAt)
	}

	// The pinned route's quota observation goes stale. That says nothing about the
	// route, so the session keeps it; an unpinned session, seeing the same state,
	// is routed to the other provider.
	stale := quotaSnap("commandcode", store.QuotaAvailable, 80)
	stale.ObservedAt = time.Now().UTC().Add(-time.Hour)
	st.snaps = []store.QuotaSnapshot{stale, quotaSnap("minimax", store.QuotaAvailable, 80)}
	if got := selectAlias(t, srv, "sess-1"); got != "cc-coding" {
		t.Fatalf("selection through a transient exclusion = %q, want the held cc-coding", got)
	}
	if got := selectAlias(t, srv, "sess-2"); got != "mm-fast" {
		t.Fatalf("unpinned session = %q, want mm-fast", got)
	}

	// Exhausted quota is durable: the route is really gone, so the session moves.
	st.snaps = []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaExhausted, 0), quotaSnap("minimax", store.QuotaAvailable, 80)}
	if got := selectAlias(t, srv, "sess-1"); got != "mm-fast" {
		t.Fatalf("selection after the pinned route was exhausted = %q, want mm-fast", got)
	}
	if got := st.affinity["sess-1"].Alias; got != "mm-fast" {
		t.Fatalf("stored alias after the move = %q, want mm-fast", got)
	}
}

// A stored pin that has expired must not decide anything: the session is
// re-evaluated, and the new route becomes its pin.
func TestSelectExpiredPinReevaluatesAndRepins(t *testing.T) {
	now := time.Now().UTC()
	st := &fakeStore{
		snaps:    []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health:   []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		affinity: map[string]store.SessionAffinity{"sess-1": pin("sess-1", "mm-fast", now.Add(-2*sessionAffinityTTL))},
		lastAttempt: map[string]time.Time{
			"commandcode": time.Now().UTC(),
			"minimax":     time.Now().UTC(),
		},
	}
	srv := affinityServer(t, st)
	// pin() computed its expiry from the old now, so it is already past.
	if st.affinity["sess-1"].ExpiresAt.After(now) {
		t.Fatal("fixture pin is not expired")
	}

	if got := selectAlias(t, srv, "sess-1"); got != "cc-coding" {
		t.Fatalf("expired pin selected %q, want a fresh cc-coding", got)
	}
	if got := st.affinity["sess-1"].Alias; got != "cc-coding" {
		t.Fatalf("stored alias after re-pin = %q, want cc-coding", got)
	}
	if !st.affinity["sess-1"].ExpiresAt.After(now) {
		t.Fatal("re-pin did not renew the expiry")
	}
}

// A manual override is the caller asking for another route, so it must re-pin
// instead of being refused by the pin it is replacing.
func TestSelectOverrideRepinsSession(t *testing.T) {
	now := time.Now().UTC()
	st := &fakeStore{
		snaps:    []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health:   []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		affinity: map[string]store.SessionAffinity{"sess-1": pin("sess-1", "cc-coding", now)},
		lastAttempt: map[string]time.Time{
			"commandcode": time.Now().UTC(),
			"minimax":     time.Now().UTC(),
		},
	}
	srv := affinityServer(t, st)

	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"sess-1","capabilities":{},"override":{"model_alias":"mm-fast"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("override status=%d body=%s", w.Code, w.Body.String())
	}
	if got := st.affinity["sess-1"].Alias; got != "mm-fast" {
		t.Fatalf("stored alias after override = %q, want mm-fast", got)
	}
	// And the next plain request stays on the overridden route.
	if got := selectAlias(t, srv, "sess-1"); got != "mm-fast" {
		t.Fatalf("selection after override = %q, want mm-fast", got)
	}
}

// An affinity write that fails must not let the caller believe the route stuck.
func TestSelectFailsWhenAffinityCannotBeStored(t *testing.T) {
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy)},
	}
	srv := affinityServer(t, st)
	st.affinityErr = context.DeadlineExceeded

	if w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"sess-1","capabilities":{}}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503 when the pin cannot be stored", w.Code, w.Body.String())
	}
}

// A decision with no eligible route pins nothing: there is no route to keep.
func TestSelectWithoutCandidatePinsNothing(t *testing.T) {
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaExhausted, 0)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy)},
	}
	srv := affinityServer(t, st)
	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"sess-1","capabilities":{}}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if len(st.affinity) != 0 {
		t.Fatalf("affinity = %+v, want none pinned without a route", st.affinity)
	}
}

var _ = bytes.NewBuffer

// A configured unavailability window reaches the HTTP boundary like any other
// hard constraint: every route inside one is the existing 409, and an override
// naming one is rejected rather than honored.
func TestSelectHonorsUnavailableWindow(t *testing.T) {
	window := func() *config.Unavailable {
		// The store stamps observations with time.Now(), so this test cannot pass
		// an instant the way the selector tests do. Two windows cover the whole
		// day: end is exclusive, so 00:00-23:59 alone leaves 23:59:00-23:59:59
		// eligible and the assertion would fail for one minute a day.
		return &config.Unavailable{
			Timezone: "UTC",
			Windows: []config.UnavailableWindow{
				{Days: []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}, Start: "00:00", End: "23:59"},
				{Days: []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}, Start: "23:59", End: "00:00"},
			},
		}
	}
	cfg := testRoutingConfig()
	for alias, m := range cfg.LiteLLM.ModelAliases {
		m.Unavailable = window()
		cfg.LiteLLM.ModelAliases[alias] = m
	}
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"sess-1","capabilities":{}}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409 while every route is in its window", w.Code, w.Body.String())
	}
	var body struct {
		Decision routing.Decision `json:"decision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r, ok := findRejectedAlias(body.Decision, "cc-coding"); !ok || r.Stage != "model_availability" {
		t.Fatalf("rejection = %+v ok=%v, want a model_availability explanation", r, ok)
	}

	// The override names an otherwise valid alias and must still be refused.
	w = postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"sess-1","capabilities":{},"override":{"model_alias":"mm-fast"}}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("override status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Decision.OverrideApplied {
		t.Fatal("override was applied to a model inside its unavailable window")
	}
	if len(st.affinity) != 0 {
		t.Fatalf("affinity = %+v, want nothing pinned without a route", st.affinity)
	}
}

func findRejectedAlias(d routing.Decision, alias string) (routing.RejectedCandidate, bool) {
	for _, r := range d.Rejected {
		if r.Alias == alias {
			return r, true
		}
	}
	return routing.RejectedCandidate{}, false
}

func TestConcurrentRouteAndReleaseHTTPHandlers(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})
	h := srv.Handler()

	const (
		sameSession = "sess-same"
		workers     = 20
	)
	var wg sync.WaitGroup
	errCh := make(chan string, workers*4)

	doSelect := func(sessionID string) {
		defer wg.Done()
		body := `{"version":1,"session_id":"` + sessionID + `","capabilities":{}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/routing/select", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testRoutingToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK && w.Code != http.StatusConflict {
			errCh <- fmt.Sprintf("select %s status=%d body=%s", sessionID, w.Code, w.Body.String())
		}
	}
	doRelease := func(sessionID string) {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/release", nil)
		req.Header.Set("Authorization", "Bearer "+testRoutingToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			errCh <- fmt.Sprintf("release %s status=%d body=%s", sessionID, w.Code, w.Body.String())
		}
	}

	for i := 0; i < workers; i++ {
		i := i
		wg.Add(3)
		go doSelect(fmt.Sprintf("sess-%d", i))
		go doSelect(sameSession)
		go doRelease(sameSession)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Error(msg)
	}

	// Release guarantee: release is idempotent and leaves no pin.
	w := postRelease(t, srv, testRoutingToken, sameSession)
	if w.Code != http.StatusOK {
		t.Fatalf("final release status=%d body=%s", w.Code, w.Body.String())
	}
	st.mu.Lock()
	_, ok := st.affinity[sameSession]
	st.mu.Unlock()
	if ok {
		// Not a failure of the concurrent flood itself; a concurrent select may
		// have re-pinned after the final release completed. Verify release is
		// still authoritative when serialized alone.
		w = postRelease(t, srv, testRoutingToken, sameSession)
		if w.Code != http.StatusOK {
			t.Fatalf("second final release status=%d body=%s", w.Code, w.Body.String())
		}
		st.mu.Lock()
		_, ok = st.affinity[sameSession]
		st.mu.Unlock()
		if ok {
			t.Fatal("pin survived serialized release after concurrent flood")
		}
	}
}
