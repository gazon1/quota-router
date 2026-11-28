package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The usage route is wired through run(), not only tested on its own: a report
// posted to the running controller is persisted with its attempt identity, and
// a repeated report is not counted twice.
func TestRunAcceptsUsageReports(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.db")
	addr := freeAddr(t)
	t.Setenv("QUOTA_ROUTER_SQLITE__PATH", path)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", addr)
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN", "test-token")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), new(slog.LevelVar)) }()
	waitForHTTP(t, "http://"+addr+"/health", 5*time.Second)
	defer func() {
		cancel()
		<-done
	}()

	body := `{"request_id":"req-1","attempt":1,"provider":"minimax","model":"MiniMax-M2.7",
		"input_tokens":100,"output_tokens":20,"provenance":"reported",
		"source":"litellm.callback","outcome":"SUCCESS","decision_id":"dec-1"}`
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/usage", strings.NewReader(body))
		require.NoError(t, err, "new request")
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err, "post #%d", i)
		resp.Body.Close()
		assert.Equal(t, http.StatusAccepted, resp.StatusCode, "post #%d status", i)
	}

	// A report without the token must be refused by the running controller.
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/usage", strings.NewReader(body))
	require.NoError(t, err, "new request")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "unauthenticated post")
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "unauthenticated status")

	rows, err := usageRows(path)
	require.NoError(t, err, "read usage")
	require.Len(t, rows, 1, "a repeated report of one attempt stores nothing")
	got := rows[0]
	assert.Equal(t, 1, got.attempt)
	assert.Equal(t, "SUCCESS", got.outcome)
	assert.Equal(t, "reported", got.provenance)
	assert.Equal(t, "dec-1", got.decision)
	assert.Equal(t, 100, got.in, "the reported input tokens")
	assert.Equal(t, 20, got.out, "the reported output tokens")
}

type usageRow struct {
	attempt             int
	outcome, provenance string
	decision            string
	in, out             int
	source              string
	observedAt          time.Time
}

func usageRows(path string) ([]usageRow, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(
		`SELECT attempt, outcome, provenance, COALESCE(decision_id, ''), COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), source, observed_at
		 FROM usage_records ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usageRow
	for rows.Next() {
		var (
			r   usageRow
			obs string
		)
		if err := rows.Scan(&r.attempt, &r.outcome, &r.provenance, &r.decision, &r.in, &r.out, &r.source, &obs); err != nil {
			return nil, err
		}
		if r.observedAt, err = time.Parse("2006-01-02T15:04:05.000000000Z", obs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
