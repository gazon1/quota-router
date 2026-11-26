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
	"time"
)

// tsLayout is the fixed-width UTC layout every timestamp column uses, so
// retention can compare text and a backup stays readable.
const tsLayout = "2006-01-02T15:04:05.000Z"

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

// SaveUsageRecord appends one local usage observation. A repeated RequestID is
// ignored, so a retried report does not double-count.
func (s *Store) SaveUsageRecord(ctx context.Context, u UsageRecord) error {
	if err := u.Validate(); err != nil {
		return fmt.Errorf("usage record: %w", err)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO usage_records (request_id, provider, model, input_tokens, output_tokens, provenance, observed_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (request_id) DO NOTHING`,
			nullString(u.RequestID), u.Provider, nullString(u.Model), u.InputTokens, u.OutputTokens, u.Provenance, ts(u.ObservedAt))
		return classify(err)
	})
}

// SaveProviderHealth appends one health observation.
func (s *Store) SaveProviderHealth(ctx context.Context, h HealthRecord) error {
	if err := h.Validate(); err != nil {
		return fmt.Errorf("provider health: %w", err)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO provider_health (provider, state, observed_at, error) VALUES (?, ?, ?, ?)`,
			h.Provider, h.State, ts(h.ObservedAt), nullString(h.Error))
		return classify(err)
	})
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
// It is routing state, not history: retention keeps these rows.
func (s *Store) LatestQuotaSnapshot(ctx context.Context) ([]QuotaSnapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, provider, state, unknown_reason, source, confidence, observed_at, error
		 FROM quota_snapshots q
		 WHERE observed_at = (SELECT MAX(observed_at) FROM quota_snapshots WHERE provider = q.provider)`)
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
	for i, id := range ids {
		windows, err := s.windows(ctx, id)
		if err != nil {
			return nil, err
		}
		out[i].Windows = windows
	}
	return out, nil
}

func (s *Store) windows(ctx context.Context, snapshotID int64) ([]QuotaWindow, error) {
	rows, err := s.db.QueryContext(ctx,
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
		`SELECT provider, state, observed_at, error
		 FROM provider_health h
		 WHERE observed_at = (SELECT MAX(observed_at) FROM provider_health WHERE provider = h.provider)`)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	var out []HealthRecord
	for rows.Next() {
		var (
			h        HealthRecord
			observed string
			errText  sql.NullString
		)
		if err := rows.Scan(&h.Provider, &h.State, &observed, &errText); err != nil {
			return nil, classify(err)
		}
		if h.ObservedAt, err = parseTS(observed); err != nil {
			return nil, err
		}
		h.Error = errText.String
		out = append(out, h)
	}
	return out, classify(rows.Err())
}

// Retention bounds history. Zero disables pruning for that table. The latest row
// per provider (or overall, for usage) is always kept, so cleanup can never
// remove the state routing depends on.
type Retention struct {
	QuotaSnapshots time.Duration
	ProviderHealth time.Duration
	UsageRecords   time.Duration
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
		for _, p := range []struct {
			table  string
			age    time.Duration
			newest string
		}{
			// quota_windows go with their snapshot through ON DELETE CASCADE.
			{"quota_snapshots", r.QuotaSnapshots, newestPerProvider("quota_snapshots")},
			{"provider_health", r.ProviderHealth, newestPerProvider("provider_health")},
			{"usage_records", r.UsageRecords, "1 = 1"},
		} {
			if p.age <= 0 {
				continue
			}
			res, err := tx.ExecContext(ctx,
				"DELETE FROM "+p.table+" WHERE observed_at < ? AND "+p.newest,
				ts(now.Add(-p.age)))
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

// newestPerProvider builds the NOT IN clause that spares the row
// LatestQuotaSnapshot and LatestProviderHealth read: max observed_at, ties
// broken by max id, per provider.
func newestPerProvider(table string) string {
	return "id NOT IN (SELECT id FROM " + table + " WHERE (provider, observed_at) IN " +
		"(SELECT provider, MAX(observed_at) FROM " + table + " GROUP BY provider))"
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
