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
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if len(sink.records) != 0 {
				t.Fatalf("stored %d records for a rejected report", len(sink.records))
			}
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
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}
	if len(sink.records) != 1 {
		t.Fatalf("records = %d, want 1", len(sink.records))
	}
	got := sink.records[0]
	if got.RequestID != "req-1" || got.Attempt != 2 || got.DecisionID != "dec-9" {
		t.Fatalf("identity = %q/%d/%q, want req-1/2/dec-9", got.RequestID, got.Attempt, got.DecisionID)
	}
	if got.Provenance != store.Reported || got.Outcome != store.OutcomePartialStream {
		t.Fatalf("provenance/outcome = %s/%s, want reported/PARTIAL_STREAM", got.Provenance, got.Outcome)
	}
	// A reported zero must survive as zero, not become "missing".
	if got.OutputTokens == nil || *got.OutputTokens != 0 {
		t.Fatalf("output_tokens = %v, want a reported 0", got.OutputTokens)
	}
	if got.ObservedAt.IsZero() {
		t.Fatal("observed_at was left unset: the arrival time must be recorded")
	}
}

// Missing usage is unknown, never zero, and a missing outcome is UNKNOWN.
func TestUsageKeepsMissingValuesUnknown(t *testing.T) {
	sink := &memSink{}
	srv := New(nil, Options{Usage: sink, AuthToken: testToken})

	w := post(t, srv, testToken, `{"provider":"minimax","provenance":"estimated","source":"agent"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}
	got := sink.records[0]
	if got.InputTokens != nil || got.OutputTokens != nil {
		t.Fatalf("tokens = %v/%v, want both unknown", got.InputTokens, got.OutputTokens)
	}
	if got.Outcome != store.OutcomeUnknown {
		t.Fatalf("outcome = %q, want UNKNOWN when the reporter did not say", got.Outcome)
	}
	if got.Attempt != 0 {
		t.Fatalf("attempt = %d, want 0 (the store maps it to the first attempt)", got.Attempt)
	}
}

// A caller sending a prompt or a conversation body must fail loudly instead of
// having the field silently dropped and its content lost.
func TestUsageRejectsUnknownFields(t *testing.T) {
	sink := &memSink{}
	srv := New(nil, Options{Usage: sink, AuthToken: testToken})

	w := post(t, srv, testToken, `{"provider":"minimax","provenance":"reported","source":"agent","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if len(sink.records) != 0 {
		t.Fatal("a report carrying unknown fields was stored")
	}
}

func TestUsageRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no provider", `{"provenance":"reported","source":"agent"}`},
		{"no source", `{"provider":"p","provenance":"reported"}`},
		{"bad provenance", `{"provider":"p","provenance":"guessed","source":"agent"}`},
		{"bad outcome", `{"provider":"p","provenance":"reported","source":"agent","outcome":"TIMEOUT"}`},
		{"negative attempt", `{"provider":"p","provenance":"reported","source":"agent","attempt":-1}`},
		{"negative tokens", `{"provider":"p","provenance":"reported","source":"agent","input_tokens":-5}`},
		{"bad timestamp", `{"provider":"p","provenance":"reported","source":"agent","observed_at":"yesterday"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &memSink{}
			srv := New(nil, Options{Usage: sink, AuthToken: testToken})
			if w := post(t, srv, testToken, tc.body); w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			if len(sink.records) != 0 {
				t.Fatal("an invalid report was stored")
			}
		})
	}
}

func TestUsageRejectsOversizedBody(t *testing.T) {
	srv := New(nil, Options{Usage: &memSink{}, AuthToken: testToken})
	big := `{"provider":"p","provenance":"reported","source":"agent","request_id":"` +
		strings.Repeat("x", maxUsageBody) + `"}`
	if w := post(t, srv, testToken, big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}

// A busy database is retryable, and the caller must be told that rather than
// being given a generic failure it would not retry.
func TestUsageReportsBusyAsRetryable(t *testing.T) {
	srv := New(nil, Options{Usage: &memSink{err: fmt.Errorf("write: %w", store.ErrBusy)}, AuthToken: testToken})
	if w := post(t, srv, testToken, `{"provider":"p","provenance":"reported","source":"agent"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}

	srv = New(nil, Options{Usage: &memSink{err: errors.New("boom")}, AuthToken: testToken})
	w := post(t, srv, testToken, `{"provider":"p","provenance":"reported","source":"agent"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	// The internal cause must not be echoed to the caller.
	if strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("response leaks the internal error: %s", w.Body)
	}
}

func TestUsageIsUnavailableWithoutASink(t *testing.T) {
	srv := New(nil, Options{AuthToken: testToken})
	if w := post(t, srv, testToken, `{"provider":"p","provenance":"reported","source":"agent"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// The response is a status, not an echo: a report is not reflected back, so no
// request content ends up in a client's logs by accident.
func TestUsageResponseHasNoContent(t *testing.T) {
	srv := New(nil, Options{Usage: &memSink{}, AuthToken: testToken})
	w := post(t, srv, testToken, `{"provider":"minimax","provenance":"reported","source":"agent","request_id":"req-7"}`)
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", w.Body, err)
	}
	if len(body) != 1 || body["status"] != "recorded" {
		t.Fatalf("body = %v, want only a status", body)
	}
}
