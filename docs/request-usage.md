# Request usage and outcomes

Issue: [gazon1/quota-router#14](https://github.com/gazon1/quota-router/issues/14)
Code: [internal/server/usage.go](../internal/server/usage.go),
[internal/store/types.go](../internal/store/types.go)

The controller records what it was told about requests it routed: one row per
**attempt**, with token counts only when a reporter supplied them, and an outcome
that is never guessed.

This is **observational data about this installation**. It is not a replica of a
provider's subscription quota or dashboard counter, and it is not a second
telemetry or routing-history system — request telemetry and routing decisions own
their own records, and this one only references them.

**Never compute remaining subscription quota as provider quota minus locally
accumulated usage.** Reported token counts describe what the provider reported
for one request; they do not prove what that request consumed from a
subscription. Tokenization, reasoning/cache/multimodal accounting and quota
dimensions can all differ.

## Endpoint

`POST /v1/usage`, bearer token from `QUOTA_ROUTER_AUTH__TOKEN`. With no token
configured the route refuses every report (`401`): there is no unauthenticated
mode.

```sh
curl -fsS -X POST http://127.0.0.1:8080/v1/usage \
  -H "Authorization: Bearer $QUOTA_ROUTER_AUTH__TOKEN" \
  -d '{
        "request_id": "req-abc",
        "attempt": 1,
        "provider": "minimax",
        "model": "MiniMax-M2.7",
        "input_tokens": 1200,
        "output_tokens": 340,
        "provenance": "reported",
        "source": "litellm.callback",
        "outcome": "SUCCESS",
        "decision_id": "dec-7"
      }'
```

| Field | Required | Meaning |
|---|---|---|
| `request_id` | no | Stable request identity. Empty means the source supplied none, and duplicates cannot be detected. |
| `attempt` | no | 1-based attempt within the request. Absent or `0` means the first attempt. Retries are separate attempts, not duplicates. |
| `provider` | yes | Provider that served the attempt. |
| `model` | no | Model that served the attempt. |
| `input_tokens` | no | Prompt tokens, only if supplied. Absent stays unknown, never `0`. |
| `output_tokens` | no | Completion tokens, only if supplied. Absent stays unknown, never `0`. |
| `provenance` | yes | `reported` (the provider's own count) or `estimated` (this installation derived it). The two are never promoted into each other. |
| `source` | yes | Channel that reported the row, for example `litellm.callback` or `agent.usage_report`. |
| `outcome` | no | `SUCCESS`, `FAILED_BEFORE_FIRST_TOKEN`, `PARTIAL_STREAM`, `CANCELLED`, or `UNKNOWN` when absent. |
| `decision_id` | no | Routing decision that routed the attempt. Routing history owns that record; this only references it. |
| `observed_at` | no | RFC 3339. Absent means the time the report arrived. |

Responses: `202` for a stored or already-stored report, `400` for a malformed or
unrepresentable one, `401` for a bad token, `409` when an event id is reused for
different content, `413` for a body over 64 KiB, `503` when the database is busy
(retry: the identity makes it safe) or when ingestion is not wired.

## Guarantees

- **Idempotent.** `(request_id, attempt)` is unique. A repeated report of an
  already-stored attempt writes nothing and still returns `202`, so a client
  retry does not double-count.
- **Conflicting reuse is a conflict.** The same event id delivered again with
  different content is not a retry: the store returns a conflict and the API
  answers `409` with the stable error envelope, instead of silently overwriting
  the stored attempt.
- **Retries are separate.** A retry is attempt 2 of the same request: the failed
  attempt keeps its outcome and no usage, the successful one keeps its own. No
  row copies another's token counts.
- **Missing stays missing.** No tokenizer runs here, and nothing is estimated to
  zero. A reported `0` is stored as `0`.
- **Unknown outcomes are UNKNOWN.** The store does not guess between a broken
  stream and a cancellation.
- **No prompt content.** The body is a closed field set: unknown JSON fields are
  rejected, so a report carrying messages or a conversation fails rather than
  having that content dropped silently. Nothing in the body is logged or echoed.

## Wiring a reporter

**The reporter is not part of this route.** Issue #14 owns the ingestion side —
identity, provenance, outcomes and idempotency, which is what this endpoint is.
Turning a channel into actual reports is a deployment integration, and the
LiteLLM callback shim is deliberately a **follow-up**
([#69](https://github.com/gazon1/quota-router/issues/69), see below).

What any reporter must do:

1. Report each **attempt** once, with the request id and attempt number it used.
2. Mark `provenance: "reported"` only for counts the upstream response returned
   (`usage.prompt_tokens`/`completion_tokens`, or the final stream chunk with
   `stream_options.include_usage`). Otherwise `estimated`, or leave the counts
   out entirely.
3. Report a cancellation as `CANCELLED` and a broken stream as `PARTIAL_STREAM`
   rather than a success. A cancelled stream yields no usage object at all; that
   is a missing count, not zero.
4. Send `decision_id` from the route decision the agent used, when it has one.

### Why the LiteLLM callback is a follow-up

Issue #14's criterion is "supported LiteLLM callbacks/events **or** agent usage
reports"; the agent report channel satisfies it, and its contract belongs to
[#20](https://github.com/gazon1/quota-router/issues/20). A LiteLLM callback shim
is separate work for three concrete reasons:

- **It is not Go and not in this service.** A `CustomLogger` is a Python file
  loaded by the proxy (`litellm_settings.callbacks: custom_callbacks.instance`),
  so it lives with the LiteLLM container and its config, not in this module.
- **The cheap path is licensed.** LiteLLM's "Custom Callback APIs" — which would
  let the proxy POST to this route with configuration only — is an Enterprise
  feature requiring `LITELLM_LICENSE`. Without a license the shim has to be a
  Python class, which means the payload mapping is our code to maintain.
- **The payload must be measured, not assumed.** Field names come from LiteLLM's
  `StandardLoggingPayload`, which changes between releases. The deployment pins
  `v1.110.0` by digest, so the mapping has to be asserted against that image the
  way [litellm-provider-compatibility.md](./litellm-provider-compatibility.md)
  asserts the request path — not copied from the current docs.

The shim also would not be free of the duplicate-report problem: a proxy retry
and a client retry can both produce a terminal callback for the same attempt.
That is exactly what `(request_id, attempt)` already absorbs, but the shim has to
derive the attempt number, and `x-litellm-call-id` identifies a *call*, not an
attempt the agent retried.


## Boundaries

- Not this issue: reconstructing a provider's dashboard counter, deriving
  remaining subscription quota or reset times from local usage, cost/billing
  accounting, and any tokenizer.
- Provider quota snapshots (`quota_snapshots`) remain a separate data source and
  are never merged with these rows.
- Routing eligibility is decided from quota and health, never from usage trends
  accumulated here.
