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
| `../.dockerignore` | Keeps `.git` and `deploy/.env` out of the controller's build context |

## Setup

```sh
cd deploy
cp .env.example .env                     # then fill in both keys
sed -i "s/^LITELLM_MASTER_KEY=/LITELLM_MASTER_KEY=$(openssl rand -hex 24)/" .env
docker compose up -d --build --wait
```

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
| `quota-controller` | 128 MiB | 5.8 MiB idle, 14 MiB peak after 5000 `/health` requests at 5.5k req/s |
| `litellm` | 1 GiB | 666 MiB idle (VmHWM), one model configured; started fine under a 512 MiB cap but sat at the cap |

The 128 MiB controller limit is the measurement target from issue #6 and holds
with ~9x headroom for the current `/health`-only workload. **It is not verified
for SQLite persistence or quota polling** (issues #12, #13) — those change the
memory profile, so re-measure before relying on the number. Likewise, 1 GiB is a
starting point for LiteLLM, not a measurement of it under model load.

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

## Compatibility note: v1.110.0 is Python, not Rust

Issue #6 asks for the Rust Router candidate to be evaluated rather than assumed.
Findings for `v1.110.0` (checked 2026-11-26):

- The image reports **Python 3.13.18**; `/app/.venv/bin/litellm --version` says
  `1.110.0`, and `python3 -c "import litellm_rust"` fails with
  `ModuleNotFoundError`.
- The tag does contain a Rust workspace (`litellm-rust/`, with `router`,
  `gateway`, `host-native` and related crates), but it is **not** what the
  published image runs. No separate Rust image tag was found on GHCR or Docker
  Hub (`litellm:rust`, `litellm-rust:latest`, `litellm:main-rust` all 404).
- So this deployment runs the **Python proxy**. Treat the Rust router as a
  separate future evaluation, not as part of this image.

`Dockerfile.controller` builds against the `go 1.22.0` / `toolchain go1.24.0`
directive in `go.mod` and pins `golang:1.24-alpine`.

## Backup and recovery

There is nothing to back up yet: no database is configured for LiteLLM, and the
controller has no persisted state (no volume is mounted). Recovering the stack
means pulling the pinned LiteLLM image, rebuilding the controller from this
checkout, and restoring `.env` — there is no data to restore. When SQLite
persistence lands (issue #12), add a named volume for the controller and a real
backup step here.

## Known gaps

- The controller has no API beyond `GET /health`, so the stack proves
  independence, health checks and limits but routes no traffic through the
  controller yet.
- Single model configured (`MiniMax-M3.1-Flash`); quota polling and routing
  (issues #10, #13) are not wired.
