# Controller HTTP API

Issue: [gazon1/quota-router#18](https://github.com/gazon1/quota-router/issues/18)
Code: [internal/server](../internal/server/server.go)

The controller exposes its local API on the configured server address
(`QUOTA_ROUTER_SERVER__ADDR`, default `:8080`). It is built on the Go standard
library `net/http` and `http.ServeMux` — no web framework.

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/health` | none | Liveness. Returns `{"status":"ok"}`. |
| GET | `/v1/status` | bearer | Latest quota and provider health per provider. |
| POST | `/v1/route` | bearer | Route decision (the issue-named spelling of the decision endpoint). |
| POST | `/v1/routing/select` | bearer | Route decision: versioned request → decision with explanation. |
| GET | `/v1/routing/decisions` | bearer | Recent decision audit records. |
| POST | `/v1/usage` | bearer | Record one request attempt (tokens, provenance, outcome). |
| POST | `/v1/sessions/{id}/release` | bearer | End one session's route use; safely repeatable. |

`/v1/route` and `/v1/routing/select` are the same handler: the contract names
the decision boundary `/v1/route`, the repository documents it as
`/v1/routing/select`.

## Error response shape

One stable error envelope is returned by every error response:

```json
{
  "error": {
    "code": "validation_failed",
    "message": "version must be 1, got 2",
    "request_id": "0f4d1b2a9c8e4f60718293a4b5c6d7e8",
    "retryable": false
  }
}
```

- `code` — machine-readable condition code, stable across releases.
- `message` — safe, caller-readable text. Never a credential, raw provider body
  or internal stack trace.
- `request_id` — per-request correlation id (`X-Request-Id` honoured when bounded,
  otherwise generated). Echoed on errors; returned in responses via the envelope.
- `retryable` — whether a well-behaved client may retry the same request.

## Status-code mapping

| Condition | Status | Code | Retryable |
|---|---|---|---|
| Malformed JSON body | 400 | `invalid_request` | no |
| Validation failure / unsupported version | 400 | `validation_failed` | no |
| Unknown JSON field (closed field sets) | 400 | `invalid_request` | no |
| Trailing content after the JSON object | 400 | `invalid_request` | no |
| Body over the bound (64 KiB on usage and route) | 413 | `payload_too_large` | no |
| Missing / invalid bearer token | 401 | `unauthorized` | no |
| Valid request, no eligible candidate | 409 | `conflict` | no |
| Conflicting usage event-id reuse | 409 | `conflict` | no |
| Known path with wrong method | 405 | `method_not_allowed` | no |
| State or persistence temporarily unavailable | 503 | `unavailable` | yes |
| Internal failure | 500 | `internal_error` | no |
| Rate-limited condition | 429 | `rate_limited` | yes |

Mapping notes:

- Malformed input and validation failures are `400`; an audit/state failure is
  `503`; a conflicting idempotency key is `409`; a busy database is `503` with
  `retryable: true` (the identity makes the retry safe).
- Local authentication: every non-`/health` endpoint requires the bearer token
  from `QUOTA_ROUTER_AUTH__TOKEN`; an unset token refuses every request. The
  comparison is constant time.
- No prompt/context fields are accepted or persisted: request bodies use closed
  field sets with unknown-field rejection.

## Endpoints in detail

### `GET /health`

No auth. `200` with `{"status":"ok"}`. Any other method is `405` with the
stable error envelope (`method_not_allowed`).

### `GET /v1/status`

Auth required. Reads the latest quota snapshot and provider health per
provider, applies the same freshness bound the selector uses, and returns:

```json
{
  "providers": [
    {
      "provider": "minimax",
      "quota": { "state": "AVAILABLE", "source": "…", "observed_at": "…", "windows": [ … ] },
      "health": { "state": "HEALTHY", "reason": null, "observed_at": "…" }
    }
  ]
}
```

Quota entries are omitted per provider until an observation exists; `quota.state`
is the actionable state and `unknown_reason` explains `UNKNOWN`.

### `POST /v1/route` / `POST /v1/routing/select`

Auth required. Body is the versioned route request (`version: 1`, session id,
capabilities, optional task class / preference / override). Bounded to 64 KiB.
`400` invalid, `409` no eligible candidate, `503` state or audit persistence
unavailable, `200` with the decision otherwise. Decision audit persistence is
mandatory.

### `GET /v1/routing/decisions`

Auth required. Returns recent decisions, newest first.

### `POST /v1/usage`

Auth required; with no token configured every report is refused (`401`).
Bounded to 64 KiB. Closed field set (unknown fields rejected, so no prompt or
context field can slip in). Idempotent on `(request_id, attempt)`: an identical
redelivery writes nothing and returns `202`; reusing an event id for different
content returns `409 conflict`.

### `POST /v1/sessions/{id}/release`

Auth required. Ends one session's route use. Repeating the release is safe and
returns the same success; it stores a release marker and does not touch quota or
health state.

## Out of scope

OpenAPI and code generation are deliberately deferred until endpoint shapes and
error semantics stabilize.
