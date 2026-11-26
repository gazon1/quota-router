# MiniMax Token Plan quota adapter

Issue: [gazon1/quota-router#10](https://github.com/gazon1/quota-router/issues/10)
Code: [internal/provider/minimax](../internal/provider/minimax)
Evidence: [compatibility matrix](./litellm-provider-compatibility.md), [API verification](./minimax-api-verification.md)

## Key type

Use the operator's **Token Plan subscription key** from Billing → M Plan in the
console, commonly prefixed `sk-cp-`. It is *not* the pay-as-you-go Open Platform
API key (`platform.minimax.io` → API Keys). The two are not interchangeable:
they are different billing paths, and the endpoint rejects the wrong type
(`base_resp.status_code=2057`, "not token plan token"). The key is a
`config.Secret`, so it prints as `[REDACTED]` and never reaches a log, an error
string or a marshaled snapshot.

## Endpoint

```
GET https://www.minimax.io/v1/token_plan/remains
Authorization: Bearer <Token Plan subscription key>
```

This is the documented **Global** Token Plan interface. No regional substitution
is made and no other quota path is tried.

The origin is enforced, not just documented. `New` rejects any base URL that would
not carry the key to `https://www.minimax.io/v1/token_plan/remains` — a wrong
host, `http`, a lookalike host such as `www.minimax.io.evil.example`, embedded
userinfo, or the wrong path — and `Fetch` re-checks before attaching the
credential, so an `Adapter` built by hand is covered too. A rejected origin never
sends a request, so the key cannot leave the process. Loopback is the one accepted
alternative, for tests and a local interposer.

The host matters: quota lives on `www.minimax.io`, while inference lives on
`api.minimax.io` (OpenAI-compatible at `/v1`, Anthropic-compatible at
`/anthropic`). `providers.<name>.base_url` configures the quota surface and
LiteLLM's `api_base` configures inference; the quota URL must never be set as a
LiteLLM `api_base`, and vice versa. `Snapshot.Source` is the fixed documented
endpoint string, so a misconfigured base URL does not fake a plausible source:
the answer never parses into a quota report, so the snapshot is `UNKNOWN`.

## Response semantics (as measured)

The schema is not fully documented publicly. It was captured from a live
permitted response; see
[the fixture](./fixtures/smoke/minimax_token_plan_remains.json) and the trimmed
copy the tests parse, [token_plan_remains.json](./fixtures/minimax/token_plan_remains.json).

```json
{"model_remains": [
  {"model_name": "general",
   "start_time": 1795687200000, "end_time": 1795705200000, "remains_time": 126683,
   "current_interval_total_count": 0, "current_interval_usage_count": 0,
   "current_interval_status": 1, "current_interval_remaining_percent": 100,
   "weekly_start_time": 1795392000000, "weekly_end_time": 1795996800000,
   "weekly_remains_time": 291726683,
   "current_weekly_total_count": 0, "current_weekly_usage_count": 0,
   "current_weekly_status": 2, "current_weekly_remaining_percent": 0}],
 "base_resp": {"status_code": 0, "status_msg": "success"}}
```

| Field | Meaning | Mapped |
|---|---|---|
| `model_name` | quota bucket (`general`, `video`, …) | window name prefix |
| `start_time` / `end_time` | rolling 5-hour window, epoch **ms** | `resets_at` = `end_time` |
| `weekly_start_time` / `weekly_end_time` | weekly window, epoch **ms** | `resets_at` |
| `current_interval_remaining_percent` | remaining quota in the 5-hour window, `0..100` | `limit` = 100, `remaining` = value |
| `current_weekly_remaining_percent` | remaining quota in the weekly window, `0..100` | same |
| `remains_time` / `weekly_remains_time` | ms left in the window, not a percentage and not a timestamp | not mapped (derivable from the reset) |
| `current_interval_total_count` / `_usage_count` | **both 0 on the measured account**; not a quota numerator | not mapped |
| `current_interval_status` / `current_weekly_status` | small integer enum, undocumented publicly | not mapped |
| `base_resp.status_code` | business status; `0` is success | decides whether the body is a quota report at all |

Only these percentages are treated as a quota figure. The response states no
used figure, so `Used` stays nil and every observation is `ConfidenceLow` — the
accounting is usable but incomplete. `current_interval_status` is
deliberately not read as a quota state: its values are undocumented, so a
mapping would be a guess, and a wrong guess here means routing on invented quota.

### The known inconsistency

The same account could return `HTTP 429`, business error `2056`
("Token Plan usage limit reached") on every inference call, while this endpoint
still reported `current_interval_remaining_percent: 100` for the 5-hour window
and `0` for the weekly one. So the endpoint is authoritative only for what it
states, and a 5-hour window claiming 100% is **not** evidence the provider can
serve a request. Two consequences are built in:

- both windows of every bucket are reported, and the normalized model's
  "worst window wins" rule turns `100%` + `0%` into `EXHAUSTED`, not `AVAILABLE`;
- provider health is a separate signal from subscription quota, and neither is
  derived from an inference result.

## Error handling

A non-200 status is mapped to both closed vocabularies and never to a quota
figure:

| Condition | Quota | Health |
|---|---|---|
| 200, `base_resp.status_code=0`, windows present | parsed | `HEALTHY` |
| 200, `status_code` 1004 / 2049 / 2057 (auth, invalid key, wrong key type) | `AUTHENTICATION_OR_PERMISSION_FAILURE` | `UNAVAILABLE` / auth |
| 200, `status_code` 1002 / 2062 (rate limit; 2062 also means no active plan) | `TRANSIENT_FETCH_FAILURE` | `DEGRADED` / rate limited |
| 200, any other `status_code`, missing `base_resp`, empty `model_remains` | `MALFORMED_RESPONSE` | `DEGRADED` / provider error |
| 200, unparseable JSON | `MALFORMED_RESPONSE` | `DEGRADED` / provider error |
| 401 / 403 | `AUTHENTICATION_OR_PERMISSION_FAILURE` | `UNAVAILABLE` / auth |
| 429 | `TRANSIENT_FETCH_FAILURE` | `DEGRADED` / rate limited |
| 5xx or other status | `TRANSIENT_FETCH_FAILURE` | `UNAVAILABLE` / provider error |
| unsafe configured origin (refused before any request) | `MALFORMED_RESPONSE` | `DEGRADED` / provider error |
| body larger than 1 MiB (refused, not truncated) | `MALFORMED_RESPONSE` | `DEGRADED` / provider error |
| network error | `TRANSIENT_FETCH_FAILURE` | `UNAVAILABLE` / unreachable |
| context deadline or `Adapter.Timeout` exceeded | `TRANSIENT_FETCH_FAILURE` | `UNAVAILABLE` / timeout |
| percentage absent, outside `0..100`, or not a number | that window is unreported, so the snapshot is `MALFORMED_RESPONSE` | `DEGRADED` |

A rejected credential or an unusable body never becomes zero remaining and never
becomes `AVAILABLE`. The raw provider error text — which can carry a key — is not
stored anywhere in the model; only the reason codes leave the adapter.

## Freshness and stale data

`Fetch` keeps the last observation that established quota and, when a later
attempt does not, returns it with `Stale` set. Only a strictly newer observation
replaces it, and a call whose own attempt was older than the retained one returns
the retained newer snapshot rather than its own older reading: when two attempts
overlap and the earlier one finishes last, neither the cache nor that caller's
return value regresses to older quota. That copy is diagnostics only:
`Snapshot.StateAt(now, maxAge)` reports `UNKNOWN` / `STALE_OBSERVATION` for it, so
no consumer can route on stale quota. Health always describes the attempt that
just ran. First-attempt failures have no last-known-good copy to return.

## Limitations

- **One bounded attempt, no retry.** `New` takes `polling.request_timeout` as the
  per-attempt bound, and the caller's context deadline also applies; the shorter
  wins. This adapter never retries: the poller in
  [internal/poll](../internal/poll) decides whether its answer is worth another
  attempt, using the classification above.
- **`Used` and the counts are never reported**, so confidence stays `LOW`.
- **The percentage is the only quota figure.** If the endpoint ever adds a
  numerator, mapping it needs a new capture, not an inference.
- **`status` enums are unused** and stay `UNKNOWN` rather than guessed.
- **The endpoint's own freshness is unknown.** A 100% window next to a live 429
  means the figure may lag the account's real standing; treat the snapshot's
  `ObservedAt` as the age of a *report*, not of the account.
- **Quota is not measured empirically** against consumption; that proof stays open
  per [API verification](./minimax-api-verification.md).
- **Intended use.** M Plan is documented for individual interactive developer
  use, with pay-as-you-go recommended for production. This adapter does not route
  or sell access; keep the single-account constraint from
  [subscription terms verification](./subscription-terms-verification.md).
