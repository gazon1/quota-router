package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestRunConfigError(t *testing.T) {
	t.Setenv("QUOTA_ROUTER_READ_TIMEOUT", "bad")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	code := run(context.Background(), log, nil)
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
}
