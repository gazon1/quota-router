package agent_test

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "Route")
	assert.Equal(t, "cc-coding", got.Alias)
	assert.Equal(t, "commandcode", got.Provider)
	assert.NotEmpty(t, got.DecisionID, "correlation ids missing")
	assert.NotEmpty(t, got.RequestID, "correlation ids missing")
	// The controller persisted that exact decision, so the ids join up.
	assert.Equal(t, got.DecisionID, h.decisionID(t), "persisted decision id")

	const prompt = "the-secret-prompt-marker"
	reply, err := h.client.Chat(ctx, got, []byte(`{"model":"ignored-by-agent","messages":[{"role":"user","content":"`+prompt+`"}]}`))
	require.NoError(t, err, "Chat")
	assert.Equal(t, http.StatusOK, reply.Status)
	assert.Equal(t, agent.OutcomeSuccess, reply.Outcome)
	// The proxy saw the routed alias and the same correlation id, and the
	// controller never saw the prompt.
	call := h.proxy.last()
	assert.Equal(t, "cc-coding", call.model, "want the routed alias cc-coding")
	// This asserts what ProxyRequest sets, not that LiteLLM honours the header:
	// on the pinned image it neither echoes nor forwards X-Request-Id, which is
	// why correlation relies on request_id/attempt rather than this header.
	assert.Equal(t, got.RequestID, call.requestID)
	for _, body := range h.controllerBodies() {
		assert.NotContains(t, body, prompt, "the controller received prompt content")
	}

	require.NoError(t, h.client.Report(ctx, got, reply.Attempt(1)), "Report")
	row := h.usageRow(t)
	assert.Equal(t, got.RequestID, row.RequestID, "usage row correlation")
	assert.Equal(t, got.DecisionID, row.DecisionID, "usage row correlation")
	assert.Equal(t, "commandcode", row.Provider, "usage row route")
	assert.Equal(t, "cc-coding", row.Model, "usage row route")
	require.NotNil(t, row.InputTokens)
	assert.Equal(t, 1200, *row.InputTokens, "want the reported 1200/34")
	require.NotNil(t, row.OutputTokens)
	assert.Equal(t, 34, *row.OutputTokens, "want the reported 1200/34")
	assert.Equal(t, store.OutcomeSuccess, row.Outcome, "usage row provenance")
	assert.Equal(t, store.Reported, row.Provenance, "usage row provenance")
	assert.Equal(t, agent.Source, row.Source, "usage row provenance")

	// A repeated report writes nothing and still succeeds (idempotent).
	require.NoError(t, h.client.Report(ctx, got, reply.Attempt(1)), "repeated Report")
	assert.Equal(t, 1, h.usageCount(t), "usage rows after a repeated report")
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
	require.Error(t, err)
	assert.ErrorContains(t, err, agent.ErrNoRoute.Error())
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
	_, err := down.Route(context.Background(), req)
	require.Error(t, err)
	assert.ErrorContains(t, err, agent.ErrControllerUnavailable.Error())

	down.Manual = agent.ManualRoute{Alias: "cc-coding", Provider: "commandcode"}
	got, err := down.Route(context.Background(), req)
	require.NoError(t, err, "manual Route")
	assert.True(t, got.Manual, "want a visible manual route with no decision id")
	assert.Equal(t, "cc-coding", got.Alias, "want a visible manual route with no decision id")
	assert.Equal(t, "", got.DecisionID, "want a visible manual route with no decision id")

	// A manual route still reports, and the row names no decision. A cancelled
	// attempt carries no counts: missing is not zero.
	require.NoError(t, h.client.Report(context.Background(), got, agent.Attempt{Number: 1, Outcome: agent.OutcomeCancelled}), "Report")
	row := h.usageRow(t)
	assert.Equal(t, "", row.DecisionID, "want no decision id and CANCELLED")
	assert.Equal(t, store.OutcomeCancelled, row.Outcome, "want no decision id and CANCELLED")
	assert.Nil(t, row.InputTokens, "cancelled attempt stored token counts")
	assert.Nil(t, row.OutputTokens, "cancelled attempt stored token counts")
}

// TestAgentRejectsInvalidRequest keeps validation at the contract boundary, so a
// malformed request never reaches the controller.
func TestAgentRejectsInvalidRequest(t *testing.T) {
	h := newHarness(t)
	_, err := h.client.Route(context.Background(), routing.RouteRequest{Version: 2, SessionID: "s", Capabilities: &routing.RequiredCapabilities{}})
	assert.Error(t, err, "version 2 was accepted")
	_, err = h.client.Route(context.Background(), routing.RouteRequest{Version: 1, SessionID: "s"})
	assert.Error(t, err, "a request without capabilities was accepted")
	assert.Empty(t, h.controllerBodies(), "an invalid request reached the controller")
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
					t.Errorf("body %q panicked: %v", body, r)
				}
			}()
			_, err := h.client.ProxyRequest(context.Background(), routed, []byte(body))
			assert.Error(t, err, "body %q was accepted", body)
		}()
	}
	// The valid empty object still works: nothing about the fix may narrow it.
	req, err := h.client.ProxyRequest(context.Background(), routed, []byte(`{}`))
	require.NoError(t, err, "empty object rejected")
	assert.Empty(t, req.Header.Get("X-Request-Id"), "on an unrouted request")
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
			require.Error(t, err, "route succeeded with manual %+v; a %s must not fall back", got, tc.name)
			assert.ErrorIs(t, err, tc.want)
			assert.NotErrorIs(t, err, agent.ErrControllerUnavailable, "err also claims the controller was unavailable")
		})
	}

	// A real connection failure still falls back, so the fix did not disable it.
	down := *h.client
	down.ControllerURL = "http://127.0.0.1:1"
	got, err := down.Route(context.Background(), req)
	require.NoError(t, err, "connection failure did not fall back")
	assert.True(t, got.Manual, "want the manual route")
	assert.Equal(t, "cc-coding", got.Alias, "want the manual route")
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
	require.Error(t, err, "want a missing-provider error")
	assert.ErrorContains(t, err, "without a provider")
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
	require.NoError(t, err, "store.Open")
	t.Cleanup(func() { st.Close() })
	require.NoError(t, st.Migrate(ctx), "Migrate")

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
	require.NoError(t, h.store.SaveQuotaSnapshot(context.Background(), store.QuotaSnapshot{
		Provider: provider, State: state, Source: "test", Confidence: store.Reported,
		ObservedAt: time.Now().UTC(),
		Windows:    []store.QuotaWindow{{Window: "5h", Limit: &limit, Used: &used, Remaining: &remaining, ResetAt: &reset}},
	}), "SaveQuotaSnapshot")
	require.NoError(t, h.store.SaveProviderHealth(context.Background(), store.HealthRecord{
		Provider: provider, State: store.Healthy, ObservedAt: time.Now().UTC(),
	}), "SaveProviderHealth")
}

func (h *harness) decisionID(t *testing.T) string {
	t.Helper()
	recs, err := h.store.LatestRoutingDecisions(context.Background(), 1)
	require.NoError(t, err, "LatestRoutingDecisions")
	require.NotEmpty(t, recs, "LatestRoutingDecisions")
	return recs[0].DecisionID
}

func (h *harness) usageRow(t *testing.T) store.UsageRecord {
	t.Helper()
	rows := h.usageRows(t)
	require.NotEmpty(t, rows, "no usage row was stored")
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
	require.NoError(t, err, "open read-only")
	defer db.Close()
	rows, err := db.Query(`SELECT request_id, attempt, provider, model, input_tokens, output_tokens, provenance, source, outcome, decision_id FROM usage_records ORDER BY id`)
	require.NoError(t, err, "query usage_records")
	defer rows.Close()
	var out []store.UsageRecord
	for rows.Next() {
		var u store.UsageRecord
		var reqID, model, prov, source, outcome, decisionID *string
		require.NoError(t, rows.Scan(&reqID, &u.Attempt, &u.Provider, &model, &u.InputTokens, &u.OutputTokens, &prov, &source, &outcome, &decisionID), "scan usage row")
		u.RequestID = deref(reqID)
		u.Model = deref(model)
		u.Provenance = store.Confidence(deref(prov))
		u.Source = deref(source)
		u.Outcome = store.Outcome(deref(outcome))
		u.DecisionID = deref(decisionID)
		out = append(out, u)
	}
	require.NoError(t, rows.Err(), "iterate usage rows")
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
