package minimax

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/quota"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const key = "sk-cp-fixture-token-plan-key"

var now = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

// fixture serves the sanitized live capture, so the tests parse the response the
// documented endpoint actually returned rather than a hand-written shape.
func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../docs/fixtures/minimax/token_plan_remains.json")
	require.NoError(t, err, "read fixture")
	var f struct {
		Response struct {
			Body json.RawMessage
		}
	}
	require.NoError(t, json.Unmarshal(b, &f), "decode fixture")
	return f.Response.Body
}

func TestParseLiveFixture(t *testing.T) {
	s := Parse(fixture(t), now, 0.1)

	// The capture reports general at 100% for the 5-hour window and 0% for the
	// weekly one. Worst wins, so the snapshot is EXHAUSTED: this is the finding
	// that a live 429 2056 refusal accompanies a "100%" 5-hour window.
	state, reason := s.State()
	assert.Equal(t, quota.Exhausted, state)
	assert.Empty(t, reason)
	// Neither count nor a used figure is reported, so the observation is usable
	// but incomplete.
	assert.Equal(t, quota.ConfidenceLow, s.Confidence)
	assert.Len(t, s.Windows, 4, "want 4 (5h+weekly for two model buckets)")
	assert.WithinDuration(t, time.UnixMilli(1795996800000).UTC(), s.Windows[1].ResetsAt, 0)
	assert.Equal(t, "general-5h", s.Windows[0].Name)
	assert.NotEmpty(t, s.Provenance["remaining"], "want the raw field names recorded")
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
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Parse(body(tc.pct), now, tc.limit).State()
			assert.Equal(t, tc.want, got)
		})
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
		t.Run(tc.name, func(t *testing.T) {
			s := Parse([]byte(tc.body), now, 0.1)
			state, reason := s.State()
			assert.Equal(t, quota.QuotaUnknown, state)
			assert.NotEmpty(t, reason, "want UNKNOWN with a reason")
			assert.Equal(t, quota.ConfidenceNone, s.Confidence, "want NONE on a body with no quota")
		})
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
		t.Run(tc.code, func(t *testing.T) {
			body := []byte(`{"base_resp":{"status_code":` + tc.code + `,"status_msg":"x"}}`)
			_, reason := Parse(body, now, 0.1).State()
			assert.Equal(t, tc.reason, reason)
		})
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
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer "+key, r.Header.Get("Authorization"), "want the Token Plan key")
				w.WriteHeader(tc.status)
				if tc.body != nil {
					_, _ = w.Write(tc.body)
				}
			}))
			defer srv.Close()

			a := &Adapter{URL: srv.URL, Key: config.Secret(key), LowFraction: 0.1}
			snap, state := a.Fetch(context.Background())
			got, reason := snap.State()
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.reason, reason)
			assert.Equal(t, tc.health, state.Health)
			assert.Equal(t, tc.healthR, state.Reason())
		})
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
	got, reason := snap.State()
	assert.Equal(t, quota.QuotaUnknown, got, "want UNKNOWN/TRANSIENT_FETCH_FAILURE")
	assert.Equal(t, quota.TransientFetchFailure, reason, "want UNKNOWN/TRANSIENT_FETCH_FAILURE")
	assert.Equal(t, quota.ReasonTimeout, state.Reason(), "want TIMEOUT")
}

func TestFetchUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // the port is now closed

	a := &Adapter{URL: url, Key: config.Secret(key)}
	_, state := a.Fetch(context.Background())
	assert.Equal(t, quota.Unavailable, state.Health, "want UNAVAILABLE/UNREACHABLE")
	assert.Equal(t, quota.ReasonUnreachable, state.Reason(), "want UNAVAILABLE/UNREACHABLE")
}

// After a failed refresh the last usable observation is kept for diagnostics and
// reads as UNKNOWN, so no consumer can route on it.
func TestStaleLastKnownGood(t *testing.T) {
	var fail atomic.Bool
	body := fixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	a := &Adapter{URL: srv.URL, Key: config.Secret(key), LowFraction: 0.1}
	good, _ := a.Fetch(context.Background())
	state, _ := good.State()
	assert.Equal(t, quota.Exhausted, state, "first fetch, want EXHAUSTED and fresh")
	assert.False(t, good.Stale, "first fetch, want EXHAUSTED and fresh")

	fail.Store(true)
	stale, health := a.Fetch(context.Background())
	assert.True(t, stale.Stale, "want a stale last-known-good copy")
	got, reason := stale.StateAt(time.Now(), time.Hour)
	assert.Equal(t, quota.QuotaUnknown, got, "want UNKNOWN/STALE_OBSERVATION")
	assert.Equal(t, quota.StaleObservation, reason, "want UNKNOWN/STALE_OBSERVATION")
	state, _ = stale.State()
	assert.Equal(t, quota.Exhausted, state, "diagnostics keep the values, only StateAt refuses them")
	// Health reports this attempt, not the retained copy.
	assert.Equal(t, quota.Unavailable, health.Health, "want UNAVAILABLE/PROVIDER_ERROR")
	assert.Equal(t, quota.ReasonServerError, health.Reason(), "want UNAVAILABLE/PROVIDER_ERROR")
}

func TestNoCredentialLeaks(t *testing.T) {
	snap, state := Parse([]byte(`{"base_resp":{"status_code":1004,"status_msg":"login fail"}}`), now, 0.1),
		quota.NewProviderState(Provider, quota.Unavailable, quota.ReasonAuthFailed, now)
	for _, v := range []any{snap, state} {
		b, err := json.Marshal(v)
		require.NoError(t, err, "Marshal()")
		assert.NotContains(t, string(b), "sk-cp", "marshaled %T carries a credential", v)
	}
	assert.NotContains(t, Source, "?", "want no query string that could carry a credential")
}

func TestNewUsesTheQuotaHost(t *testing.T) {
	a, err := New(config.Provider{BaseURL: "https://www.minimax.io/v1", APIKey: config.Secret(key)}, 0.1, 10*time.Second)
	require.NoError(t, err, "New()")
	assert.Equal(t, "https://www.minimax.io/v1/token_plan/remains", a.URL)
	assert.Equal(t, config.Secret(key), a.Key)
	assert.Equal(t, 0.1, a.LowFraction)
	assert.Equal(t, 10*time.Second, a.Timeout)
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
		t.Run(name, func(t *testing.T) {
			s := Parse([]byte(body), now, 0.1)
			state, _ := s.State()
			assert.Equal(t, quota.Available, state, "a reset time never changes quota")
		})
	}
	s := Parse([]byte(bodies["end present"]), now, 0.1)
	assert.False(t, s.Windows[0].ResetsAt.IsZero(), "want a dated window")
	assert.Equal(t, quota.ConfidenceLow, s.Confidence, "want a dated window")
	for _, name := range []string{"end absent", "end zero"} {
		t.Run(name, func(t *testing.T) {
			s := Parse([]byte(bodies[name]), now, 0.1)
			assert.True(t, s.Windows[0].ResetsAt.IsZero(), "want zero")
			assert.Equal(t, quota.ConfidenceLow, s.Confidence, "want LOW")
		})
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
	got, _ := snap.State()
	assert.Equal(t, quota.QuotaUnknown, got, "want UNKNOWN")
	assert.Equal(t, quota.ReasonTimeout, state.Reason(), "want TIMEOUT")
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
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(config.Provider{BaseURL: tc.baseURL, APIKey: config.Secret(key)}, 0.1, time.Second)
			assert.ErrorIs(t, err, errUnsafeOrigin)
		})
	}

	// Loopback stays available for tests and local interposition.
	for _, ok := range []string{"http://127.0.0.1:8080/v1", "http://localhost:8080/v1", "http://[::1]:8080/v1"} {
		t.Run(ok, func(t *testing.T) {
			_, err := New(config.Provider{BaseURL: ok}, 0.1, time.Second)
			assert.NoError(t, err, "want a loopback origin to be accepted")
		})
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
	got, reason := snap.State()
	assert.Equal(t, quota.QuotaUnknown, got, "want UNKNOWN/MALFORMED_RESPONSE")
	assert.Equal(t, quota.MalformedResponse, reason, "want UNKNOWN/MALFORMED_RESPONSE")
	assert.Equal(t, quota.Degraded, state.Health, "want DEGRADED")
	assert.Equal(t, int32(0), transport.calls.Load(), "the key must not leave the process")
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

	// This asserts rejection, not timing: a 1 MiB transfer under -race is slow
	// enough on a cold run to trip a tight bound and report the wrong reason.
	a := &Adapter{URL: srv.URL + path, Key: config.Secret(key), Timeout: 30 * time.Second}
	snap, _ := a.Fetch(context.Background())
	got, reason := snap.State()
	assert.Equal(t, quota.QuotaUnknown, got, "want UNKNOWN/MALFORMED_RESPONSE")
	assert.Equal(t, quota.MalformedResponse, reason, "want UNKNOWN/MALFORMED_RESPONSE")
}

// A slower, older attempt must not overwrite a newer observation, and the older
// call must not return its own older reading either: a caller receiving an
// apparently fresh snapshot that regresses quota is the real defect.
func TestSlowerOlderFetchDoesNotRegressCallers(t *testing.T) {
	started := make(chan struct{}) // the older handler is waiting
	release := make(chan struct{}) // let the older attempt finish
	var first atomic.Bool
	body := fixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			// The older attempt holds its response until the newer one is stored.
			close(started)
			<-release
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"model_remains":[{"model_name":"general",` +
				`"current_interval_remaining_percent":20,"current_weekly_remaining_percent":20}],` +
				`"base_resp":{"status_code":0}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	a := &Adapter{URL: srv.URL + path, Key: config.Secret(key), LowFraction: 0.1, Timeout: 5 * time.Second}
	type result struct {
		snap  quota.Snapshot
		state quota.ProviderState
	}
	older := make(chan result, 1)
	go func() {
		snap, state := a.Fetch(context.Background())
		older <- result{snap, state}
	}()

	// Explicit handshake instead of a sleep: the older request is provably
	// blocked in its handler before the newer one starts.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.Fail(t, "the first request never reached the server")
	}
	newer, newerHealth := a.Fetch(context.Background())
	close(release)
	got := <-older

	state, _ := newer.State()
	assert.Equal(t, quota.Exhausted, state, "newer fetch, want EXHAUSTED")
	// The returned value is what a caller routes on, so assert on it, not only on
	// the retained cache.
	state, _ = got.snap.State()
	assert.Equal(t, quota.Exhausted, state, "older fetch returned, want the retained newer EXHAUSTED observation")
	assert.False(t, got.snap.Stale, "older fetch marked the retained newer observation stale, want fresh")
	assert.False(t, got.snap.ObservedAt.Before(newer.ObservedAt), "returned observation is older than the newer fetch, want the newest kept")
	// Health describes the attempt that just completed, not the retained copy.
	assert.Equal(t, quota.Healthy, got.state.Health, "older fetch health, want HEALTHY: it reached the provider")
	assert.Equal(t, quota.ReasonNone, got.state.Reason(), "older fetch health, want HEALTHY: it reached the provider")
	assert.Equal(t, quota.Healthy, newerHealth.Health, "newer fetch health, want HEALTHY")

	// The fixture is EXHAUSTED (weekly 0%); the older body is LOW_QUOTA (20%).
	a.mu.Lock()
	retained := a.lastGood
	a.mu.Unlock()
	state, _ = retained.State()
	assert.Equal(t, quota.Exhausted, state, "retained, want the newer EXHAUSTED observation")
	assert.False(t, newer.ObservedAt.Before(retained.ObservedAt), "retained observation is newer than the latest fetch, want the latest kept")
}
