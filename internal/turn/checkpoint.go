package turn

import "errors"

// Checkpoint is the agent-owned state a turn boundary carries into the next
// turn. It is what makes a model switch not a replay: tool results that already
// ran travel here, so the next model reads them instead of executing them again.
//
// The fields hold summaries the agent wrote, never a provider payload, a raw
// error or a whole file. That boundary is enforced, not promised: tool results
// come from CompletedResult and FailedResult, and Validate refuses an entry that
// bypassed them. Call Validate before the checkpoint leaves for another model.
type Checkpoint struct {
	// Goal is the task as the agent understands it now, after any decisions.
	Goal string
	// Completed lists work already done, including tools that already ran.
	Completed []string
	// Decisions records what was settled and why, so the next model does not
	// re-litigate it.
	Decisions []string
	// Pending lists work that remains, in the order the agent intends.
	Pending []string
	// References names relevant material rather than embedding it.
	References []string
	// ToolResults carries executed tool calls so migration never replays them.
	ToolResults []ToolResult
}

// ToolStatus is how a tool call ended. An attempt with any executed tool has
// side effects the next model must not repeat.
type ToolStatus string

const (
	// ToolCompleted means the call ran and its result is in ToolResult.Result.
	ToolCompleted ToolStatus = "COMPLETED"
	// ToolFailed means the call ran and failed; running it again is still a
	// second side effect, so the failure travels with the checkpoint.
	ToolFailed ToolStatus = "FAILED"
	// ToolNotStarted means the call was planned but never executed.
	ToolNotStarted ToolStatus = "NOT_STARTED"
)

// known reports whether s is one of the declared statuses.
func (s ToolStatus) known() bool {
	switch s {
	case ToolCompleted, ToolFailed, ToolNotStarted:
		return true
	}
	return false
}

// ToolFailure is the closed vocabulary of why a tool call failed. The raw error
// never crosses the boundary: a provider error can carry a credential in a URL
// or a header, and the next model would print it. Same discipline as
// quota.Failed, which replaces an undeclared reason with a safe code.
type ToolFailure string

const (
	// ToolErrored means the call ran and reported a failure.
	ToolErrored ToolFailure = "TOOL_ERROR"
	// ToolDenied means the call was refused, for example by policy or permission.
	ToolDenied ToolFailure = "DENIED"
	// ToolTimedOut means the call exceeded its deadline.
	ToolTimedOut ToolFailure = "TIMEOUT"
	// ToolOutcomeUnknown means the call may or may not have run, for example a
	// connection dropped after dispatch. It counts as executed: a retry could
	// repeat the side effect.
	ToolOutcomeUnknown ToolFailure = "OUTCOME_UNKNOWN"
)

// known reports whether f is one of the declared failures.
func (f ToolFailure) known() bool {
	switch f {
	case ToolErrored, ToolDenied, ToolTimedOut, ToolOutcomeUnknown:
		return true
	}
	return false
}

// maxResultRunes bounds what a completed call may carry into a checkpoint. A
// result longer than this is a file dump rather than a summary, and resending it
// to another model is the cost this contract exists to avoid.
const maxResultRunes = 4096

// ToolResult is one tool call and its outcome, kept so a migrated turn resumes
// after it instead of at it.
type ToolResult struct {
	Call   string
	Status ToolStatus
	// Result is the agent's summary of what the call returned, or a ToolFailure
	// code for a call that failed. Empty when the call did not start.
	Result string
}

// CompletedResult builds the entry of a call that ran and returned. Over-long
// output is truncated, because a whole file must not cross the boundary.
func CompletedResult(call, result string) ToolResult {
	return ToolResult{Call: call, Status: ToolCompleted, Result: truncate(result)}
}

// FailedResult builds the entry of a call that ran and failed. An undeclared
// failure becomes ToolErrored rather than storing raw error text, which may
// carry a credential.
func FailedResult(call string, failure ToolFailure) ToolResult {
	if !failure.known() {
		failure = ToolErrored
	}
	return ToolResult{Call: call, Status: ToolFailed, Result: string(failure)}
}

// PlannedResult builds the entry of a call that never started.
func PlannedResult(call string) ToolResult {
	return ToolResult{Call: call, Status: ToolNotStarted}
}

// truncate keeps a summary a summary: it runs on runes, so a multi-byte
// character is never cut in half, and the marker counts against the cap, so the
// stored result never exceeds it.
func truncate(s string) string {
	if len(s) <= maxResultRunes {
		// Cheap path: a byte-length under the cap cannot exceed it in runes.
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxResultRunes {
		return s
	}
	const marker = "…[truncated]"
	return string(runes[:maxResultRunes-len([]rune(marker))]) + marker
}

// Validate reports the entries that break the boundary: an undeclared status, a
// failed call whose result is raw text, a not-started call carrying a result, an
// over-long result, or a call with no name. Each of those either leaks a
// credential or pays to resend material the checkpoint exists to keep out.
func (c Checkpoint) Validate() error {
	var errs []error
	for _, r := range c.ToolResults {
		if r.Call == "" {
			errs = append(errs, errors.New("tool result with an empty call name"))
			continue
		}
		switch {
		case !r.Status.known():
			errs = append(errs, errors.New("undeclared tool status for "+r.Call))
		case r.Status == ToolFailed && !ToolFailure(r.Result).known():
			errs = append(errs, errors.New("failed tool result must carry a declared failure, not raw output: "+r.Call))
		case r.Status == ToolNotStarted && r.Result != "":
			errs = append(errs, errors.New("a call that never started must not carry a result: "+r.Call))
		case len([]rune(r.Result)) > maxResultRunes:
			errs = append(errs, errors.New("tool result is a dump, not a summary: "+r.Call))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Executed counts tool calls that already ran, completed or failed. Any of them
// means a replay would repeat a side effect; zero does not prove none ran.
func (c Checkpoint) Executed() int {
	executed := 0
	for _, r := range c.ToolResults {
		if r.Status == ToolCompleted || r.Status == ToolFailed {
			executed++
		}
	}
	return executed
}

// Rebuild is how the next model's context is constructed.
type Rebuild string

const (
	// FromCheckpoint sends the compact checkpoint plus only the recent and
	// extracted history and tool results the turn needs. This is the default and
	// the only rebuild a migration uses on its own.
	FromCheckpoint Rebuild = "CHECKPOINT"
	// FullTranscript resends the whole transcript. It is never a fallback and
	// never automatic: it is an explicit decision with its own estimated cost,
	// because the next model processes every token again with no KV cache reuse.
	FullTranscript Rebuild = "FULL_TRANSCRIPT"
)

// Budget bounds what a migration may cost in input tokens. Zero is not
// "unlimited": a budget nobody set must not authorize a rebuild.
type Budget struct {
	MaxInputTokens int64
}

// Migration is the route decision for the next turn, taken at a boundary.
type Migration struct {
	// Model is the model id the next turn starts on.
	Model string
	// Rebuild is how the next model's context is built.
	Rebuild Rebuild
	// InputTokens estimates the input tokens the rebuilt context will cost.
	// Zero means nobody estimated it.
	InputTokens int64
}

// Allowed reports whether this migration may proceed within b. It fails closed:
// an unknown rebuild, an unestimated cost, an unset budget or a model-less
// migration is refused, because each of those means paying for input the
// installation cannot account for.
func (m Migration) Allowed(b Budget) bool {
	if m.Model == "" || m.InputTokens <= 0 || b.MaxInputTokens <= 0 {
		return false
	}
	if m.Rebuild != FromCheckpoint && m.Rebuild != FullTranscript {
		return false
	}
	return m.InputTokens <= b.MaxInputTokens
}
