// SQLite operational settings and schema migration.
//
// Driver: modernc.org/sqlite, a pure-Go port of SQLite. Chosen over
// mattn/go-sqlite3 because the controller builds with CGO_ENABLED=0 for a static
// Alpine binary (deploy/Dockerfile.controller): a CGO driver would need a C
// toolchain in the image and could not produce a static binary. Cost, measured:
// 11.1 MB stripped binary and 10.2 MiB idle RSS, up from 10.9 MB and 5.8 MiB.
//
// Migrations: goose (github.com/pressly/goose/v3), SQL files embedded in the
// binary, applied before the listener starts. No Go migrations and no ORM.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
)

// MaxConns bounds the pool. SQLite allows one writer at a time; the extra
// connections let a status reader run while the poller commits, which WAL makes
// possible. More would only add idle handles.
const MaxConns = 4

// Store is the persisted quota state.
type Store struct {
	db   *sql.DB
	path string
	log  *slog.Logger
}

// Config carries the SQLite settings this package needs.
type Config struct {
	Path        string
	BusyTimeout time.Duration
}

// Open opens the database and applies the operational settings on every
// connection. It does not migrate; call Migrate before serving.
func Open(cfg Config, log *slog.Logger) (*Store, error) {
	if err := chmodDB(cfg.Path); err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", cfg.Path, err)
	}
	db, err := sql.Open("sqlite", dsn(cfg))
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", cfg.Path, classify(err))
	}
	db.SetMaxOpenConns(MaxConns)
	db.SetMaxIdleConns(MaxConns)
	db.SetConnMaxIdleTime(time.Minute)

	s := &Store{db: db, path: cfg.Path, log: log}
	// sql.Open is lazy: this first ping is what fails on an unreadable, missing
	// directory or corrupt file, before any request is served.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite %q: %w", cfg.Path, classify(err))
	}
	return s, nil
}

// dsn builds the connection string. Pragmas apply per connection, so the pool
// cannot hand out a connection that skipped them.
//
//	busy_timeout      wait for a lock instead of failing at once
//	journal_mode      WAL: a reader does not block the writer's commit
//	synchronous       NORMAL: safe under WAL (a crash may lose the last commit,
//	                  never corrupt the file) and skips an fsync per write
//	foreign_keys      retention deleting a snapshot cascades to its windows
//	txlock immediate  a write transaction takes the write lock up front and
//	                  waits on busy_timeout instead of failing mid-transaction
func dsn(cfg Config) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busyMillis(cfg.BusyTimeout), 10)+")")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	return "file:" + escapePath(filepath.ToSlash(cfg.Path)) + "?" + q.Encode()
}

// busyMillis rounds a positive duration up, because Millisecond() truncates: a
// configured "500us" would otherwise become busy_timeout(0) and silently give the
// database no waiting period at all.
func busyMillis(d time.Duration) int64 {
	ms := d.Milliseconds()
	if ms == 0 && d > 0 {
		return 1
	}
	return ms
}

// escapePath percent-encodes the characters that would otherwise be URI syntax,
// and nothing else, so a "?", "#", "%" or space in a valid file name stays part
// of the name. Without it the driver opens a different file than the one
// Migrate locks. A leading "./" keeps SQLite from reading the path as a special
// URI such as ":memory:": that form silently opens an in-memory database, which
// would drop every persisted observation on restart.
//
// url.URL is not used here for the opposite reason: with a relative path such as
// the default "quota-router.db" it renders "file://quota-router.db" — an
// authority, not a relative path — and the driver then fails to open anything.
// Slashes stay literal so relative paths keep working.
func escapePath(p string) string {
	var b strings.Builder
	if strings.HasPrefix(p, ":") {
		b.WriteString("./")
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '?', '#', '%', ' ':
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// Migrate applies pending versioned migrations and must complete before the
// listener starts: an unmigrated database fails startup instead of serving.
//
// Each migration runs in its own transaction (goose default for SQL), so a
// failure leaves the database at the last version that committed, never half
// migrated.
//
// Serialization: the run holds an advisory lock on "<path>.lock" so two
// controller processes pointed at one database cannot apply the same migration.
// The supported deployment is still one controller per database; see
// docs/storage.md.
func (s *Store) Migrate(ctx context.Context) error {
	lock, err := acquireLock(s.path + ".lock")
	if err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer lock.release()

	root, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.db, root,
		goose.WithSlog(s.log),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return fmt.Errorf("prepare migrations: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		// A partial apply means earlier migrations committed; say how many, so an
		// operator knows where the schema stopped.
		var partial *goose.PartialError
		if errors.As(err, &partial) {
			return fmt.Errorf("apply migrations: %d applied, then %w", len(partial.Applied), classify(err))
		}
		return fmt.Errorf("apply migrations: %w", classify(err))
	}
	for _, r := range results {
		s.log.Info("migration applied", "version", r.Source.Version, "file", filepath.Base(r.Source.Path))
	}
	return nil
}

// dbPerm is owner-only. The database holds provider observations and controller
// state; SQLite creates its files with the process umask (0644 under 0022),
// which would make them world-readable inside the container.
const dbPerm = 0o600

// chmodDB creates the database file if it is missing, owner-only, and brings an
// existing one to the same mode. SQLite keeps the mode of a file it opens, so
// this covers both the fresh and the existing-database case; a file we may not
// chmod fails startup with the OS error rather than being served world-readable.
func chmodDB(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE, dbPerm)
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		// A path that is a directory, or one whose directory is missing, is an
		// unusable database location, not a permission problem.
		if os.IsPermission(err) {
			return errors.Join(ErrReadOnly, err)
		}
		return errors.Join(ErrIO, err)
	}
	// OpenFile accepts an existing directory on some systems, and chmod on one
	// would strip its execute permission. Refuse anything that is not a regular
	// file before touching its mode.
	st, err := os.Stat(path)
	if err != nil {
		return errors.Join(ErrIO, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("open sqlite %q: not a regular file", path)
	}
	if err := os.Chmod(path, dbPerm); err != nil {
		return errors.Join(ErrIO, err)
	}
	return nil
}

// Version reports the highest applied migration version, 0 when none is applied.
func (s *Store) Version(ctx context.Context) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = 1").Scan(&v)
	if err != nil {
		return 0, classify(err)
	}
	return v, nil
}
