package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	snaps         []store.QuotaSnapshot
	health        []store.HealthRecord
	decisions     []store.RoutingDecisionRecord
	snapErr       error
	healthErr     error
	saveErr       error
	controller    map[string]string
	controllerErr error
}

func (f *fakeStore) ControllerState(_ context.Context, key string) (string, bool, error) {
	if f.controllerErr != nil {
		return "", false, f.controllerErr
	}
	v, ok := f.controller[key]
	return v, ok, nil
}

func (f *fakeStore) LatestQuotaSnapshot(context.Context) ([]store.QuotaSnapshot, error) {
	if f.snapErr != nil {
		return nil, f.snapErr
	}
	return f.snaps, nil
}
func (f *fakeStore) LatestProviderHealth(context.Context) ([]store.HealthRecord, error) {
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	return f.health, nil
}
func (f *fakeStore) SaveRoutingDecision(_ context.Context, r store.RoutingDecisionRecord) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.decisions = append(f.decisions, r)
	return nil
}
func (f *fakeStore) LatestRoutingDecisions(_ context.Context, limit int) ([]store.RoutingDecisionRecord, error) {
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

const testRoutingToken = "secret-token"

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
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}

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
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}

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

func TestSelectEndpointInvalidVersion(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}
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
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}

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
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}

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
	st := &fakeStore{snapErr: context.DeadlineExceeded}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}

	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"s1","capabilities":{}}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
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
	srv.selector = routing.Selector{Config: testRoutingConfig()}
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
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}
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
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = st
	srv.decisions = st

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
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken})
	srv.store = &fakeStore{}
	srv.decisions = &fakeStore{}
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
	srv := New(nil, Options{Config: testRoutingConfig(), AuthToken: testRoutingToken})
	srv.store = &fakeStore{}
	srv.decisions = &fakeStore{}
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
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = st

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

// The status endpoint applies the same freshness bound as the selector: a
// provider whose health observation is older than the bound must not be
// reported healthy.
func TestStatusEndpointStaleHealthNotHealthy(t *testing.T) {
	cfg := testRoutingConfig()
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = &fakeStore{
		snaps: []store.QuotaSnapshot{quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{{
			Provider:   "minimax",
			State:      store.Healthy,
			ObservedAt: time.Now().UTC().Add(-2 * time.Hour),
		}},
	}

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
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = &fakeStore{snapErr: context.DeadlineExceeded}
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
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}

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

// Session release ends a session's route use and must be safely repeatable.
func TestReleaseEndpoint(t *testing.T) {
	fr := &fakeReleaser{}
	srv := New(nil, Options{AuthToken: testToken})
	srv.releases = fr

	w := postRelease(t, srv, testToken, "sess-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "released" {
		t.Fatalf("status = %q, want released", body.Status)
	}
	if len(fr.keys) != 1 || fr.keys[0] != releaseKeyPrefix+"sess-1" {
		t.Fatalf("keys = %v, want [%q]", fr.keys, releaseKeyPrefix+"sess-1")
	}

	w = postRelease(t, srv, testToken, "sess-1")
	if w.Code != http.StatusOK {
		t.Fatalf("repeat release status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestReleaseEndpointValidation(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken})
	srv.releases = &fakeReleaser{}

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

func TestReleaseEndpointUnavailableWithoutReleaser(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken})
	if w := postRelease(t, srv, testToken, "sess-1"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("release status=%d body=%s", w.Code, w.Body.String())
	}
}

// A released session must not keep its retained affinity: the selection path
// reads the release marker and drops the session's affinity.
func TestApplyReleasedClearsAffinity(t *testing.T) {
	ctx := context.Background()
	srv := New(nil, Options{AuthToken: testToken})
	srv.store = &fakeStore{controller: map[string]string{releaseKeyPrefix + "sess-1": releaseValueReleased}}

	state := routing.SelectState{Affinity: &routing.Affinity{SessionID: "sess-1", Alias: "cc-coding"}}
	if err := srv.applyReleased(ctx, &state, "sess-1"); err != nil {
		t.Fatalf("applyReleased released session: %v", err)
	}
	if state.Affinity != nil {
		t.Fatalf("affinity for released session = %+v, want nil", state.Affinity)
	}

	state = routing.SelectState{Affinity: &routing.Affinity{SessionID: "sess-2", Alias: "cc-coding"}}
	if err := srv.applyReleased(ctx, &state, "sess-2"); err != nil {
		t.Fatalf("applyReleased live session: %v", err)
	}
	if state.Affinity == nil {
		t.Fatal("affinity for a live session must be kept")
	}
}

func TestSelectEndpointHonorsSessionRelease(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy)},
		controller: map[string]string{
			releaseKeyPrefix + "sess-1": releaseValueReleased,
		},
	}
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken})
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}

	w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"sess-1","capabilities":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

type fakeReleaser struct {
	err  error
	keys []string
}

func (f *fakeReleaser) SaveControllerState(_ context.Context, key, _ string, _ time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.keys = append(f.keys, key)
	return nil
}

var _ = bytes.NewBuffer
