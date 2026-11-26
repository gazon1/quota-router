package turn

import (
	"encoding/json"
	"testing"
)

func TestDecideFailsClosedForEveryInterruptionPoint(t *testing.T) {
	tests := []struct {
		stage Stage
		want  Disposition
	}{
		// Never sent, so starting over costs nothing and repeats nothing.
		{NotDispatched, StartOver},
		// Sent, no output: the provider may already have charged the context.
		{AwaitingFirstToken, Surface},
		// Part of the answer reached the caller; a retry would send it twice.
		{Streaming, Surface},
		// A tool ran; a retry would repeat its side effect.
		{AfterToolExecution, Surface},
		// An undeclared stage proves nothing about side effects.
		{Stage("SOMETHING_ELSE"), Surface},
		{Stage(""), Surface},
	}
	for _, tc := range tests {
		if got := Decide(tc.stage); got != tc.want {
			t.Errorf("Decide(%q) = %s, want %s", tc.stage, got, tc.want)
		}
	}
}

func TestCheckpointCarriesExecutedToolsInsteadOfReplayingThem(t *testing.T) {
	cp := Checkpoint{
		Goal:      "file the report",
		Completed: []string{"read issue #21"},
		Decisions: []string{"one checkpoint type, no per-provider copies"},
		Pending:   []string{"write docs", "run tests"},
		References: []string{
			"internal/turn/checkpoint.go",
		},
		ToolResults: []ToolResult{
			{Call: "gh issue view 21", Status: ToolCompleted, Result: "issue body"},
			{Call: "go test ./...", Status: ToolFailed, Result: "one failure"},
			{Call: "gh pr create", Status: ToolNotStarted},
		},
	}
	if got := cp.Executed(); got != 2 {
		t.Fatalf("Executed() = %d, want 2: completed and failed calls both ran", got)
	}
	// A failed call is still a second side effect if replayed, so the attempt
	// that had one may only surface.
	if got := Decide(AfterToolExecution); got != Surface {
		t.Fatalf("Decide(AFTER_TOOL_EXECUTION) = %s, want SURFACE", got)
	}

	// Recovery: the checkpoint is what crosses the boundary, so it must survive
	// serialization with its pending work and tool outcomes intact.
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	var back Checkpoint
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	if back.Goal != cp.Goal || len(back.Pending) != 2 || back.Executed() != 2 {
		t.Fatalf("checkpoint did not survive the boundary: %+v", back)
	}
	if back.ToolResults[0] != cp.ToolResults[0] {
		t.Fatalf("tool result lost: %+v", back.ToolResults[0])
	}
}

func TestMigrationRefusedWithoutAnAccountableCost(t *testing.T) {
	budget := Budget{MaxInputTokens: 100_000}
	tests := []struct {
		name string
		m    Migration
		want bool
	}{
		{"checkpoint within budget", Migration{Model: "minimax/MiniMax-M2", Rebuild: FromCheckpoint, InputTokens: 4_000}, true},
		{"explicit full replay within budget", Migration{Model: "minimax/MiniMax-M2", Rebuild: FullTranscript, InputTokens: 90_000}, true},
		{"over budget", Migration{Model: "minimax/MiniMax-M2", Rebuild: FullTranscript, InputTokens: 120_000}, false},
		{"no estimate", Migration{Model: "minimax/MiniMax-M2", Rebuild: FromCheckpoint}, false},
		{"no model", Migration{Rebuild: FromCheckpoint, InputTokens: 4_000}, false},
		{"unknown rebuild", Migration{Model: "minimax/MiniMax-M2", InputTokens: 4_000}, false},
		{"finite tokens, but nobody set a budget", Migration{Model: "minimax/MiniMax-M2", Rebuild: FromCheckpoint, InputTokens: 4_000}, false},
	}
	for _, tc := range tests {
		b := budget
		if tc.name == "finite tokens, but nobody set a budget" {
			b = Budget{}
		}
		if got := tc.m.Allowed(b); got != tc.want {
			t.Errorf("%s: Allowed() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
