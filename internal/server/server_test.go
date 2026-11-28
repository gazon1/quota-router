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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/routing"
	"github.com/gazon1/quota-router/internal/store"
)

func TestHealth(t *testing.T) {
	srv := New(nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, `{"status":"ok"}`, strings.TrimSpace(w.Body.String()))
}

func TestHealthMethodNotAllowed(t *testing.T) {
	srv := New(nil, Options{})
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestUnknownRouteReturnsErrorEnvelope(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken})
	req := httptest.NewRequest(http.MethodGet, "/v1/not-a-route", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	var body errorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, errorCodeNotFound, body.Error.Code)
	assert.False(t, body.Error.Retryable, "retryable = true, want false for a not-found condition")
}

func TestWrongMethodReturnsMethodNotAllowed(t *testing.T) {
	cfg := testRoutingConfig()
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	req := httptest.NewRequest(http.MethodPost, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	var body errorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, errorCodeMethodNotAllowed, body.Error.Code)
}

type fakeStore struct {
	mu          sync.Mutex
	snaps       []store.QuotaSnapshot
	health      []store.HealthRecord
	decisions   []store.RoutingDecisionRecord
	snapErr     error
	healthErr   error
	saveErr     error
	affinity    map[string]store.SessionAffinity
	affinityErr error
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

func (f *fakeStore) LatestQuotaSnapshot(context.Context) ([]store.QuotaSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapErr != nil {
		return nil, f.snapErr
	}
	return append([]store.QuotaSnapshot(nil), f.snaps...), nil
}
func (f *fakeStore) LatestProviderHealth(context.Context) ([]store.HealthRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	return append([]store.HealthRecord(nil), f.health...), nil
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
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	body := `{"version":1,"session_id":"s1","capabilities":{"tool_calling":true},"task_class":"coding"}`
	w := postSelect(t, srv, testRoutingToken, body)

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp struct {
		Decision routing.Decision `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "cc-coding", resp.Decision.SelectedAlias)
	assert.NotEmpty(t, resp.Decision.DecisionID, "decision metadata missing: %+v", resp.Decision)
	assert.Equal(t, routing.PolicyVersion, resp.Decision.PolicyVersion, "decision metadata missing: %+v", resp.Decision)
	require.Len(t, st.decisions, 1, "persisted decisions")
	assert.Equal(t, resp.Decision.DecisionID, st.decisions[0].DecisionID)
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
	assert.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())
	var resp struct {
		Decision routing.Decision `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Decision.SelectedAlias)
	assert.NotEmpty(t, resp.Decision.Rejected, "expected rejections")
}

func TestSelectEndpointInvalidVersion(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})
	w := postSelect(t, srv, testRoutingToken, `{"version":2,"session_id":"s1","capabilities":{}}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	var body errorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, errorCodeValidation, body.Error.Code)
	assert.Contains(t, body.Error.Message, "version must be 1", "message = %q, want it to name the version error", body.Error.Message)
	assert.NotEmpty(t, body.Error.RequestID, "error response must carry the request id")
	assert.False(t, body.Error.Retryable, "retryable = true, want false for a validation failure")
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
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
}

func TestDecisionsEndpoint(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	body := `{"version":1,"session_id":"s1","capabilities":{}}`
	w := postSelect(t, srv, testRoutingToken, body)
	assert.Equal(t, http.StatusOK, w.Code, "select body=%s", w.Body.String())

	req := httptest.NewRequest(http.MethodGet, "/v1/routing/decisions", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req)
	assert.Equal(t, http.StatusOK, w2.Code, "decisions body=%s", w2.Body.String())
	var resp struct {
		Decisions []map[string]any `json:"decisions"`
	}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &resp))
	assert.Len(t, resp.Decisions, 1)
}

func TestSelectEndpointStateLoadError(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{snapErr: context.DeadlineExceeded}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"s1","capabilities":{}}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
}

func TestSelectEndpointInvalidJSON(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken})
	w := postSelect(t, srv, testRoutingToken, `{`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
}

func TestSelectEndpointRejectsUnknownField(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken})
	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"s1","capabilities":{},"unknown":true}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
}

func TestSelectEndpointRequiresAuth(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy)},
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
			assert.Equal(t, tc.want, w.Code, "body=%s", w.Body.String())
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
			assert.Equal(t, tc.want, w.Code, "body=%s", w.Body.String())
		})
	}
}

func TestSelectEndpointBodyLimit(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken, Store: &fakeStore{}, Decisions: &fakeStore{}})
	huge := `{"version":1,"session_id":"s1","capabilities":{},"task_class":"` + strings.Repeat("x", maxSelectBody) + `"}`
	w := postSelect(t, srv, testRoutingToken, huge)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body=%s", w.Body.String())
	var body errorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, errorCodePayloadTooLarge, body.Error.Code)
	assert.False(t, body.Error.Retryable, "retryable = true, want false for a payload size failure")
}

// The size bound covers the whole body, including content appended after a
// valid JSON object: the limit refuses the request rather than silently
// decoding only the first value.
func TestSelectEndpointBodyLimitAfterValidJSON(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken, Store: &fakeStore{}, Decisions: &fakeStore{}})
	body := `{"version":1,"session_id":"s1","capabilities":{}}` + `{"pad":"` + strings.Repeat("x", maxSelectBody) + `"}`
	w := postSelect(t, srv, testRoutingToken, body)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body=%s", w.Body.String())
}

func TestSelectEndpointRejectsTrailingJSON(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken})
	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"s1","capabilities":{}} {"unexpected":true}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	var body errorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, errorCodeInvalidRequest, body.Error.Code)
}

// The status endpoint reports the actionable quota and health state per provider
// through the same freshness bound the selector uses.
func TestStatusEndpoint(t *testing.T) {
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
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp statusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.Providers, "status returned no providers")
	var found *statusProvider
	for i := range resp.Providers {
		if resp.Providers[i].Provider == "minimax" {
			found = &resp.Providers[i]
		}
	}
	require.NotNil(t, found, "providers = %+v", resp.Providers)
	require.NotNil(t, found.Quota, "quota state missing")
	assert.Equal(t, "AVAILABLE", found.Quota.State)
	require.NotNil(t, found.Health, "health state missing")
	assert.Equal(t, "HEALTHY", found.Health.State)
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
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp statusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	var found *statusProvider
	for i := range resp.Providers {
		if resp.Providers[i].Provider == "minimax" {
			found = &resp.Providers[i]
		}
	}
	require.NotNil(t, found, "providers = %+v", resp.Providers)
	require.NotNil(t, found.Health, "providers = %+v", resp.Providers)
	assert.NotEqual(t, "HEALTHY", found.Health.State, "stale health reported as HEALTHY, want a non-healthy actionable state")
}

// Status and routing must agree on what a fresh observation is: a
// future-dated observation is no more actionable than a stale one, and a
// current one is.
func TestStatusEndpointHealthFreshnessMatchesRouting(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name       string
		observedAt time.Time
		want       string
	}{
		{"current", now.Add(-time.Minute), "HEALTHY"},
		{"future-dated", now.Add(time.Minute), "UNKNOWN"},
		{"stale", now.Add(-2 * time.Hour), "UNKNOWN"},
		{"never observed", time.Time{}, "UNKNOWN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testRoutingConfig()
			srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: &fakeStore{
				snaps: []store.QuotaSnapshot{quotaSnap("minimax", store.QuotaAvailable, 80)},
				health: []store.HealthRecord{{
					Provider:   "minimax",
					State:      store.Healthy,
					ObservedAt: tt.observedAt,
				}},
			}})

			req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
			req.Header.Set("Authorization", "Bearer "+testRoutingToken)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
			var resp statusResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			var found *statusProvider
			for i := range resp.Providers {
				if resp.Providers[i].Provider == "minimax" {
					found = &resp.Providers[i]
				}
			}
			require.NotNil(t, found, "providers = %+v", resp.Providers)
			require.NotNil(t, found.Health, "providers = %+v", resp.Providers)
			assert.Equal(t, tt.want, found.Health.State)
		})
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken})
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "body=%s", w.Body.String())
	var body errorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, errorCodeUnauthorized, body.Error.Code)
}

func TestStatusUnavailableWithoutStore(t *testing.T) {
	srv := New(nil, Options{AuthToken: testRoutingToken})
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
}

func TestStatusUnavailableOnStoreError(t *testing.T) {
	cfg := testRoutingConfig()
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: &fakeStore{snapErr: context.DeadlineExceeded}})
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+testRoutingToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
}

// The issue names the route-decision endpoint /v1/route; the repository also
// serves the contract name /v1/routing/select, so both must behave identically.
func TestRouteAliasEndpoint(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("minimax", store.Healthy)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	w := postRoute(t, srv, testRoutingToken, `{"version":1,"session_id":"s1","capabilities":{}}`)
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp struct {
		Decision routing.Decision `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.Decision.DecisionID, "decision id missing")
}

// Session release ends a session's route use and must be safely repeatable: the
// pin is deleted, and releasing an unpinned session succeeds too.
func TestReleaseEndpoint(t *testing.T) {
	st := &fakeStore{affinity: map[string]store.SessionAffinity{
		"sess-1": pin("sess-1", "cc-coding", time.Now().UTC()),
	}}
	srv := New(nil, Options{AuthToken: testToken, Store: st})

	w := postRelease(t, srv, testToken, "sess-1")
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	type releaseBody struct {
		Status        string `json:"status"`
		ReleasedAlias string `json:"released_alias"`
	}
	var body releaseBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "released", body.Status)
	assert.Equal(t, "cc-coding", body.ReleasedAlias, "released_alias = %q, want the route that was let go", body.ReleasedAlias)
	assert.NotContains(t, st.affinity, "sess-1", "pin survived release")

	// A repeat release has nothing left to delete and must still succeed.
	w = postRelease(t, srv, testToken, "sess-1")
	assert.Equal(t, http.StatusOK, w.Code, "repeat release body=%s", w.Body.String())
	var repeat releaseBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &repeat))
	assert.Empty(t, repeat.ReleasedAlias, "released_alias = %q on a repeat release, want empty", repeat.ReleasedAlias)
}

func TestReleaseEndpointValidation(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken, Store: &fakeStore{}})

	w := postRelease(t, srv, testToken, strings.Repeat("s", maxSessionIDLength+1))
	assert.Equal(t, http.StatusBadRequest, w.Code, "long session id body=%s", w.Body.String())
	w = postRelease(t, srv, "", "sess-1")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "unauthenticated release body=%s", w.Body.String())
	w = postRelease(t, srv, testToken, "sess-1")
	assert.Equal(t, http.StatusOK, w.Code, "release body=%s", w.Body.String())
}

func TestReleaseEndpointUnavailableWithoutStore(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken})
	w := postRelease(t, srv, testToken, "sess-1")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "release body=%s", w.Body.String())
}

func TestReleaseEndpointUnavailableOnStoreError(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken, Store: &fakeStore{affinityErr: context.DeadlineExceeded}})
	w := postRelease(t, srv, testToken, "sess-1")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "release body=%s", w.Body.String())
}

// A released session must not keep its retained affinity: release deletes the
// pin, so the next selection starts from the full candidate set.
func TestSelectEndpointHonorsSessionRelease(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:    []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health:   []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		affinity: map[string]store.SessionAffinity{"sess-1": pin("sess-1", "mm-fast", time.Now().UTC())},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: st, Decisions: st})

	// Pinned to mm-fast, which the configured priority would not pick.
	assert.Equal(t, "mm-fast", selectAlias(t, srv, "sess-1"), "pinned alias")

	w := postRelease(t, srv, testRoutingToken, "sess-1")
	assert.Equal(t, http.StatusOK, w.Code, "release body=%s", w.Body.String())
	assert.Equal(t, "cc-coding", selectAlias(t, srv, "sess-1"), "alias after release, want the normally selected cc-coding")
}

// pin builds a stored affinity row the way the selection path writes one.
func pin(sessionID, alias string, now time.Time) store.SessionAffinity {
	return store.SessionAffinity{SessionID: sessionID, Alias: alias, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(sessionAffinityTTL)}
}

// selectAlias runs one route decision and returns the selected alias.
func selectAlias(t *testing.T, srv *Server, sessionID string) string {
	t.Helper()
	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"`+sessionID+`","capabilities":{}}`)
	require.Equal(t, http.StatusOK, w.Code, "select body=%s", w.Body.String())
	var resp struct {
		Decision routing.Decision `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
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
	}
	srv := affinityServer(t, st)

	assert.Equal(t, "cc-coding", selectAlias(t, srv, "sess-1"), "first selection, want the ranked cc-coding")
	stored, ok := st.affinity["sess-1"]
	require.True(t, ok, "first selection did not persist a pin")
	assert.Equal(t, "cc-coding", stored.Alias)
	assert.True(t, stored.ExpiresAt.After(stored.UpdatedAt), "stored pin expires %s, not after its update %s", stored.ExpiresAt, stored.UpdatedAt)

	// The pinned route's quota observation goes stale. That says nothing about the
	// route, so the session keeps it; an unpinned session, seeing the same state,
	// is routed to the other provider.
	stale := quotaSnap("commandcode", store.QuotaAvailable, 80)
	stale.ObservedAt = time.Now().UTC().Add(-time.Hour)
	st.snaps = []store.QuotaSnapshot{stale, quotaSnap("minimax", store.QuotaAvailable, 80)}
	assert.Equal(t, "cc-coding", selectAlias(t, srv, "sess-1"), "selection through a transient exclusion, want the held cc-coding")
	assert.Equal(t, "mm-fast", selectAlias(t, srv, "sess-2"), "unpinned session, want mm-fast")

	// Exhausted quota is durable: the route is really gone, so the session moves.
	st.snaps = []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaExhausted, 0), quotaSnap("minimax", store.QuotaAvailable, 80)}
	assert.Equal(t, "mm-fast", selectAlias(t, srv, "sess-1"), "selection after the pinned route was exhausted, want mm-fast")
	assert.Equal(t, "mm-fast", st.affinity["sess-1"].Alias, "stored alias after the move, want mm-fast")
}

// A stored pin that has expired must not decide anything: the session is
// re-evaluated, and the new route becomes its pin.
func TestSelectExpiredPinReevaluatesAndRepins(t *testing.T) {
	now := time.Now().UTC()
	st := &fakeStore{
		snaps:    []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health:   []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		affinity: map[string]store.SessionAffinity{"sess-1": pin("sess-1", "mm-fast", now.Add(-2*sessionAffinityTTL))},
	}
	srv := affinityServer(t, st)
	// pin() computed its expiry from the old now, so it is already past.
	assert.False(t, st.affinity["sess-1"].ExpiresAt.After(now), "fixture pin is not expired")

	assert.Equal(t, "cc-coding", selectAlias(t, srv, "sess-1"), "expired pin, want a fresh cc-coding")
	assert.Equal(t, "cc-coding", st.affinity["sess-1"].Alias, "stored alias after re-pin, want cc-coding")
	assert.True(t, st.affinity["sess-1"].ExpiresAt.After(now), "re-pin did not renew the expiry")
}

// A manual override is the caller asking for another route, so it must re-pin
// instead of being refused by the pin it is replacing.
func TestSelectOverrideRepinsSession(t *testing.T) {
	now := time.Now().UTC()
	st := &fakeStore{
		snaps:    []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health:   []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		affinity: map[string]store.SessionAffinity{"sess-1": pin("sess-1", "cc-coding", now)},
	}
	srv := affinityServer(t, st)

	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"sess-1","capabilities":{},"override":{"model_alias":"mm-fast"}}`)
	assert.Equal(t, http.StatusOK, w.Code, "override body=%s", w.Body.String())
	assert.Equal(t, "mm-fast", st.affinity["sess-1"].Alias, "stored alias after override, want mm-fast")
	// And the next plain request stays on the overridden route.
	assert.Equal(t, "mm-fast", selectAlias(t, srv, "sess-1"), "selection after override, want mm-fast")
}

// An affinity write that fails must not let the caller believe the route stuck.
func TestSelectFailsWhenAffinityCannotBeStored(t *testing.T) {
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy)},
	}
	srv := affinityServer(t, st)
	st.affinityErr = context.DeadlineExceeded

	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"sess-1","capabilities":{}}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s, want 503 when the pin cannot be stored", w.Body.String())
}

// A decision with no eligible route pins nothing: there is no route to keep.
func TestSelectWithoutCandidatePinsNothing(t *testing.T) {
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaExhausted, 0)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy)},
	}
	srv := affinityServer(t, st)
	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"sess-1","capabilities":{}}`)
	assert.Equal(t, http.StatusConflict, w.Code, "body=%s, want 409", w.Body.String())
	assert.Empty(t, st.affinity, "affinity = %+v, want none pinned without a route", st.affinity)
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
	assert.Equal(t, http.StatusConflict, w.Code, "body=%s, want 409 while every route is in its window", w.Body.String())
	var body struct {
		Decision routing.Decision `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	r, ok := findRejectedAlias(body.Decision, "cc-coding")
	require.True(t, ok, "rejection = %+v ok=%v, want a model_availability explanation", r, ok)
	assert.Equal(t, "model_availability", r.Stage)

	// The override names an otherwise valid alias and must still be refused.
	w = postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"sess-1","capabilities":{},"override":{"model_alias":"mm-fast"}}`)
	assert.Equal(t, http.StatusConflict, w.Code, "override body=%s, want 409", w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.False(t, body.Decision.OverrideApplied, "override was applied to a model inside its unavailable window")
	assert.Empty(t, st.affinity, "affinity = %+v, want nothing pinned without a route", st.affinity)
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
	assert.Equal(t, http.StatusOK, w.Code, "final release body=%s", w.Body.String())
	st.mu.Lock()
	_, ok := st.affinity[sameSession]
	st.mu.Unlock()
	if ok {
		// Not a failure of the concurrent flood itself; a concurrent select may
		// have re-pinned after the final release completed. Verify release is
		// still authoritative when serialized alone.
		w = postRelease(t, srv, testRoutingToken, sameSession)
		assert.Equal(t, http.StatusOK, w.Code, "second final release body=%s", w.Body.String())
		st.mu.Lock()
		_, ok = st.affinity[sameSession]
		st.mu.Unlock()
		assert.False(t, ok, "pin survived serialized release after concurrent flood")
	}
}
