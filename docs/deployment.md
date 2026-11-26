# Home-host deployment (Docker Compose)

Issue: [gazon1/quota-router#6](https://github.com/gazon1/quota-router/issues/6)

Two containers on one host: the **LiteLLM Proxy** serves model traffic, the **Go
Quota Controller** is a separate service that will hold quota state. There is no
`depends_on` edge between them — LiteLLM starts and serves on its own, and keeps
serving when the Controller is stopped.

## Files

| Path | Purpose |
|---|---|
| `compose.yaml` | Compose stack: `litellm`, `quota-controller` |
| `Dockerfile.controller` | Builds the controller (static Go binary on Alpine, non-root) |
| `litellm-config.yaml` | LiteLLM model list and master key |
| `.env.example` | Template for `.env` (secrets; `deploy/.gitignore` excludes it) |
| `compose_test.go` | Contract test for the acceptance criteria below |
| `../.dockerignore` | Keeps `.git`, `deploy/.env` and `config.yaml` out of the controller's build context |
| `../config.example.yaml` | Placeholder controller config; see [Configuration](../README.md#configuration) |

## Controller configuration in containers

The controller starts on built-in defaults and takes no config file here, so
none is baked into the image. To change a setting, pass it as an environment
variable and to inject a credential, mount it as a secret file:

```yaml
  quota-controller:
    environment:
      QUOTA_ROUTER_OBSERVABILITY__LEVEL: debug
      QUOTA_ROUTER_PROVIDERS__MINIMAX__API_KEY_FILE: /run/secrets/minimax_api_key
    secrets:
      - minimax_api_key
```

Secrets reach the container at runtime and never enter the image or the build
context. `.env` is loaded by Compose for LiteLLM only; the controller ignores it.

A secret file must be readable by the container user (`65532:65532`, `nonroot`)
and should be owned by it with mode `0600`; `0644` root-owned is the loosest
setting to use. The controller checks only readability, not the mode, so the
permission is a deployment responsibility — a world-readable secret is accepted
without a warning. Compose bind-mounted files inherit host permissions, so
`install -m 600 -o 65532` the file before `up`. An explicit
`QUOTA_ROUTER_..._API_KEY` wins over its `..._API_KEY_FILE` counterpart.

## Prerequisites

Docker Engine **28.0.0 or newer**. Docker documents that in releases older than
28.0.0, hosts on the same L2 segment can reach ports published to localhost — so
the `127.0.0.1:4000:4000` binding in `compose.yaml` is only loopback-only on 28+.
See [port publishing](https://docs.docker.com/engine/network/port-publishing/)
and [moby/moby#45610](https://github.com/moby/moby/issues/45610). On an older
Engine, do not rely on the binding: keep the host off untrusted networks or add a
host firewall rule that drops inbound `tcp/4000`.

## Setup

```sh
cd deploy
test -f .env || install -m 600 .env.example .env   # only on first setup
grep -q '^LITELLM_MASTER_KEY=.\+' .env || \
  sed -i "s/^LITELLM_MASTER_KEY=/LITELLM_MASTER_KEY=$(openssl rand -hex 24)/" .env
$EDITOR .env                            # set MINIMAX_API_KEY (and keep the master key)
docker compose up -d --build --wait
```

Both commands are idempotent on purpose: re-running setup must not overwrite an
existing `.env`. A plain copy would blank `MINIMAX_API_KEY` and rotate the master
key, breaking every client that already holds it.

`LITELLM_MASTER_KEY` must not be empty: LiteLLM refuses to start without it
rather than accept unauthenticated requests. `.env.example` ships it empty on
purpose — a placeholder like `sk-change-me` is a *working* credential, so a
forgotten replacement would silently authenticate anyone who guessed it.

## Startup, shutdown, logs

```sh
docker compose up -d --wait           # start both and block until healthy
docker compose ps                     # status incl. health
docker compose logs -f litellm        # follow proxy logs
docker compose logs -f quota-controller
docker compose restart quota-controller
docker compose stop quota-controller  # proxy keeps serving; this is intentional
docker compose down                   # stop and remove containers + network
```

Both containers run with `restart: unless-stopped`, so they come back after a
host reboot or a Docker daemon restart until you explicitly stop them.

## Ports and networking

| Service | Published | Notes |
|---|---|---|
| `litellm` | `127.0.0.1:4000` | Only local API port. `127.0.0.1` prefix keeps it off the LAN; use `"4000:4000"` to serve the LAN, and put a TLS terminator in front before you do. |
| `quota-controller` | none | Reachable only as `http://quota-controller:8080` from inside the `internal` network. Its `/health` is not an admin or debug surface worth exposing. |

Publish the controller's port only when its real API exists (issue #18), and
then bind it to `127.0.0.1` as well.

LiteLLM's admin UI lives on the same port under `/ui`. It is not a separate
port, so the `127.0.0.1` binding covers it too.

## Health checks

| Service | Probe | Start period |
|---|---|---|
| `quota-controller` | `wget -qO- http://127.0.0.1:8080/health` | 5s |
| `litellm` | `GET /health/liveliness` via `urllib` | 40s |

LiteLLM's probe calls `/health/liveliness`, not `/health`: the latter sends a real
request to every configured model and would spend tokens on each probe.

## Resource limits (measured, not assumed)

Measured on this host with `docker stats` and `/proc/1/status` `VmHWM`:

| Service | Limit | Measured |
|---|---:|---|
| `quota-controller` | 128 MiB | 10.2 MiB idle with SQLite persistence (5.8 MiB before it), 14 MiB peak after 5000 `/health` requests at 5.5k req/s |
| `litellm` | 1 GiB | 666 MiB idle (VmHWM), one model configured; started fine under a 512 MiB cap but sat at the cap |

The 128 MiB controller limit is the measurement target from issue #6. SQLite
persistence (issue #12) raised idle VmHWM from 5.8 to **10.2 MiB**, so the limit
keeps ~12x headroom for the `/health`-only workload. **Quota polling (issue #13)
is still unmeasured** — it adds in-memory state and a request path, so re-measure
before relying on the number. Likewise, 1 GiB is a starting point for LiteLLM,
not a measurement of it under model load.

Check current values with:

```sh
docker stats --no-stream
```

## Upgrading

LiteLLM is pinned by tag **and** digest:

```
ghcr.io/berriai/litellm:v1.110.0@sha256:4295b064b22eb88f9530e616afc812edd47048891ba64896bfc3fa74e2b883c3
```

Never switch that to `latest` or `stable`. Bumping it is a deliberate edit:

```sh
# 1. Read the release notes and confirm the new digest
curl -s https://api.github.com/repos/BerriAI/litellm/releases/tags/vX.Y.Z | head -20
docker buildx imagetools inspect ghcr.io/berriai/litellm:vX.Y.Z

# 2. Edit the image line in deploy/compose.yaml (tag + digest)

# 3. Pull, restart, verify
docker compose pull litellm
docker compose up -d --wait litellm
curl -s http://127.0.0.1:4000/health/readiness

# 4. Roll back by restoring the previous image line and re-running step 3
```

The controller is built locally from this checkout, so upgrading it is
`git pull && docker compose up -d --build --wait quota-controller`.

## Compatibility note: what is Rust in v1.110.0, and what is not

Issue #6 asks for the Rust Router candidate to be evaluated rather than assumed.
Measured in the pinned image `@sha256:4295b064…b883c3` (2026-11-26):

- The proxy is **Python 3.13.18** (`/app/.venv/bin/litellm --version` → `1.110.0`).
- It **does** ship compiled Rust: `litellm/rust_bridge/_native.abi3.so`, a 45 MB
  native extension (1100+ `rustc`/`/rust/` symbols) importing cleanly and
  exporting `RouterCoreHandle`, `NativeDatabase`, `NativeRedisClient`,
  `NativeRateLimits`, `TokenCounter` and `acompletion`/`amessages`/`aresponses`.
- **Which route runs on Rust is a per-route decision**, not a global switch.
  `litellm.rust_bridge.catalog.RULES` in this image:

  | Route | Rollout | Meaning |
  |---|---|---|
  | `chat_completions` | `PYTHON_ONLY` | always Python |
  | `embeddings`, `responses`, `token_counter`, `tokenizer` | `PYTHON_ONLY` | always Python |
  | `messages` (non-Anthropic) | `PYTHON_ONLY` | always Python |
  | `messages` (provider `anthropic`) | `RUST_OPT_IN` | Rust if enabled |
  | `ocr` | `RUST_REQUIRED` | always Rust |
  | `transcription` (provider `bedrock`) | `RUST_REQUIRED` | always Rust |

- `RUST_OPT_IN` is driven by `LITELLM_RUST`, measured in the image:

  | `LITELLM_RUST` | `rust_enabled()` | decision |
  |---|---|---|
  | unset | `False` | `PYTHON` |
  | `true` | `True` | `RUST_WITH_FALLBACK` |
  | `false` | `False` | `PYTHON` |

  `RUST_WITH_FALLBACK` still falls back to Python when the native call declines.

So for **this** deployment — chat completions through `openai/MiniMax-M3.1-Flash` —
the request path is **Python regardless of `LITELLM_RUST`**, because the route is
`PYTHON_ONLY`. Rust is load-bearing only for `ocr` and Bedrock `transcription`.
Setting `LITELLM_RUST=true` adds Rust to Anthropic `messages`; that is untested
here and should be measured before adoption.

`Dockerfile.controller` builds against the `go 1.23.0` / `toolchain go1.24.0`
directive in `go.mod` (koanf v2 requires 1.23) and pins `golang:1.24-alpine`.

## Backup and recovery

The only stateful data is the controller's SQLite database, in the
`controller-db` named volume. LiteLLM has no database here. Recovering the stack
means pulling the pinned LiteLLM image, rebuilding the controller from this
checkout, restoring `.env`, and restoring the database volume.

Back up with `VACUUM INTO` — a plain copy of the file misses rows still in
`-wal`. The full procedure, the sidecar rules and the restore steps are in
[storage.md](./storage.md#backup-and-restore).

## Known gaps

- The controller has no API beyond `GET /health`, so the stack proves
  independence, health checks and limits but routes no traffic through the
  controller yet.
- Single model configured (`MiniMax-M3.1-Flash`); quota polling and routing
  (issues #10, #13) are not wired.
