# quota-router

Minimal Go quota controller bootstrap.

## Build

```sh
go build ./cmd/quota-controller
```

## Run

```sh
go run ./cmd/quota-controller
```

The server listens on `:8080` by default and exposes `GET /health`, which returns `{"status":"ok"}`.

## Domain model

[internal/quota](./internal/quota) defines three signals that are deliberately
separate types, because none can be derived from another:

| Signal | Type | Meaning |
|---|---|---|
| Subscription quota | `Snapshot` | Windows from a documented provider quota source, with observed-at, source, confidence and raw-field provenance |
| Local usage | `Usage` | Requests and tokens this installation saw; `Origin` records provider-reported vs locally observed vs estimated |
| Provider health | `ProviderState` | Healthy, degraded, unavailable or unknown, plus an optional routing cooldown and a closed `HealthReason` code |

Unusable quota is `UNKNOWN` with an `UnknownReason` (`UNSUPPORTED_NO_DOCUMENTED_QUOTA_API`,
`TRANSIENT_FETCH_FAILURE`, `STALE_OBSERVATION`, `MALFORMED_RESPONSE`,
`AUTHENTICATION_OR_PERMISSION_FAILURE`, `NOT_YET_OBSERVED`) and no window values,
never zero remaining. A last-known-good snapshot kept after a failed refresh is
marked `Stale` and reads as `UNKNOWN` through `StateAt`. A cooldown is a routing
policy exclusion and never changes quota or health.

[internal/provider/minimax](./internal/provider/minimax) implements the MiniMax
Token Plan adapter that produces these observations; see
[docs/minimax-quota-adapter.md](./docs/minimax-quota-adapter.md) for the key
type, endpoint, response semantics and limitations.

[internal/provider/commandcode](./internal/provider/commandcode) is the mirror
case: the #4 gate found no documented Command Code quota interface, so it reports
`UNKNOWN`/`UNSUPPORTED_NO_DOCUMENTED_QUOTA_API` without making a request. See
[docs/commandcode-quota-adapter.md](./docs/commandcode-quota-adapter.md) for why
the undocumented `/alpha` routes are deliberately not implemented.


Three details worth knowing before using the model:

- Any unusable window makes the whole snapshot `UNKNOWN`. A known-low 5-hour
  window next to a weekly window with no reported remaining is not `LOW_QUOTA`,
  because the issue requires missing or unparseable quota to become `UNKNOWN`
  and a consumer cannot otherwise tell a partial snapshot from a complete one.
- `StateAt` takes a freshness bound and fails closed: a non-positive `maxAge`
  is `UNKNOWN`, not "any age". Pass the bound the caller actually intends.
- `Observed` copies its windows (and normalizes reset times to UTC), so mutating
  the values you passed cannot change a `Snapshot`. Non-finite numbers (NaN,
  infinities) are `MALFORMED_RESPONSE`.
- There is no stored state to disagree with the windows: `Snapshot.State()`
  derives the state from `Windows` on every read.
- Both reason vocabularies are enforced, not just documented: `Failed` and
  `NewProviderState` replace an undeclared reason with a safe code, so turning
  a raw provider error into a reason cannot smuggle a credential into the model.

## Turn boundaries

[internal/turn](./internal/turn) is where one model's turn ends and another may
start: a stopped attempt is `SURFACE`d, never retried, unless it was
`NOT_DISPATCHED`; executed tool results travel in a `Checkpoint` across the
boundary, built through `CompletedResult`/`FailedResult` and checked by
`Validate` so a raw error, a credential shape or a whole file cannot cross it; and
a migration is refused without an estimated input-token cost within a configured
budget, and a full-transcript replay without a named decision. See
[docs/turn-boundary-migration.md](./docs/turn-boundary-migration.md)
for the stage table and why a model switch is a context rebuild, not a
continuation.

## Configuration

Configuration is loaded by [koanf v2](https://github.com/knadh/koanf) into typed
structs ([internal/config](./internal/config/config.go)) and validated once at
startup. Precedence, applied in this order:

1. built-in defaults ([`config.Default`](./internal/config/config.go));
2. an optional YAML file named by `QUOTA_ROUTER_CONFIG_FILE`;
3. environment variables, which override the file.

The file is optional: with no file and no variables the service starts on
defaults. A file that is set but unreadable or unparsable is a startup error
that names the path. Unknown settings are startup errors, so a typo cannot
silently fall back to a default. Invalid values are reported together, and
errors name the setting without printing its value.

### Environment variables

Prefix `QUOTA_ROUTER_`; `__` separates YAML nesting levels, so a single
underscore stays available inside a field name.

| Variable | Default | Description |
|---|---|---|
| `QUOTA_ROUTER_CONFIG_FILE` | unset | Path to the optional YAML config file |
| `QUOTA_ROUTER_SERVER__ADDR` | `:8080` | HTTP listen address (must not be empty) |
| `QUOTA_ROUTER_SERVER__SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown timeout |
| `QUOTA_ROUTER_SERVER__READ_HEADER_TIMEOUT` | `5s` | HTTP read header timeout |
| `QUOTA_ROUTER_SERVER__READ_TIMEOUT` | `5s` | HTTP read timeout |
| `QUOTA_ROUTER_SERVER__WRITE_TIMEOUT` | `10s` | HTTP write timeout |
| `QUOTA_ROUTER_SERVER__IDLE_TIMEOUT` | `60s` | HTTP idle timeout |
| `QUOTA_ROUTER_LITELLM__BASE_URL` | `http://127.0.0.1:4000` | LiteLLM proxy URL |
| `QUOTA_ROUTER_LITELLM__MODEL_ALIASES__<ALIAS>` | unset | Routing alias → `<provider>/<model>` |
| `QUOTA_ROUTER_PROVIDERS__<NAME>__BASE_URL` | unset | Provider API URL |
| `QUOTA_ROUTER_PROVIDERS__<NAME>__API_KEY` | unset | Provider credential (secret) |
| `QUOTA_ROUTER_POLLING__INTERVAL` | `30s` | Quota polling interval |
| `QUOTA_ROUTER_POLLING__REQUEST_TIMEOUT` | `10s` | Per-poll request timeout |
| `QUOTA_ROUTER_ROUTING__LOW_QUOTA_THRESHOLD` | `0.1` | Scarce-quota fraction, in `(0, 1]` |
| `QUOTA_ROUTER_SQLITE__PATH` | `quota-router.db` | SQLite file path |
| `QUOTA_ROUTER_SQLITE__BUSY_TIMEOUT` | `5s` | SQLite busy timeout |
| `QUOTA_ROUTER_SQLITE__RETENTION__QUOTA_SNAPSHOTS` | `720h` | Keep quota snapshots this long (`0` keeps all) |
| `QUOTA_ROUTER_SQLITE__RETENTION__PROVIDER_HEALTH` | `720h` | Keep health observations this long (`0` keeps all) |
| `QUOTA_ROUTER_SQLITE__RETENTION__USAGE_RECORDS` | `2160h` | Keep usage records this long (`0` keeps all) |
| `QUOTA_ROUTER_AUTH__TOKEN` | unset | Bearer token for the controller API (secret) |
| `QUOTA_ROUTER_OBSERVABILITY__LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

Durations use Go syntax (`5s`, `1m30s`). An empty variable counts as unset, so
it falls through to the file and then the default. The matching YAML structure
and every default are in [config.example.yaml](./config.example.yaml).

A `base_url` must be an absolute `http://` or `https://` URL with no embedded
credentials: `https://user:password@host` is a startup error, because the base
URL is an ordinary string that a log line or a JSON dump prints in full. Put the
credential in `api_key` or `auth.token` instead.

The six flat server variables this service used before the nested model
(`QUOTA_ROUTER_ADDR`, `QUOTA_ROUTER_SHUTDOWN_TIMEOUT`,
`QUOTA_ROUTER_READ_HEADER_TIMEOUT`, `QUOTA_ROUTER_READ_TIMEOUT`,
`QUOTA_ROUTER_WRITE_TIMEOUT`, `QUOTA_ROUTER_IDLE_TIMEOUT`) still work as
deprecated aliases for their `QUOTA_ROUTER_SERVER__*` equivalents, so an
existing deployment keeps starting. The nested name wins when both are set.
Migrate and drop the flat names; they are removed in a later change.

### Secrets

Secrets are injected at runtime and never committed: they are read from an
environment variable or, in containers, from a mounted secret file. A `_FILE`
variable names a file whose trimmed contents become the setting; it works only
for secret settings (`auth.token`, `providers.<name>.api_key`), and any other
`_FILE` is a startup error rather than a silently read path:

```sh
QUOTA_ROUTER_PROVIDERS__MINIMAX__API_KEY_FILE=/run/secrets/minimax_api_key
QUOTA_ROUTER_AUTH__TOKEN_FILE=/run/secrets/controller_token
```

The file must exist and be readable, or startup fails and names it; the path is
never used as the value. A `_FILE` variable loads with the same precedence as
its plain counterpart: when `QUOTA_ROUTER_AUTH__TOKEN` is set, the token file is
not read at all, so an unreadable file cannot fail a configured value.
Credentials print as `[REDACTED]` through `%s`, `%v`, `%#v`, `slog` text and
JSON logs, and `json.Marshal`; an invalid URL is reported without echoing it, so
a `user:password@host` cannot reach a log. `.env` is loaded only by Docker
Compose for local development, never by the service.

The config file accepts no credentials: a `Secret` there (`auth.token`,
`providers.<name>.api_key`) is a startup error naming the setting, because the
file is meant to be copied into a deployment or a repository. Use the
environment or a mounted secret file.

Permissions on mounted secret files are the operator's responsibility, and the
controller only requires the file to be readable by the process — it does not
inspect the mode. Mount them `0644` or stricter, owned by the container user
(`65532` in [deploy/Dockerfile.controller](./deploy/Dockerfile.controller)); `0600` is
the safe choice. A
world-readable secret is accepted silently, so check it in the deployment, not
here.

Local files: `config.yaml` and `.env` are git-ignored; keep real ones
`chmod 600` (`install -m 600`). Only the `*.example.*` files are tracked, and
they contain placeholders only.

## Storage

The controller persists subscription quota snapshots, local usage, provider
health and controller state in a local SQLite database
(`QUOTA_ROUTER_SQLITE__PATH`). Migrations are embedded in the binary and run
before the listener starts; a database this build cannot migrate fails startup.

**Run one controller per database file, on a local filesystem.** Retention,
WAL, the backup procedure and the failure behaviour are in
[docs/storage.md](./docs/storage.md).

## Deploy

```sh
cd deploy
test -f .env || install -m 600 .env.example .env   # first setup only; do not clobber
$EDITOR .env                                      # fill in BOTH keys
docker compose up -d --build --wait
```

LiteLLM listens on `127.0.0.1:4000`; the controller is internal-only. See
[docs/deployment.md](./docs/deployment.md) for ports, limits, health checks, upgrade
and rollback commands.

## Development

```sh
gofmt -l .        # should print nothing
go vet ./...
go test ./...
```
