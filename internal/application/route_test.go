package application

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/routing"
	"github.com/gazon1/quota-router/internal/store"
)

type fakeState struct {
	snaps     []store.QuotaSnapshot
	health    []store.HealthRecord
	snapErr   error
	healthErr error
}

func (f *fakeState) LatestQuotaSnapshot(context.Context) ([]store.QuotaSnapshot, error) {
	if f.snapErr != nil {
		return nil, f.snapErr
	}
	return f.snaps, nil
}

func (f *fakeState) LatestProviderHealth(context.Context) ([]store.HealthRecord, error) {
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	return f.health, nil
}

type fakeAffinity struct {
	mu      sync.Mutex
	m       map[string]store.SessionAffinity
	readErr error
	saveErr error
	delErr  error
}

func (f *fakeAffinity) SessionAffinity(_ context.Context, sessionID string) (store.SessionAffinity, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return store.SessionAffinity{}, false, f.readErr
	}
	a, ok := f.m[sessionID]
	return a, ok, nil
}

func (f *fakeAffinity) SaveSessionAffinity(_ context.Context, a store.SessionAffinity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.m == nil {
		f.m = map[string]store.SessionAffinity{}
	}
	if existing, ok := f.m[a.SessionID]; ok {
		a.CreatedAt = existing.CreatedAt
	}
	f.m[a.SessionID] = a
	return nil
}

func (f *fakeAffinity) DeleteSessionAffinity(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	delete(f.m, sessionID)
	return nil
}

type fakeDecisions struct {
	mu      sync.Mutex
	records []store.RoutingDecisionRecord
	err     error
}

func (f *fakeDecisions) SaveRoutingDecision(_ context.Context, r store.RoutingDecisionRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.records = append(f.records, r)
	return nil
}

func (f *fakeDecisions) LatestRoutingDecisions(_ context.Context, limit int) ([]store.RoutingDecisionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit <= 0 || limit > len(f.records) {
		limit = len(f.records)
	}
	out := make([]store.RoutingDecisionRecord, 0, limit)
	for i := len(f.records) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, f.records[i])
	}
	return out, nil
}

func testConfig() config.Config {
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

func routeReq(sessionID string) routing.RouteRequest {
	return routing.RouteRequest{Version: 1, SessionID: sessionID, Capabilities: &routing.RequiredCapabilities{}}
}

func newTestService() (*RouteService, *fakeState, *fakeAffinity, *fakeDecisions) {
	cfg := testConfig()
	state := &fakeState{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
	}
	aff := &fakeAffinity{m: map[string]store.SessionAffinity{}}
	dec := &fakeDecisions{}
	svc := NewRouteService(nil, cfg, routing.Selector{Config: cfg}, state, aff, dec)
	return svc, state, aff, dec
}

func TestRouteWithoutHTTPOrSQLite(t *testing.T) {
	svc, _, _, dec := newTestService()
	got, err := svc.Route(context.Background(), routeReq("s1"))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got.SelectedAlias != "cc-coding" {
		t.Fatalf("selected=%q, want cc-coding", got.SelectedAlias)
	}
	if got.DecisionID == "" || got.PolicyVersion != routing.PolicyVersion {
		t.Fatalf("decision metadata missing: %+v", got)
	}
	if len(dec.records) != 1 || dec.records[0].DecisionID != got.DecisionID {
		t.Fatalf("decision not persisted: %+v", dec.records)
	}
}

func TestRoutePersistsDecisionBeforeAffinity(t *testing.T) {
	cfg := testConfig()
	state := &fakeState{
		snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
	}
	order := &orderStore{fakeAffinity: &fakeAffinity{m: map[string]store.SessionAffinity{}}, fakeDecisions: &fakeDecisions{}}
	svc := NewRouteService(nil, cfg, routing.Selector{Config: cfg}, state, order, order)

	if _, err := svc.Route(context.Background(), routeReq("s1")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	order.mu.Lock()
	defer order.mu.Unlock()
	if len(order.events) != 2 || order.events[0] != "decision" || order.events[1] != "affinity" {
		t.Fatalf("order=%v, want [decision affinity]", order.events)
	}
}

type orderStore struct {
	*fakeAffinity
	*fakeDecisions
	mu     sync.Mutex
	events []string
}

func (o *orderStore) SaveRoutingDecision(ctx context.Context, r store.RoutingDecisionRecord) error {
	err := o.fakeDecisions.SaveRoutingDecision(ctx, r)
	o.mu.Lock()
	o.events = append(o.events, "decision")
	o.mu.Unlock()
	return err
}

func (o *orderStore) SaveSessionAffinity(ctx context.Context, a store.SessionAffinity) error {
	err := o.fakeAffinity.SaveSessionAffinity(ctx, a)
	o.mu.Lock()
	o.events = append(o.events, "affinity")
	o.mu.Unlock()
	return err
}

func TestRouteDecisionPersistenceFailureReturnsError(t *testing.T) {
	svc, _, aff, dec := newTestService()
	dec.err = errors.New("boom")
	_, err := svc.Route(context.Background(), routeReq("s1"))
	if err == nil {
		t.Fatal("expected error")
	}
	if len(aff.m) != 0 {
		t.Fatalf("affinity written despite decision failure: %+v", aff.m)
	}
}

func TestRouteAffinityFailureReturnsError(t *testing.T) {
	svc, _, aff, dec := newTestService()
	aff.saveErr = errors.New("boom")
	_, err := svc.Route(context.Background(), routeReq("s1"))
	if err == nil {
		t.Fatal("expected error")
	}
	if len(dec.records) != 1 {
		t.Fatalf("decision records=%d, want 1 (audited before affinity failure surfaced)", len(dec.records))
	}
}

func TestRouteSameSessionSerialized(t *testing.T) {
	cfg := testConfig()
	state := &gateState{
		fakeState: &fakeState{
			snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
			health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		},
		read: make(chan struct{}),
		gate: make(chan struct{}),
	}
	aff := &fakeAffinity{m: map[string]store.SessionAffinity{}}
	dec := &fakeDecisions{}
	svc := NewRouteService(nil, cfg, routing.Selector{Config: cfg}, state, aff, dec)

	done1 := make(chan error, 1)
	go func() { _, err := svc.Route(context.Background(), routeReq("s1")); done1 <- err }()
	<-state.read

	done2 := make(chan error, 1)
	go func() { _, err := svc.Route(context.Background(), routeReq("s1")); done2 <- err }()
	select {
	case err := <-done2:
		t.Fatalf("second same-session route completed early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(state.gate)
	if err := <-done1; err != nil {
		t.Fatalf("route1: %v", err)
	}
	if err := <-done2; err != nil {
		t.Fatalf("route2: %v", err)
	}
}

type gateState struct {
	*fakeState
	once sync.Once
	read chan struct{}
	gate chan struct{}
	n    atomic.Int32
}

func (g *gateState) LatestQuotaSnapshot(ctx context.Context) ([]store.QuotaSnapshot, error) {
	if g.n.Add(1) == 1 {
		g.once.Do(func() { close(g.read) })
		<-g.gate
	}
	return g.fakeState.LatestQuotaSnapshot(ctx)
}

func TestRouteDifferentSessionsConcurrent(t *testing.T) {
	cfg := testConfig()
	state := &gateState{
		fakeState: &fakeState{
			snaps:  []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
			health: []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		},
		read: make(chan struct{}),
		gate: make(chan struct{}),
	}
	aff := &fakeAffinity{m: map[string]store.SessionAffinity{}}
	dec := &fakeDecisions{}
	svc := NewRouteService(nil, cfg, routing.Selector{Config: cfg}, state, aff, dec)

	done1 := make(chan error, 1)
	go func() { _, err := svc.Route(context.Background(), routeReq("s1")); done1 <- err }()
	<-state.read

	done2 := make(chan error, 1)
	go func() { _, err := svc.Route(context.Background(), routeReq("s2")); done2 <- err }()
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("route2: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("different session blocked on unrelated session lock")
	}
	close(state.gate)
	if err := <-done1; err != nil {
		t.Fatalf("route1: %v", err)
	}
}

func TestRouteRenewsAffinityTTL(t *testing.T) {
	svc, _, aff, _ := newTestService()
	first, err := svc.Route(context.Background(), routeReq("s1"))
	if err != nil {
		t.Fatalf("first Route: %v", err)
	}
	pin1 := aff.m["s1"]
	time.Sleep(10 * time.Millisecond)
	second, err := svc.Route(context.Background(), routeReq("s1"))
	if err != nil {
		t.Fatalf("second Route: %v", err)
	}
	if second.SelectedAlias != first.SelectedAlias {
		t.Fatalf("selected=%q want %q", second.SelectedAlias, first.SelectedAlias)
	}
	pin2 := aff.m["s1"]
	if !pin2.UpdatedAt.After(pin1.UpdatedAt) {
		t.Fatalf("updated_at not renewed: %v <= %v", pin2.UpdatedAt, pin1.UpdatedAt)
	}
	if !pin2.ExpiresAt.After(pin1.ExpiresAt) {
		t.Fatalf("expires_at not renewed: %v <= %v", pin2.ExpiresAt, pin1.ExpiresAt)
	}
	if !pin2.CreatedAt.Equal(pin1.CreatedAt) {
		t.Fatalf("created_at changed: %v != %v", pin2.CreatedAt, pin1.CreatedAt)
	}
}
func TestSessionLockIsPerSession(t *testing.T) {
	var l sessionLocks
	unlockA := l.lock("a")
	taken := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		unlockB := l.lock("b")
		close(taken)
		<-release
		unlockB()
	}()
	select {
	case <-taken:
	case <-time.After(time.Second):
		t.Fatal("lock on session b blocked on session a: the lock is not per session")
	}
	close(release)
	<-done
	unlockA()

	l.mu.Lock()
	entries := len(l.m)
	l.mu.Unlock()
	if entries != 0 {
		t.Fatalf("lock table holds %d entries after every holder left, want 0", entries)
	}
}
