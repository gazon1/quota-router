package minimax

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/quota"
)

const key = "sk-cp-fixture-token-plan-key"

var now = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

// fixture serves the sanitized live capture, so the tests parse the response the
// documented endpoint actually returned rather than a hand-written shape.
func fixture(t *testing.T) []byte {
	b, err := os.ReadFile("../../../docs/fixtures/minimax/token_plan_remains.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f struct {
		Response struct {
			Body json.RawMessage
		}
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return f.Response.Body
}

func TestParseLiveFixture(t *testing.T) {
	s := Parse(fixture(t), now, 0.1)

	// The capture reports general at 100% for the 5-hour window and 0% for the
	// weekly one. Worst wins, so the snapshot is EXHAUSTED: this is the finding
	// that a live 429 2056 refusal accompanies a "100%" 5-hour window.
	if state, reason := s.State(); state != quota.Exhausted || reason != "" {
		t.Fatalf("State() = %s/%s, want EXHAUSTED", state, reason)
	}
	// Neither count nor a used figure is reported, so the observation is usable
	// but incomplete.
	if s.Confidence != quota.ConfidenceLow {
		t.Fatalf("Confidence = %s, want LOW", s.Confidence)
	}
	if len(s.Windows) != 4 {
		t.Fatalf("Windows = %d, want 4 (5h+weekly for two model buckets)", len(s.Windows))
	}
	if got, want := s.Windows[1].ResetsAt, time.UnixMilli(1795996800000).UTC(); !got.Equal(want) {
		t.Errorf("weekly ResetsAt = %s, want %s", got, want)
	}
	if got, want := s.Windows[0].Name, "general-5h"; got != want {
		t.Errorf("Window name = %s, want %s", got, want)
	}
	if s.Provenance["remaining"] == "" {
		t.Errorf("Provenance = %v, want the raw field names recorded", s.Provenance)
	}
}

func TestParseStates(t *testing.T) {
	body := func(pct string) []byte {
		return []byte(`{"model_remains":[{"model_name":"general","end_time":1795723200000,` +
			`"current_interval_remaining_percent":` + pct + `,"weekly_end_time":1795996800000,` +
			`"current_weekly_remaining_percent":` + pct + `}],"base_resp":{"status_code":0}}`)
	}
	tests := []struct {
		name  string
		pct   string
		want  quota.QuotaState
		limit float64
	}{
		{"normal", "100", quota.Available, 0.1},
		{"at threshold is low", "10", quota.LowQuota, 0.1},
		{"below threshold", "1", quota.LowQuota, 0.1},
		{"exhausted", "0", quota.Exhausted, 0.1},
		{"threshold disabled", "1", quota.Available, 0},
	}
	for _, tc := range tests {
		if got, _ := Parse(body(tc.pct), now, tc.limit).State(); got != tc.want {
			t.Errorf("%s: State() = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// A percentage outside the reported scale, or absent, must not become a zero
// remaining and must not become AVAILABLE: the snapshot stays UNKNOWN.
func TestParseNeverInventsQuota(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing weekly percent", body: `{"model_remains":[{"model_name":"general","end_time":1,"current_interval_remaining_percent":100}],"base_resp":{"status_code":0}}`},
		{name: "percent above 100", body: `{"model_remains":[{"model_name":"general","end_time":1,"current_interval_remaining_percent":101,"current_weekly_remaining_percent":101}],"base_resp":{"status_code":0}}`},
		{name: "negative percent", body: `{"model_remains":[{"model_name":"general","end_time":1,"current_interval_remaining_percent":-1,"current_weekly_remaining_percent":50}],"base_resp":{"status_code":0}}`},
		{name: "percent as string", body: `{"model_remains":[{"model_name":"general","end_time":1,"current_interval_remaining_percent":"100","current_weekly_remaining_percent":100}],"base_resp":{"status_code":0}}`},
		{name: "renamed fields", body: `{"model_remains":[{"model_name":"general","end_time":1,"interval_remaining_pct":100}],"base_resp":{"status_code":0}}`},
		{name: "no model_remains", body: `{"model_remains":[],"base_resp":{"status_code":0}}`},
		{name: "no base_resp", body: `{"model_remains":[{"model_name":"general","end_time":1,"current_interval_remaining_percent":100,"current_weekly_remaining_percent":100}]}`},
		{name: "malformed json", body: `{"model_remains":[`},
		{name: "empty body", body: ``},
	}
	for _, tc := range tests {
		s := Parse([]byte(tc.body), now, 0.1)
		if state, reason := s.State(); state != quota.QuotaUnknown || reason == "" {
			t.Errorf("%s: State() = %s/%s, want UNKNOWN with a reason", tc.name, state, reason)
		}
		if s.Confidence != quota.ConfidenceNone {
			t.Errorf("%s: Confidence = %s, want NONE on a body with no quota", tc.name, s.Confidence)
		}
	}
}

// The endpoint answers 200 when it rejects a credential, so the business status
// is what decides the reason.
func TestParseBusinessErrors(t *testing.T) {
	tests := []struct {
		code   string
		reason quota.UnknownReason
	}{
		{"1004", quota.AuthFailure},
		{"2049", quota.AuthFailure},
		{"2057", quota.AuthFailure},
		{"1002", quota.TransientFetchFailure},
		{"2062", quota.TransientFetchFailure},
		{"9999", quota.MalformedResponse},
	}
	for _, tc := range tests {
		body := []byte(`{"base_resp":{"status_code":` + tc.code + `,"status_msg":"x"}}`)
		if _, reason := Parse(body, now, 0.1).State(); reason != tc.reason {
			t.Errorf("status_code %s: State() reason = %s, want %s", tc.code, reason, tc.reason)
		}
	}
}

func TestFetchStates(t *testing.T) {
	ok := fixture(t)
	tests := []struct {
		name    string
		status  int
		body    []byte
		want    quota.QuotaState
		reason  quota.UnknownReason
		health  quota.Health
		healthR quota.HealthReason
	}{
		{"success", 200, ok, quota.Exhausted, "", quota.Healthy, quota.ReasonNone},
		{"unauthorized", 401, nil, quota.QuotaUnknown, quota.AuthFailure, quota.Unavailable, quota.ReasonAuthFailed},
		{"forbidden", 403, nil, quota.QuotaUnknown, quota.AuthFailure, quota.Unavailable, quota.ReasonAuthFailed},
		{"rate limited", 429, nil, quota.QuotaUnknown, quota.TransientFetchFailure, quota.Degraded, quota.ReasonRateLimited},
		{"server error", 503, nil, quota.QuotaUnknown, quota.TransientFetchFailure, quota.Unavailable, quota.ReasonServerError},
		{"bad auth in body", 200, []byte(`{"base_resp":{"status_code":1004}}`), quota.QuotaUnknown, quota.AuthFailure, quota.Unavailable, quota.ReasonAuthFailed},
		{"garbage body", 200, []byte(`not json`), quota.QuotaUnknown, quota.MalformedResponse, quota.Degraded, quota.ReasonServerError},
	}
	for _, tc := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got, want := r.Header.Get("Authorization"), "Bearer "+key; got != want {
				t.Errorf("Authorization = %q, want the Token Plan key", got)
			}
			w.WriteHeader(tc.status)
			if tc.body != nil {
				_, _ = w.Write(tc.body)
			}
		}))
		defer srv.Close()

		a := &Adapter{URL: srv.URL, Key: config.Secret(key), LowFraction: 0.1}
		snap, state := a.Fetch(context.Background())
		if got, reason := snap.State(); got != tc.want || reason != tc.reason {
			t.Errorf("%s: quota = %s/%s, want %s/%s", tc.name, got, reason, tc.want, tc.reason)
		}
		if state.Health != tc.health || state.Reason() != tc.healthR {
			t.Errorf("%s: health = %s/%s, want %s/%s", tc.name, state.Health, state.Reason(), tc.health, tc.healthR)
		}
	}
}

func TestFetchHonoursDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hold the request until the client gives up
	}))
	defer srv.Close()

	a := &Adapter{URL: srv.URL, Key: config.Secret(key), LowFraction: 0.1}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	snap, state := a.Fetch(ctx)
	if got, reason := snap.State(); got != quota.QuotaUnknown || reason != quota.TransientFetchFailure {
		t.Errorf("quota = %s/%s, want UNKNOWN/TRANSIENT_FETCH_FAILURE", got, reason)
	}
	if state.Reason() != quota.ReasonTimeout {
		t.Errorf("health reason = %s, want TIMEOUT", state.Reason())
	}
}

func TestFetchUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // the port is now closed

	a := &Adapter{URL: url, Key: config.Secret(key)}
	_, state := a.Fetch(context.Background())
	if state.Health != quota.Unavailable || state.Reason() != quota.ReasonUnreachable {
		t.Errorf("health = %s/%s, want UNAVAILABLE/UNREACHABLE", state.Health, state.Reason())
	}
}

// After a failed refresh the last usable observation is kept for diagnostics and
// reads as UNKNOWN, so no consumer can route on it.
func TestStaleLastKnownGood(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fixture(t))
	}))
	defer srv.Close()

	a := &Adapter{URL: srv.URL, Key: config.Secret(key), LowFraction: 0.1}
	good, _ := a.Fetch(context.Background())
	if state, _ := good.State(); state != quota.Exhausted || good.Stale {
		t.Fatalf("first fetch = %s stale=%v, want EXHAUSTED and fresh", state, good.Stale)
	}

	fail.Store(true)
	stale, health := a.Fetch(context.Background())
	if !stale.Stale {
		t.Fatalf("second fetch Stale = false, want a stale last-known-good copy")
	}
	if got, reason := stale.StateAt(time.Now(), time.Hour); got != quota.QuotaUnknown || reason != quota.StaleObservation {
		t.Errorf("StateAt() = %s/%s, want UNKNOWN/STALE_OBSERVATION", got, reason)
	}
	if state, _ := stale.State(); state != quota.Exhausted {
		t.Errorf("raw State() = %s: diagnostics keep the values, only StateAt refuses them", state)
	}
	// Health reports this attempt, not the retained copy.
	if health.Health != quota.Unavailable || health.Reason() != quota.ReasonServerError {
		t.Errorf("health = %s/%s, want UNAVAILABLE/PROVIDER_ERROR", health.Health, health.Reason())
	}
}

func TestNoCredentialLeaks(t *testing.T) {
	snap, state := Parse([]byte(`{"base_resp":{"status_code":1004,"status_msg":"login fail"}}`), now, 0.1),
		quota.NewProviderState(Provider, quota.Unavailable, quota.ReasonAuthFailed, now)
	for _, v := range []any{snap, state} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		if strings.Contains(string(b), "sk-cp") {
			t.Errorf("marshaled %T carries a credential: %s", v, b)
		}
	}
	if strings.Contains(Source, "?") {
		t.Errorf("Source = %s, want no query string that could carry a credential", Source)
	}
}

func TestNewUsesTheQuotaHost(t *testing.T) {
	a, err := New(config.Provider{BaseURL: "https://www.minimax.io/v1", APIKey: config.Secret(key)}, 0.1, 10*time.Second)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got, want := a.URL, "https://www.minimax.io/v1/token_plan/remains"; got != want {
		t.Errorf("URL = %s, want %s", got, want)
	}
	if a.Key != config.Secret(key) || a.LowFraction != 0.1 || a.Timeout != 10*time.Second {
		t.Errorf("New() = %+v, want the configured key and threshold", a)
	}
}

// Reset boundaries: the reported end is the reset; an absent one leaves the
// window undated, which lowers confidence but is not a quota value.
func TestParseResetBoundary(t *testing.T) {
	bodies := map[string]string{
		"end present": `{"model_remains":[{"model_name":"general","end_time":1795723200000,` +
			`"current_interval_remaining_percent":100,"weekly_end_time":1795996800000,` +
			`"current_weekly_remaining_percent":100}],"base_resp":{"status_code":0}}`,
		"end absent": `{"model_remains":[{"model_name":"general",` +
			`"current_interval_remaining_percent":100,"current_weekly_remaining_percent":100}],` +
			`"base_resp":{"status_code":0}}`,
		"end zero": `{"model_remains":[{"model_name":"general","end_time":0,` +
			`"current_interval_remaining_percent":100,"weekly_end_time":0,` +
			`"current_weekly_remaining_percent":100}],"base_resp":{"status_code":0}}`,
	}
	for name, body := range bodies {
		s := Parse([]byte(body), now, 0.1)
		if state, _ := s.State(); state != quota.Available {
			t.Errorf("%s: State() = %s, want AVAILABLE: a reset time never changes quota", name, state)
		}
	}
	if s := Parse([]byte(bodies["end present"]), now, 0.1); s.Windows[0].ResetsAt.IsZero() || s.Confidence != quota.ConfidenceLow {
		t.Errorf("end present: ResetsAt zero or confidence %s, want a dated window", s.Confidence)
	}
	for _, name := range []string{"end absent", "end zero"} {
		s := Parse([]byte(bodies[name]), now, 0.1)
		if !s.Windows[0].ResetsAt.IsZero() {
			t.Errorf("%s: ResetsAt = %s, want zero", name, s.Windows[0].ResetsAt)
		}
		if s.Confidence != quota.ConfidenceLow {
			t.Errorf("%s: Confidence = %s, want LOW", name, s.Confidence)
		}
	}
}

// A deadline-less caller still gets a bounded attempt, so a hung provider cannot
// block a poller forever.
func TestFetchBoundsItself(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	a := &Adapter{URL: srv.URL, Key: config.Secret(key), Timeout: 50 * time.Millisecond}
	snap, state := a.Fetch(context.Background())
	if got, _ := snap.State(); got != quota.QuotaUnknown {
		t.Errorf("quota = %s, want UNKNOWN", got)
	}
	if state.Reason() != quota.ReasonTimeout {
		t.Errorf("health reason = %s, want TIMEOUT", state.Reason())
	}
}

// A configuration mistake must not carry the Token Plan key to another origin:
// the inference host is the realistic one, since it is the neighbouring setting.
func TestNewRejectsUnsafeOrigins(t *testing.T) {
	bad := []struct{ name, baseURL string }{
		{"inference host", "https://api.minimax.io/v1"},
		{"anthropic inference host", "https://api.minimax.io/anthropic"},
		{"plain http quota host", "http://www.minimax.io/v1"},
		{"lookalike host", "https://www.minimax.io.evil.example/v1"},
		{"other host", "https://example.com/v1"},
		{"wrong path", "https://www.minimax.io/v2"},
		{"embedded credentials", "https://user:pass@www.minimax.io/v1"},
		{"not a URL", "://"},
	}
	for _, tc := range bad {
		if _, err := New(config.Provider{BaseURL: tc.baseURL, APIKey: config.Secret(key)}, 0.1, time.Second); !errors.Is(err, errUnsafeOrigin) {
			t.Errorf("%s: New() error = %v, want errUnsafeOrigin", tc.name, err)
		}
	}

	// Loopback stays available for tests and local interposition.
	for _, ok := range []string{"http://127.0.0.1:8080/v1", "http://localhost:8080/v1", "http://[::1]:8080/v1"} {
		if _, err := New(config.Provider{BaseURL: ok}, 0.1, time.Second); err != nil {
			t.Errorf("%s: New() error = %v, want a loopback origin to be accepted", ok, err)
		}
	}
}

// The check also guards an Adapter built by hand, and it runs before any request:
// the transport must never be reached, so the key cannot leave the process even
// if the host resolves.
func TestUnsafeOriginNeverSendsTheKey(t *testing.T) {
	transport := &countingTransport{}
	a := &Adapter{
		Client:  &http.Client{Transport: transport},
		URL:     "https://api.minimax.io/v1" + path, // the inference host
		Key:     config.Secret(key),
		Timeout: time.Second,
	}
	snap, state := a.Fetch(context.Background())
	if got, reason := snap.State(); got != quota.QuotaUnknown || reason != quota.MalformedResponse {
		t.Errorf("quota = %s/%s, want UNKNOWN/MALFORMED_RESPONSE", got, reason)
	}
	if state.Health != quota.Degraded {
		t.Errorf("health = %s, want DEGRADED", state.Health)
	}
	if n := transport.calls.Load(); n != 0 {
		t.Errorf("transport was called %d times, want 0: the key must not leave the process", n)
	}
}

// countingTransport records whether any request was attempted at all.
type countingTransport struct{ calls atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return nil, errors.New("transport must not be reached")
}

// An answer past the documented size is rejected, not silently truncated into a
// plausible-looking body.
func TestFetchRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte(" "), maxBody+1))
	}))
	defer srv.Close()

	a := &Adapter{URL: srv.URL + path, Key: config.Secret(key), Timeout: time.Second}
	snap, _ := a.Fetch(context.Background())
	if got, reason := snap.State(); got != quota.QuotaUnknown || reason != quota.MalformedResponse {
		t.Errorf("quota = %s/%s, want UNKNOWN/MALFORMED_RESPONSE", got, reason)
	}
}

// A slower, older attempt must not overwrite a newer observation: retained
// last-known-good quota would silently go backwards.
func TestSlowerOlderFetchDoesNotOverwriteNewer(t *testing.T) {
	release := make(chan struct{})
	var first atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			// The older attempt holds the response until the newer one is stored.
			<-release
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"model_remains":[{"model_name":"general",` +
				`"current_interval_remaining_percent":20,"current_weekly_remaining_percent":20}],` +
				`"base_resp":{"status_code":0}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fixture(t))
	}))
	defer srv.Close()

	a := &Adapter{URL: srv.URL + path, Key: config.Secret(key), LowFraction: 0.1, Timeout: 5 * time.Second}
	older := make(chan quota.Snapshot, 1)
	go func() {
		snap, _ := a.Fetch(context.Background())
		older <- snap
	}()

	// Give the first request time to start and block, then take the newer one.
	time.Sleep(50 * time.Millisecond)
	newer, _ := a.Fetch(context.Background())
	close(release)
	<-older

	// The fixture is EXHAUSTED (weekly 0%); the older body is LOW_QUOTA (20%).
	a.mu.Lock()
	retained := a.lastGood
	a.mu.Unlock()
	if state, _ := retained.State(); state != quota.Exhausted {
		t.Fatalf("retained = %s, want the newer EXHAUSTED observation", state)
	}
	if newer.ObservedAt.Before(retained.ObservedAt) {
		t.Fatalf("retained observation is newer than the latest fetch, want the latest kept")
	}
}
