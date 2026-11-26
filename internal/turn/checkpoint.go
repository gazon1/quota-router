package turn

// Checkpoint is the agent-owned state a turn boundary carries into the next
// turn. It is what makes a model switch not a replay: tool results that already
// ran travel here, so the next model reads them instead of executing them again.
//
// A checkpoint never holds a provider payload, a credential or a whole file:
// References names material (a path, an id, a URL) and the agent decides whether
// re-reading it is worth the tokens. Nothing here is derived from a failed
// response body, which may carry a credential.
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

// ToolResult is one tool call and its outcome, kept so a migrated turn resumes
// after it instead of at it.
type ToolResult struct {
	Call   string
	Status ToolStatus
	Result string
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
	Model   string
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
