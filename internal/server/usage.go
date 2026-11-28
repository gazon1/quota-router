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
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gazon1/quota-router/internal/routing"
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
//
// Every field is optional except the ones the schema cannot do without
// (provider, source). The body is a closed set: an unknown field is rejected
// rather than ignored, so a report cannot slip a prompt through.
type usageReport struct {
	RequestID    string `json:"request_id"`
	Attempt      int    `json:"attempt"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	TaskClass    string `json:"task_class"`
	RouteSource  string `json:"route_source"`
	InputTokens  *int   `json:"input_tokens"`
	OutputTokens *int   `json:"output_tokens"`
	// Provenance is the row-level fallback for reporters that do not name a
	// channel per count. InputProvenance and OutputProvenance win over it.
	Provenance string `json:"provenance"`
	// InputProvenance and OutputProvenance say which channel produced the
	// matching count. A count without a provenance is rejected.
	InputProvenance  string `json:"input_provenance"`
	OutputProvenance string `json:"output_provenance"`
	Source           string `json:"source"`
	Outcome          string `json:"outcome"`
	ErrorClass       string `json:"error_class"`
	StreamState      string `json:"stream_state"`
	TTFTMS           *int64 `json:"ttft_ms"`
	LatencyMS        *int64 `json:"latency_ms"`
	DecisionID       string `json:"decision_id"`
	ObservedAt       string `json:"observed_at"`
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		// Without a token the route would be an unauthenticated write into the
		// database, so an unset token refuses every report.
		s.writeError(w, r, http.StatusUnauthorized, errorCodeUnauthorized, "invalid or missing bearer token", false)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUsageBody))
	if err != nil {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, errorCodePayloadTooLarge, "report too large", false)
		return
	}
	var rep usageReport
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		s.writeError(w, r, http.StatusBadRequest, errorCodeInvalidRequest, "malformed report: "+err.Error(), false)
		return
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		s.writeError(w, r, http.StatusBadRequest, errorCodeInvalidRequest, "malformed report: unexpected content after JSON object", false)
		return
	}

	rec, err := rep.record(time.Now())
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, errorCodeValidation, err.Error(), false)
		return
	}
	if s.usage == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, errorCodeUnavailable, "usage ingestion is not configured", false)
		return
	}
	if err := s.usage.SaveUsageRecord(r.Context(), rec); err != nil {
		// A busy database is the caller's to retry, and the identity makes that
		// safe: the second report stores nothing. A reused event id for different
		// content is a conflict, not a retry of the same event.
		if errors.Is(err, store.ErrUsageConflict) {
			s.writeError(w, r, http.StatusConflict, errorCodeConflict, "usage event id already reported with different content", false)
			return
		}
		if errors.Is(err, store.ErrBusy) {
			s.writeError(w, r, http.StatusServiceUnavailable, errorCodeUnavailable, "database busy, retry", true)
			return
		}
		s.log.ErrorContext(r.Context(), "persist usage record", "error", err)
		s.writeError(w, r, http.StatusInternalServerError, errorCodeInternal, "could not store the report", false)
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
	var observedAt time.Time
	observedAtReported := r.ObservedAt != ""
	if observedAtReported {
		t, err := time.Parse(time.RFC3339Nano, r.ObservedAt)
		if err != nil {
			return store.UsageRecord{}, errors.New("observed_at must be RFC 3339")
		}
		observedAt = t
	}
	if observedAt.IsZero() {
		observedAt = now
	}
	taskClass := strings.TrimSpace(r.TaskClass)
	if taskClass != "" && !routing.TaskClass(taskClass).Known() {
		return store.UsageRecord{}, fmt.Errorf("task_class %q is not a known task class", taskClass)
	}
	inputProvenance, err := provenance("input", r.InputProvenance, r.Provenance, r.InputTokens != nil)
	if err != nil {
		return store.UsageRecord{}, err
	}
	outputProvenance, err := provenance("output", r.OutputProvenance, r.Provenance, r.OutputTokens != nil)
	if err != nil {
		return store.UsageRecord{}, err
	}
	rec := store.UsageRecord{
		RequestID:          strings.TrimSpace(r.RequestID),
		Attempt:            r.Attempt,
		Provider:           strings.TrimSpace(r.Provider),
		Model:              strings.TrimSpace(r.Model),
		TaskClass:          taskClass,
		RouteSource:        store.RouteSource(strings.TrimSpace(r.RouteSource)),
		InputTokens:        r.InputTokens,
		OutputTokens:       r.OutputTokens,
		InputProvenance:    inputProvenance,
		OutputProvenance:   outputProvenance,
		Source:             strings.TrimSpace(r.Source),
		Outcome:            store.Outcome(r.Outcome),
		ErrorClass:         store.ErrorClass(strings.TrimSpace(r.ErrorClass)),
		StreamState:        store.StreamState(strings.TrimSpace(r.StreamState)),
		TTFTMS:             r.TTFTMS,
		LatencyMS:          r.LatencyMS,
		DecisionID:         strings.TrimSpace(r.DecisionID),
		ObservedAt:         observedAt,
		ObservedAtReported: observedAtReported,
		ReceivedAt:         now,
	}
	if rec.Outcome == "" {
		rec.Outcome = store.OutcomeUnknown
	}
	if err := rec.Validate(); err != nil {
		return store.UsageRecord{}, err
	}
	return rec, nil
}

// provenance resolves the channel of a token count. The count's own provenance
// wins; the row-level value is the fallback for reporters that name one
// channel for the whole report.
//
// An unknown channel is rejected here, before any count is considered: a
// reporter that names a channel nobody can aggregate on has made a mistake,
// and dropping it silently would hide that. A known channel without a count is
// dropped instead: it names the channel of a number that was never observed,
// and keeping it would make the stored row claim a count it does not have.
func provenance(name, specific, fallback string, countPresent bool) (store.TokenProvenance, error) {
	value := strings.TrimSpace(specific)
	if value == "" {
		value = strings.TrimSpace(fallback)
	}
	if value == "" {
		return "", nil
	}
	p := store.TokenProvenance(value)
	if !p.Valid() {
		return "", fmt.Errorf("%s_provenance %q is not a documented provenance", name, value)
	}
	if !countPresent {
		return "", nil
	}
	return p, nil
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
