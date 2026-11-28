package store

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// migrationProvider returns a goose provider over the embedded migrations, so a
// test can move the schema to a version and back.
func migrationProvider(t *testing.T, s *Store) *goose.Provider {
	t.Helper()
	root, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.db, root,
		goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return provider
}

// A row that predates the event id gets one when the schema moves: the id is
// derived from the row's identity, so it is the same id the store would have
// written, and a row without a request id has no identity to derive from.
func TestLegacyRowsGetStableEventIDs(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	provider := migrationProvider(t, s)

	// Back to the shape before the event table: the row is written in the old
	// columns, with no event id at all.
	if _, err := provider.DownTo(ctx, 6); err != nil {
		t.Fatalf("DownTo 6: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_records (request_id, attempt, provider, model, input_tokens,
		    output_tokens, provenance, source, outcome, observed_at)
		 VALUES ('req-legacy', 1, 'minimax', 'mm-fast', 10, 5, 'reported', 'legacy', 'SUCCESS', ?),
		        ('req-legacy', 2, 'minimax', 'mm-fast', 20, 6, 'reported', 'legacy', 'SUCCESS', ?),
		        (NULL, 1, 'minimax', 'mm-fast', 30, 7, 'reported', 'legacy', 'SUCCESS', ?)`,
		ts(time.Now().UTC()), ts(time.Now().UTC()), ts(time.Now().UTC())); err != nil {
		t.Fatalf("insert legacy rows: %v", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT request_id, attempt, event_id FROM usage_records ORDER BY id`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	type row struct{ requestID, eventID string }
	var got []row
	for rows.Next() {
		var r row
		var attempt int
		var requestID, eventID sql.NullString
		if err := rows.Scan(&requestID, &attempt, &eventID); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		r.requestID = requestID.String
		r.eventID = eventID.String
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("events = %d, want the three legacy rows", len(got))
	}
	if got[0].eventID == "" {
		t.Fatal("the legacy row with a request id has no event id")
	}
	// The id is the one the store derives for the same identity.
	if want := eventID("req-legacy", 1); got[0].eventID != want {
		t.Fatalf("event id = %q, want the derived %q", got[0].eventID, want)
	}
	// A retry is a different event.
	if got[1].eventID == got[0].eventID {
		t.Fatalf("event ids = %q and %q, want one id per attempt", got[0].eventID, got[1].eventID)
	}
	// A row without a request id has no identity to derive from.
	if got[2].eventID != "" {
		t.Fatalf("event id = %q, want none for a row without a request id", got[2].eventID)
	}

	// Rolling back and migrating again must not change the identities.
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	var again string
	if err := s.db.QueryRowContext(ctx,
		`SELECT event_id FROM usage_records WHERE request_id = 'req-legacy' AND attempt = 1`).Scan(&again); err != nil {
		t.Fatalf("query event again: %v", err)
	}
	if again != got[0].eventID {
		t.Fatalf("event id = %q after a rollback, want the same %q", again, got[0].eventID)
	}
}

// Rolling back to the single-provenance shape must not promote an estimate: the
// old shape can hold one provenance for both counts, so it has to degrade to
// the weaker of the two.
func TestDownMigrationKeepsTheWeakerProvenance(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	provider := migrationProvider(t, s)

	for _, tc := range []struct {
		name          string
		input, output TokenProvenance
		want          string
	}{
		{"estimated output", TokenProxyReported, TokenEstimated, "estimated"},
		{"estimated input", TokenEstimated, TokenProviderReported, "estimated"},
		{"both reported", TokenProxyReported, TokenProviderReported, "reported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO usage_records (request_id, attempt, event_id, schema_version, provider,
				    input_tokens, input_provenance, output_tokens, output_provenance,
				    source, outcome, observed_at, observed_at_reported, received_at)
				 VALUES (?, 1, 'e1', 1, 'minimax', 10, ?, 5, ?, 'agent', 'SUCCESS', ?, 0, ?)`,
				"req-"+tc.name, string(tc.input), string(tc.output), ts(time.Now().UTC()), ts(time.Now().UTC())); err != nil {
				t.Fatalf("insert event: %v", err)
			}
		})
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT request_id, provenance FROM usage_records ORDER BY id`)
	if err != nil {
		t.Fatalf("query rows: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var requestID, provenance string
		if err := rows.Scan(&requestID, &provenance); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		got[requestID] = provenance
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	for _, tc := range []struct {
		name string
		want string
	}{
		{"req-estimated output", "estimated"},
		{"req-estimated input", "estimated"},
		{"req-both reported", "reported"},
	} {
		if got[tc.name] != tc.want {
			t.Fatalf("provenance of %s = %q, want %q", tc.name, got[tc.name], tc.want)
		}
	}
}
