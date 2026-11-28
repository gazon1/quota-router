// Package poll schedules quota polling: one independently supervised loop per
// configured provider, at a fixed interval, with bounded retries for transient
// failures only and every outcome persisted.
//
// Polling is not usage accounting: local usage is event or report driven and is
// never derived from a poll. A poll that succeeds also does not make quota
// AVAILABLE — the state written here comes from the provider's own reported
// windows through quota.Snapshot.StateAt, so an observation that is old, stale
// or unusable is persisted as UNKNOWN rather than as current quota.
package poll

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/gazon1/quota-router/internal/quota"
	"github.com/gazon1/quota-router/internal/store"
)

// Source is one provider adapter: a single bounded fetch that reports what it
// could establish. minimax.Adapter is the production implementation, and its
// Fetch is the whole retry policy input — classification stays provider policy.
type Source interface {
	Fetch(ctx context.Context) (quota.Snapshot, quota.ProviderState)
}

// Sink persists what a poll observed. store.Store implements it.
type Sink interface {
	// SavePollResult persists one poll cycle — snapshot, health and the
	// poll-state anchor — as a single transaction, so a cycle is never observed
	// half-written and a failed cycle cannot leave a newer snapshot behind an
	// anchor that was not advanced.
	SavePollResult(ctx context.Context, provider string, snap store.QuotaSnapshot, health store.HealthRecord, attemptAt, successAt time.Time) error
}

// Target is one configured provider and the adapter that polls it. The name is
// what every persisted row is written under.
type Target struct {
	Name   string
	Source Source
}

// Retry bounds. A poll is one attempt plus at most Attempts-1 retries, so a
// provider cannot stretch a single poll beyond its interval, and a provider that
// stays down is retried at the normal cadence instead of in a tight loop.
const (
	defaultAttempts  = 3
	defaultBaseDelay = 500 * time.Millisecond
	defaultMaxDelay  = 5 * time.Second
)

// Poller polls every target until its context is cancelled.
type Poller struct {
	// Interval is the normal cadence between polls of one provider.
	Interval time.Duration
	// StaleAfter is the freshness bound: an observation older than this is
	// persisted as UNKNOWN/STALE_OBSERVATION, never as current quota.
	StaleAfter time.Duration
	// Attempts bounds one poll, the first attempt included. Values below one
	// mean a single attempt.
	Attempts int
	// BaseDelay and MaxDelay bound the exponential backoff between retries.
	BaseDelay time.Duration
	MaxDelay  time.Duration

	Sink    Sink
	Log     *slog.Logger
	Targets []Target

	// wait and jitter are the clock and the randomness, replaceable in tests so
	// scheduling is deterministic instead of sleep-driven.
	wait   func(ctx context.Context, d time.Duration) bool
	jitter func(time.Duration) time.Duration
}

// loopState owns the retry counters for one provider loop. Each loop owns its
// own state, so providers polled concurrently never share a counter and never
// mix one provider's attempts into another's history.
type loopState struct {
	// consecutiveFailures counts committed cycles that did not establish quota;
	// the per-attempt debug log reports it, the retry count and the duration, so
	// a provider outage is diagnosable from logs alone. It counts only cycles that
	// were persisted: a failed write is logged with persist_ok=false and leaves
	// the failure history of the last committed cycle alone.
	consecutiveFailures int
	// processLastSuccessAt is the newest committed cycle in this process that
	// established quota; a restart starts the history over even though poll_state
	// keeps the durable anchor. It is a loop diagnostic, not the durable last
	// success a reader compares against.
	processLastSuccessAt time.Time
}

// New builds a poller with the production defaults for retry.
func New(interval, staleAfter time.Duration, log *slog.Logger, sink Sink, targets ...Target) *Poller {
	if log == nil {
		log = slog.Default()
	}
	return &Poller{
		Interval:   interval,
		StaleAfter: staleAfter,
		Attempts:   defaultAttempts,
		BaseDelay:  defaultBaseDelay,
		MaxDelay:   defaultMaxDelay,
		Sink:       sink,
		Log:        log,
		Targets:    targets,
		wait:       sleep,
		jitter:     spread,
	}
}

// Run polls every target until ctx is cancelled, then waits for the polls in
// flight and returns: no goroutine outlives it, so the caller may close the
// store afterwards.
//
// Each provider gets its own goroutine, so a provider that hangs, fails or backs
// off never delays another provider's polls or state updates.
func (p *Poller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range p.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.loop(ctx, t)
		}()
	}
	wg.Wait()
}

// loop polls one provider forever: one poll at a time, then the normal interval.
//
// The interval is a wait between polls, not a ticker, so an outage — or a poll
// that took longer than expected — resumes with exactly one poll and no burst of
// catch-up polls for the ticks that passed.
func (p *Poller) loop(ctx context.Context, t Target) {
	var st loopState
	for {
		p.poll(ctx, t, &st)
		if !p.wait(ctx, p.Interval) {
			return
		}
	}
}

// poll performs one poll: an attempt, and retries while the adapter classified
// the failure as transient. Every attempt is persisted, so last_attempt_at moves
// even when the poll never succeeds, while the loop's process-local last success
// keeps its own value until a committed cycle establishes quota again.
func (p *Poller) poll(ctx context.Context, t Target, st *loopState) {
	for attempt := 1; ; attempt++ {
		started := time.Now().UTC()
		snap, state := t.Source.Fetch(ctx)
		p.record(ctx, t.Name, st, snap, state, attempt, started)

		if attempt >= p.attempts() || !retryable(snap, state) {
			return
		}
		if !p.wait(ctx, p.jitter(p.delay(attempt))) {
			return
		}
	}
}

// record persists one attempt. A cancelled context is not an observation: the
// attempt was cut short by shutdown, so it must not be stored as a failure.
func (p *Poller) record(ctx context.Context, provider string, st *loopState, snap quota.Snapshot, state quota.ProviderState, attempt int, started time.Time) {
	if ctx.Err() != nil {
		return
	}
	at := time.Now().UTC()
	// Success means the attempt produced an observation the provider stands
	// behind, not that quota is AVAILABLE: an unusable or retained copy is not a
	// success, and its state is still derived from the windows below.
	success := snap.Confidence != quota.ConfidenceNone && !snap.Stale
	var successAt time.Time
	if success {
		successAt = snap.ObservedAt
	}
	var cycleErr error
	if err := p.Sink.SavePollResult(ctx, provider, observation(provider, snap, p.StaleAfter, at), health(provider, state), at, successAt); err != nil {
		cycleErr = err
		p.Log.Error("persist poll result", "provider", provider, "error", cycleErr)
	}

	// A success only counts once the cycle committed: the observation has to be
	// durably anchored before it becomes the provider's last success, so a failed
	// persistence leaves the failure history of the last committed cycle alone.
	if cycleErr == nil {
		if success {
			st.consecutiveFailures = 0
			st.processLastSuccessAt = successAt
		} else {
			st.consecutiveFailures++
		}
	}

	// started precedes the fetch and the persistence below, so the duration
	// covers the whole attempt — provider response and sink writes — not just
	// the provider call.
	p.Log.Debug("polled provider", "provider", provider,
		"health", state.Health, "reason", state.Reason(), "fresh", success,
		"attempt", attempt, "retries", attempt-1, "attempt_duration_ms", time.Since(started).Milliseconds(),
		"consecutive_failures", st.consecutiveFailures, "process_last_success_at", st.processLastSuccessAt.UTC().Format(time.RFC3339Nano),
		"last_attempt_at", at.UTC().Format(time.RFC3339Nano), "persist_ok", cycleErr == nil)
}

// attempts is the retry bound in the form the loop needs: at least one attempt.
func (p *Poller) attempts() int {
	if p.Attempts < 1 {
		return 1
	}
	return p.Attempts
}

// delay is the wait before retry n (n counts from one): exponential and capped,
// so the bound is reached quickly and stays there.
func (p *Poller) delay(n int) time.Duration {
	d := p.BaseDelay
	for i := 1; i < n && d < p.MaxDelay; i++ {
		d *= 2
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	if d < 0 {
		return 0
	}
	return d
}

// spread jitters a delay: the upper half stays, the lower half is random, so
// controllers that lost a provider together do not retry in lockstep.
func spread(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := int64(d / 2)
	return time.Duration(half + rand.Int64N(half+1))
}

// sleep waits for d unless ctx ends first; false means cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// retryable reports whether another attempt within this poll may establish
// quota. Classification is the adapter's: a rejected credential, an unusable
// body or a provider with no quota API answers the same way on a second try, so
// those are not retried; timeouts, unreachable hosts, rate limits and 5xx are.
func retryable(snap quota.Snapshot, state quota.ProviderState) bool {
	switch state.Health {
	case quota.Degraded, quota.Unavailable:
	default:
		// HEALTHY established something; UNKNOWN says nothing went wrong that a
		// retry could fix.
		return false
	}
	if st, reason := snap.State(); st == quota.QuotaUnknown {
		switch reason {
		case quota.AuthFailure, quota.MalformedResponse, quota.UnsupportedNoQuotaAPI:
			return false
		}
	}
	return true
}

// observation maps an observation to what is persisted. Its state comes from StateAt, so
// a last-known-good copy retained after a failed refresh is stored as UNKNOWN /
// stale — its windows stay for diagnostics, and no reader can mistake them for
// current quota.
//
// shortcut: staleness is decided here, when the row is written, so a row written
// just before the process died still reads as its old state. A reader must
// compare poll_state.last_attempt_at (already persisted) against the freshness
// bound; do that in the read path when routing or the status API lands.
func observation(provider string, s quota.Snapshot, staleAfter time.Duration, now time.Time) store.QuotaSnapshot {
	state, reason := s.StateAt(now, staleAfter)
	out := store.QuotaSnapshot{
		Provider: provider,
		State:    store.QuotaState(state),
		Source:   s.Source,
		// Subscription quota is never a local estimate, so every row is reported;
		// confidence in the observation is carried by State and Source instead.
		Confidence: store.Reported,
		ObservedAt: s.ObservedAt,
	}
	if state == quota.QuotaUnknown {
		r := unknownReason(reason)
		out.UnknownReason = &r
	}
	// The quota model carries no unit, so windows are stored without one rather
	// than guessed from the provider that produced them.
	for _, w := range s.Windows {
		out.Windows = append(out.Windows, store.QuotaWindow{
			Window:    w.Name,
			Limit:     w.Limit,
			Used:      w.Used,
			Remaining: w.Remaining,
			ResetAt:   resetAt(w.ResetsAt),
		})
	}
	return out
}

func resetAt(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// unknownReason maps the model's reason onto the persisted enum, which is a
// smaller closed set. An unmapped reason becomes "malformed": something about
// the observation could not be used.
func unknownReason(r quota.UnknownReason) store.UnknownReason {
	switch r {
	case quota.UnsupportedNoQuotaAPI:
		return store.ReasonUnsupported
	case quota.TransientFetchFailure:
		return store.ReasonFetchFailed
	case quota.StaleObservation:
		return store.ReasonStale
	case quota.AuthFailure:
		return store.ReasonAuth
	case quota.NotYetObserved:
		return store.ReasonNotObserved
	}
	return store.ReasonMalformed
}

// health maps an observation to what is persisted. The reason code is the stored
// error, so a health row explains itself without carrying provider text.
func health(provider string, s quota.ProviderState) store.HealthRecord {
	r := store.HealthRecord{
		Provider:   provider,
		State:      store.HealthState(s.Health),
		ObservedAt: s.ObservedAt,
		Error:      string(s.Reason()),
	}
	if !s.CooldownUntil.IsZero() {
		cu := s.CooldownUntil
		r.CooldownUntil = &cu
	}
	return r
}
