package turn

import (
	"encoding/json"
	"reflect"
	"strings"
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
		Goal:       "file the report",
		Completed:  []string{"read issue #21"},
		Decisions:  []string{"one checkpoint type, no per-provider copies"},
		Pending:    []string{"write docs", "run tests"},
		References: []string{"internal/turn/checkpoint.go"},
		ToolResults: []ToolResult{
			CompletedResult("gh issue view 21", "issue body"),
			FailedResult("go test ./...", ToolErrored),
			PlannedResult("gh pr create"),
		},
	}
	if err := cp.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
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
	// serialization intact, every field included.
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	var back Checkpoint
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	if !reflect.DeepEqual(back, cp) {
		t.Fatalf("checkpoint did not survive the boundary:\n got %+v\nwant %+v", back, cp)
	}
}

func TestCheckpointRefusesARawErrorOrAWholeFile(t *testing.T) {
	// The boundary is enforced, not promised: a credential-bearing error string
	// must not be able to cross it, and neither must a file dump.
	tests := []struct {
		name string
		cp   Checkpoint
	}{
		{"raw provider error kept as the result", Checkpoint{ToolResults: []ToolResult{
			{Call: "fetch quota", Status: ToolFailed, Result: "401 Unauthorized: Authorization: Bearer sk-live-abc"},
		}}},
		{"undeclared failure code", Checkpoint{ToolResults: []ToolResult{
			{Call: "fetch quota", Status: ToolFailed, Result: "SOMETHING_ELSE"},
		}}},
		{"undeclared status", Checkpoint{ToolResults: []ToolResult{
			{Call: "fetch quota", Status: ToolStatus("RUNNING"), Result: "not really"},
		}}},
		{"a call that never started carries a result", Checkpoint{ToolResults: []ToolResult{
			{Call: "gh pr create", Status: ToolNotStarted, Result: "a plan"},
		}}},
		{"call with no name", Checkpoint{ToolResults: []ToolResult{
			{Status: ToolCompleted, Result: "orphan"},
		}}},
	}
	for _, tc := range tests {
		if err := tc.cp.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", tc.name)
		}
	}
}

func TestCompletedResultIsASummaryNotADump(t *testing.T) {
	// 4096 runes is the cap; anything longer is a file, not a result.
	dump := strings.Repeat("x", maxResultRunes+1)
	got := CompletedResult("read file", dump)
	if n := len([]rune(got.Result)); n > maxResultRunes {
		t.Fatalf("CompletedResult kept %d runes, want at most %d", n, maxResultRunes)
	}
	cp := Checkpoint{ToolResults: []ToolResult{got}}
	if err := cp.Validate(); err != nil {
		t.Fatalf("Validate() rejected a truncated result: %v", err)
	}

	// A multi-byte character must not be cut in half by the cap.
	wide := strings.Repeat("ж", maxResultRunes+10)
	got = CompletedResult("read file", wide)
	if !strings.HasSuffix(got.Result, "…[truncated]") || strings.Contains(got.Result, "\uFFFD") {
		t.Fatalf("CompletedResult cut a character in half: %q", got.Result[maxResultRunes-2:])
	}
	// Exactly at the cap is allowed and untouched.
	atCap := CompletedResult("read file", strings.Repeat("y", maxResultRunes))
	if atCap.Result != strings.Repeat("y", maxResultRunes) {
		t.Fatal("CompletedResult truncated a result that was exactly within the cap")
	}
}

func TestFailedResultReplacesAnUndeclaredFailure(t *testing.T) {
	// Same discipline as quota.Failed: a caller can pass a raw error string, so
	// anything undeclared becomes a safe code instead of being stored.
	if got := FailedResult("fetch quota", ToolFailure("sk-live-abc")); got.Result != string(ToolErrored) {
		t.Fatalf("FailedResult stored %q, want %q", got.Result, ToolErrored)
	}
	if got := FailedResult("fetch quota", ToolDenied); got.Result != string(ToolDenied) {
		t.Fatalf("FailedResult replaced a declared failure: %q", got.Result)
	}
}

func TestLargeContextMigrationPaysForItsRebuild(t *testing.T) {
	// The same turn, the same failed route, two rebuilds. A model switch has no
	// KV cache to inherit, so the full transcript is input tokens the next model
	// processes and this installation pays for a second time.
	budget := Budget{MaxInputTokens: 100_000}
	sameTurn := "minimax/MiniMax-M2"

	full := Migration{Model: sameTurn, Rebuild: FullTranscript, InputTokens: 240_000}
	if full.Allowed(budget) {
		t.Fatal("a 240k-token full replay was allowed within a 100k budget")
	}
	// The compact rebuild of the very same turn fits, so the refusal above is
	// about the rebuild's cost, not about migration being unavailable at all.
	compact := Migration{Model: sameTurn, Rebuild: FromCheckpoint, InputTokens: 6_000}
	if !compact.Allowed(budget) {
		t.Fatal("the checkpoint rebuild of the same turn was refused")
	}

	// An estimate belongs to its own rebuild: reusing the cheap checkpoint
	// estimate to authorize a full replay must not pass. Only the fields are
	// public, so the caller's mistake looks exactly like this.
	reused := Migration{Model: sameTurn, Rebuild: FullTranscript, InputTokens: compact.InputTokens}
	if !reused.Allowed(budget) {
		t.Fatal("a full replay with a checkpoint-sized estimate was refused, which the budget alone cannot detect")
	}
	// That is why Allowed is the last check and not the only one: the estimate is
	// the caller's, and only the caller knows which rebuild it measured. This is
	// the gap #23 closes with real token accounting.
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
		{"at the budget", Migration{Model: "minimax/MiniMax-M2", Rebuild: FromCheckpoint, InputTokens: 100_000}, true},
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
