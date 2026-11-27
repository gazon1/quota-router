// Request-usage ingestion: one report is one attempt of one request, and the
// store deduplicates on (request_id, attempt).
//
// The body is a closed set of fields and unknown ones are rejected rather than
// ignored, so a caller cannot slip a prompt or a conversation history through a
// report and have it silently dropped — it fails instead. Nothing in the body
// is logged or echoed back.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gazon1/quota-router/internal/store"
)

// UsageSink persists one attempt. store.Store implements it.
type UsageSink interface {
	SaveUsageRecord(ctx context.Context, u store.UsageRecord) error
}

// maxUsageBody bounds one report. A usage record is a few hundred bytes; the
// limit exists so an unterminated body cannot hold a connection open.
const maxUsageBody = 64 << 10

// usageReport is the wire form. Pointers keep "not supplied" distinct from a
// reported zero, so a missing token count is stored as NULL rather than 0.
type usageReport struct {
	RequestID    string `json:"request_id"`
	Attempt      int    `json:"attempt"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	InputTokens  *int   `json:"input_tokens"`
	OutputTokens *int   `json:"output_tokens"`
	Provenance   string `json:"provenance"`
	Source       string `json:"source"`
	Outcome      string `json:"outcome"`
	DecisionID   string `json:"decision_id"`
	ObservedAt   string `json:"observed_at"`
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		// Without a token the route would be an unauthenticated write into the
		// database, so an unset token refuses every report.
		s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing bearer token"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUsageBody))
	if err != nil {
		s.writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "report too large"})
		return
	}
	var rep usageReport
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed report: " + err.Error()})
		return
	}

	rec, err := rep.record(time.Now())
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if s.usage == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "usage ingestion is not configured"})
		return
	}
	if err := s.usage.SaveUsageRecord(r.Context(), rec); err != nil {
		// A busy database is the caller's to retry, and the identity makes that
		// safe: the second report stores nothing.
		if errors.Is(err, store.ErrBusy) {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database busy, retry"})
			return
		}
		s.log.ErrorContext(r.Context(), "persist usage record", "error", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not store the report"})
		return
	}
	// 202, not 201: a repeated report for an already-stored attempt writes
	// nothing and is still a success, so the caller need not tell them apart.
	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "recorded"})
}

// record converts the wire form. Missing values stay missing: no token count is
// invented, an absent outcome is UNKNOWN and an absent observed_at is the time
// the report arrived.
func (r usageReport) record(now time.Time) (store.UsageRecord, error) {
	rec := store.UsageRecord{
		RequestID:    strings.TrimSpace(r.RequestID),
		Attempt:      r.Attempt,
		Provider:     strings.TrimSpace(r.Provider),
		Model:        strings.TrimSpace(r.Model),
		InputTokens:  r.InputTokens,
		OutputTokens: r.OutputTokens,
		Provenance:   store.Confidence(r.Provenance),
		Source:       strings.TrimSpace(r.Source),
		Outcome:      store.Outcome(r.Outcome),
		DecisionID:   strings.TrimSpace(r.DecisionID),
		ObservedAt:   now,
	}
	if r.ObservedAt != "" {
		t, err := time.Parse(time.RFC3339Nano, r.ObservedAt)
		if err != nil {
			return store.UsageRecord{}, errors.New("observed_at must be RFC 3339")
		}
		rec.ObservedAt = t
	}
	if rec.Outcome == "" {
		rec.Outcome = store.OutcomeUnknown
	}
	if err := rec.Validate(); err != nil {
		return store.UsageRecord{}, err
	}
	return rec, nil
}

// authorized compares the bearer token in constant time. An unconfigured token
// refuses every report: there is no unauthenticated mode.
func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(h[len(prefix):])), []byte(s.token)) == 1
}
