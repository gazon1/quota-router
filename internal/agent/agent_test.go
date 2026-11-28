package agent_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/agent"
	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/routing"
	"github.com/gazon1/quota-router/internal/server"
	"github.com/gazon1/quota-router/internal/store"
)

// TestAgentContract is the #20 contract test: route, call the proxy directly
// with the returned alias, report the attempt, and prove the controller outage
// behaviour. It runs against a real controller (server + SQLite), not a stub, so
// the documented contract is what is exercised.
func TestAgentContract(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "commandcode", store.QuotaAvailable, 80)
	h.seed(t, "minimax", store.QuotaAvailable, 80)

	ctx := context.Background()
	got, err := h.client.Route(ctx, routing.RouteRequest{
		Version:      routing.RouteRequestVersionV1,
		SessionID:    "session-1",
		Capabilities: &routing.RequiredCapabilities{ToolCalling: true},
		TaskClass:    routing.TaskCoding,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got.Alias != "cc-coding" {
		t.Fatalf("alias = %q, want cc-coding", got.Alias)
	}
	if got.Provider != "commandcode" {
		t.Fatalf("provider = %q, want commandcode", got.Provider)
	}
	if got.DecisionID == "" || got.RequestID == "" {
		t.Fatalf("correlation ids missing: %+v", got)
	}
	// The controller persisted that exact decision, so the ids join up.
	if id := h.decisionID(t); id != got.DecisionID {
		t.Fatalf("persisted decision id = %q, want %q", id, got.DecisionID)
	}

	const prompt = "the-secret-prompt-marker"
	reply, err := h.client.Chat(ctx, got, []byte(`{"model":"ignored-by-agent","messages":[{"role":"user","content":"`+prompt+`"}]}`))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if reply.Status != http.StatusOK || reply.Outcome != agent.OutcomeSuccess {
		t.Fatalf("reply = %d %s", reply.Status, reply.Outcome)
	}
	// The proxy saw the routed alias and the same correlation id, and the
	// controller never saw the prompt.
	call := h.proxy.last()
	if call.model != "cc-coding" {
		t.Fatalf("proxy model = %q, want the routed alias cc-coding", call.model)
	}
	// This asserts what ProxyRequest sets, not that LiteLLM honours the header:
	// on the pinned image it neither echoes nor forwards X-Request-Id, which is
	// why correlation relies on request_id/attempt rather than this header.
	if call.requestID != got.RequestID {
		t.Fatalf("proxy X-Request-Id = %q, want %q", call.requestID, got.RequestID)
	}
	for _, body := range h.controllerBodies() {
		if strings.Contains(body, prompt) {
			t.Fatalf("the controller received prompt content: %s", body)
		}
	}

	if err := h.client.Report(ctx, got, reply.Attempt(1)); err != nil {
		t.Fatalf("Report: %v", err)
	}
	row := h.usageRow(t)
	if row.RequestID != got.RequestID || row.DecisionID != got.DecisionID {
		t.Fatalf("usage row correlation = %+v, want request %q decision %q", row, got.RequestID, got.DecisionID)
	}
	if row.Provider != "commandcode" || row.Model != "cc-coding" {
		t.Fatalf("usage row route = %q/%q", row.Provider, row.Model)
	}
	if row.InputTokens == nil || *row.InputTokens != 1200 || row.OutputTokens == nil || *row.OutputTokens != 34 {
		t.Fatalf("usage row tokens = %+v, want the reported 1200/34", row)
	}
	if row.Outcome != store.OutcomeSuccess || row.Provenance != store.Reported || row.Source != agent.Source {
		t.Fatalf("usage row provenance = %q/%q/%q", row.Outcome, row.Provenance, row.Source)
	}

	// A repeated report writes nothing and still succeeds (idempotent).
	if err := h.client.Report(ctx, got, reply.Attempt(1)); err != nil {
		t.Fatalf("repeated Report: %v", err)
	}
	if n := h.usageCount(t); n != 1 {
		t.Fatalf("usage rows = %d after a repeated report, want 1", n)
	}
}

// TestAgentNoRouteIsNotAnOutage proves a policy answer is not turned into a
// manual route: 409 means no eligible model, which a fallback would overrule.
func TestAgentNoRouteIsNotAnOutage(t *testing.T) {
	h := newHarness(t)
	h.client.Manual = agent.ManualRoute{Alias: "mm-fast", Provider: "minimax"}
	h.seed(t, "commandcode", store.QuotaExhausted, 0)
	h.seed(t, "minimax", store.QuotaExhausted, 0)

	_, err := h.client.Route(context.Background(), routing.RouteRequest{
		Version:      routing.RouteRequestVersionV1,
		SessionID:    "session-1",
		Capabilities: &routing.RequiredCapabilities{},
	})
	if err == nil || !strings.Contains(err.Error(), agent.ErrNoRoute.Error()) {
		t.Fatalf("err = %v, want %v", err, agent.ErrNoRoute)
	}
}

// TestAgentControllerDownFailsClosedAndManual is the documented outage rule:
// with no manual route the call fails closed; with one it proceeds visibly and
// reports usage with no decision id.
func TestAgentControllerDownFailsClosedAndManual(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "commandcode", store.QuotaAvailable, 80)
	req := routing.RouteRequest{
		Version:      routing.RouteRequestVersionV1,
		SessionID:    "session-1",
		Capabilities: &routing.RequiredCapabilities{},
	}

	down := *h.client
	down.ControllerURL = "http://127.0.0.1:1" // nothing listens here
	if _, err := down.Route(context.Background(), req); err == nil || !strings.Contains(err.Error(), agent.ErrControllerUnavailable.Error()) {
		t.Fatalf("err = %v, want %v", err, agent.ErrControllerUnavailable)
	}

	down.Manual = agent.ManualRoute{Alias: "cc-coding", Provider: "commandcode"}
	got, err := down.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("manual Route: %v", err)
	}
	if !got.Manual || got.Alias != "cc-coding" || got.DecisionID != "" {
		t.Fatalf("manual route = %+v, want a visible manual route with no decision id", got)
	}

	// A manual route still reports, and the row names no decision. A cancelled
	// attempt carries no counts: missing is not zero.
	if err := h.client.Report(context.Background(), got, agent.Attempt{Number: 1, Outcome: agent.OutcomeCancelled}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	row := h.usageRow(t)
	if row.DecisionID != "" || row.Outcome != store.OutcomeCancelled {
		t.Fatalf("manual usage row = %+v, want no decision id and CANCELLED", row)
	}
	if row.InputTokens != nil || row.OutputTokens != nil {
		t.Fatalf("cancelled attempt stored token counts %+v", row)
	}
}

// TestAgentRejectsInvalidRequest keeps validation at the contract boundary, so a
// malformed request never reaches the controller.
func TestAgentRejectsInvalidRequest(t *testing.T) {
	h := newHarness(t)
	if _, err := h.client.Route(context.Background(), routing.RouteRequest{Version: 2, SessionID: "s", Capabilities: &routing.RequiredCapabilities{}}); err == nil {
		t.Fatal("version 2 was accepted")
	}
	if _, err := h.client.Route(context.Background(), routing.RouteRequest{Version: 1, SessionID: "s"}); err == nil {
		t.Fatal("a request without capabilities was accepted")
	}
	if len(h.controllerBodies()) != 0 {
		t.Fatalf("an invalid request reached the controller: %v", h.controllerBodies())
	}
}

// TestProxyRequestRejectsNonObjectBodies is a regression test: "null" unmarshals
// into a nil map with no error, so an unguarded write to it panicked the agent.
// Every case must return an ordinary error.
func TestProxyRequestRejectsNonObjectBodies(t *testing.T) {
	h := newHarness(t)
	routed := agent.Routed{Alias: "cc-coding", Provider: "commandcode"}
	for _, body := range []string{"null", "[]", "", "{oops", `"str"`, "123", "   "} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("body %q panicked: %v", body, r)
				}
			}()
			if _, err := h.client.ProxyRequest(context.Background(), routed, []byte(body)); err == nil {
				t.Fatalf("body %q was accepted", body)
			}
		}()
	}
	// The valid empty object still works: nothing about the fix may narrow it.
	req, err := h.client.ProxyRequest(context.Background(), routed, []byte(`{}`))
	if err != nil {
		t.Fatalf("empty object rejected: %v", err)
	}
	if got := req.Header.Get("X-Request-Id"); got != "" {
		t.Fatalf("X-Request-Id = %q on an unrouted request", got)
	}
}

// TestManualFallbackIgnoresCancellation is a regression test: a cancelled or
// timed-out caller must not be turned into a manual route, because the abandoned
// request would then continue down another path.
func TestManualFallbackIgnoresCancellation(t *testing.T) {
	h := newHarness(t)
	h.client.Manual = agent.ManualRoute{Alias: "cc-coding", Provider: "commandcode"}
	req := routing.RouteRequest{
		Version:      routing.RouteRequestVersionV1,
		SessionID:    "session-1",
		Capabilities: &routing.RequiredCapabilities{},
	}

	// A slow controller so the caller's own deadline is what expires.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()

	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{name: "cancelled", want: context.Canceled, ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // cancelled before the call, so no fallback is reachable
			return ctx, cancel
		}},
		{name: "deadline", want: context.DeadlineExceeded, ctx: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := *h.client
			c.ControllerURL = slow.URL
			ctx, cancel := tc.ctx()
			defer cancel()
			got, err := c.Route(ctx, req)
			if err == nil {
				t.Fatalf("route succeeded with manual %+v; a %s must not fall back", got, tc.name)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want errors.Is(_, %v)", err, tc.want)
			}
			if errors.Is(err, agent.ErrControllerUnavailable) {
				t.Fatalf("err = %v also claims the controller was unavailable", err)
			}
		})
	}

	// A real connection failure still falls back, so the fix did not disable it.
	down := *h.client
	down.ControllerURL = "http://127.0.0.1:1"
	got, err := down.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("connection failure did not fall back: %v", err)
	}
	if !got.Manual || got.Alias != "cc-coding" {
		t.Fatalf("route = %+v, want the manual route", got)
	}
}

// TestRouteRejectsDecisionWithoutProvider: an alias with no provider cannot be
// reported, so it must fail at route time rather than at report time.
func TestRouteRejectsDecisionWithoutProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"decision":{"decision_id":"dec-1","selected_alias":"cc-coding","policy_version":"routing.v1"}}`)
	}))
	defer srv.Close()

	c := &agent.Client{ControllerURL: srv.URL, Token: contractToken}
	_, err := c.Route(context.Background(), routing.RouteRequest{
		Version:      routing.RouteRequestVersionV1,
		SessionID:    "session-1",
		Capabilities: &routing.RequiredCapabilities{},
	})
	if err == nil || !strings.Contains(err.Error(), "without a provider") {
		t.Fatalf("err = %v, want a missing-provider error", err)
	}
}

// --- harness ---------------------------------------------------------------

type harness struct {
	client *agent.Client
	store  *store.Store
	path   string
	proxy  *proxyStub

	mu       sync.Mutex
	received []string
}

// proxyStub is the LiteLLM stand-in: it records what the agent sent and answers
// one OpenAI-shaped completion with a usage object.
type proxyStub struct {
	srv *httptest.Server

	mu    sync.Mutex
	calls []proxyCall
}

type proxyCall struct {
	model     string
	requestID string
	prompt    string
}

func (p *proxyStub) last() proxyCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		return proxyCall{}
	}
	return p.calls[len(p.calls)-1]
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(store.Config{Path: path, BusyTimeout: 5 * time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	h := &harness{
		store: st,
		path:  path,
		proxy: &proxyStub{},
	}
	h.proxy.srv = httptest.NewServer(http.HandlerFunc(h.proxy.handle))
	t.Cleanup(h.proxy.srv.Close)

	ctl := server.New(nil, server.Options{
		Config:    contractConfig(),
		AuthToken: contractToken,
		Store:     st,
		Usage:     st,
	})
	// The recorder proves the criterion the hard way: whatever body reaches the
	// controller, the test can look for prompt content in it.
	recorder := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.received = append(h.received, string(body))
		h.mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		ctl.Handler().ServeHTTP(w, r)
	})
	ctlSrv := httptest.NewServer(recorder)
	t.Cleanup(ctlSrv.Close)

	h.client = &agent.Client{
		ControllerURL: ctlSrv.URL,
		ProxyURL:      h.proxy.srv.URL,
		Token:         contractToken,
		ProxyKey:      "proxy-key",
		HTTP:          &http.Client{Timeout: 5 * time.Second},
	}
	return h
}

func (h *harness) controllerBodies() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.received...)
}

const contractToken = "contract-token"

func contractConfig() config.Config {
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

func (p *proxyStub) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	prompt := ""
	if len(req.Messages) > 0 {
		prompt = req.Messages[0].Content
	}
	p.mu.Lock()
	p.calls = append(p.calls, proxyCall{model: req.Model, requestID: r.Header.Get("X-Request-Id"), prompt: prompt})
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1200,"completion_tokens":34}}`)
}

// seed makes one provider routable and fresh, the way polling would.
func (h *harness) seed(t *testing.T, provider string, state store.QuotaState, remaining float64) {
	t.Helper()
	limit := 100.0
	used := limit - remaining
	reset := time.Now().Add(time.Hour).UTC()
	if err := h.store.SaveQuotaSnapshot(context.Background(), store.QuotaSnapshot{
		Provider: provider, State: state, Source: "test", Confidence: store.Reported,
		ObservedAt: time.Now().UTC(),
		Windows:    []store.QuotaWindow{{Window: "5h", Limit: &limit, Used: &used, Remaining: &remaining, ResetAt: &reset}},
	}); err != nil {
		t.Fatalf("SaveQuotaSnapshot: %v", err)
	}
	if err := h.store.SaveProviderHealth(context.Background(), store.HealthRecord{
		Provider: provider, State: store.Healthy, ObservedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveProviderHealth: %v", err)
	}
}

func (h *harness) decisionID(t *testing.T) string {
	t.Helper()
	recs, err := h.store.LatestRoutingDecisions(context.Background(), 1)
	if err != nil || len(recs) == 0 {
		t.Fatalf("LatestRoutingDecisions: %v (%d)", err, len(recs))
	}
	return recs[0].DecisionID
}

func (h *harness) usageRow(t *testing.T) store.UsageRecord {
	t.Helper()
	rows := h.usageRows(t)
	if len(rows) == 0 {
		t.Fatal("no usage row was stored")
	}
	return rows[len(rows)-1]
}

func (h *harness) usageCount(t *testing.T) int {
	t.Helper()
	return len(h.usageRows(t))
}

// usageRows reads the table through its own connection, so the test observes
// what was persisted rather than what the handler returned.
func (h *harness) usageRows(t *testing.T) []store.UsageRecord {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.path+"?mode=ro")
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT request_id, attempt, provider, model, input_tokens, output_tokens, provenance, source, outcome, decision_id FROM usage_records ORDER BY id`)
	if err != nil {
		t.Fatalf("query usage_records: %v", err)
	}
	defer rows.Close()
	var out []store.UsageRecord
	for rows.Next() {
		var u store.UsageRecord
		var reqID, model, prov, source, outcome, decisionID *string
		if err := rows.Scan(&reqID, &u.Attempt, &u.Provider, &model, &u.InputTokens, &u.OutputTokens, &prov, &source, &outcome, &decisionID); err != nil {
			t.Fatalf("scan usage row: %v", err)
		}
		u.RequestID = deref(reqID)
		u.Model = deref(model)
		u.Provenance = store.Confidence(deref(prov))
		u.Source = deref(source)
		u.Outcome = store.Outcome(deref(outcome))
		u.DecisionID = deref(decisionID)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate usage rows: %v", err)
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
