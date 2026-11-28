package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain keeps every test database inside a temporary directory: run() opens
// the configured SQLite path, and the default would litter the package.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "quota-controller-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("QUOTA_ROUTER_SQLITE__PATH", filepath.Join(dir, "test.db"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestRunConfigError(t *testing.T) {
	t.Setenv("QUOTA_ROUTER_SERVER__READ_TIMEOUT", "bad")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	code := run(context.Background(), log, new(slog.LevelVar))
	assert.Equal(t, 1, code)
}

func TestRunAppliesConfiguredLogLevel(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", addr)
	t.Setenv("QUOTA_ROUTER_OBSERVABILITY__LEVEL", "error")

	// The production constructor, so a change to main's wiring fails here.
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug) // the handler must honour the LevelVar
	var buf synchronizedBuffer
	log := newLogger(&buf, level)

	log.Debug("before")
	require.Contains(t, buf.String(), "before", "the handler does not follow the LevelVar at all")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, log, level)
	}()

	// The level is set right after config.Load, before the listener starts.
	waitForTCP(t, addr, 2*time.Second)
	assert.Equal(t, slog.LevelError, level.Level(), "the level from the configuration")

	buf.Reset()
	log.Debug("still dropped")
	log.Error("kept")
	got := buf.String()
	assert.NotContains(t, got, "still dropped", "the handler ignored the configured level")
	assert.Contains(t, got, "kept", "the error record was dropped too")
	cancel()
	assert.Equal(t, 0, <-done, "run()")
}

// synchronizedBuffer is a bytes.Buffer safe for the server goroutine's writes.
type synchronizedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *synchronizedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// TestRunRefusesUnservedAlias is the acceptance criterion "validate aliases
// against the LiteLLM configuration during startup": an alias the proxy does not
// serve cannot work, so the controller must not start and serve.
func TestRunRefusesUnservedAlias(t *testing.T) {
	srv := modelList(t, http.StatusOK, `{"data":[{"id":"minimax/MiniMax-M3"}]}`)
	t.Setenv("QUOTA_ROUTER_LITELLM__BASE_URL", srv.URL)
	t.Setenv("QUOTA_ROUTER_LITELLM__API_KEY", "master-key")
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__MODEL", "minimax/MiniMax-M2.7")
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", freeAddr(t))

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	assert.Equal(t, 1, run(context.Background(), log, new(slog.LevelVar)), "run() for an alias LiteLLM does not serve")
}

// TestRunServesWhileCatalogueUnavailable keeps the design rule from #6: the two
// containers have no dependency edge, so a proxy that is not up yet must not
// stop the controller.
func TestRunServesWhileCatalogueUnavailable(t *testing.T) {
	// Nothing listens here: a closed port is the "proxy not started yet" case.
	closed := freeAddr(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__BASE_URL", "http://"+closed)
	t.Setenv("QUOTA_ROUTER_LITELLM__API_KEY", "master-key")
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__MODEL", "minimax/MiniMax-M2.7")

	addr := freeAddr(t)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", addr)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, log, new(slog.LevelVar)) }()

	waitForTCP(t, addr, 5*time.Second) // it started despite the unreachable proxy
	cancel()
	assert.Equal(t, 0, <-done, "run() when the catalogue is merely unavailable")
}

// modelList serves the given answer as LiteLLM's GET /v1/models.
func modelList(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/models", r.URL.Path)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")
	defer ln.Close()

	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", ln.Addr().String())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	code := run(context.Background(), log, new(slog.LevelVar))
	assert.Equal(t, 1, code)
}

func TestRunGracefulShutdownOnCancel(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", addr)
	t.Setenv("QUOTA_ROUTER_SERVER__SHUTDOWN_TIMEOUT", "2s")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, log, new(slog.LevelVar))
	}()

	waitForHTTP(t, "http://"+addr+"/health", 2*time.Second)
	cancel()

	select {
	case code := <-done:
		assert.Equal(t, 0, code, "run()")
	case <-time.After(5 * time.Second):
		require.Fail(t, "run() did not exit after cancellation")
	}
}

func TestRunShutdownTimeout(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", addr)
	t.Setenv("QUOTA_ROUTER_SERVER__SHUTDOWN_TIMEOUT", "50ms")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	blocked := make(chan struct{}, 1)
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block" {
			select {
			case blocked <- struct{}{}:
			default:
			}
			<-release
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	done := make(chan int, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		done <- runWithHandler(ctx, log, handler)
	}()

	waitForTCP(t, addr, 2*time.Second)

	client := &http.Client{Timeout: 2 * time.Second}
	respCh := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + addr + "/block")
		if err == nil {
			resp.Body.Close()
		}
		respCh <- err
	}()

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		require.Fail(t, "handler did not receive /block request")
	}

	cancel()

	select {
	case code := <-done:
		close(release)
		assert.Equal(t, 1, code, "run() on shutdown timeout")
	case <-time.After(3 * time.Second):
		close(release)
		require.Fail(t, "run() did not exit after shutdown timeout")
	}

	select {
	case <-respCh:
	case <-time.After(2 * time.Second):
		require.Fail(t, "blocked request did not complete")
	}
}

// runWithHandler serves with a test handler instead of the /health-only one.
func runWithHandler(ctx context.Context, log *slog.Logger, handler http.Handler) int {
	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "error", err)
		return 1
	}
	return serve(ctx, log, cfg, handler)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")
	addr := ln.Addr().String()
	require.NoError(t, ln.Close(), "close listener")
	return addr
}

func waitForHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Fail(t, "the server did not become ready", "server at %s did not become ready within %s", url, timeout)
}

func waitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Fail(t, "the server did not accept connections", "server at %s did not accept connections within %s", addr, timeout)
}

// TestPrunerRunsWhileServing proves retention is not a startup-only step: a
// controller that keeps running must not accumulate history its configuration
// says it dropped. Driven through the real run(), so the wiring is covered too.
func TestPrunerRunsWhileServing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	st, err := store.Open(store.Config{Path: path, BusyTimeout: 5 * time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err, "Open")
	require.NoError(t, st.Migrate(context.Background()), "Migrate")
	old := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		require.NoError(t, st.SaveUsageRecord(context.Background(), store.UsageRecord{
			Source: "test", Provider: "p", ObservedAt: old,
		}), "SaveUsageRecord")
	}
	// Retention of one hour, so the ticker has exactly the seeded rows to remove.
	ctx := context.Background()
	stop := startPruner(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), st, config.Retention{UsageRecords: time.Hour}, 20*time.Millisecond)

	// The ticker must delete the expired rows without touching anything else.
	pruned := false
	deadline := time.Now().Add(3 * time.Second)
	for {
		if n, err := countUsageRows(path); err == nil && n == 0 {
			pruned = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !pruned {
		stop()
		st.Close()
		require.Fail(t, "expired usage rows survived 3s: the pruner never ran")
	}
	// A fresh row must survive the same ticker.
	require.NoError(t, st.SaveUsageRecord(ctx, store.UsageRecord{
		Source: "test", Provider: "p", ObservedAt: time.Now(),
	}), "SaveUsageRecord")
	time.Sleep(60 * time.Millisecond)
	n, err := countUsageRows(path)
	if err != nil || n != 1 {
		stop()
		st.Close()
		require.Fail(t, "unexpected row count after the tick", "rows = %d (err %v) after the tick, want the 1 fresh row", n, err)
	}
	stop() // must wait for an in-flight tick before the store closes
	require.NoError(t, st.Close(), "Close")
}

// countUsageRows reads the usage table through its own connection, so the test
// observes what the store persisted rather than what it cached.
func countUsageRows(path string) (int, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// TestRetentionIsFatalOnlyWhenFull pins the policy: a transient cleanup failure
// must not stop a controller that can otherwise serve, but a full database must.
func TestRetentionIsFatalOnlyWhenFull(t *testing.T) {
	assert.True(t, retentionBlocksStartup(store.ErrFull), "a full database must block startup: writes cannot grow")
	for _, err := range []error{store.ErrBusy, store.ErrIO, errTest} {
		assert.False(t, retentionBlocksStartup(err), "%v blocks startup; it is transient and the controller can serve", err)
	}
	assert.False(t, retentionBlocksStartup(nil), "nil blocks startup")
}

var errTest = errors.New("wrapped transient failure")

// The poller is wired into run(), not only tested on its own: a configured
// provider is polled, its observation persisted, and both timestamps recorded.
func TestRunPollsConfiguredProvider(t *testing.T) {
	quota := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model_remains":[{"model_name":"general",` +
			`"current_interval_remaining_percent":80,"current_weekly_remaining_percent":80}],` +
			`"base_resp":{"status_code":0}}`))
	}))
	defer quota.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "poll.db")
	addr := freeAddr(t)
	t.Setenv("QUOTA_ROUTER_SQLITE__PATH", path)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", addr)
	t.Setenv("QUOTA_ROUTER_POLLING__INTERVAL", "50ms")
	t.Setenv("QUOTA_ROUTER_POLLING__REQUEST_TIMEOUT", "20ms")
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__BASE_URL", quota.URL+"/v1")
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__API_KEY", "sk-cp-test-key")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- run(ctx, log, new(slog.LevelVar)) }()

	waitForHTTP(t, "http://"+addr+"/health", 5*time.Second)

	// The first poll is immediate, so the state must appear without waiting for
	// an interval to elapse.
	var attempt, success time.Time
	var pollErr error
	polled := false
	deadline := time.Now().Add(5 * time.Second)
	for {
		attempt, success, pollErr = pollState(path, "minimax")
		if pollErr == nil && !attempt.IsZero() && !success.IsZero() {
			polled = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !polled {
		cancel()
		<-done
		require.Fail(t, "the poll state was not recorded", "poll state = %s/%s (err %v), want both timestamps", attempt, success, pollErr)
	}

	cancel()
	select {
	case code := <-done:
		assert.Equal(t, 0, code, "run()")
	case <-time.After(5 * time.Second):
		require.Fail(t, "run() did not exit: the poller outlived the shutdown")
	}

	// The persisted observation is quota the provider reported, with windows.
	snapshots, err := latestSnapshots(path)
	require.NoError(t, err, "read snapshots")
	require.Len(t, snapshots, 1, "one snapshot for one provider")
	assert.Equal(t, "AVAILABLE", snapshots[0].state, "from the reported 80% windows")
	assert.Equal(t, 2, snapshots[0].windows, "the 5h and weekly windows persisted")
}

// An unset api_key would otherwise start a controller that polls nothing.
func TestPollingTargetsRejectsUnsupportedAndIncomplete(t *testing.T) {
	full := config.Provider{BaseURL: "https://www.minimax.io/v1", APIKey: "sk-cp-test"}

	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"no adapter", config.Config{Providers: map[string]config.Provider{"other": full}}, "no quota adapter"},
		{"no key", config.Config{Providers: map[string]config.Provider{"minimax": {BaseURL: full.BaseURL}}}, "api_key is not set"},
		{"wrong host", config.Config{Providers: map[string]config.Provider{"minimax": {BaseURL: "https://api.minimax.io/v1", APIKey: full.APIKey}}}, "base_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pollingTargets(tc.cfg)
			require.Error(t, err, "expected a rejection naming %q", tc.want)
			assert.ErrorContains(t, err, tc.want)
		})
	}

	_, err := pollingTargets(config.Config{})
	require.NoError(t, err, "an unconfigured controller must start idle")
	targets, err := pollingTargets(config.Config{Providers: map[string]config.Provider{"minimax": full}})
	require.NoError(t, err, "pollingTargets")
	require.Len(t, targets, 1)
	assert.Equal(t, "minimax", targets[0].Name)
}

// pollState reads the poll bookkeeping through its own connection, so the test
// sees what was persisted rather than what the controller held.
func pollState(path, provider string) (attempt, success time.Time, err error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	defer db.Close()
	var a string
	var s sql.NullString
	err = db.QueryRow(`SELECT last_attempt_at, last_success_at FROM poll_state WHERE provider = ?`, provider).Scan(&a, &s)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if attempt, err = time.Parse("2006-01-02T15:04:05.000000000Z", a); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if s.Valid {
		if success, err = time.Parse("2006-01-02T15:04:05.000000000Z", s.String); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	return attempt, success, nil
}

type persistedSnapshot struct {
	state   string
	windows int
}

func latestSnapshots(path string) ([]persistedSnapshot, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT s.state, (SELECT COUNT(*) FROM quota_windows w WHERE w.snapshot_id = s.id)
		FROM quota_snapshots s ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []persistedSnapshot
	for rows.Next() {
		var s persistedSnapshot
		if err := rows.Scan(&s.state, &s.windows); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
