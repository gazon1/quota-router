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
	srv := New(nil, config.Default(), nil)
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
	srv := New(nil, config.Default(), nil)
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

type fakeStore struct {
	snaps     []store.QuotaSnapshot
	health    []store.HealthRecord
	decisions []store.RoutingDecisionRecord
	snapErr   error
	healthErr error
	saveErr   error
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

func TestSelectEndpoint(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
	}
	srv := New(nil, cfg, nil)
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}

	body := `{"version":1,"session_id":"s1","capabilities":{"tool_calling":true},"task_class":"coding"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/routing/select", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

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
	srv := New(nil, cfg, nil)
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}

	body := `{"version":1,"session_id":"s1","capabilities":{}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/routing/select", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
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
	srv := New(nil, cfg, nil)
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}
	req := httptest.NewRequest(http.MethodPost, "/v1/routing/select", strings.NewReader(`{"version":2,"session_id":"s1","capabilities":{}}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
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
	if len(resp.Decision.Reasons) == 0 || !strings.Contains(resp.Decision.Reasons[0], "request invalid") {
		t.Fatalf("reasons=%v", resp.Decision.Reasons)
	}
}

func TestDecisionsEndpoint(t *testing.T) {
	cfg := testRoutingConfig()
	st := &fakeStore{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
	}
	srv := New(nil, cfg, nil)
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}

	body := `{"version":1,"session_id":"s1","capabilities":{}}`
	r := httptest.NewRequest(http.MethodPost, "/v1/routing/select", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("select status=%d body=%s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/routing/decisions", nil)
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
	srv := New(nil, cfg, nil)
	srv.store = st
	srv.decisions = st
	srv.selector = routing.Selector{Config: cfg}

	req := httptest.NewRequest(http.MethodPost, "/v1/routing/select", strings.NewReader(`{"version":1,"session_id":"s1","capabilities":{}}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestSelectEndpointInvalidJSON(t *testing.T) {
	srv := New(nil, testRoutingConfig(), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/routing/select", strings.NewReader(`{`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestSelectEndpointRejectsUnknownField(t *testing.T) {
	srv := New(nil, testRoutingConfig(), nil)
	srv.selector = routing.Selector{Config: testRoutingConfig()}
	req := httptest.NewRequest(http.MethodPost, "/v1/routing/select", strings.NewReader(`{"version":1,"session_id":"s1","capabilities":{},"unknown":true}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

var _ = bytes.NewBuffer
