package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// migrationLock serializes migration runs across processes through an advisory
// lock on a sidecar file. flock is released by the kernel when the process dies,
// so a killed controller cannot leave the database permanently locked.
type migrationLock struct{ f *os.File }

// acquireLock takes an exclusive lock on path, creating it if needed. It blocks
// until the lock is free, matching the "wait, do not race" intent.
func acquireLock(path string) (*migrationLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &migrationLock{f: f}, nil
}

func (l *migrationLock) release() {
	unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	l.f.Close()
}
