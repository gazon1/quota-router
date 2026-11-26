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
| `QUOTA_ROUTER_AUTH__TOKEN` | unset | Bearer token for the controller API (secret) |
| `QUOTA_ROUTER_OBSERVABILITY__LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

Durations use Go syntax (`5s`, `1m30s`). An empty variable counts as unset, so
it falls through to the file and then the default. The matching YAML structure
and every default are in [config.example.yaml](./config.example.yaml).

### Secrets

Secrets are injected at runtime and never committed: they are read from an
environment variable or, in containers, from a mounted secret file. Any
variable ending in `_FILE` names a file whose trimmed contents become the
setting:

```sh
QUOTA_ROUTER_PROVIDERS__MINIMAX__API_KEY_FILE=/run/secrets/minimax_api_key
QUOTA_ROUTER_AUTH__TOKEN_FILE=/run/secrets/controller_token
```

The file must exist and be readable, or startup fails and names it; the path is
never used as the value. A `_FILE` variable loads with the same precedence as
its plain counterpart, so an explicit `QUOTA_ROUTER_AUTH__TOKEN` wins over
`QUOTA_ROUTER_AUTH__TOKEN_FILE`. Credentials print as `[REDACTED]` through `%s`,
`%v`, `%#v`, `slog` text and JSON logs, and `json.Marshal`; an invalid URL is
reported without echoing it, so a `user:password@host` cannot reach a log.
`.env` is loaded only by Docker Compose for local development, never by the
service.

Permissions on mounted secret files are the operator's responsibility, and the
controller only requires the file to be readable by the process — it does not
inspect the mode. Mount them `0644` or stricter, owned by the container user
(`65532` in `Dockerfile.controller`); `0600` is the safe choice. A
world-readable secret is accepted silently, so check it in the deployment, not
here.

Local files: `config.yaml` and `.env` are git-ignored; keep real ones
`chmod 600` (`install -m 600`). Only the `*.example.*` files are tracked, and
they contain placeholders only.

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
