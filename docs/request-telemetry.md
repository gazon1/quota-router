# Request and routing telemetry (issue #23)

Issue: [gazon1/quota-router#23](https://github.com/gazon1/quota-router/issues/23)
Code: [internal/store/types.go](../internal/store/types.go),
[internal/store/query.go](../internal/store/query.go),
[internal/server/usage.go](../internal/server/usage.go),
[internal/agent/agent.go](../internal/agent/agent.go)

This is the **canonical request/outcome event**: one row per *attempt* of one
request, with the route facts and the outcome facts of that attempt. It is the
single source of truth for what this installation observed about its own
requests — there is no second event table, no event bus and no analytics side
car.

The wire contract of that event is [docs/request-usage.md](./request-usage.md)
(how a reporter submits it). This document is the contract itself: the fields,
what "missing" means, the identity and deduplication rules, the privacy
boundary and the retention.

**Local request usage is not provider subscription quota.** A reported token
count says what one response reported about one request. It does not say how
much subscription quota that request consumed, and the two are never subtracted
from each other. Provider quota and health live in their own tables
(`quota_snapshots`, `quota_windows`, `provider_health`) and are polled by a
different subsystem (#13).

## The event

One row of `usage_records` is one event. The row *is* the contract; the Go type
is `store.UsageRecord`.

| Field | Type | Meaning | When it is missing |
|---|---|---|---|
| `event_id` | text | Stable id of this event. Derived from `(request_id, attempt)` when the reporter supplies none. | Never derived from request content; it is not a metric label. |
| `schema_version` | int | Version of the event contract the row was written with (`1` today). | Always present. |
| `request_id` | text | Request identity from the agent (#20). | Empty means the reporter supplied none, so duplicates cannot be detected. |
| `attempt` | int | 1-based attempt within the request. | `0` is stored as `1`. |
| `provider` | text | Provider that served the attempt. | Required. |
| `model` | text | Configured alias or model name that served it. | Absent when the reporter did not name one. |
| `task_class` | text | Bounded class of work (`routine`, `coding`, `reasoning`). Owned by the routing request contract. | Absent when the request named none. |
| `route_source` | text | `controller` (the controller selected the route) or `manual` (the agent used its manual fallback). | Absent when the reporter did not say. |
| `input_tokens` | int | Prompt tokens. | Absent when not reported — never `0`. |
| `input_provenance` | text | Channel that produced `input_tokens`. | Absent exactly when `input_tokens` is absent. |
| `output_tokens` | int | Completion tokens. | Absent when not reported — never `0`. |
| `output_provenance` | text | Channel that produced `output_tokens`. | Absent exactly when `output_tokens` is absent. |
| `source` | text | Channel that reported the row (`agent.usage_report`, `litellm.callback`, …). | Required. |
| `outcome` | text | How the attempt ended (see below). | `UNKNOWN` when the reporter did not say. |
| `error_class` | text | Normalized failure reason (see below). | Absent for an attempt that did not fail, and for a failure with no class. |
| `stream_state` | text | `not_streamed`, `complete` or `interrupted`. | Absent when the reporter did not say. |
| `ttft_ms` | int | Time to first token, measured by a trusted reporting boundary. | Absent when not measured — never `0`. |
| `latency_ms` | int | Total duration of the attempt, in milliseconds. | Absent when not measured — never `0`. |
| `decision_id` | text | The routing decision that routed the attempt. | Absent for a manual route. |
| `observed_at` | text | When the **source** observed the attempt (UTC, fixed width). | Server-stamped arrival time when the reporter supplied none. |
| `observed_at_reported` | bool | Whether `observed_at` came from the reporter. | `false` means the server stamped it. |
| `received_at` | text | When **this installation** stored the report (UTC). | Always present. |

There is no total-token column: the total is the sum of the two counts, and
storing it would be a third number that can disagree with the other two.

### Bounded values

Enums are closed sets, and the schema constrains them. A value outside the set
is rejected by name instead of being stored:

| Set | Values |
|---|---|
| outcome | `SUCCESS`, `FAILED_BEFORE_FIRST_TOKEN`, `PARTIAL_STREAM`, `CANCELLED`, `AMBIGUOUS`, `UNKNOWN` |
| error class | `timeout`, `rate_limited`, `auth`, `invalid_request`, `provider_error`, `network`, `unknown` |
| stream state | `not_streamed`, `complete`, `interrupted` |
| route source | `controller`, `manual` |
| token provenance | `provider_reported`, `proxy_reported`, `agent_reported`, `estimated`, `reported` (legacy rows) |
| task class | `routine`, `coding`, `reasoning` (owned by [internal/routing](../internal/routing)) |

`error_class` is a **class**, never a provider message. A raw provider error
string does not fit the set, so it cannot be stored — that is the redaction
boundary for failures, and it is enforced by the same enum check as everything
else.

The task class is checked at the **storage** boundary, not only at the wire:
the store validates it against the routing vocabulary
([internal/routing](../internal/routing)), and the schema carries a `CHECK`
constraint on it, so a direct write cannot store a class nobody can aggregate
on either.

`AMBIGUOUS` is neither success nor failure: it says the attempt may or may
not have been served (a timeout after dispatch, for example). It never implies
that nothing was charged.

## Token provenance

Every count says which channel produced it:

| Value | Meaning |
|---|---|
| `provider_reported` | The provider's own accounting (the response's usage object). |
| `proxy_reported` | The proxy (LiteLLM) reported it. |
| `agent_reported` | The agent reported it; the agent does not know the upstream channel. |
| `estimated` | This installation derived it (for example by counting tokens itself). |
| `reported` | Legacy rows written before per-count provenance existed: the exact channel was never recorded, so it stays ambiguous. |

Rules:

- **A count and its provenance travel together.** A count without provenance is
  an unsourced number and is rejected; a provenance without a count describes a
  number nobody observed and is dropped at the wire boundary.
- **Nothing is merged.** The two counts of one attempt may come from different
  channels, and each keeps its own.
- **An estimate is never promoted.** An estimated count stays `estimated` next
  to an observed one; a consumer that wants only observations filters on the
  provenance, it does not read the number and hope.
- **Nothing is reconciled.** If two sources disagree about the same attempt,
  the second report is a conflict (`409`), not a silent merge and not an
  invented exact value.

## Identity, attempts and deduplication

- The **ingestion identity** is `(request_id, attempt)`. It is unique in the
  schema.
- A **retry is a new attempt**, not a second copy of the usage it replaces:
  attempt 1 keeps its own outcome and no usage, attempt 2 keeps its own.
- A **repeated report of the same attempt with the same content** writes
  nothing and still succeeds (`202`). Retrying the HTTP call is safe.
- A **repeated report with different content** is a conflicting reuse: the
  store keeps the first observation and answers `409`. That covers a changed
  token count, a changed outcome and a changed provenance. The disagreement
  itself is kept — see [Conflicts](#conflicts).
- `event_id` is derived from the identity, so the same attempt always has the
  same id and two attempts never share one. It exists to correlate a row with
  logs and with the routing decision — it is **never** a Prometheus label.
- A report **without** a `request_id` cannot be deduplicated and is stored as
  sent.
- A row that **predates the event id** gets one when the schema migrates: the
  id is derived from the row's identity, so it is the same id the store would
  have written. Migrating again, or rolling back and migrating once more,
  leaves the identities unchanged. A row without a `request_id` has no
  identity to derive from and keeps no id.

## Conflicts

Two sources may disagree about the same attempt. The disagreement is not
dropped: the stored event keeps the **first** observation, and the conflict is
recorded next to it, so both sides of the disagreement stay visible.

A conflict row holds the identity of the attempt, the name of the field the
sources disagreed on, and the two values with the channel that produced each
one (the provenance of a token count). It holds **no provider payload**: a
conflict is not a second copy of the report that caused it, so no prompt, no
transcript and no raw provider response can reach this table.

`store.QueryUsageConflicts` is the read interface, next to
`store.QueryUsageRecords`:

```go
conflicts, err := st.QueryUsageConflicts(ctx, store.UsageConflictQuery{
    RequestID: "req-1",
    Attempt:   1,
})
```

- A **repeated report with identical content** is not a conflict: it writes
  nothing and records no disagreement.
- The **same** conflict reported again is one row, not one per redelivery.
- A conflict is **bounded**: at most 8 rows per attempt, and the rows are
  removed by the same retention run that removes the events they belong to.
- The API still answers `409` for a conflicting report. The caller learns that
  its report did not land; the store keeps the evidence of why.

## Time

Two clocks, never reconciled:

- `observed_at` is the **source's** clock. A report that supplied it keeps it,
  even when the source clock is skewed.
- `received_at` is **this installation's** clock, the moment the row was
  stored.

The gap between them is the report delay. A report that supplied no
`observed_at` gets the arrival time and is marked
`observed_at_reported = false`, so a reader can tell a source observation from
arrival bookkeeping.

A **delayed** report is still an observation of its own moment: it is stored,
and the query API orders by observation time, so a late report does not appear
as the newest event.

## Reading events

`store.QueryUsageRecords` is the read interface. It filters by time window,
provider, model and task class, orders by observation time (newest first) and
is bounded: 100 rows by default, 1000 at most.

```go
rows, err := st.QueryUsageRecords(ctx, store.UsageQuery{
    Since:     lastHour,
    Provider:  "minimax",
    TaskClass: "coding",
    Limit:     500,
})
```

Consumers read this, not SQL, so the stored shape can change without every
consumer changing with it.

**#24 and #28** consume exactly these rows and must not reinterpret them: a
`SUCCESS` is a completed attempt, an `estimated` count is an estimate, and an
absent count is not zero. **#26** derives metrics from the same semantics and
must aggregate on the bounded values only — outcome, error class, stream
state, provider, task class — never on `event_id`, `request_id` or
`decision_id`.

## The route side

`routing_decisions` stays the record of a routing decision, and it carries the
route facts of the event: `task_class` and `explicit` (whether the selected
route was an explicit manual override). The event references the decision with
`decision_id`; it does not copy the payload.

The policy version belongs to the decision and is not duplicated into the
event, so the two can never disagree.

## Privacy boundary

What is **never** persisted:

- prompts, transcripts, model responses, tool arguments or tool results;
- authorization headers, API keys or any other credential;
- raw provider payloads or provider error text;
- arbitrary user-supplied metadata.

The body of a usage report is a **closed field set**: an unknown JSON field is
rejected (`400`), so a report that tries to carry a prompt fails loudly instead
of having it silently dropped. The persisted record has no free-text field
that could hold one — the only descriptive fields are bounded enums and
identifier columns of at most 256 bytes.

Correlation identifiers (`request_id`, `decision_id`, `event_id`) live in the
event record and in structured logs. They are not metric labels.

## Retention

`sqlite.retention.usage_records` bounds the history. The default is **90
days**; `0` keeps the table forever.

- The configured age is **clamped to 365 days** inside the store, so storage
  stays bounded even when the configuration asks for more history than the
  controller holds.
- Cleanup deletes in **bounded batches** (1000 rows per statement, at most 100
  statements per run), so a cleanup never holds the write lock for the whole
  table. Whatever is left over is removed by the next run.
- A failed cleanup **never** blocks routing or a request: the controller logs a
  warning and retries at the next tick. Only a full database fails startup,
  because every write would fail anyway.

## Runtime integration

Two boundaries, both non-invasive:

- **Controller boundary** — the routing decision is persisted with the
  selected route, the task class and whether it was an explicit override.
  Telemetry records the decision; it never changes it.
- **Reporting boundary** — the agent (or any other reporter) reports the
  finished attempt to `POST /v1/usage`. The agent measures the latency it
  observed and names the channel that produced the counts.

A telemetry failure must not change behaviour:

- A usage report that cannot be stored answers `500`/`503` to the reporter and
  nothing else: the request the reporter was describing has already finished,
  and the route it used is unchanged.
- The controller stays out of the streaming path. The agent talks to the proxy
  directly.
- The agent's report is best effort by construction — the caller decides
  whether a failed report is worth retrying, and the identity makes that safe.
- **A failed call is still an attempt.** When the proxy call fails, the agent
  hands the caller an attempt that can be reported: a failure before the
  request was sent is a failure (`FAILED_BEFORE_FIRST_TOKEN`), and a failure
  after it is `AMBIGUOUS`, because nothing in the failure says whether the
  provider served it. A failed report never replaces the proxy error — the
  caller learns both.

## Boundaries

- Not this issue: Prometheus exposition (#26), provider quota polling (#13),
  candidate selection (#17), performance-based routing (#24/#25).
- Not this issue: OpenTelemetry, tracing, alerting or an event platform.
- Not this issue: estimating provider billing, or claiming that an estimate
  equals provider-billed usage.
