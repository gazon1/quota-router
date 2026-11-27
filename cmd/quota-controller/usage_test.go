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
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post #%d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("post #%d status = %d, want 202", i, resp.StatusCode)
		}
	}

	// A report without the token must be refused by the running controller.
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/usage", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	rows, err := usageRows(path)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: a repeated report of one attempt stores nothing", len(rows))
	}
	got := rows[0]
	if got.attempt != 1 || got.outcome != "SUCCESS" || got.provenance != "reported" || got.decision != "dec-1" {
		t.Fatalf("row = %+v, want attempt 1 of SUCCESS reported by dec-1", got)
	}
	if got.in != 100 || got.out != 20 {
		t.Fatalf("tokens = %d/%d, want the reported 100/20", got.in, got.out)
	}
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
