package turn

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Checkpoint is the agent-owned state a turn boundary carries into the next
// turn. It is what makes a model switch not a replay: tool results that already
// ran travel here, so the next model reads them instead of executing them again.
//
// Every field holds a summary the agent wrote, never a provider payload, a raw
// error or a whole file. Validate enforces what a type can enforce — bounded
// size, closed vocabularies, and the credential shapes this repository already
// refuses (config refuses a URL with embedded credentials, quota refuses a raw
// provider error) — and refuses the rest. It cannot prove prose is secret-free:
// a caller that pastes raw tool output into Goal has bypassed the contract, and
// the fix is to summarize, not to forward. Call Validate before the checkpoint
// leaves for another model.
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

// maxFieldRunes bounds every string in a checkpoint. A value past this is a file
// dump rather than a summary, and resending it to another model is the cost this
// contract exists to avoid.
const maxFieldRunes = 4096

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
// output is truncated and marked, because a whole file must not cross the
// boundary; the marker tells the next model to re-read the source if it needs
// the rest, instead of trusting a silently cut summary.
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
	if len(s) <= maxFieldRunes {
		// Cheap path: a byte-length under the cap cannot exceed it in runes.
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxFieldRunes {
		return s
	}
	const marker = "…[truncated]"
	return string(runes[:maxFieldRunes-len([]rune(marker))]) + marker
}

// Validate reports the fields that break the boundary. It is the gate before a
// checkpoint leaves for another model: a field that is a file, or carries the
// shape of a credential, is refused rather than sent.
func (c Checkpoint) Validate() error {
	var errs []error

	check := func(field, s string) {
		if n := len([]rune(s)); n > maxFieldRunes {
			errs = append(errs, fmt.Errorf("%s is %d runes, which is a file and not a summary", field, n))
		}
		if shape := credentialShape(s); shape != "" {
			errs = append(errs, fmt.Errorf("%s carries %s, which must not cross the boundary", field, shape))
		}
	}

	check("goal", c.Goal)
	for i, s := range c.Completed {
		check(fmt.Sprintf("completed[%d]", i), s)
	}
	for i, s := range c.Decisions {
		check(fmt.Sprintf("decisions[%d]", i), s)
	}
	for i, s := range c.Pending {
		check(fmt.Sprintf("pending[%d]", i), s)
	}
	for i, s := range c.References {
		check(fmt.Sprintf("references[%d]", i), s)
	}

	for _, r := range c.ToolResults {
		if r.Call == "" {
			errs = append(errs, errors.New("tool result with an empty call name"))
			continue
		}
		check("tool call "+r.Call, r.Call)
		check("tool result of "+r.Call, r.Result)
		switch {
		case !r.Status.known():
			errs = append(errs, errors.New("undeclared tool status for "+r.Call))
		case r.Status == ToolFailed && !ToolFailure(r.Result).known():
			errs = append(errs, errors.New("failed tool result must carry a declared failure, not raw output: "+r.Call))
		case r.Status == ToolNotStarted && r.Result != "":
			errs = append(errs, errors.New("a call that never started must not carry a result: "+r.Call))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// credentialShape names the credential form s carries, or "" when it carries
// none. It is deliberately narrow — the header and query shapes a provider error
// arrives in, plus the URL rule config.checkURL already applies. It is not a
// secret scanner and does not claim to be one; it catches the shapes this
// installation itself hands around, and it must not reject ordinary prose, or a
// caller learns to ignore the error.
func credentialShape(s string) string {
	lower := strings.ToLower(s)
	// Every occurrence counts: "token= unset; token=sk-live-abc" must not hide
	// the second one behind the first placeholder. A header or a parameter
	// without a value ("authorization: none required") is prose.
	for _, p := range []struct{ name, needle string }{
		{"an Authorization header", "authorization:"},
		{"an api_key parameter", "api_key="},
		{"a token parameter", "token="},
		{"a password parameter", "password="},
	} {
		if anyValueAfter(lower, p.needle) {
			return p.name
		}
	}
	// An explicit scheme names its value, so a single word after "Bearer" is a
	// credential whatever its alphabet: prose continues with more words ("Bearer
	// responsibility belongs to the policy owner"), a token does not.
	if anyBearerToken(lower) {
		return "a bearer token"
	}
	for _, u := range urlsIn(s) {
		// The same rule as config.checkURL: user:password@host is a credential.
		// The URL is extracted first, because a URL inside a sentence does not
		// parse as one and an error there must not read as "no credential".
		if parsed, err := url.Parse(u); err == nil && parsed.User != nil {
			return "credentials embedded in a URL"
		}
	}
	// A bare JWT carries no keyword to key on; its shape is the declaration.
	for _, w := range strings.Fields(s) {
		if strings.HasPrefix(strings.ToLower(w), "eyj") && strings.Count(w, ".") == 2 {
			return "a JWT"
		}
	}
	return ""
}

// functionWords are the grammar that can legitimately follow "bearer" in a
// sentence. Anything else after the scheme is treated as a value.
var functionWords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "is": true, "are": true,
	"was": true, "were": true, "as": true, "in": true, "on": true, "to": true,
	"for": true, "by": true, "and": true, "or": true, "not": true, "be": true,
	"has": true, "have": true, "had": true, "may": true, "must": true,
}

// placeholders are the values nobody means as a credential.
var placeholders = map[string]bool{
	"": true, "none": true, "unset": true, "empty": true, "optional": true,
	"required": true, "redacted": true, "***": true,
}

// anyValueAfter reports whether any occurrence of needle is followed by a value
// that is not a placeholder.
func anyValueAfter(lower, needle string) bool {
	for from := 0; ; {
		i := strings.Index(lower[from:], needle)
		if i < 0 {
			return false
		}
		rest := strings.Fields(lower[from+i+len(needle):])
		if len(rest) > 0 && !placeholders[strings.Trim(rest[0], "\"'`(),.;:")] {
			return true
		}
		from += i + len(needle)
		if from >= len(lower) {
			return false
		}
	}
}

// anyBearerToken reports whether s mentions the Bearer scheme followed by a value
// nobody marked as a placeholder. The scheme name is case-insensitive
// (RFC 7235 §2.1), so "bearer abcdefghijklmnop was rejected" is a credential in a
// log line just as much as "Bearer abcdefghijklmnop".
//
// Nothing here tries to decide whether a value is a credential or an English
// word: character, vowel and length heuristics could not separate
// "abcdefghijklmnop" from prose, and a wrong guess sends a live credential to the
// next model. The rule fails closed instead, with one narrow escape: a listed
// function word ("the bearer of this report", "the bearer is the owner") is
// grammar, never a credential. Everything else after the scheme is refused, and
// prose that needs more rephrases — a documented and recoverable cost, unlike a
// leaked credential.
func anyBearerToken(lower string) bool {
	for from := 0; ; {
		i := strings.Index(lower[from:], "bearer ")
		if i < 0 {
			return false
		}
		fields := strings.Fields(lower[from+i+len("bearer "):])
		if len(fields) > 0 {
			w := strings.Trim(fields[0], "\"'`(),.;:")
			if !placeholders[w] && !functionWords[w] {
				return true
			}
		}
		from += i + len("bearer ")
		if from >= len(lower) {
			return false
		}
	}
}

// urlsIn returns the http(s) URLs s mentions, including one inside a sentence.
func urlsIn(s string) []string {
	var out []string
	for _, scheme := range []string{"http://", "https://"} {
		from := 0
		for {
			i := strings.Index(strings.ToLower(s[from:]), scheme)
			if i < 0 {
				break
			}
			start := from + i
			end := start
			for end < len(s) && !strings.ContainsRune(" \t\n\"'<>()[]{}", rune(s[end])) {
				end++
			}
			out = append(out, s[start:end])
			from = end
		}
	}
	return out
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

// RebuildKind is how the next model's context is constructed.
type RebuildKind string

const (
	// FromCheckpoint sends the compact checkpoint plus only the recent and
	// extracted history and tool results the turn needs. This is the default.
	FromCheckpoint RebuildKind = "CHECKPOINT"
	// FullTranscript resends the whole transcript. It is never a fallback and
	// never automatic: the next model processes every token again with no KV
	// cache reuse, so it costs a whole input pass.
	FullTranscript RebuildKind = "FULL_TRANSCRIPT"
)

// Rebuild pairs a kind with its input-token estimate, so the number cannot be
// lent to a different rebuild by a field mix-up. Whether it measures the right
// context is still the caller's word: only accounting (#23) knows the real cost.
type Rebuild struct {
	Kind RebuildKind
	// InputTokens estimates what this rebuild costs the next model. Zero means
	// nobody estimated it.
	InputTokens int64
}

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
	// AuthorizedBy names the policy or agent that decided on a full-context
	// replay. The issue requires that decision to be explicit, so Allowed
	// refuses a full replay without it; a checkpoint rebuild needs none.
	AuthorizedBy string
}

// Allowed reports whether this migration may proceed within b. It fails closed:
// an unknown rebuild, an unestimated cost, an unset budget, a model-less
// migration and an unauthorized full replay are all refused, because each of
// those means paying for input the installation cannot account for.
//
// The estimate is the caller's number. Allowed can compare it with the budget,
// but it cannot tell whether the number measures the rebuild it sits in, nor
// what the failed route was actually charged. Real accounting is #23.
func (m Migration) Allowed(b Budget) bool {
	if m.Model == "" || m.Rebuild.InputTokens <= 0 || b.MaxInputTokens <= 0 {
		return false
	}
	switch m.Rebuild.Kind {
	case FromCheckpoint:
		// The compact rebuild is the default and needs no separate decision.
	case FullTranscript:
		if strings.TrimSpace(m.AuthorizedBy) == "" {
			return false
		}
	default:
		return false
	}
	return m.Rebuild.InputTokens <= b.MaxInputTokens
}
