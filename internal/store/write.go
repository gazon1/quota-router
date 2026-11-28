// Writes, latest-state reads and retention.
//
// Every write is explicit SQL in one transaction, so a snapshot and its windows
// land together or not at all. A failed write returns the classified error and
// changes nothing: callers keep their in-memory state and must not overwrite it
// with a partial read.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// tsLayout is the fixed-width UTC layout every timestamp column uses, so
// retention can compare text and a backup stays readable.
//
// Nanoseconds, not milliseconds: observations that differ only below a
// millisecond would otherwise collapse into a tie, and the tie is broken by id —
// so a stale observation that arrived late could outrank a newer one and become
// the current state. RFC 3339 with 9 fractional digits keeps the fixed width and
// lexicographic order.
const tsLayout = "2006-01-02T15:04:05.000000000Z"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

// SaveQuotaSnapshot appends one snapshot with its windows.
func (s *Store) SaveQuotaSnapshot(ctx context.Context, snap QuotaSnapshot) error {
	if err := snap.Validate(); err != nil {
		return fmt.Errorf("quota snapshot: %w", err)
	}
	var reason *string
	if snap.UnknownReason != nil {
		r := string(*snap.UnknownReason)
		reason = &r
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO quota_snapshots (provider, state, unknown_reason, source, confidence, observed_at, error)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			snap.Provider, snap.State, reason, snap.Source, snap.Confidence, ts(snap.ObservedAt), nullString(snap.Error))
		if err != nil {
			return classify(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return classify(err)
		}
		for _, w := range snap.Windows {
			var resetAt *string
			if w.ResetAt != nil {
				r := ts(*w.ResetAt)
				resetAt = &r
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO quota_windows (snapshot_id, window, unit, limit_value, used_value, remaining_value, reset_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				id, w.Window, nullString(w.Unit), w.Limit, w.Used, w.Remaining, resetAt); err != nil {
				return classify(err)
			}
		}
		return nil
	})
}

// SaveUsageRecord appends one request attempt. A report for an attempt that is
// already stored is ignored when the event content is identical, so a retried
// report does not double-count tokens; a retry is a different attempt and gets
// its own row. An empty outcome is stored as UNKNOWN and attempt 0 as the first
// attempt, because neither is a value this store may invent. Reusing an event
// identity for different content is ErrUsageConflict, not an overwrite.
func (s *Store) SaveUsageRecord(ctx context.Context, u UsageRecord) error {
	if u.Attempt == 0 {
		u.Attempt = 1
	}
	if u.Outcome == "" {
		u.Outcome = OutcomeUnknown
	}
	observedAtSet := u.ObservedAtReported
	if u.ObservedAt.IsZero() {
		u.ObservedAt = time.Now().UTC()
	}
	if err := u.Validate(); err != nil {
		return fmt.Errorf("usage record: %w", err)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO usage_records
			   (request_id, attempt, provider, model, input_tokens, output_tokens, provenance, source, outcome, decision_id, latency_ms, error_class, observed_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (request_id, attempt) DO NOTHING`,
			nullString(u.RequestID), u.Attempt, u.Provider, nullString(u.Model), u.InputTokens, u.OutputTokens,
			u.Provenance, u.Source, u.Outcome, nullString(u.DecisionID), u.LatencyMS, nullString(string(u.ErrorClass)), ts(u.ObservedAt))
		if err != nil {
			return classify(err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return classify(err)
		}
		if rows == 0 {
			// The attempt identity was taken by an earlier report. The same
			// event redelivered is a no-op; the same identity carrying different
			// content is a conflicting reuse and fails the write.
			return s.usageConflict(ctx, tx, u, observedAtSet)
		}
		return nil
	})
}

// usageConflict decides what a skipped duplicate means. A redelivered event with
// identical content leaves the stored attempt alone and succeeds; the same event
// id reused for different content is a conflict, not a retry, and must surface.
// observedAtSet reports whether the reporter supplied an observation timestamp;
// a redelivery that omitted it carries server-stamped arrival bookkeeping, not
// event content, so it must not conflict on that alone.
func (s *Store) usageConflict(ctx context.Context, tx *sql.Tx, u UsageRecord, observedAtSet bool) error {
	var (
		provider   string
		model      sql.NullString
		input      sql.NullInt64
		output     sql.NullInt64
		prov       string
		src        string
		outcome    string
		decision   sql.NullString
		latency    sql.NullInt64
		errClass   sql.NullString
		observedAt string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT provider, model, input_tokens, output_tokens, provenance, source, outcome, decision_id, latency_ms, error_class, observed_at
		 FROM usage_records WHERE request_id = ? AND attempt = ?`,
		u.RequestID, u.Attempt).Scan(&provider, &model, &input, &output, &prov, &src, &outcome, &decision, &latency, &errClass, &observedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUsageConflict
		}
		return classify(err)
	}
	storedObservedAt, err := parseTS(observedAt)
	if err != nil {
		return fmt.Errorf("usage record: stored observed_at: %w", err)
	}
	existing := UsageRecord{
		RequestID:    u.RequestID,
		Attempt:      u.Attempt,
		Provider:     provider,
		Model:        model.String,
		InputTokens:  intFromNull(&input),
		OutputTokens: intFromNull(&output),
		Provenance:   Confidence(prov),
		Source:       src,
		Outcome:      Outcome(outcome),
		DecisionID:   decision.String,
		LatencyMS:    intFromNull(&latency),
		ErrorClass:   ErrorClass(errClass.String),
		ObservedAt:   storedObservedAt,
	}
	if existing.Provider == "" || existing.Source == "" || existing.Outcome == "" {
		return ErrUsageConflict
	}
	same := existing.Provider == u.Provider &&
		existing.Model == u.Model &&
		intValueEqual(existing.InputTokens, u.InputTokens) &&
		intValueEqual(existing.OutputTokens, u.OutputTokens) &&
		existing.Provenance == u.Provenance &&
		existing.Source == u.Source &&
		existing.Outcome == u.Outcome &&
		existing.DecisionID == u.DecisionID &&
		intValueEqual(existing.LatencyMS, u.LatencyMS) &&
		existing.ErrorClass == u.ErrorClass
	if same && observedAtSet && !existing.ObservedAt.Equal(u.ObservedAt) {
		same = false
	}
	if !same {
		return ErrUsageConflict
	}
	return nil
}

// intValueEqual compares optional counts without dereferencing a nil: an absent
// count is unknown, not zero, and must not match a reported zero.
func intValueEqual(a, b *int) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func intFromNull(v *sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

// SaveProviderHealth appends one health observation.
func (s *Store) SaveProviderHealth(ctx context.Context, h HealthRecord) error {
	if err := h.Validate(); err != nil {
		return fmt.Errorf("provider health: %w", err)
	}
	var cooldownUntil *string
	if h.CooldownUntil != nil && !h.CooldownUntil.IsZero() {
		v := ts(*h.CooldownUntil)
		cooldownUntil = &v
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO provider_health (provider, state, observed_at, error, cooldown_until) VALUES (?, ?, ?, ?, ?)`,
			h.Provider, h.State, ts(h.ObservedAt), nullString(h.Error), cooldownUntil)
		return classify(err)
	})
}

// SavePollState records when a provider was last polled and, when the attempt
// established quota, when it last did. A zero successAt leaves the recorded
// success alone: a failure must not erase when quota was last known good, and
// the two timestamps are read independently.
func (s *Store) SavePollState(ctx context.Context, provider string, attemptAt, successAt time.Time) error {
	if provider == "" {
		return errors.New("poll state: provider must not be empty")
	}
	if attemptAt.IsZero() {
		return errors.New("poll state: attempt time must be set")
	}
	var success *string
	if !successAt.IsZero() {
		v := ts(successAt)
		success = &v
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO poll_state (provider, last_attempt_at, last_success_at) VALUES (?, ?, ?)
			 ON CONFLICT (provider) DO UPDATE SET
			   last_attempt_at = excluded.last_attempt_at,
			   last_success_at = COALESCE(excluded.last_success_at, poll_state.last_success_at)`,
			provider, ts(attemptAt), success)
		return classify(err)
	})
}

// PollState reports when a provider was last polled and last succeeded. A zero
// success time means no attempt has established quota yet.
func (s *Store) PollState(ctx context.Context, provider string) (attemptAt, successAt time.Time, err error) {
	var attempt, success sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT last_attempt_at, last_success_at FROM poll_state WHERE provider = ?`, provider).
		Scan(&attempt, &success)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, time.Time{}, classify(err)
	}
	if attemptAt, err = parseTS(attempt.String); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if success.Valid {
		if successAt, err = parseTS(success.String); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	return attemptAt, successAt, nil
}

// SaveControllerState stores one controller key. The latest write wins.
func (s *Store) SaveControllerState(ctx context.Context, key, value string, updatedAt time.Time) error {
	if key == "" {
		return errors.New("controller state: key must not be empty")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO controller_state (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, value, ts(updatedAt))
		return classify(err)
	})
}

// ControllerState reads one controller key; ok is false when it was never written.
func (s *Store) ControllerState(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT value FROM controller_state WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, classify(err)
	}
	return value, true, nil
}

// LatestQuotaSnapshot returns the newest snapshot per provider with its windows.
// It is routing state, not history: retention keeps these rows. The row is
// chosen by newestPerProvider, the same predicate retention spares.
func (s *Store) LatestQuotaSnapshot(ctx context.Context) ([]QuotaSnapshot, error) {
	// One read transaction: a prune running between the snapshot query and the
	// window queries would otherwise delete the selected rows (and cascade their
	// windows), so the caller would get a snapshot with no windows.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, classify(err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, provider, state, unknown_reason, source, confidence, observed_at, error
		 FROM quota_snapshots WHERE `+newestPerProvider("quota_snapshots", true))
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	var out []QuotaSnapshot
	ids := []int64{}
	for rows.Next() {
		var (
			id              int64
			snap            QuotaSnapshot
			reason, errText sql.NullString
			observedAt      string
		)
		if err := rows.Scan(&id, &snap.Provider, &snap.State, &reason, &snap.Source, &snap.Confidence, &observedAt, &errText); err != nil {
			return nil, classify(err)
		}
		if snap.ObservedAt, err = parseTS(observedAt); err != nil {
			return nil, err
		}
		if reason.Valid {
			r := UnknownReason(reason.String)
			snap.UnknownReason = &r
		}
		snap.Error = errText.String
		out = append(out, snap)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	// Windows are read on the same transaction, before it goes away.
	rows.Close()
	for i, id := range ids {
		windows, err := windows(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out[i].Windows = windows
	}
	return out, nil
}

func windows(ctx context.Context, tx *sql.Tx, snapshotID int64) ([]QuotaWindow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT window, unit, limit_value, used_value, remaining_value, reset_at
		 FROM quota_windows WHERE snapshot_id = ? ORDER BY window`, snapshotID)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	var out []QuotaWindow
	for rows.Next() {
		var (
			w       QuotaWindow
			unit    sql.NullString
			resetAt sql.NullString
		)
		if err := rows.Scan(&w.Window, &unit, &w.Limit, &w.Used, &w.Remaining, &resetAt); err != nil {
			return nil, classify(err)
		}
		w.Unit = unit.String
		if resetAt.Valid {
			t, err := parseTS(resetAt.String)
			if err != nil {
				return nil, err
			}
			w.ResetAt = &t
		}
		out = append(out, w)
	}
	return out, classify(rows.Err())
}

// LatestProviderHealth returns the newest health observation per provider.
func (s *Store) LatestProviderHealth(ctx context.Context) ([]HealthRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider, state, observed_at, error, cooldown_until
		 FROM provider_health WHERE `+newestPerProvider("provider_health", true))
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	var out []HealthRecord
	for rows.Next() {
		var (
			h             HealthRecord
			observed      string
			errText       sql.NullString
			cooldownUntil sql.NullString
		)
		if err := rows.Scan(&h.Provider, &h.State, &observed, &errText, &cooldownUntil); err != nil {
			return nil, classify(err)
		}
		if h.ObservedAt, err = parseTS(observed); err != nil {
			return nil, err
		}
		h.Error = errText.String
		if cooldownUntil.Valid {
			t, err := parseTS(cooldownUntil.String)
			if err != nil {
				return nil, err
			}
			h.CooldownUntil = &t
		}
		out = append(out, h)
	}
	return out, classify(rows.Err())
}

// Retention bounds history. Zero disables pruning for that table. The latest row
// per provider (or overall, for usage) is always kept, so cleanup can never
// remove the state routing depends on.
type Retention struct {
	QuotaSnapshots   time.Duration
	ProviderHealth   time.Duration
	UsageRecords     time.Duration
	RoutingDecisions time.Duration
}

// Prune deletes rows older than the retention bounds, keeping the newest row per
// provider even when it is older than the bound. It returns the deleted counts.
//
// "Newest" means the same row LatestQuotaSnapshot reads — max observed_at per
// provider, not max id — so an out-of-order write cannot make cleanup delete the
// state routing depends on.
func (s *Store) Prune(ctx context.Context, r Retention, now time.Time) (deleted map[string]int64, err error) {
	deleted = map[string]int64{}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		// Expired affinity is not history kept by a retention bound: its own TTL
		// is what says it stopped meaning anything, so it goes regardless of the
		// configured ages. Readers already refuse it; this only stops the table
		// from growing with sessions nobody released. Reported only when rows
		// actually went, so a zero-retention run still reports nothing deleted.
		res, err := tx.ExecContext(ctx, `DELETE FROM session_affinity WHERE expires_at <= ?`, ts(now))
		if err != nil {
			return classify(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return classify(err)
		}
		if n > 0 {
			deleted["session_affinity"] = n
		}

		for _, p := range []struct {
			table string
			age   time.Duration
			stmt  string
		}{
			// quota_windows go with their snapshot through ON DELETE CASCADE.
			// The spared ids are materialized: inside a DELETE the window
			// function would otherwise be re-evaluated as rows disappear, and
			// a later row would take the top rank and be spared too.
			{"quota_snapshots", r.QuotaSnapshots,
				"WITH spared AS MATERIALIZED (" + newestPerProvider("quota_snapshots", false) + ") " +
					"DELETE FROM quota_snapshots WHERE observed_at < ? AND id NOT IN (SELECT id FROM spared)"},
			{"provider_health", r.ProviderHealth,
				"WITH spared AS MATERIALIZED (" + newestPerProvider("provider_health", false) + ") " +
					"DELETE FROM provider_health WHERE observed_at < ? AND id NOT IN (SELECT id FROM spared)"},
			{"usage_records", r.UsageRecords,
				"DELETE FROM usage_records WHERE observed_at < ?"},
			{"routing_decisions", r.RoutingDecisions,
				"DELETE FROM routing_decisions WHERE created_at < ?"},
		} {
			if p.age <= 0 {
				continue
			}
			res, err := tx.ExecContext(ctx, p.stmt, ts(now.Add(-p.age)))
			if err != nil {
				return classify(err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return classify(err)
			}
			deleted[p.table] = n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return deleted, nil
}

// tx runs fn in one transaction and rolls back on any error.
func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classify(err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return classify(err)
	}
	return nil
}

// newestPerProvider is the one row per provider that the latest readers return:
// the newest observed_at, ties broken by the highest id. Retention spares
// exactly that row and the readers select it, so cleanup can never delete what
// routing depends on. usedAsPredicate reads the row condition the readers put
// after WHERE; the bare form selects the ids for retention's NOT IN.
func newestPerProvider(table string, usedAsPredicate bool) string {
	ranked := "(SELECT id, ROW_NUMBER() OVER (PARTITION BY provider ORDER BY observed_at DESC, id DESC) rn FROM " + table + ")"
	if usedAsPredicate {
		return "id IN (SELECT id FROM " + ranked + " WHERE rn = 1)"
	}
	return "SELECT id FROM " + ranked + " WHERE rn = 1"
}

func parseTS(v string) (time.Time, error) {
	t, err := time.Parse(tsLayout, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("stored timestamp %q: %w", v, err)
	}
	return t, nil
}

func nullString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// SaveRoutingDecision appends a routing decision audit record together with the
// candidates the selector evaluated.
//
// Both are written in one transaction: a decision whose candidates are missing
// is not valid history, and a candidate row without its decision is an orphan.
// A failure anywhere in the write leaves the database exactly as it was.
func (s *Store) SaveRoutingDecision(ctx context.Context, r RoutingDecisionRecord) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("routing decision: %w", err)
	}
	var affinityExpiresAt *string
	if r.AffinityExpiresAt != nil && !r.AffinityExpiresAt.IsZero() {
		v := ts(*r.AffinityExpiresAt)
		affinityExpiresAt = &v
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO routing_decisions
			   (decision_id, policy_version, session_id, task_class, requested_alias, requested_provider,
			    selected_alias, selected_provider, selected_reason_code, selected_reason, affinity_expires_at,
			    payload, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.DecisionID, r.PolicyVersion, r.SessionID, nullString(r.TaskClass), nullString(r.RequestedAlias),
			nullString(r.RequestedProvider), nullString(r.SelectedAlias), nullString(r.SelectedProvider),
			nullString(r.SelectedReasonCode), nullString(r.SelectedReason), affinityExpiresAt,
			r.Payload, ts(r.CreatedAt))
		if err != nil {
			return classify(err)
		}
		rowID, err := res.LastInsertId()
		if err != nil {
			return classify(err)
		}
		for _, c := range r.Candidates {
			if err := insertDecisionCandidate(ctx, tx, rowID, r.DecisionID, r.CreatedAt, c); err != nil {
				return err
			}
		}
		return nil
	})
}

// insertDecisionCandidate writes one evaluated candidate of a decision. The
// values that were not observed are written as NULL, never as a zero or a
// healthy-looking default.
func insertDecisionCandidate(ctx context.Context, tx *sql.Tx, rowID int64, decisionID string, createdAt time.Time, c RoutingDecisionCandidate) error {
	var quotaObservedAt, quotaResetAt, healthObservedAt, cooldownUntil *string
	for _, f := range []struct {
		dst **string
		src *time.Time
	}{
		{&quotaObservedAt, c.QuotaObservedAt},
		{&quotaResetAt, c.QuotaResetAt},
		{&healthObservedAt, c.HealthObservedAt},
		{&cooldownUntil, c.CooldownUntil},
	} {
		if f.src == nil || f.src.IsZero() {
			continue
		}
		v := ts(*f.src)
		*f.dst = &v
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO routing_decision_candidates
		   (decision_row_id, decision_id, ordinal, alias, provider, selected, eligible, eliminated_stage,
		    reason_code, reason, transient, quota_state, quota_unknown_reason, quota_observed_at, quota_reset_at,
		    health_state, health_reason, health_observed_at, cooldown_until, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rowID, decisionID, c.Ordinal, c.Alias, c.Provider, boolInt(c.Selected), boolInt(c.Eligible),
		nullString(c.EliminatedStage), c.ReasonCode, nullString(c.Reason), boolInt(c.Transient),
		nullString(c.QuotaState), nullString(c.QuotaUnknownReason), quotaObservedAt, quotaResetAt,
		nullString(c.HealthState), nullString(c.HealthReason), healthObservedAt, cooldownUntil, ts(createdAt))
	return classify(err)
}

// boolInt maps a flag to the integer SQLite stores in a CHECK-bounded column.
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// LatestRoutingDecisions returns the newest routing decisions, newest first,
// each with the candidate set that was evaluated for it.
func (s *Store) LatestRoutingDecisions(ctx context.Context, limit int) ([]RoutingDecisionRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, decision_id, policy_version, session_id, task_class, requested_alias, requested_provider,
		        selected_alias, selected_provider, selected_reason_code, selected_reason, affinity_expires_at,
		        payload, created_at
		 FROM routing_decisions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	var (
		out   []*RoutingDecisionRecord
		rowID []int64
	)
	for rows.Next() {
		var (
			r                RoutingDecisionRecord
			id               int64
			taskClass        sql.NullString
			requestedAlias   sql.NullString
			requestedProvide sql.NullString
			selectedAlias    sql.NullString
			selectedProvider sql.NullString
			selectedCode     sql.NullString
			selectedReason   sql.NullString
			affinityExpires  sql.NullString
			createdAt        string
		)
		if err := rows.Scan(&id, &r.DecisionID, &r.PolicyVersion, &r.SessionID, &taskClass, &requestedAlias,
			&requestedProvide, &selectedAlias, &selectedProvider, &selectedCode, &selectedReason,
			&affinityExpires, &r.Payload, &createdAt); err != nil {
			return nil, classify(err)
		}
		r.TaskClass = taskClass.String
		r.RequestedAlias = requestedAlias.String
		r.RequestedProvider = requestedProvide.String
		r.SelectedAlias = selectedAlias.String
		r.SelectedProvider = selectedProvider.String
		r.SelectedReasonCode = selectedCode.String
		r.SelectedReason = selectedReason.String
		if affinityExpires.Valid {
			t, err := parseTS(affinityExpires.String)
			if err != nil {
				return nil, err
			}
			r.AffinityExpiresAt = &t
		}
		if r.CreatedAt, err = parseTS(createdAt); err != nil {
			return nil, err
		}
		rec := r
		out = append(out, &rec)
		rowID = append(rowID, id)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	rows.Close()

	// The candidates are read for the decisions this call returned, in one
	// query: a candidate row belongs to the decision it was evaluated for, and
	// a decision without its candidates is not valid history.
	candidates, err := s.decisionCandidates(ctx, rowID)
	if err != nil {
		return nil, err
	}
	result := make([]RoutingDecisionRecord, 0, len(out))
	for i, rec := range out {
		rec.Candidates = candidates[rowID[i]]
		result = append(result, *rec)
	}
	return result, nil
}

// decisionCandidates reads the candidate rows of the given decision rows,
// grouped by the decision row they belong to.
func (s *Store) decisionCandidates(ctx context.Context, rowIDs []int64) (map[int64][]RoutingDecisionCandidate, error) {
	out := map[int64][]RoutingDecisionCandidate{}
	if len(rowIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(rowIDs)), ",")
	args := make([]any, 0, len(rowIDs))
	for _, id := range rowIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT decision_row_id, ordinal, alias, provider, selected, eligible, eliminated_stage, reason_code,
		        reason, transient, quota_state, quota_unknown_reason, quota_observed_at, quota_reset_at,
		        health_state, health_reason, health_observed_at, cooldown_until
		 FROM routing_decision_candidates WHERE decision_row_id IN (`+placeholders+`) ORDER BY decision_row_id, ordinal`, args...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			rowID                           int64
			c                               RoutingDecisionCandidate
			selected, eligible, transient   int
			eliminatedStage                 sql.NullString
			reason                          sql.NullString
			quotaState, quotaReason         sql.NullString
			healthState, healthReason       sql.NullString
			quotaObservedAt, quotaResetAt   sql.NullString
			healthObservedAt, cooldownUntil sql.NullString
		)
		if err := rows.Scan(&rowID, &c.Ordinal, &c.Alias, &c.Provider, &selected, &eligible, &eliminatedStage,
			&c.ReasonCode, &reason, &transient, &quotaState, &quotaReason, &quotaObservedAt, &quotaResetAt,
			&healthState, &healthReason, &healthObservedAt, &cooldownUntil); err != nil {
			return nil, classify(err)
		}
		c.Selected = selected == 1
		c.Eligible = eligible == 1
		c.Transient = transient == 1
		c.EliminatedStage = eliminatedStage.String
		c.Reason = reason.String
		c.QuotaState = quotaState.String
		c.QuotaUnknownReason = quotaReason.String
		c.HealthState = healthState.String
		c.HealthReason = healthReason.String
		for _, f := range []struct {
			dst **time.Time
			src sql.NullString
		}{
			{&c.QuotaObservedAt, quotaObservedAt},
			{&c.QuotaResetAt, quotaResetAt},
			{&c.HealthObservedAt, healthObservedAt},
			{&c.CooldownUntil, cooldownUntil},
		} {
			if !f.src.Valid {
				continue
			}
			t, err := parseTS(f.src.String)
			if err != nil {
				return nil, err
			}
			*f.dst = &t
		}
		out[rowID] = append(out[rowID], c)
	}
	return out, classify(rows.Err())
}

// SaveSessionAffinity pins a session to a route. The latest write wins, except
// for created_at: a re-pin moves the alias and its expiry but not when the
// session was first pinned. Re-pinning the same alias with a fresh expiry is how
// an active session keeps its route alive.
func (s *Store) SaveSessionAffinity(ctx context.Context, a SessionAffinity) error {
	if err := a.Validate(); err != nil {
		return fmt.Errorf("session affinity: %w", err)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO session_affinity (session_id, alias, created_at, updated_at, expires_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (session_id) DO UPDATE SET
			   alias = excluded.alias,
			   updated_at = excluded.updated_at,
			   expires_at = excluded.expires_at`,
			a.SessionID, a.Alias, ts(a.CreatedAt), ts(a.UpdatedAt), ts(a.ExpiresAt))
		return classify(err)
	})
}

// SessionAffinity reads one session's pin; ok is false when none is stored. An
// expired pin is returned as found: the caller owns the clock it evaluates the
// pin against, and this preserves ok=false for "never pinned" only.
func (s *Store) SessionAffinity(ctx context.Context, sessionID string) (SessionAffinity, bool, error) {
	var (
		a                     SessionAffinity
		created, updated, exp string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT session_id, alias, created_at, updated_at, expires_at FROM session_affinity WHERE session_id = ?`, sessionID).
		Scan(&a.SessionID, &a.Alias, &created, &updated, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionAffinity{}, false, nil
	}
	if err != nil {
		return SessionAffinity{}, false, classify(err)
	}
	for _, f := range []struct {
		dst *time.Time
		src string
	}{{&a.CreatedAt, created}, {&a.UpdatedAt, updated}, {&a.ExpiresAt, exp}} {
		t, err := parseTS(f.src)
		if err != nil {
			return SessionAffinity{}, false, err
		}
		*f.dst = t
	}
	return a, true, nil
}

// DeleteSessionAffinity removes a session's pin. A session that was never pinned
// is not an error: releasing twice is the documented, repeatable behaviour.
func (s *Store) DeleteSessionAffinity(ctx context.Context, sessionID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM session_affinity WHERE session_id = ?`, sessionID)
		return classify(err)
	})
}
