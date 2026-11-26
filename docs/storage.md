# Local storage (issue #12)

The controller persists four separate signals in one local SQLite database:
**subscription quota snapshots**, **local usage**, **provider health**, and
**controller state**. Nothing else reads or writes that file.

They are separate tables on purpose. Local usage is measured by this installation
and is not a substitute for subscription quota; provider health says a provider
is reachable, which does not prove its quota is available. No table is derived
from another.

## Driver and build implications

| Choice | |
|---|---|
| Driver | [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) — a pure-Go SQLite port |
| Migrations | [`github.com/pressly/goose/v3`](https://github.com/pressly/goose) — SQL only, embedded in the binary |

`modernc.org/sqlite` was chosen over `mattn/go-sqlite3` because the controller
builds with `CGO_ENABLED=0` for a static Alpine binary
([Dockerfile.controller](../deploy/Dockerfile.controller)). A CGO driver would
need a C toolchain in the build image and could not produce a static binary.

Costs, measured on this host (linux/amd64):

| | Before (no SQLite) | After |
|---|---:|---:|
| `-trimpath -ldflags="-s -w"` binary | 10.9 MB | **11.1 MB** |
| Untrimmed build | — | 17.1 MB |
| `VmHWM`, idle container | 5.8 MiB | **10.2 MiB** |
| Runtime C dependencies | none | none |

The extra ~0.2 MB in the stripped binary is the pure-Go SQLite engine; the
untrimmed figure is larger only because the debug data for it is kept. The idle
RSS doubles but stays far below the 128 MiB cap in `deploy/compose.yaml`, which
remains valid.

The driver is x86-64/ARM-64 pure Go, so `CGO_ENABLED=0 go build` works anywhere
Go does. Nothing here requires `libsqlite3`.

## Schema and migrations

The schema is one versioned SQL migration,
[migrations/0001_persistence.sql](../internal/store/migrations/0001_persistence.sql),
embedded with `go:embed`. There is no second copy and no ORM: every query is
explicit SQL in [internal/store](../internal/store).

| Table | Holds |
|---|---|
| `quota_snapshots` | provider, quota state, UNKNOWN reason, source, confidence, observed-at, reset-at (via windows), safe error text |
| `quota_windows` | one row per quota window (`5h`, `weekly`, …) with limit/used/remaining/reset-at, each nullable so "not reported" ≠ `0` |
| `usage_records` | locally observed usage, `reported` vs `estimated`, deduplicated by `request_id` |
| `provider_health` | reachability state, independent of quota |
| `controller_state` | key/value controller state, upserted |

`UNKNOWN` states are constrained by the schema: `unknown_reason` is required
exactly when `state = 'UNKNOWN'` and must be one of the documented reasons, so a
missing or stale observation cannot be stored as `AVAILABLE`.

Migrations run **before the listener starts**; a database this build cannot
migrate fails startup and names the cause. Each migration runs in its own
transaction (goose default for SQL), so a failure leaves the database at the last
version that committed — never half migrated. A `PartialError` reports how many
migrations landed before the failure.

## Single-writer deployment

**Run one controller per database file.** The controller is a single-writer
service.

Migrations are serialized with an advisory `flock` on `<db>.lock`, so even two
controllers pointed at one file cannot apply the same migration concurrently. That
guard is process-level only: it does not make two controllers a supported
deployment, because WAL allows one writer at a time and both would contend for it.

The database lives on a **local** filesystem. SQLite's WAL and locking need real
`fsync` and byte-range locks; over most network filesystems (NFS, CIFS) they are
not guaranteed. Do not put `QUOTA_ROUTER_SQLITE__PATH` on a network share.

## Operational settings

Applied on **every** connection from the DSN, so the pool cannot hand out a
connection that skipped them:

| Pragma | Value | Why |
|---|---|---|
| `busy_timeout` | `sqlite.busy_timeout` (default 5s) | wait for a lock instead of failing at once |
| `journal_mode` | `WAL` | a status read does not block the poller's commit |
| `synchronous` | `NORMAL` | safe under WAL (a crash may lose the last commit, never corrupt the file) and skips an `fsync` per write |
| `foreign_keys` | `1` | retention deleting a snapshot cascades to its windows |
| `_txlock` | `immediate` | a write transaction takes the write lock up front and waits on the busy timeout, rather than failing its upgrade mid-transaction |

Pool: `MaxOpenConns = MaxIdleConns = 4`. SQLite allows one writer, so more
connections only buy readers that can run during a commit.

### Why WAL, and what it costs

WAL is enabled because the controller's whole value is a *fresh* quota snapshot:
the poller must commit while status reads continue. With the default rollback
journal, that reader takes a shared lock and the writer waits for it.

The cost is sidecar files and a backup that must account for them:

- `quota-router.db-wal` and `quota-router.db-shm` sit next to the database while
  it is open. They are part of the live state; do not treat them as leftovers.
- A clean shutdown checkpoints the WAL and removes both sidecars, so a stopped
  controller usually leaves a single file. **Do not rely on that**: a crash, a
  `SIGKILL` or a full disk can leave them behind, and a backup taken while the
  controller runs always sees them.
- A plain `cp quota-router.db` **is not a backup**: committed rows may still be in
  `-wal`. Use `VACUUM INTO` (below).
- WAL needs the database's directory to be writable, not just the file. The
  `controller-db` volume supplies that; a read-only mount does not.
- The unusual mode combination "WAL requires write access even to read" applies:
  a reader of a WAL database needs a writable directory for `-shm`.

WAL was tested for the failure modes it adds: readers concurrent with writers
(`TestConcurrentWritesAndReads`), a lock held by another process
(`TestBusyIsClassifiedAndLosesNoWrite`), and a backup taken under WAL
(`TestBackupIsConsistentUnderWAL`).

## Failure behaviour

Every write is explicit and transactional. A failed write returns a classified
error and changes nothing; callers keep their in-memory state and must never
overwrite it with a partial read. Nothing in the store turns a failed write into
a success.

| Sentinels ([errors.go](../internal/store/errors.go)) | Cause | Meaning |
|---|---|---|
| `ErrBusy` | `SQLITE_BUSY`, `SQLITE_LOCKED` | another writer holds the lock; retryable, state unchanged |
| `ErrFull` | `SQLITE_FULL` | disk or database full; the write did not happen |
| `ErrIO` | `SQLITE_IOERR`, `SQLITE_CORRUPT`, `SQLITE_NOTADB`, `SQLITE_CANTOPEN` | I/O or corruption; not retryable in-process |
| `ErrReadOnly` | `SQLITE_READONLY` | the file is not writable |

A migration failure is fatal at startup; it is not retried in-process.

## Retention

`sqlite.retention.*` bounds history. `0` keeps a table forever.

| Setting | Default |
|---|---|
| `retention.quota_snapshots` | 30 days |
| `retention.provider_health` | 30 days |
| `retention.usage_records` | 90 days |

Cleanup runs at startup and **never removes what routing reads**: the newest
snapshot per provider and the newest health row per provider always survive, even
when they are older than the bound. Usage has no such rule because routing does
not read it. Cleanup is one transaction, and a failure is a startup error — a
full disk is exactly what cleanup exists to avoid.

## Backup and restore

A backup is one self-contained file with no `-wal`/`-shm` dependency.

**Recommended, and verified on this deployment:** stop the controller, then copy
the file. A clean shutdown checkpoints the WAL, so the database is complete on
its own — measured: `integrity_check` returns `ok` and the copy holds every row.
The controller is a poller with no client traffic, so a stop-and-copy window is
harmless.

```sh
cd deploy
docker compose stop quota-controller
docker compose cp quota-controller:/var/lib/quota-router/quota-router.db \
  ./quota-router-$(date +%F).db
docker compose start quota-controller
```

**Live backup, no downtime:** `VACUUM INTO` takes a consistent snapshot while the
controller runs. It needs a `sqlite3` CLI, which the controller image does not
ship (it is not there to be an attack surface), so run it from the host against
a file you can reach, or call `Store.Backup` from Go:

```sh
sqlite3 quota-router.db "VACUUM INTO '/backup/quota-router-$(date +%F).db'"
```

`Store.Backup(path)` never overwrites: it snapshots to a private temporary file
and publishes it with a hard link, which fails if `path` appeared meanwhile. It
returns an error naming the target instead of replacing an existing backup, and a
failed run removes only its own temporary file.

Either way the result is a single file. Do **not** back up with a plain `cp`
while the controller runs: rows still in `-wal` are missed.

Restore. **`docker compose cp` writes the file owned by root, and the controller
runs as uid 65532**, so the ownership fix is not optional: without it the
controller fails startup with `sqlite: database is read-only` and restart-loops.

```sh
cd deploy
docker compose stop quota-controller
docker compose cp ./quota-router-2026-11-26.db \
  quota-controller:/var/lib/quota-router/quota-router.db
# Stale sidecars from the replaced file would otherwise be replayed over it.
# `rm -f` succeeds whether or not they exist: a clean shutdown leaves none.
docker compose run --rm --entrypoint sh quota-controller -c \
  'rm -f /var/lib/quota-router/quota-router.db-wal /var/lib/quota-router/quota-router.db-shm'
# cp runs as root; hand the file back to the container user. `chown` itself needs
# -u 0: as the unprivileged container user it fails and the controller then
# restart-loops on "database is read-only".
docker compose run --rm -u 0 --entrypoint chown quota-controller 65532:65532 \
  /var/lib/quota-router/quota-router.db
docker compose start quota-controller
```

Outside Compose, the same steps: stop the controller, replace the database
file, delete `-wal`/`-shm`, make sure the file is writable by the controller's
uid, start. Migrations re-run on start, so restoring an older backup upgrades it
to the current schema. The `.lock` sidecar is not part of the database and needs
no handling.

Restoring an older backup is safe because the controller re-learns quota state by
polling (issue #13). The restored snapshot is marked with an old `observed_at`;
treat it as stale, not current.

## Verifying a backup is usable

A backup is only useful if it opens and answers:

```sh
sqlite3 /backup/quota-router-2026-11-26.db \
  'SELECT provider, state, observed_at FROM quota_snapshots ORDER BY observed_at DESC LIMIT 5'
```

`TestBackupIsConsistentUnderWAL` asserts the same in CI: the backup has no
sidecars and returns rows that were still in the WAL.
