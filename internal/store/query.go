// Reads over the canonical request/outcome events.
//
// Consumers (historical analysis, route comparison, metrics derivation) read
// events through this typed query instead of SQL, so the stored shape can
// change without every consumer changing with it.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// UsageQuery selects request/outcome events. Every field is optional: an empty
// filter matches every event in the window.
type UsageQuery struct {
	// Since and Until bound the observation time. A zero bound is open.
	Since, Until time.Time
	// Provider, Model and TaskClass narrow the events further. Empty matches
	// any value.
	Provider, Model, TaskClass string
	// Limit bounds how many events are returned. Zero uses the default, and
	// the default is capped: a query never reads the whole table.
	Limit int
}

const (
	// defaultUsageQueryLimit is what a query without a limit returns.
	defaultUsageQueryLimit = 100
	// maxUsageQueryLimit is the hard cap on one query.
	maxUsageQueryLimit = 1000
)

// QueryUsageRecords returns the matching events, newest observation first.
//
// The order is by observation time and not by insertion: a report that arrived
// late describes an earlier moment, and a consumer sorting by insertion order
// would read it as the newest event.
func (s *Store) QueryUsageRecords(ctx context.Context, q UsageQuery) ([]UsageRecord, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultUsageQueryLimit
	}
	if limit > maxUsageQueryLimit {
		limit = maxUsageQueryLimit
	}
	var (
		where []string
		args  []any
	)
	if !q.Since.IsZero() {
		where = append(where, "observed_at >= ?")
		args = append(args, ts(q.Since))
	}
	if !q.Until.IsZero() {
		where = append(where, "observed_at <= ?")
		args = append(args, ts(q.Until))
	}
	if q.Provider != "" {
		where = append(where, "provider = ?")
		args = append(args, q.Provider)
	}
	if q.Model != "" {
		where = append(where, "model = ?")
		args = append(args, q.Model)
	}
	if q.TaskClass != "" {
		where = append(where, "task_class = ?")
		args = append(args, q.TaskClass)
	}
	query := `SELECT ` + usageColumns + ` FROM usage_records`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY observed_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	var out []UsageRecord
	for rows.Next() {
		rec, err := scanUsageRecord(rows)
		if err != nil {
			return nil, classify(err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// UsageConflictQuery selects the recorded conflicts between two reports of the
// same event. Every field is optional: an empty filter matches every conflict.
type UsageConflictQuery struct {
	// RequestID narrows the conflicts to one request. Empty matches any.
	RequestID string
	// Attempt narrows the conflicts to one attempt. Zero matches any attempt.
	Attempt int
	// Since and Until bound the observation time. A zero bound is open.
	Since, Until time.Time
	// Limit bounds how many conflicts are returned. Zero uses the default, and
	// the default is capped: a query never reads the whole table.
	Limit int
}

// QueryUsageConflicts returns the conflicts between two reports of the same
// event, newest first.
//
// A conflict is what a consumer sees when two sources disagree about one
// attempt: the stored event keeps the first observation, and these rows keep
// both sides of the disagreement.
func (s *Store) QueryUsageConflicts(ctx context.Context, q UsageConflictQuery) ([]UsageConflict, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultUsageQueryLimit
	}
	if limit > maxUsageQueryLimit {
		limit = maxUsageQueryLimit
	}
	var (
		where []string
		args  []any
	)
	if q.RequestID != "" {
		where = append(where, "request_id = ?")
		args = append(args, q.RequestID)
	}
	if q.Attempt > 0 {
		where = append(where, "attempt = ?")
		args = append(args, q.Attempt)
	}
	if !q.Since.IsZero() {
		where = append(where, "observed_at >= ?")
		args = append(args, ts(q.Since))
	}
	if !q.Until.IsZero() {
		where = append(where, "observed_at <= ?")
		args = append(args, ts(q.Until))
	}
	query := `SELECT request_id, attempt, field, original_value, original_provenance,
	                 incoming_value, incoming_provenance, source, observed_at, received_at
	          FROM usage_conflicts`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	var out []UsageConflict
	for rows.Next() {
		var (
			c                          UsageConflict
			originalProv, incomingProv sql.NullString
			observedAt, receivedAt     string
		)
		if err := rows.Scan(&c.RequestID, &c.Attempt, &c.Field, &c.OriginalValue, &originalProv,
			&c.IncomingValue, &incomingProv, &c.Source, &observedAt, &receivedAt); err != nil {
			return nil, classify(err)
		}
		c.OriginalProvenance = TokenProvenance(originalProv.String)
		c.IncomingProvenance = TokenProvenance(incomingProv.String)
		if c.ObservedAt, err = parseTS(observedAt); err != nil {
			return nil, err
		}
		if c.ReceivedAt, err = parseTS(receivedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// LatestRoutingDecisions returns the newest routing decisions, newest first.
func (s *Store) LatestRoutingDecisions(ctx context.Context, limit int) ([]RoutingDecisionRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT decision_id, policy_version, session_id, selected_alias, selected_provider, task_class, explicit, payload, created_at
		 FROM routing_decisions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	var out []RoutingDecisionRecord
	for rows.Next() {
		var (
			rec                        RoutingDecisionRecord
			alias, provider, taskClass sql.NullString
			explicit                   int
			createdAt                  string
		)
		if err := rows.Scan(&rec.DecisionID, &rec.PolicyVersion, &rec.SessionID, &alias, &provider,
			&taskClass, &explicit, &rec.Payload, &createdAt); err != nil {
			return nil, classify(err)
		}
		rec.SelectedAlias = alias.String
		rec.SelectedProvider = provider.String
		rec.TaskClass = taskClass.String
		rec.Explicit = explicit == 1
		if rec.CreatedAt, err = parseTS(createdAt); err != nil {
			return nil, fmt.Errorf("routing decision: %w", err)
		}
		out = append(out, rec)
	}
	return out, classify(rows.Err())
}
