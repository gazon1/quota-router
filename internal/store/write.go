// Writes, latest-state reads and retention.
//
// Every write is explicit SQL in one transaction, so a snapshot and its windows
// land together or not at all. A failed write returns the classified error and
// changes nothing: callers keep their in-memory state and must not overwrite it
// with a partial read.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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

// SaveUsageRecord appends one request/outcome event. A report for an attempt
// that is already stored is ignored when the event content is identical, so a
// retried report does not double-count tokens; a retry is a different attempt
// and gets its own row. An empty outcome is stored as UNKNOWN and attempt 0 as
// the first attempt, because neither is a value this store may invent. Reusing
// an event identity for different content is ErrUsageConflict, not an
// overwrite.
func (s *Store) SaveUsageRecord(ctx context.Context, u UsageRecord) error {
	if u.Attempt == 0 {
		u.Attempt = 1
	}
	if u.Outcome == "" {
		u.Outcome = OutcomeUnknown
	}
	if u.SchemaVersion == 0 {
		u.SchemaVersion = EventSchemaVersion
	}
	observedAtSet := u.ObservedAtReported
	if u.ObservedAt.IsZero() {
		u.ObservedAt = time.Now().UTC()
	}
	if u.ReceivedAt.IsZero() {
		u.ReceivedAt = time.Now().UTC()
	}
	if u.EventID == "" {
		u.EventID = eventID(u.RequestID, u.Attempt)
	}
	if err := u.Validate(); err != nil {
		return fmt.Errorf("usage record: %w", err)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO usage_records
			   (request_id, attempt, event_id, schema_version, provider, model, task_class, route_source,
			    input_tokens, input_provenance, output_tokens, output_provenance, source, outcome, error_class,
			    stream_state, ttft_ms, latency_ms, decision_id, observed_at, observed_at_reported, received_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (request_id, attempt) DO NOTHING`,
			nullString(u.RequestID), u.Attempt, u.EventID, u.SchemaVersion,
			u.Provider, nullString(u.Model), nullString(u.TaskClass), nullString(string(u.RouteSource)),
			u.InputTokens, nullString(string(u.InputProvenance)),
			u.OutputTokens, nullString(string(u.OutputProvenance)),
			u.Source, u.Outcome, nullString(string(u.ErrorClass)), nullString(string(u.StreamState)),
			u.TTFTMS, u.LatencyMS, nullString(u.DecisionID),
			ts(u.ObservedAt), u.ObservedAtReported, ts(u.ReceivedAt))
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

// EventSchemaVersion versions the canonical request/outcome event contract. A
// stored event carries the version it was written with, so a reader can tell
// which shape it sees.
const EventSchemaVersion = 1

// eventID derives the stable id of an event from its identity. The same
// request and attempt always produce the same id, and the id reveals neither
// the request id nor anything about the request content.
func eventID(requestID string, attempt int) string {
	if requestID == "" {
		// Without a request id there is no stable identity to derive from.
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", requestID, attempt)))
	return hex.EncodeToString(sum[:])[:32]
}

// usageConflict decides what a skipped duplicate means. A redelivered event with
// identical content leaves the stored attempt alone and succeeds; the same event
// id reused for different content is a conflict, not a retry, and must surface.
// observedAtSet reports whether the reporter supplied an observation timestamp;
// a redelivery that omitted it carries server-stamped arrival bookkeeping, not
// event content, so it must not conflict on that alone.
func (s *Store) usageConflict(ctx context.Context, tx *sql.Tx, u UsageRecord, observedAtSet bool) error {
	existing, err := usageRecordByIdentity(ctx, tx, u.RequestID, u.Attempt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUsageConflict
		}
		return classify(err)
	}
	if existing.Provider == "" || existing.Source == "" || existing.Outcome == "" {
		return ErrUsageConflict
	}
	same := existing.Provider == u.Provider &&
		existing.Model == u.Model &&
		existing.TaskClass == u.TaskClass &&
		existing.RouteSource == u.RouteSource &&
		intValueEqual(existing.InputTokens, u.InputTokens) &&
		intValueEqual(existing.OutputTokens, u.OutputTokens) &&
		existing.InputProvenance == u.InputProvenance &&
		existing.OutputProvenance == u.OutputProvenance &&
		existing.Source == u.Source &&
		existing.Outcome == u.Outcome &&
		existing.ErrorClass == u.ErrorClass &&
		existing.StreamState == u.StreamState &&
		intValueEqualInt64(existing.TTFTMS, u.TTFTMS) &&
		intValueEqualInt64(existing.LatencyMS, u.LatencyMS) &&
		existing.DecisionID == u.DecisionID
	if same && observedAtSet && !existing.ObservedAt.Equal(u.ObservedAt) {
		same = false
	}
	if !same {
		return ErrUsageConflict
	}
	return nil
}

// usageColumns is the column list of one stored event, in the order
// scanUsageRecord reads it.
const usageColumns = `request_id, attempt, event_id, schema_version, provider, model, task_class, route_source,
	input_tokens, input_provenance, output_tokens, output_provenance, source, outcome, error_class,
	stream_state, ttft_ms, latency_ms, decision_id, observed_at, observed_at_reported, received_at`

// usageRecordByIdentity reads the stored event of one attempt.
func usageRecordByIdentity(ctx context.Context, tx *sql.Tx, requestID string, attempt int) (UsageRecord, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+usageColumns+` FROM usage_records WHERE request_id = ? AND attempt = ?`,
		requestID, attempt)
	return scanUsageRecord(row)
}

// scanUsageRecord reads one event row. A nullable column stays absent: it is
// never turned into a zero or an empty string.
func scanUsageRecord(row interface{ Scan(...any) error }) (UsageRecord, error) {
	var (
		u                                                    UsageRecord
		model, taskClass, routeSource, inputProv, outputProv sql.NullString
		errorClass, streamState                              sql.NullString
		input, output                                        sql.NullInt64
		ttft, latency                                        sql.NullInt64
		decisionID                                           sql.NullString
		observedAt, receivedAt                               string
		observedAtReported                                   int
	)
	err := row.Scan(&u.RequestID, &u.Attempt, &u.EventID, &u.SchemaVersion, &u.Provider, &model,
		&taskClass, &routeSource, &input, &inputProv, &output, &outputProv, &u.Source, &u.Outcome,
		&errorClass, &streamState, &ttft, &latency, &decisionID, &observedAt, &observedAtReported, &receivedAt)
	if err != nil {
		return UsageRecord{}, err
	}
	u.Model = model.String
	u.TaskClass = taskClass.String
	u.RouteSource = RouteSource(routeSource.String)
	u.InputTokens = intFromNull(&input)
	u.OutputTokens = intFromNull(&output)
	u.InputProvenance = TokenProvenance(inputProv.String)
	u.OutputProvenance = TokenProvenance(outputProv.String)
	u.ErrorClass = ErrorClass(errorClass.String)
	u.StreamState = StreamState(streamState.String)
	u.TTFTMS = int64FromNull(&ttft)
	u.LatencyMS = int64FromNull(&latency)
	u.DecisionID = decisionID.String
	u.ObservedAtReported = observedAtReported == 1
	if u.ObservedAt, err = parseTS(observedAt); err != nil {
		return UsageRecord{}, err
	}
	if u.ReceivedAt, err = parseTS(receivedAt); err != nil {
		return UsageRecord{}, err
	}
	return u, nil
}

// intValueEqual compares optional counts without dereferencing a nil: an absent
// count is unknown, not zero, and must not match a reported zero.
func intValueEqual(a, b *int) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

// intValueEqualInt64 is intValueEqual for the millisecond measurements.
func intValueEqualInt64(a, b *int64) bool {
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

func int64FromNull(v *sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
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

// maxUsageRetention is the hard upper bound on how long request/outcome events
// are kept. A configured age above it is clamped here, so storage stays bounded
// even when the configuration asks for more history than the controller holds.
const maxUsageRetention = 365 * 24 * time.Hour

// pruneBatch is how many rows one cleanup statement deletes, and
// maxPruneBatches bounds how many batches one run performs. Pruning in batches
// keeps each statement short: a cleanup that ran as one huge delete would hold
// the write lock long enough to make a request wait for it.
//
// They are variables so a test can lower the bound and observe it; the
// production values are the ones below.
var (
	pruneBatch      = 1000
	maxPruneBatches = 100
)

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

		// The two high-volume tables are pruned in bounded batches, and the
		// event table's age is clamped to the hard upper bound.
		if age := r.UsageRecords; age > 0 {
			if age > maxUsageRetention {
				age = maxUsageRetention
			}
			n, err := deleteBatched(ctx, tx, "usage_records", "observed_at < ?", ts(now.Add(-age)))
			if err != nil {
				return err
			}
			deleted["usage_records"] = n
		}
		if age := r.RoutingDecisions; age > 0 {
			n, err := deleteBatched(ctx, tx, "routing_decisions", "created_at < ?", ts(now.Add(-age)))
			if err != nil {
				return err
			}
			deleted["routing_decisions"] = n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return deleted, nil
}

// deleteBatched removes the rows a predicate selects, at most pruneBatch rows
// per statement and maxPruneBatches statements per run. The run is bounded, so
// cleanup cannot grow without limit; whatever is left is removed by the next
// run.
func deleteBatched(ctx context.Context, tx *sql.Tx, table, where string, args ...any) (int64, error) {
	var total int64
	for i := 0; i < maxPruneBatches; i++ {
		stmt := `DELETE FROM ` + table + ` WHERE id IN (SELECT id FROM ` + table + ` WHERE ` + where + ` LIMIT ?)`
		params := make([]any, 0, len(args)+1)
		params = append(params, args...)
		params = append(params, int64(pruneBatch))
		res, err := tx.ExecContext(ctx, stmt, params...)
		if err != nil {
			return 0, classify(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, classify(err)
		}
		total += n
		if n < int64(pruneBatch) {
			break
		}
	}
	return total, nil
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

// SaveRoutingDecision appends a routing decision audit record.
func (s *Store) SaveRoutingDecision(ctx context.Context, r RoutingDecisionRecord) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("routing decision: %w", err)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO routing_decisions (decision_id, policy_version, session_id, selected_alias, selected_provider, task_class, explicit, payload, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.DecisionID, r.PolicyVersion, r.SessionID, nullString(r.SelectedAlias), nullString(r.SelectedProvider),
			nullString(r.TaskClass), r.Explicit, r.Payload, ts(r.CreatedAt))
		return classify(err)
	})
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
