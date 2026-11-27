package server

import "sync"

// sessionLocks serializes the operations that read and write one session's route
// pin. Selection reads the pin, decides, and writes it back; a release deletes it.
// Interleaved, a selection that read the pin before a release could write its old
// decision back afterwards and silently undo the release — the one act that is
// supposed to be unconditional.
//
// The lock is per session, not global: unrelated sessions keep routing
// concurrently, and only two requests for the same session contend. Entries are
// reference counted and dropped when the last holder leaves, so a long-running
// controller does not accumulate one mutex per session it has ever seen.
type sessionLocks struct {
	mu sync.Mutex
	m  map[string]*sessionLockEntry
}

type sessionLockEntry struct {
	mu   sync.Mutex
	refs int
}

// lock acquires the session's lock and returns the function that releases it.
func (l *sessionLocks) lock(sessionID string) (unlock func()) {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*sessionLockEntry{}
	}
	e, ok := l.m[sessionID]
	if !ok {
		e = &sessionLockEntry{}
		l.m[sessionID] = e
	}
	e.refs++
	l.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			// Only the holder that took the last reference removes the entry, and
			// it does so while still holding the map lock, so a concurrent caller
			// either finds this entry or creates a new one after the delete.
			delete(l.m, sessionID)
		}
		l.mu.Unlock()
	}
}
