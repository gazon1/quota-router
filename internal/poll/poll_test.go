package poll

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/quota"
	"github.com/gazon1/quota-router/internal/store"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recorder is a Sink that records what a poll persisted, so a test asserts on
// what would have reached the database without one.
type recorder struct {
	mu       sync.Mutex
	snaps    []store.QuotaSnapshot
	health   []store.HealthRecord
	attempts map[string]time.Time
	success  map[string]time.Time
	// onSave runs before each snapshot write, so a test can block a poll.
	onSave func()
}

func newRecorder() *recorder {
	return &recorder{attempts: map[string]time.Time{}, success: map[string]time.Time{}}
}

func (r *recorder) SaveQuotaSnapshot(_ context.Context, s store.QuotaSnapshot) error {
	if r.onSave != nil {
		r.onSave()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snaps = append(r.snaps, s)
	return nil
}

func (r *recorder) SaveProviderHealth(_ context.Context, h store.HealthRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health = append(r.health, h)
	return nil
}

func (r *recorder) SavePollState(_ context.Context, provider string, attemptAt, successAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts[provider] = attemptAt
	if !successAt.IsZero() {
		r.success[provider] = successAt
	}
	return nil
}

func (r *recorder) snapshots() []store.QuotaSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.QuotaSnapshot(nil), r.snaps...)
}

func (r *recorder) successAt(provider string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.success[provider]
}

// scripted is a Source whose answers are chosen by the test, one per call.
type scripted struct {
	mu      sync.Mutex
	calls   int
	answers []answer
}

type answer struct {
	snap  quota.Snapshot
	state quota.ProviderState
}

func (s *scripted) Fetch(context.Context) (quota.Snapshot, quota.ProviderState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if len(s.answers) == 0 {
		return good(), healthy()
	}
	if i >= len(s.answers) {
		return s.answers[len(s.answers)-1].snap, s.answers[len(s.answers)-1].state
	}
	return s.answers[i].snap, s.answers[i].state
}

func (s *scripted) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func windows(remaining float64) []quota.Window {
	limit := 100.0
	r := remaining
	used := limit - remaining
	return []quota.Window{{
		Name: "5h", Limit: &limit, Used: &used, Remaining: &r,
		ResetsAt: time.Now().Add(time.Hour).UTC(),
	}}
}

func good() quota.Snapshot {
	return quota.Observed("minimax", "test", time.Now().UTC(), windows(80), 0.1)
}

func healthy() quota.ProviderState {
	return quota.NewProviderState("minimax", quota.Healthy, quota.ReasonNone, time.Now().UTC())
}

func transient() answer {
	return answer{
		snap:  quota.Failed("minimax", "test", time.Now().UTC(), quota.TransientFetchFailure),
		state: quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonTimeout, time.Now().UTC()),
	}
}

// newTestPoller builds a poller with instant waits, so no test sleeps.
func newTestPoller(rec *recorder, targets ...Target) *Poller {
	p := New(time.Hour, 3*time.Hour, testLog(), rec, targets...)
	p.wait = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	p.jitter = func(d time.Duration) time.Duration { return d }
	return p
}

// A provider whose poll never returns must not delay the other provider's polls
// or its state updates.
func TestProviderIsolation(t *testing.T) {
	rec := newRecorder()
	release := make(chan struct{})
	stuck := func(context.Context) (quota.Snapshot, quota.ProviderState) {
		<-release
		return good(), healthy()
	}
	p := newTestPoller(rec,
		Target{Name: "stuck", Source: SourceFunc(stuck)},
		Target{Name: "fast", Source: SourceFunc(func(context.Context) (quota.Snapshot, quota.ProviderState) {
			return good(), healthy()
		})},
	)
	p.Interval = time.Millisecond // a real tick, so the isolation is observable

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	// Polls of the healthy provider keep landing while the other is still in its
	// first attempt.
	deadline := time.Now().Add(2 * time.Second)
	for {
		polls := rec.snapshots()
		if len(polls) >= 3 {
			for _, s := range polls {
				if s.Provider != "fast" {
					close(release)
					cancel()
					<-done
					t.Fatalf("persisted %q while that provider was stuck", s.Provider)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			close(release)
			cancel()
			<-done
			t.Fatalf("healthy provider persisted %d polls while another was stuck, want 3+", len(polls))
		}
		time.Sleep(time.Millisecond)
	}

	close(release)
	cancel()
	<-done
}

// A cancelled context stops the poller without leaking a goroutine that would
// later write to a closed store.
func TestStopsOnCancellation(t *testing.T) {
	rec := newRecorder()
	started := make(chan struct{}, 1)
	// The attempt blocks past cancellation, so shutdown must not wait for it
	// forever and must not persist a half-finished attempt either.
	blocking := func(ctx context.Context) (quota.Snapshot, quota.ProviderState) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return quota.Failed("minimax", "test", time.Now().UTC(), quota.TransientFetchFailure),
			quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonTimeout, time.Now().UTC())
	}
	p := newTestPoller(rec, Target{Name: "minimax", Source: SourceFunc(blocking)})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation: goroutine leaked")
	}
	if n := len(rec.snapshots()); n != 0 {
		t.Fatalf("persisted %d snapshots for a cancelled attempt, want 0", n)
	}
}

// A transient failure is retried within the poll; the attempt count is capped.
func TestTransientFailureIsRetriedAndCapped(t *testing.T) {
	rec := newRecorder()
	src := &scripted{}
	src.answers = []answer{transient()} // every attempt fails the same way

	p := newTestPoller(rec, Target{Name: "minimax", Source: src})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.poll(ctx, p.Targets[0])

	if got := src.callCount(); got != defaultAttempts {
		t.Fatalf("attempts = %d, want the cap of %d", got, defaultAttempts)
	}
	// Each attempt is persisted, so last-attempt tracks the retries.
	if got := len(rec.snapshots()); got != defaultAttempts {
		t.Fatalf("persisted %d snapshots, want one per attempt (%d)", got, defaultAttempts)
	}
	// No attempt succeeded, so no success time was recorded at all.
	if got := rec.successAt("minimax"); !got.IsZero() {
		t.Fatalf("last success = %s, want none after only failures", got)
	}
}

// A permanent failure is not retried: a rejected credential answers the same way.
func TestPermanentFailureIsNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name  string
		snap  quota.Snapshot
		state quota.ProviderState
	}{
		{"auth", quota.Failed("minimax", "test", time.Now().UTC(), quota.AuthFailure),
			quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonAuthFailed, time.Now().UTC())},
		{"malformed", quota.Failed("minimax", "test", time.Now().UTC(), quota.MalformedResponse),
			quota.NewProviderState("minimax", quota.Degraded, quota.ReasonServerError, time.Now().UTC())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder()
			src := &scripted{}
			src.answers = []answer{{snap: tc.snap, state: tc.state}}
			p := newTestPoller(rec, Target{Name: "minimax", Source: src})

			p.poll(context.Background(), p.Targets[0])
			if got := src.callCount(); got != 1 {
				t.Fatalf("attempts = %d, want 1: a permanent failure is not retried", got)
			}
		})
	}
}

// A later success records its own timestamp, and a later failure keeps it.
func TestAttemptAndSuccessTimestampsAreIndependent(t *testing.T) {
	rec := newRecorder()
	src := &scripted{}
	succeededAt := time.Now().UTC()
	src.answers = []answer{
		{snap: quota.Observed("minimax", "test", succeededAt, windows(80), 0.1), state: healthy()},
		transient(),
	}
	p := newTestPoller(rec, Target{Name: "minimax", Source: src})
	p.Attempts = 1 // one attempt per poll, so the sequence is exactly the script

	src.mu.Lock()
	src.answers[1].state = quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonTimeout, time.Now().UTC())
	src.mu.Unlock()

	p.poll(context.Background(), p.Targets[0])
	if got := rec.successAt("minimax"); !got.Equal(succeededAt) {
		t.Fatalf("last success = %s, want the observation time %s", got, succeededAt)
	}

	p.poll(context.Background(), p.Targets[0]) // fails
	if got := rec.successAt("minimax"); !got.Equal(succeededAt) {
		t.Fatalf("a failure overwrote last success: %s, want %s", got, succeededAt)
	}
	if got := rec.attempts["minimax"]; !got.After(succeededAt) {
		t.Fatalf("last attempt = %s, want the failure's own later time", got)
	}
}

// A successful poll writes the state the provider reported, never AVAILABLE by
// default: an exhausted provider stays exhausted.
func TestSuccessDoesNotImplyAvailable(t *testing.T) {
	rec := newRecorder()
	src := &scripted{}
	src.answers = []answer{{
		snap:  quota.Observed("minimax", "test", time.Now().UTC(), windows(0), 0.1),
		state: healthy(),
	}}
	p := newTestPoller(rec, Target{Name: "minimax", Source: src})
	p.poll(context.Background(), p.Targets[0])

	snaps := rec.snapshots()
	if len(snaps) != 1 {
		t.Fatalf("persisted %d snapshots, want 1", len(snaps))
	}
	if snaps[0].State != store.QuotaExhausted {
		t.Fatalf("state = %s, want EXHAUSTED from the reported window", snaps[0].State)
	}

	// A healthy provider whose body established no quota is UNKNOWN, not AVAILABLE.
	rec = newRecorder()
	src = &scripted{}
	src.answers = []answer{{snap: quota.Failed("minimax", "test", time.Now().UTC(), quota.MalformedResponse), state: healthy()}}
	p = newTestPoller(rec, Target{Name: "minimax", Source: src})
	p.Attempts = 1
	p.poll(context.Background(), p.Targets[0])
	snaps = rec.snapshots()
	if len(snaps) != 1 || snaps[0].State != store.QuotaUnknown {
		t.Fatalf("state = %+v, want UNKNOWN for a body with no usable quota", snaps)
	}
	if snaps[0].UnknownReason == nil || *snaps[0].UnknownReason != store.ReasonMalformed {
		t.Fatalf("unknown_reason = %v, want malformed", snaps[0].UnknownReason)
	}
}

// An observation past its freshness bound is persisted as UNKNOWN, with its
// windows kept for diagnostics.
func TestStaleTransitionIsPersistedAsUnknown(t *testing.T) {
	rec := newRecorder()
	src := &scripted{}
	src.answers = []answer{{
		snap:  quota.Observed("minimax", "test", time.Now().Add(-2*time.Hour).UTC(), windows(80), 0.1),
		state: healthy(),
	}}
	p := newTestPoller(rec, Target{Name: "minimax", Source: src})
	p.StaleAfter = time.Hour
	p.poll(context.Background(), p.Targets[0])

	snaps := rec.snapshots()
	if len(snaps) != 1 {
		t.Fatalf("persisted %d snapshots, want 1", len(snaps))
	}
	if snaps[0].State != store.QuotaUnknown {
		t.Fatalf("state = %s, want UNKNOWN for an observation past its freshness bound", snaps[0].State)
	}
	if snaps[0].UnknownReason == nil || *snaps[0].UnknownReason != store.ReasonStale {
		t.Fatalf("unknown_reason = %v, want stale", snaps[0].UnknownReason)
	}
	if len(snaps[0].Windows) != 1 {
		t.Fatalf("windows = %d, want the stale values kept for diagnostics", len(snaps[0].Windows))
	}
}

// A poll slower than the interval must not overlap the next one: one poll at a
// time per provider, with the next one starting only after the previous returns.
func TestNoOverlappingPolls(t *testing.T) {
	rec := newRecorder()
	var mu sync.Mutex
	inFlight, peak := 0, 0
	slow := func(context.Context) (quota.Snapshot, quota.ProviderState) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond) // longer than the interval below
		mu.Lock()
		inFlight--
		mu.Unlock()
		return good(), healthy()
	}

	p := newTestPoller(rec, Target{Name: "minimax", Source: SourceFunc(slow)})
	p.Interval = time.Millisecond
	p.wait = sleep // the real wait, so overlap would show up in the counter

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	p.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if peak != 1 {
		t.Fatalf("peak concurrent polls = %d, want 1: polls for one provider overlapped", peak)
	}
	if len(rec.snapshots()) < 2 {
		t.Fatalf("persisted %d polls, want several to make the assertion meaningful", len(rec.snapshots()))
	}
}

// After an outage the loop resumes with one poll per tick, not a burst of
// catch-up polls for the ticks that passed.
func TestRecoveryDoesNotBurst(t *testing.T) {
	rec := newRecorder()
	release := make(chan struct{})
	p := newTestPoller(rec, Target{Name: "minimax", Source: SourceFunc(func(context.Context) (quota.Snapshot, quota.ProviderState) {
		<-release // the outage: the attempt outlasts the interval before answering
		return transient().snap, transient().state
	})})
	p.Attempts = 1 // a failed attempt is not retried within the poll
	p.Interval = 5 * time.Millisecond
	p.wait = sleep // the real wait, so the cadence is observable

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	defer cancel()

	// The outage lasts past the interval, then ends in the failed attempt above.
	time.Sleep(10 * time.Millisecond)
	failedAt := time.Now().UTC()
	close(release)

	// The failed attempt is persisted, then the loop waits the interval before
	// the single recovery poll.
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.snapshots()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("outage attempt was not persisted")
		}
		time.Sleep(time.Millisecond)
	}

	// Recovery starts one poll after the full interval: no burst of catch-up
	// polls for the ticks the outage used up.
	deadline = time.Now().Add(2 * time.Second)
	for len(rec.snapshots()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("recovery did not resume with a single poll after the interval")
		}
		time.Sleep(time.Millisecond)
	}
	if elapsed := time.Now().UTC().Sub(failedAt); elapsed < p.Interval {
		t.Fatalf("recovery started %s after the failed attempt, want >= the interval of %s", elapsed, p.Interval)
	}

	// The normal cadence continues after the recovery.
	deadline = time.Now().Add(2 * time.Second)
	for len(rec.snapshots()) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("polling did not resume its normal cadence after the recovery")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	<-done

	// Outage, one recovery poll, then the next normal cadence: exactly one poll
	// per interval, and the missed ticks were not replayed.
	if got := len(rec.snapshots()); got != 3 {
		t.Fatalf("persisted %d polls, want 3: one per cadence, no catch-up burst", got)
	}
}

// The retry delay is exponential, capped, and jittered within the upper half.
func TestBackoffIsBoundedAndJittered(t *testing.T) {
	p := New(time.Minute, time.Hour, testLog(), newRecorder())
	p.BaseDelay, p.MaxDelay = 100*time.Millisecond, 400*time.Millisecond
	p.jitter = spread

	for _, tc := range []struct {
		n    int
		want time.Duration
	}{{1, 100 * time.Millisecond}, {2, 200 * time.Millisecond}, {3, 400 * time.Millisecond}, {4, 400 * time.Millisecond}, {9, 400 * time.Millisecond}} {
		if got := p.delay(tc.n); got != tc.want {
			t.Errorf("delay(%d) = %s, want %s", tc.n, got, tc.want)
		}
	}

	// Jitter never exceeds the delay and stays in the documented range.
	for i := 0; i < 200; i++ {
		got := spread(400 * time.Millisecond)
		if got < 200*time.Millisecond || got > 400*time.Millisecond {
			t.Fatalf("spread(400ms) = %s, want [200ms, 400ms]", got)
		}
	}
}

// A non-positive backoff bound must not turn a retry into a spin.
func TestZeroBaseDelayDoesNotSpin(t *testing.T) {
	p := New(time.Minute, time.Hour, testLog(), newRecorder())
	p.BaseDelay, p.MaxDelay = 0, 0
	if got := p.delay(3); got != 0 {
		t.Fatalf("delay = %s, want 0", got)
	}
	// wait(0) with a live context returns immediately, which the loop tolerates
	// because the attempt count is still capped.
	if !sleep(context.Background(), 0) {
		t.Fatal("sleep(0) with a live context returned false")
	}
}

// SourceFunc adapts a function to Source, so a test needs no adapter type.
type SourceFunc func(ctx context.Context) (quota.Snapshot, quota.ProviderState)

func (f SourceFunc) Fetch(ctx context.Context) (quota.Snapshot, quota.ProviderState) { return f(ctx) }

// Every outcome a poller can record must pass the store's own validation: a
// mapping that trips a constraint would only show up as a failed write in
// production, where it is logged and the observation is lost.
func TestOutcomesSurviveStoreValidation(t *testing.T) {
	st, err := store.Open(store.Config{Path: filepath.Join(t.TempDir(), "test.db"), BusyTimeout: 5 * time.Second}, testLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	cases := map[string]answer{
		"success":   {snap: quota.Observed("minimax", "test", time.Now().UTC(), windows(80), 0.1), state: healthy()},
		"exhausted": {snap: quota.Observed("minimax", "test", time.Now().UTC(), windows(0), 0.1), state: healthy()},
		"transient": transient(),
		"auth": {snap: quota.Failed("minimax", "test", time.Now().UTC(), quota.AuthFailure),
			state: quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonAuthFailed, time.Now().UTC())},
		"malformed": {snap: quota.Failed("minimax", "test", time.Now().UTC(), quota.MalformedResponse),
			state: quota.NewProviderState("minimax", quota.Degraded, quota.ReasonServerError, time.Now().UTC())},
		"rate limited": {snap: quota.Failed("minimax", "test", time.Now().UTC(), quota.TransientFetchFailure),
			state: quota.NewProviderState("minimax", quota.Degraded, quota.ReasonRateLimited, time.Now().UTC())},
		"unreachable": {snap: quota.Failed("minimax", "test", time.Now().UTC(), quota.TransientFetchFailure),
			state: quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonUnreachable, time.Now().UTC())},
		"stale copy": {snap: func() quota.Snapshot {
			s := quota.Observed("minimax", "test", time.Now().Add(-time.Hour).UTC(), windows(80), 0.1)
			s.Stale = true
			return s
		}(), state: healthy()},
	}

	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			src := &scripted{}
			src.answers = []answer{a}
			p := newTestPoller(newRecorder(), Target{Name: "minimax", Source: src})
			p.Attempts = 1
			p.StaleAfter = time.Minute

			snapshot := observation("minimax", a.snap, p.StaleAfter, time.Now())
			if err := snapshot.Validate(); err != nil {
				t.Fatalf("quota snapshot does not validate: %v", err)
			}
			if err := health("minimax", a.state).Validate(); err != nil {
				t.Fatalf("health record does not validate: %v", err)
			}
			// And the store accepts it, constraints included.
			if err := st.SaveQuotaSnapshot(context.Background(), snapshot); err != nil {
				t.Fatalf("SaveQuotaSnapshot: %v", err)
			}
			if err := st.SaveProviderHealth(context.Background(), health("minimax", a.state)); err != nil {
				t.Fatalf("SaveProviderHealth: %v", err)
			}
		})
	}
}
