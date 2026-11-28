package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/store"
)

// gatedStore blocks the first affinity read until a gate is released, so a test
// can interleave a release with a selection at the exact point that used to race:
// after the selection read the pin, before it wrote one back.
type gatedStore struct {
	*fakeStore
	once  sync.Once
	read  chan struct{} // closed when the first read has happened
	gate  chan struct{} // the first read waits for this
	reads int
	mu    sync.Mutex
}

func (g *gatedStore) SessionAffinity(ctx context.Context, sessionID string) (store.SessionAffinity, bool, error) {
	g.mu.Lock()
	g.reads++
	first := g.reads == 1
	g.mu.Unlock()

	if first {
		g.once.Do(func() { close(g.read) })
		<-g.gate
	}
	return g.fakeStore.SessionAffinity(ctx, sessionID)
}

// A release must not be undone by a selection that read the pin before the
// release landed: the two operations are serialized per session, so the selection
// finishes (and pins) first, and the release then removes what it wrote.
func TestReleaseIsNotUndoneByInFlightSelection(t *testing.T) {
	base := &fakeStore{
		snaps:    []store.QuotaSnapshot{quotaSnap("commandcode", store.QuotaAvailable, 80), quotaSnap("minimax", store.QuotaAvailable, 80)},
		health:   []store.HealthRecord{healthRec("commandcode", store.Healthy), healthRec("minimax", store.Healthy)},
		affinity: map[string]store.SessionAffinity{"sess-1": pin("sess-1", "cc-coding", time.Now().UTC())},
	}
	gated := &gatedStore{fakeStore: base, read: make(chan struct{}), gate: make(chan struct{})}
	cfg := testRoutingConfig()
	srv := New(nil, Options{Config: cfg, AuthToken: testRoutingToken, Store: gated, Decisions: gated})

	// Start the selection; it will block holding the session lock, right after
	// reading the pin.
	selectDone := make(chan int, 1)
	go func() {
		w := postSelect(t, srv, testRoutingToken, `{"version":1,"session_id":"sess-1","capabilities":{}}`)
		selectDone <- w.Code
	}()
	<-gated.read

	// Release must wait for the selection, not interleave with it.
	releaseDone := make(chan int, 1)
	go func() {
		w := postRelease(t, srv, testRoutingToken, "sess-1")
		releaseDone <- w.Code
	}()

	select {
	case code := <-releaseDone:
		t.Fatalf("release completed (status %d) while a selection held the session lock", code)
	case <-time.After(100 * time.Millisecond):
		// Still waiting, which is what serialization means.
	}

	close(gated.gate)
	if code := <-selectDone; code != 200 {
		t.Fatalf("selection status = %d, want 200", code)
	}
	if code := <-releaseDone; code != 200 {
		t.Fatalf("release status = %d, want 200", code)
	}

	// The release is the last word: the pin the selection wrote must be gone.
	if _, ok := base.affinity["sess-1"]; ok {
		t.Fatalf("pin = %+v survived the release: the in-flight selection undid it", base.affinity["sess-1"])
	}
}
