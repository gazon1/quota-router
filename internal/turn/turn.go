// Package turn defines where one model's turn ends and where another may start,
// so a quota or provider failure can neither replay work nor migrate a turn that
// is already in flight.
//
// A model switch is not a continuation: the next model inherits no KV cache, so
// everything it receives is input tokens it processes and the installation pays
// for again. Migration is therefore a context rebuild at a boundary, from a
// checkpoint, within an input-token budget, never a transparent retry.
package turn

// Stage is how far an attempt got before it stopped.
type Stage string

const (
	// NotDispatched means the attempt never left this installation, for example
	// because local quota policy rejected it. Nothing was sent, delivered or
	// executed, so nothing can repeat.
	NotDispatched Stage = "NOT_DISPATCHED"
	// AwaitingFirstToken means the request was sent and no output arrived. The
	// provider may still have processed and charged the context, and the outcome
	// is unknown: a retry can pay for the same input twice.
	AwaitingFirstToken Stage = "AWAITING_FIRST_TOKEN"
	// Streaming means part of the response reached the caller. A retry would
	// deliver that part a second time.
	Streaming Stage = "STREAMING"
	// AfterToolExecution means at least one tool ran. A retry would repeat its
	// side effect; the checkpoint, not a replay, carries it forward.
	AfterToolExecution Stage = "AFTER_TOOL_EXECUTION"
)

// Disposition is what a stopped attempt permits.
type Disposition string

const (
	// StartOver means the attempt is indistinguishable from one that never
	// happened, so the same turn may start on another model. This is not a
	// migration: no turn is in flight and no state is lost.
	StartOver Disposition = "START_OVER"
	// Surface means the attempt may have been charged, delivered or executed.
	// The caller reports the failure, keeps this turn's route decision as it was,
	// and reconsiders the route at the next turn boundary. It never retries
	// automatically.
	Surface Disposition = "SURFACE"
)

// Decide reports what a stopped attempt permits. An undeclared stage is treated
// as one that may have delivered output: nothing proves it was free of side
// effects, and a guess here duplicates work the user already saw.
func Decide(s Stage) Disposition {
	switch s {
	case NotDispatched:
		return StartOver
	case AwaitingFirstToken, Streaming, AfterToolExecution:
		return Surface
	}
	return Surface
}
