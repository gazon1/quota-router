// SQLite error classification.
//
// Every write failure is reported to the caller; nothing here turns a failed
// write into a silent success, because replacing a valid in-memory quota state
// with a partially written one would route on wrong data. What this file adds is
// the ability to tell a retryable lock conflict from a fatal disk or I/O fault.
package store

import (
	"errors"

	sqlite3 "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

// ErrBusy reports the database was locked by another writer past the busy
// timeout: retryable, and the state is unchanged.
var ErrBusy = errors.New("sqlite: database busy")

// ErrFull reports the disk or database is full: the write did not happen.
var ErrFull = errors.New("sqlite: disk full")

// ErrIO reports an I/O or corruption fault. Not retryable in-process.
var ErrIO = errors.New("sqlite: I/O failure")

// ErrReadOnly reports the database file is not writable.
var ErrReadOnly = errors.New("sqlite: database is read-only")

// classify wraps a driver error with one of the sentinels above, keeping the
// original message. Unrecognised errors are returned unchanged.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var se *sqlite3.Error
	if !errors.As(err, &se) {
		return err
	}
	// Extended result codes carry the primary code in the low byte.
	primary := se.Code() & 0xff
	sentinel := map[int]error{
		sqlitelib.SQLITE_BUSY:     ErrBusy,
		sqlitelib.SQLITE_LOCKED:   ErrBusy,
		sqlitelib.SQLITE_FULL:     ErrFull,
		sqlitelib.SQLITE_READONLY: ErrReadOnly,
		sqlitelib.SQLITE_CANTOPEN: ErrIO,
		sqlitelib.SQLITE_IOERR:    ErrIO,
		sqlitelib.SQLITE_CORRUPT:  ErrIO,
		sqlitelib.SQLITE_NOTADB:   ErrIO,
	}[primary]
	if sentinel == nil {
		return err
	}
	return errors.Join(sentinel, err)
}
