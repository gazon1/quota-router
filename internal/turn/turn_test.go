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
	dump := strings.Repeat("x", maxFieldRunes+1)
	got := CompletedResult("read file", dump)
	if n := len([]rune(got.Result)); n > maxFieldRunes {
		t.Fatalf("CompletedResult kept %d runes, want at most %d", n, maxFieldRunes)
	}
	cp := Checkpoint{ToolResults: []ToolResult{got}}
	if err := cp.Validate(); err != nil {
		t.Fatalf("Validate() rejected a truncated result: %v", err)
	}

	// A multi-byte character must not be cut in half by the cap.
	wide := strings.Repeat("ж", maxFieldRunes+10)
	got = CompletedResult("read file", wide)
	if !strings.HasSuffix(got.Result, "…[truncated]") || strings.Contains(got.Result, "\uFFFD") {
		t.Fatalf("CompletedResult cut a character in half: %q", got.Result[maxFieldRunes-2:])
	}
	// Exactly at the cap is allowed and untouched.
	atCap := CompletedResult("read file", strings.Repeat("y", maxFieldRunes))
	if atCap.Result != strings.Repeat("y", maxFieldRunes) {
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
	// The same turn, same failed route, two rebuilds. A model switch inherits no
	// KV cache, so a full transcript is input tokens the next model processes —
	// and this installation pays for — a second time.
	budget := Budget{MaxInputTokens: 100_000}
	sameTurn := "minimax/MiniMax-M2"

	full := Migration{Model: sameTurn, Rebuild: Rebuild{Kind: FullTranscript, InputTokens: 240_000}, AuthorizedBy: "agent"}
	if full.Allowed(budget) {
		t.Fatal("a 240k-token full replay was allowed within a 100k budget")
	}
	// A checkpoint rebuild of the same turn fits, so the refusal above is about
	// what the rebuild costs, not about migration being unavailable at all.
	compact := Migration{Model: sameTurn, Rebuild: Rebuild{Kind: FromCheckpoint, InputTokens: 6_000}}
	if !compact.Allowed(budget) {
		t.Fatal("the checkpoint rebuild of the same turn was refused")
	}

	// The estimate now lives inside the rebuild it prices, so it cannot be
	// lent to a different one by a field mix-up. Whether the number is right is
	// still the caller's, and #23 supplies real accounting.
}

func TestFullReplayNeedsAnExplicitDecision(t *testing.T) {
	// The issue asks that a deliberate full-context replay be an explicit
	// decision, not something that happens by filling in a struct.
	budget := Budget{MaxInputTokens: 100_000}
	replay := Migration{Model: "minimax/MiniMax-M2", Rebuild: Rebuild{Kind: FullTranscript, InputTokens: 90_000}}
	if replay.Allowed(budget) {
		t.Fatal("a full replay without a named decision was allowed")
	}
	// Whitespace is not a name: it authorizes nothing.
	replay.AuthorizedBy = "   "
	if replay.Allowed(budget) {
		t.Fatal("a blank authorization was accepted")
	}
	replay.AuthorizedBy = "policy: owner-approved-large-context"
	if !replay.Allowed(budget) {
		t.Fatal("a budgeted, explicitly authorized full replay was refused")
	}
}

func TestValidateGuardsEveryFieldNotJustToolResults(t *testing.T) {
	// The boundary is the checkpoint, not one field: prose pasted into Goal or
	// References must be refused just like a raw tool result.
	raw := "401 Unauthorized: Authorization: Bearer sk-live-abc"
	tests := []struct {
		name string
		cp   Checkpoint
	}{
		{"goal carries a credential", Checkpoint{Goal: raw}},
		{"completed carries a credential", Checkpoint{Completed: []string{"fetched " + raw}}},
		{"decisions carries a credential", Checkpoint{Decisions: []string{"api_key=sk-live-abc"}}},
		{"pending carries a credential", Checkpoint{Pending: []string{"retry with token=sk-live-abc"}}},
		{"references carries a credential", Checkpoint{References: []string{"https://user:pw@api.example.com/quota"}}},
		// A credentialed URL inside a sentence does not parse as a URL, so it
		// must be extracted before it is judged.
		{"goal mentions a credentialed URL in prose", Checkpoint{Goal: "see https://user:pw@api.example.com/quota for details"}},
		{"tool call name carries a credential", Checkpoint{ToolResults: []ToolResult{
			CompletedResult("curl -H 'Authorization: Bearer sk-live-abc'", "200 OK"),
		}}},
		{"goal is a whole file", Checkpoint{Goal: strings.Repeat("line\n", maxFieldRunes)}},
	}
	for _, tc := range tests {
		if err := tc.cp.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", tc.name)
		}
	}

	// A plain reference and a plain summary stay allowed: the guard is narrow on
	// purpose, so it does not reject the checkpoint it exists to protect.
	ok := Checkpoint{
		Goal:       "wire the turn boundary",
		Completed:  []string{"read issue #21"},
		Decisions:  []string{"one checkpoint type"},
		Pending:    []string{"write docs"},
		References: []string{"internal/turn/checkpoint.go", "https://github.com/gazon1/quota-router/pull/58"},
		ToolResults: []ToolResult{
			CompletedResult("go test ./...", "81 passed"),
		},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate() rejected a legitimate checkpoint: %v", err)
	}

	// Plain prose must survive, including long ordinary words after "Bearer" and
	// headers or parameters with no value: a guard that rejects everything is a
	// guard a caller learns to ignore.
	for _, prose := range []string{
		"authorization: none required",
		"token= unset until the operator configures it",
		"Bearer none, request authentication was skipped",
		"password= empty is not a credential",
	} {
		if err := (Checkpoint{Goal: prose}).Validate(); err != nil {
			t.Fatalf("Validate() rejected plain prose %q: %v", prose, err)
		}
	}
	// The RFC spelling fails closed: a summary that needs to mention it rephrases
	// ("bearer auth is handled by middleware") instead of quoting it, and that
	// cost is documented. A live credential is not recoverable.
	for _, quoted := range []string{
		"Bearer responsibility belongs to the policy owner",
		"Bearer credentials are required for this endpoint",
		"request failed: Bearer abcdefghijklmnop was rejected",
	} {
		if err := (Checkpoint{Goal: quoted}).Validate(); err == nil {
			t.Errorf("Validate() let %q through", quoted)
		}
	}
	// The scheme is case-insensitive, so lowercase does not escape either.
	for _, quoted := range []string{"request failed: bearer abcdefghijklmnop was rejected", "bearer sk-live-1", "first Bearer abc; second Bearer xyz"} {
		if err := (Checkpoint{Goal: quoted}).Validate(); err == nil {
			t.Errorf("Validate() let lowercase %q through", quoted)
		}
	}
	// The documented cost: prose with a longer word after "bearer" is refused and
	// must be rephrased. A function word is the one escape, so "bearer of" and
	// "bearer is" still read as prose.
	for _, prose := range []string{
		"the bearer of this report is the owner",
		"the bearer is the owner",
		"bearer may be absent in config",
	} {
		if err := (Checkpoint{Goal: prose}).Validate(); err != nil {
			t.Errorf("Validate() rejected prose %q: %v", prose, err)
		}
	}

	// But a real token, a bare JWT, and a credentialed URL in prose are caught.
	for _, raw := range []string{
		"sent with Bearer sk-live-abcdef123",
		"Authorization: Bearer sk-live-1",
		"api_key=sk-live-abc",
		"use eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abc",
		"see https://user:pw@api.example.com/quota for details",
		// An opaque, letters-only bearer value is still a credential.
		"Bearer abcdefghijklmnop",
		// A placeholder must not hide a real value in the next occurrence.
		"token= unset; token=sk-live-abc",
		"api_key= optional, api_key=sk-live-1",
	} {
		if err := (Checkpoint{Goal: raw}).Validate(); err == nil {
			t.Errorf("Validate() let %q through", raw)
		}
	}

	// Ordinary values that only look suspicious stay legitimate: a token count,
	// a commit SHA, a plain URL, a duration.
	for _, fine := range []string{
		"prompt_tokens=40 completion_tokens=12",
		"fixed in commit 4610f1b8cb4ab4dfc4e4b4ff259be0782797ab43",
		"https://github.com/gazon1/quota-router/pull/58",
		"migration completed in 3.2s",
	} {
		if err := (Checkpoint{Goal: fine}).Validate(); err != nil {
			t.Errorf("Validate() rejected a legitimate value %q: %v", fine, err)
		}
	}
}

func TestMigrationRefusedWithoutAnAccountableCost(t *testing.T) {
	budget := Budget{MaxInputTokens: 100_000}
	tests := []struct {
		name string
		m    Migration
		want bool
	}{
		{"checkpoint within budget", Migration{Model: "minimax/MiniMax-M2", Rebuild: Rebuild{Kind: FromCheckpoint, InputTokens: 4_000}}, true},
		{"explicit full replay within budget", Migration{Model: "minimax/MiniMax-M2", Rebuild: Rebuild{Kind: FullTranscript, InputTokens: 90_000}, AuthorizedBy: "agent"}, true},
		{"over budget", Migration{Model: "minimax/MiniMax-M2", Rebuild: Rebuild{Kind: FullTranscript, InputTokens: 120_000}, AuthorizedBy: "agent"}, false},
		{"at the budget", Migration{Model: "minimax/MiniMax-M2", Rebuild: Rebuild{Kind: FromCheckpoint, InputTokens: 100_000}}, true},
		{"no estimate", Migration{Model: "minimax/MiniMax-M2", Rebuild: Rebuild{Kind: FromCheckpoint}}, false},
		{"no model", Migration{Rebuild: Rebuild{Kind: FromCheckpoint, InputTokens: 4_000}}, false},
		{"unknown rebuild", Migration{Model: "minimax/MiniMax-M2", Rebuild: Rebuild{InputTokens: 4_000}}, false},
		{"finite tokens, but nobody set a budget", Migration{Model: "minimax/MiniMax-M2", Rebuild: Rebuild{Kind: FromCheckpoint, InputTokens: 4_000}}, false},
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
