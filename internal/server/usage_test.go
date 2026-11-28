package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gazon1/quota-router/internal/store"
)

const testToken = "secret-token"

// memSink records what the handler tried to persist, keeping store semantics
// out of these tests: dedup is the store's job and is asserted there.
type memSink struct {
	mu      sync.Mutex
	records []store.UsageRecord
	err     error
}

func (m *memSink) SaveUsageRecord(_ context.Context, u store.UsageRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.records = append(m.records, u)
	return nil
}

func post(t *testing.T, srv *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/usage", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// The route is a write into the database, so an unset token must refuse rather
// than serve unauthenticated.
func TestUsageRequiresToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string // configured
		send  string // presented
		want  int
	}{
		{"no token configured", "", testToken, http.StatusUnauthorized},
		{"no token presented", testToken, "", http.StatusUnauthorized},
		{"wrong token", testToken, "other", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &memSink{}
			srv := New(nil, Options{Usage: sink, AuthToken: tc.token})
			w := post(t, srv, tc.send, `{"provider":"minimax"}`)
			assert.Equal(t, tc.want, w.Code)
			assert.Empty(t, sink.records, "stored %d records for a rejected report", len(sink.records))
		})
	}
}

func TestUsageAcceptsProviderReportedAttempt(t *testing.T) {
	sink := &memSink{}
	srv := New(nil, Options{Usage: sink, AuthToken: testToken})

	w := post(t, srv, testToken, `{
		"request_id": "req-1",
		"attempt": 2,
		"provider": "minimax",
		"model": "MiniMax-M2.7",
		"input_tokens": 120,
		"output_tokens": 0,
		"provenance": "reported",
		"source": "litellm.callback",
		"outcome": "PARTIAL_STREAM",
		"decision_id": "dec-9"
	}`)
	assert.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body)
	require.Len(t, sink.records, 1)
	got := sink.records[0]
	assert.Equal(t, "req-1", got.RequestID)
	assert.Equal(t, 2, got.Attempt)
	assert.Equal(t, "dec-9", got.DecisionID)
	assert.Equal(t, store.TokenReportedLegacy, got.InputProvenance)
	assert.Equal(t, store.OutcomePartialStream, got.Outcome)
	// A reported zero must survive as zero, not become "missing".
	require.NotNil(t, got.OutputTokens, "output_tokens = %v, want a reported 0", got.OutputTokens)
	assert.Equal(t, 0, *got.OutputTokens)
	assert.False(t, got.ObservedAt.IsZero(), "observed_at was left unset: the arrival time must be recorded")
}

// Missing usage is unknown, never zero, and a missing outcome is UNKNOWN.
func TestUsageKeepsMissingValuesUnknown(t *testing.T) {
	sink := &memSink{}
	srv := New(nil, Options{Usage: sink, AuthToken: testToken})

	w := post(t, srv, testToken, `{"provider":"minimax","provenance":"estimated","source":"agent"}`)
	assert.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body)
	require.Len(t, sink.records, 1)
	got := sink.records[0]
	assert.Nil(t, got.InputTokens, "tokens = %v/%v, want both unknown", got.InputTokens, got.OutputTokens)
	assert.Nil(t, got.OutputTokens, "tokens = %v/%v, want both unknown", got.InputTokens, got.OutputTokens)
	assert.Equal(t, store.OutcomeUnknown, got.Outcome, "outcome = %q, want UNKNOWN when the reporter did not say", got.Outcome)
	assert.Equal(t, 0, got.Attempt, "attempt = %d, want 0 (the store maps it to the first attempt)", got.Attempt)
}

// A caller sending a prompt or a conversation body must fail loudly instead of
// having the field silently dropped and its content lost.
func TestUsageRejectsUnknownFields(t *testing.T) {
	sink := &memSink{}
	srv := New(nil, Options{Usage: sink, AuthToken: testToken})

	w := post(t, srv, testToken, `{"provider":"minimax","provenance":"reported","source":"agent","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, sink.records, "a report carrying unknown fields was stored")
}

func TestUsageRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no provider", `{"provenance":"reported","source":"agent"}`},
		{"no source", `{"provider":"p","provenance":"reported"}`},
		{"bad provenance", `{"provider":"p","provenance":"guessed","source":"agent"}`},
		{"bad outcome", `{"provider":"p","provenance":"reported","source":"agent","outcome":"TIMEOUT"}`},
		{"bad task class", `{"provider":"p","source":"agent","task_class":"teleportation"}`},
		{"bad route source", `{"provider":"p","source":"agent","route_source":"guess"}`},
		{"bad stream state", `{"provider":"p","source":"agent","stream_state":"half"}`},
		{"raw provider message as error class", `{"provider":"p","source":"agent","error_class":"provider said: sk-secret"}`},
		{"negative attempt", `{"provider":"p","provenance":"reported","source":"agent","attempt":-1}`},
		{"negative tokens", `{"provider":"p","provenance":"reported","source":"agent","input_tokens":-5}`},
		{"negative latency", `{"provider":"p","source":"agent","latency_ms":-1}`},
		{"bad timestamp", `{"provider":"p","provenance":"reported","source":"agent","observed_at":"yesterday"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &memSink{}
			srv := New(nil, Options{Usage: sink, AuthToken: testToken})
			w := post(t, srv, testToken, tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Empty(t, sink.records, "an invalid report was stored")
		})
	}
}

func TestUsageRejectsOversizedBody(t *testing.T) {
	srv := New(nil, Options{Usage: &memSink{}, AuthToken: testToken})
	big := `{"provider":"p","provenance":"reported","source":"agent","request_id":"` +
		strings.Repeat("x", maxUsageBody) + `"}`
	w := post(t, srv, testToken, big)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

// A busy database is retryable, and the caller must be told that rather than
// being given a generic failure it would not retry.
func TestUsageReportsBusyAsRetryable(t *testing.T) {
	srv := New(nil, Options{Usage: &memSink{err: fmt.Errorf("write: %w", store.ErrBusy)}, AuthToken: testToken})
	w := post(t, srv, testToken, `{"provider":"p","provenance":"reported","source":"agent"}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	srv = New(nil, Options{Usage: &memSink{err: errors.New("boom")}, AuthToken: testToken})
	w = post(t, srv, testToken, `{"provider":"p","provenance":"reported","source":"agent"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	// The internal cause must not be echoed to the caller.
	assert.NotContains(t, w.Body.String(), "boom", "response leaks the internal error: %s", w.Body)
}

func TestUsageConflictOnConflictingEventReuse(t *testing.T) {
	srv := New(nil, Options{Usage: &memSink{err: store.ErrUsageConflict}, AuthToken: testToken})
	w := post(t, srv, testToken, `{"provider":"p","provenance":"reported","source":"agent"}`)
	assert.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	var body errorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, errorCodeConflict, body.Error.Code)
	assert.False(t, body.Error.Retryable, "retryable = true, want false for a conflicting reuse")
}

func TestUsageIsUnavailableWithoutASink(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken})
	w := post(t, srv, testToken, `{"provider":"p","provenance":"reported","source":"agent"}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// The canonical event fields survive the wire: the handler maps them onto the
// persisted record without inventing or dropping any of them.
func TestUsageAcceptsCanonicalEventFields(t *testing.T) {
	sink := &memSink{}
	srv := New(nil, Options{Usage: sink, AuthToken: testToken})

	w := post(t, srv, testToken, `{
		"request_id": "req-1",
		"attempt": 1,
		"provider": "minimax",
		"model": "MiniMax-M2.7",
		"task_class": "coding",
		"route_source": "controller",
		"input_tokens": 120,
		"input_provenance": "provider_reported",
		"output_tokens": 34,
		"output_provenance": "proxy_reported",
		"source": "litellm.callback",
		"outcome": "PARTIAL_STREAM",
		"error_class": "network",
		"stream_state": "interrupted",
		"ttft_ms": 250,
		"latency_ms": 1900,
		"decision_id": "dec-9"
	}`)
	assert.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body)
	require.Len(t, sink.records, 1)
	got := sink.records[0]
	assert.Equal(t, "coding", got.TaskClass)
	assert.Equal(t, store.RouteController, got.RouteSource)
	assert.Equal(t, store.TokenProviderReported, got.InputProvenance)
	assert.Equal(t, store.TokenProxyReported, got.OutputProvenance)
	assert.Equal(t, store.ErrorNetwork, got.ErrorClass)
	assert.Equal(t, store.StreamInterrupted, got.StreamState)
	require.NotNil(t, got.TTFTMS, "ttft_ms = %v, want the measured 250", got.TTFTMS)
	assert.Equal(t, int64(250), *got.TTFTMS)
	require.NotNil(t, got.LatencyMS, "latency_ms = %v, want the measured 1900", got.LatencyMS)
	assert.Equal(t, int64(1900), *got.LatencyMS)
}

// A provenance without a count describes a number nobody observed, so it is
// dropped rather than stored: the row must not claim a count it does not have.
func TestUsageDropsProvenanceWithoutCount(t *testing.T) {
	sink := &memSink{}
	srv := New(nil, Options{Usage: sink, AuthToken: testToken})

	w := post(t, srv, testToken, `{"provider":"minimax","provenance":"reported","source":"agent"}`)
	assert.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body)
	require.Len(t, sink.records, 1)
	got := sink.records[0]
	assert.Empty(t, got.InputProvenance, "provenance = %q, want none without a count", got.InputProvenance)
	assert.Empty(t, got.OutputProvenance, "provenance = %q, want none without a count", got.OutputProvenance)
	assert.Nil(t, got.InputTokens, "tokens = %v, want unknown", got.InputTokens)
	assert.Nil(t, got.OutputTokens, "tokens = %v, want unknown", got.OutputTokens)
}

// The response is a status, not an echo: a report is not reflected back, so no
// request content ends up in a client's logs by accident.
func TestUsageResponseHasNoContent(t *testing.T) {
	srv := New(nil, Options{Usage: &memSink{}, AuthToken: testToken})
	w := post(t, srv, testToken, `{"provider":"minimax","provenance":"reported","source":"agent","request_id":"req-7"}`)
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body %q is not JSON", w.Body)
	assert.Len(t, body, 1, "body = %v, want only a status", body)
	assert.Equal(t, "recorded", body["status"], "body = %v, want only a status", body)
}
