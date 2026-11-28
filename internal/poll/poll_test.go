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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	var offending string
	deadline := time.Now().Add(2 * time.Second)
	for {
		polls := rec.snapshots()
		if len(polls) >= 3 {
			for _, s := range polls {
				if s.Provider != "fast" {
					offending = s.Provider
					break
				}
			}
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	polls := len(rec.snapshots())

	close(release)
	cancel()
	<-done

	assert.Empty(t, offending, "persisted %q while that provider was stuck", offending)
	assert.True(t, polls >= 3, "healthy provider persisted %d polls while another was stuck, want 3+", polls)
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
		require.Fail(t, "Run did not return after cancellation: goroutine leaked")
	}
	assert.Empty(t, rec.snapshots(), "persisted %d snapshots for a cancelled attempt, want 0", len(rec.snapshots()))
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

	assert.Equal(t, defaultAttempts, src.callCount(), "attempts, want the cap")
	// Each attempt is persisted, so last-attempt tracks the retries.
	assert.Len(t, rec.snapshots(), defaultAttempts, "one persisted snapshot per attempt")
	// No attempt succeeded, so no success time was recorded at all.
	assert.True(t, rec.successAt("minimax").IsZero(), "no success time after only failures")
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
			assert.Equal(t, 1, src.callCount(), "a permanent failure is not retried")
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
	assert.WithinDuration(t, succeededAt, rec.successAt("minimax"), 0, "last success is the observation time")

	p.poll(context.Background(), p.Targets[0]) // fails
	assert.WithinDuration(t, succeededAt, rec.successAt("minimax"), 0, "a failure overwrote last success")
	assert.True(t, rec.attempts["minimax"].After(succeededAt), "last attempt is the failure's own later time")
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
	require.Len(t, snaps, 1)
	assert.Equal(t, store.QuotaExhausted, snaps[0].State, "from the reported window")

	// A healthy provider whose body established no quota is UNKNOWN, not AVAILABLE.
	rec = newRecorder()
	src = &scripted{}
	src.answers = []answer{{snap: quota.Failed("minimax", "test", time.Now().UTC(), quota.MalformedResponse), state: healthy()}}
	p = newTestPoller(rec, Target{Name: "minimax", Source: src})
	p.Attempts = 1
	p.poll(context.Background(), p.Targets[0])
	snaps = rec.snapshots()
	require.Len(t, snaps, 1)
	assert.Equal(t, store.QuotaUnknown, snaps[0].State, "for a body with no usable quota")
	require.NotNil(t, snaps[0].UnknownReason)
	assert.Equal(t, store.ReasonMalformed, *snaps[0].UnknownReason)
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
	require.Len(t, snaps, 1)
	assert.Equal(t, store.QuotaUnknown, snaps[0].State, "for an observation past its freshness bound")
	require.NotNil(t, snaps[0].UnknownReason)
	assert.Equal(t, store.ReasonStale, *snaps[0].UnknownReason)
	assert.Len(t, snaps[0].Windows, 1, "the stale values are kept for diagnostics")
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
	assert.Equal(t, 1, peak, "polls for one provider overlapped")
	assert.True(t, len(rec.snapshots()) >= 2, "persisted %d polls, want several to make the assertion meaningful", len(rec.snapshots()))
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
	timedOut := false
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.snapshots()) == 0 {
		if time.Now().After(deadline) {
			timedOut = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	require.False(t, timedOut, "outage attempt was not persisted")

	// Recovery starts one poll after the full interval: no burst of catch-up
	// polls for the ticks the outage used up.
	timedOut = false
	deadline = time.Now().Add(2 * time.Second)
	for len(rec.snapshots()) < 2 {
		if time.Now().After(deadline) {
			timedOut = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	require.False(t, timedOut, "recovery did not resume with a single poll after the interval")
	elapsed := time.Now().UTC().Sub(failedAt)
	assert.True(t, elapsed >= p.Interval, "recovery started %s after the failed attempt, want >= the interval of %s", elapsed, p.Interval)

	// The normal cadence continues after the recovery.
	timedOut = false
	deadline = time.Now().Add(2 * time.Second)
	for len(rec.snapshots()) < 3 {
		if time.Now().After(deadline) {
			timedOut = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	require.False(t, timedOut, "polling did not resume its normal cadence after the recovery")

	cancel()
	<-done

	// Outage, one recovery poll, then the next normal cadence: exactly one poll
	// per interval, and the missed ticks were not replayed.
	assert.Len(t, rec.snapshots(), 3, "one per cadence, no catch-up burst")
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
		assert.Equal(t, tc.want, p.delay(tc.n), "delay(%d)", tc.n)
	}

	// Jitter never exceeds the delay and stays in the documented range.
	for i := 0; i < 200; i++ {
		got := spread(400 * time.Millisecond)
		assert.True(t, got >= 200*time.Millisecond && got <= 400*time.Millisecond,
			"spread(400ms) = %s, want [200ms, 400ms]", got)
	}
}

// A non-positive backoff bound must not turn a retry into a spin.
func TestZeroBaseDelayDoesNotSpin(t *testing.T) {
	p := New(time.Minute, time.Hour, testLog(), newRecorder())
	p.BaseDelay, p.MaxDelay = 0, 0
	assert.Equal(t, time.Duration(0), p.delay(3))
	// wait(0) with a live context returns immediately, which the loop tolerates
	// because the attempt count is still capped.
	assert.True(t, sleep(context.Background(), 0), "sleep(0) with a live context returned false")
}

// SourceFunc adapts a function to Source, so a test needs no adapter type.
type SourceFunc func(ctx context.Context) (quota.Snapshot, quota.ProviderState)

func (f SourceFunc) Fetch(ctx context.Context) (quota.Snapshot, quota.ProviderState) { return f(ctx) }

// Every outcome a poller can record must pass the store's own validation: a
// mapping that trips a constraint would only show up as a failed write in
// production, where it is logged and the observation is lost.
func TestOutcomesSurviveStoreValidation(t *testing.T) {
	st, err := store.Open(store.Config{Path: filepath.Join(t.TempDir(), "test.db"), BusyTimeout: 5 * time.Second}, testLog())
	require.NoError(t, err, "Open")
	defer st.Close()
	require.NoError(t, st.Migrate(context.Background()), "Migrate")

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
			require.NoError(t, snapshot.Validate(), "quota snapshot does not validate")
			require.NoError(t, health("minimax", a.state).Validate(), "health record does not validate")
			// And the store accepts it, constraints included.
			require.NoError(t, st.SaveQuotaSnapshot(context.Background(), snapshot), "SaveQuotaSnapshot")
			require.NoError(t, st.SaveProviderHealth(context.Background(), health("minimax", a.state)), "SaveProviderHealth")
		})
	}
}
