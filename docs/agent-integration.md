# Agent integration: route, call, report

Issue: [gazon1/quota-router#20](https://github.com/gazon1/quota-router/issues/20)
Code: [internal/agent](../internal/agent/agent.go)

This is the agent-side half of the routing contract. The agent asks the
controller which model to use, calls **LiteLLM directly** with the alias it got
back, and reports what happened. The controller is a decision point, never a data
path.

Three hops, one request:

```
agent ──POST /v1/route (metadata only)──▶ controller
agent ──POST /v1/chat/completions──────▶ LiteLLM  (prompt, stream)
agent ──POST /v1/usage────────────────▶ controller (tokens, outcome)
```

The prompt, the conversation and the stream never reach the controller. Neither
does the controller proxy the model call: if it is down, the agent's stream is
unaffected — only new routing decisions stop.

## What the agent sends to the controller

Only the versioned route request of
[docs/routing-request.md](./routing-request.md): `version`, `session_id`,
`capabilities`, and optionally `task_class`, `preference`, `override`. The body
is a closed field set on the controller side, so a request carrying `messages` or
a transcript is rejected with `400 invalid_request` rather than having the
content quietly dropped.

The client mints a **request id** and sends it as `X-Request-Id` on the route
call, so the decision the controller persists is joined to the agent's own log
line even before a decision id exists.

## The three-correlation-ids rule

| Id | Minted by | Travels as |
|---|---|---|
| `request_id` | agent, once per request | `X-Request-Id` on `/v1/route`, then `request_id` in the usage report |
| `decision_id` | controller, in the route response | `decision_id` in the usage report |
| `attempt` | agent, 1-based per request | `attempt` in the usage report |

The proxy hop is **not** correlated by `X-Request-Id`. Measured on the pinned
`v1.110.0` image: a client-supplied `X-Request-Id` is neither echoed in the
response nor forwarded upstream, and LiteLLM mints its own
`x-litellm-call-id` per call. `ProxyRequest` still sets the header (it is
harmless and some gateways honour it), but do not rely on it to join a proxy
call to a report. Join on `request_id` + `attempt`, which the agent controls on
both ends, and read `x-litellm-call-id` from the proxy response if a call id is
needed in the agent's own logs.

`request_id` is what makes a usage report idempotent: `(request_id, attempt)` is
unique in the usage store, so a redelivery writes nothing and a retry is a new
attempt rather than a duplicate. `decision_id` is what joins the attempt to the
routing decision that chose the model; the usage row references the decision, it
does not copy it.

A retry after a failed attempt is **attempt 2**: the failed attempt keeps its own
outcome and counts, and the successful one keeps its own. No row copies another's
token counts.

## The model call

```go
req, _ := client.ProxyRequest(ctx, routed, body)   // "model" replaced with routed.Alias
resp, _ := http.DefaultClient.Do(req)              // or read the SSE body yourself
```

The alias must be the alias the controller returned, not one the caller decided
on: `ProxyRequest` overwrites `model` with it so a caller cannot drift. The
stream is read by the agent — the controller is not a hop in it.

The body must be a JSON **object**. Anything else — `null`, `[]`, a bare string
or number, an empty body, malformed JSON — is an ordinary error; `null` in
particular unmarshals into a nil map without complaint, so the check is explicit
rather than relying on the decoder.

A decision whose `selected_alias` arrives with no `selected_provider` is rejected
at `Route`: the provider is required by the usage report and cannot be derived
from an alias, so failing there says what is wrong instead of deferring it to a
report that cannot be sent.

Non-streaming callers can use `Client.Chat`, which performs the call and maps the
response to an outcome: `SUCCESS` for a 2xx, `FAILED_BEFORE_FIRST_TOKEN`
otherwise. A complete response cannot be a partial stream, so those are the only
two outcomes that path reports.

A streaming caller finishes the stream and reports its own outcome:

| What happened | Outcome |
|---|---|
| stream completed | `SUCCESS` |
| nothing arrived | `FAILED_BEFORE_FIRST_TOKEN` |
| some output, then it stopped | `PARTIAL_STREAM` |
| caller walked away | `CANCELLED` |

A cancelled stream yields **no** `usage` object. That is a missing count, not
zero: leave `InputTokens`/`OutputTokens` nil. Each count names the channel that
produced it: `proxy_reported` for a count the proxy returned, `agent_reported`
for a count the agent itself observed, `estimated` for a count this
installation derived. The two are never promoted into each other, and a count
without a provenance is refused — see
[docs/request-telemetry.md](./request-telemetry.md).

## When the controller is unavailable

The rule is **fail closed, with one explicit and auditable exception**:

| Condition | Result |
|---|---|
| Connection refused, DNS/TLS failure, `5xx` | `ErrControllerUnavailable` — **no route, no call**, unless `Manual` is configured |
| Caller's context cancelled or deadline exceeded | the caller's own error, unchanged — **never** a manual route |
| `409` no eligible model | `ErrNoRoute` — never replaced by the manual route |
| `400` invalid request, `401` bad token | surfaced as an error — a bug or a misconfiguration, not an outage |

Cancellation is deliberately not an outage. The route exists to serve *this*
request; if the caller abandoned it, substituting a manual route would let the
abandoned work continue down another path. The returned error satisfies
`errors.Is(err, context.Canceled)` / `errors.Is(err, context.DeadlineExceeded)`,
and it does **not** also match `ErrControllerUnavailable`. Note that an
`http.Client` timeout expires the same way, so a client-level `Timeout` below the
controller's own response time surfaces as a deadline, not as a fallback.

The manual route is set by the operator in the agent's configuration, never
invented by the agent at call time:

```go
client.Manual = agent.ManualRoute{Alias: "cc-coding", Provider: "commandcode"}
```

When it applies, `Routed.Manual` is true and `Routed.DecisionID` is empty. The
usage report it produces therefore names no decision — and that gap is the audit
trail: a `manual: true` route is a request the router did not choose. An empty
`Manual` (the zero value) fails closed, which is the default.

Why `409` is not an outage: the controller answered. It said no model is eligible
right now. Falling back there would make the agent overrule the policy that
existed to stop exactly that.

## End-to-end example

Two containers, as in [docs/deployment.md](./deployment.md): `quota-controller`
on `:8080`, `litellm` on `:4000`.

```go
client := &agent.Client{
    ControllerURL: "http://127.0.0.1:8080",
    ProxyURL:      "http://127.0.0.1:4000",
    Token:         config.Secret(os.Getenv("QUOTA_ROUTER_AUTH__TOKEN")),
    ProxyKey:      config.Secret(os.Getenv("LITELLM_MASTER_KEY")),
    Manual:        agent.ManualRoute{Alias: "free-coding", Provider: "commandcode"},
}

routed, err := client.Route(ctx, routing.RouteRequest{
    Version:      routing.RouteRequestVersionV1,
    SessionID:    sessionID,
    Capabilities: &routing.RequiredCapabilities{ToolCalling: true},
    TaskClass:    routing.TaskCoding,
})
if err != nil {
    return err // fail closed: there is nothing to call
}

reply, err := client.Chat(ctx, routed, body)
if err != nil {
    return err // nothing reached the model; no usage row to report
}
// Use reply.Body (the proxy's response) at the exact alias in routed.Alias.

// Reporting is best-effort: the response is already delivered. Retry it if you
// want the row; the endpoint is idempotent on (request_id, attempt).
_ = client.Report(ctx, routed, reply.Attempt(1))
```

### A failed call is an attempt too

`Chat` returns the error and nothing else. When the failed call must also be
reported — a timeout after the request was dispatched, for example — use
`ChatAttempt`, which returns the attempt to report next to the error:

```go
reply, attempt, err := client.ChatAttempt(ctx, routed, body)
if err != nil {
    // attempt says how far the call got:
    //   FAILED_BEFORE_FIRST_TOKEN — the request never left the agent
    //   AMBIGUOUS                  — it did, and nothing says whether the
    //                                provider served it
    //   CANCELLED                  — the caller abandoned it
    //
    // A failed report never replaces the proxy error: ReportAttempt returns
    // both, so the caller learns why the call failed and that the telemetry
    // did not land.
    return client.ReportAttempt(ctx, routed, attempt, err)
}
_ = client.Report(ctx, routed, reply.Attempt(1))
```

The attempt carries no attempt number: only the caller knows which attempt of
the request this is.

The exchange on the wire, controller first:

```sh
curl -fsS -X POST http://127.0.0.1:8080/v1/route \
  -H "Authorization: Bearer $QUOTA_ROUTER_AUTH__TOKEN" \
  -H "X-Request-Id: 4c1f…" \
  -d '{"version":1,"session_id":"s-1","capabilities":{"tool_calling":true},"task_class":"coding"}'
```

```json
{"decision":{"request":{"version":1,"session_id":"s-1","capabilities":{"tool_calling":true},"task_class":"coding"},
 "selected_alias":"free-coding","selected_provider":"commandcode","decision_id":"dec-…",
 "policy_version":"routing.v1","reasons":["…"],"override_requested":false,"override_applied":false}}
```

then the proxy with that alias, then:

```sh
curl -fsS -X POST http://127.0.0.1:8080/v1/usage \
  -H "Authorization: Bearer $QUOTA_ROUTER_AUTH__TOKEN" \
  -d '{"request_id":"4c1f…","attempt":1,"provider":"commandcode","model":"free-coding",
       "task_class":"coding","route_source":"controller",
       "input_tokens":1200,"input_provenance":"proxy_reported",
       "output_tokens":34,"output_provenance":"proxy_reported",
       "source":"agent.usage_report","outcome":"SUCCESS","latency_ms":1900,
       "decision_id":"dec-…"}'
```

`model` is the routing alias, not the provider's model id: it is what the agent
knows and what the decision names, so the row joins the decision without the
agent having to resolve the alias to `commandcode/stealth/glyph-cluster:free`.

## Timeouts and reporting after a transport failure

`Client.HTTP` is the caller's transport; when nil, one shared client with a 60s
timeout is used. A streaming agent should set its own `HTTP` and budget beyond the
longest stream it expects, since a non-streaming completion on a large model can
easily outlive a short cap — and that cap expiring reads as a deadline (no
fallback), not as an outage.

A `Report` that fails on transport is ambiguous: the row may or may not have been
stored. Redelivery is safe because `(request_id, attempt)` deduplicates, so retry
with the same ids rather than minting new ones — a fresh `request_id` would store
a second row for the same attempt.

The same is true of the *call* that failed. A timeout after the request was
dispatched is reported as `AMBIGUOUS`: the provider may well have served it, and
the failure says nothing about that. A failure before the request was written
never reached the provider and is reported as `FAILED_BEFORE_FIRST_TOKEN`.

## Testing your integration

[agent_test.go](../internal/agent/agent_test.go) is the contract test: a real
controller with a real SQLite store, a stub proxy, and the properties this
document relies on —

1. the decision the agent used is the decision the controller persisted;
2. the proxy is called with the returned alias, and no prompt content ever
   reaches the controller;
3. the usage row joins both ids, keeps the reported counts and provenance, and a
   repeated report does not double-count;
4. a controller outage fails closed, or uses the manual route visibly — while a
   `409` does not fall back at all;
5. a cancelled or expired caller never falls back, and a non-object chat body is
   an error rather than a panic.

It is worth copying this shape for any non-Go agent: a fake proxy, a real
controller, and an assertion that the bytes reaching the controller contain no
prompt.

### Verified against the pinned image

The usage field names above are not taken from the OpenAI spec. They were
measured on `ghcr.io/berriai/litellm:v1.110.0` (digest `4295b06…`) with an
OpenAI-shaped upstream behind it, and the captured response bodies live in
[internal/agent/testdata](../internal/agent/testdata) where the Go tests parse
them. `deploy/interop/run-interop.sh` reproduces the capture: it starts the
pinned image, asserts it reports version `1.110.0`, drives a non-streaming call
and an SSE call with `stream_options.include_usage`, and fails if
`usage.prompt_tokens` / `usage.completion_tokens` are absent or renamed.

Two measured behaviours worth knowing:

- Usage arrives in a non-streaming response as `usage.prompt_tokens` /
  `usage.completion_tokens`, and in a stream on a **final chunk with an empty
  `choices` array** — so a stream reader must keep reading after `finish_reason`
  to find it. Without `include_usage` there is no usage chunk at all.
- What this proves is LiteLLM's serialization, since the upstream is local. A
  real provider's *numbers* are its own; this check says nothing about them.


## What is deliberately not here

- **Session affinity and lease lifecycle** — [#19](https://github.com/gazon1/quota-router/issues/19).
  The route request carries a session id, but the HTTP endpoint does not yet apply
  affinity. A released session (`POST /v1/sessions/{id}/release`) is honoured, and
  releasing is a request the agent sends itself.
- **Turn-boundary migration** — [#21](https://github.com/gazon1/quota-router/issues/21).
  Switching models mid-turn is a context rebuild from a checkpoint, not a retry,
  and it is the agent's decision.
- **A LiteLLM callback shim** — [#69](https://github.com/gazon1/quota-router/issues/69).
  The agent report channel is the one this document covers.
- **Prometheus metrics and telemetry** — [#23](https://github.com/gazon1/quota-router/issues/23),
  [#26](https://github.com/gazon1/quota-router/issues/26). The usage rows are
  accounting, not a metrics source.
