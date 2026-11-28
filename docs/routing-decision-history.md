# Routing decision history

Issue: [gazon1/quota-router#59](https://github.com/gazon1/quota-router/issues/59)
Code: [internal/routing/reasons.go](../internal/routing/reasons.go),
[internal/routing/candidates.go](../internal/routing/candidates.go),
[internal/store/write.go](../internal/store/write.go),
[internal/store/migrations/0007_routing_decision_candidates.sql](../internal/store/migrations/0007_routing_decision_candidates.sql)

The controller persists **why** a provider/model was selected and **why** every
other candidate was not, then correlates the decision with the outcome of the
request it routed.

This is decision history, not a new routing algorithm. **Routing never reads the
history.** Selection reads quota snapshots, provider health and the session's
route pin — never a previous decision, never an outcome, never an aggregate. The
history is observational: it explains what happened, it does not influence what
happens next.

## What is persisted

### The decision (`routing_decisions`)

One row per completed decision, immutable once written.

| Field | Meaning |
|---|---|
| `decision_id` | Stable id, also returned by the route API and referenced by usage rows |
| `policy_version` | The selection policy that produced it (`routing.v1`) |
| `session_id` | The agent session the decision belongs to |
| `task_class` | The agent-supplied task class, NULL when the request carried none |
| `requested_alias` / `requested_provider` | What the request asked for (override first, then preference); NULL when it named neither |
| `selected_alias` / `selected_provider` | The route that was chosen; NULL when no candidate was eligible |
| `selected_reason_code` | The stable code of the policy result that chose the route |
| `selected_reason` | The prose beside the code, for a human reader |
| `affinity_expires_at` | When the session's route lease expires, when the decision was made under a pin; NULL otherwise |
| `payload` | The decision JSON — the immutable snapshot of the whole decision, including the candidate set |
| `created_at` | UTC, indexed |

**Never persisted:** request prompts, conversation history, completions, tool
arguments, credentials, authorization headers, raw provider responses. The
decision payload is the routing decision and nothing else; the route request
carries no prompt content by construction.

### The candidates (`routing_decision_candidates`)

One row per candidate the selector actually evaluated, in evaluation order.

| Field | Meaning |
|---|---|
| `decision_row_id` | The decision this candidate belongs to; `ON DELETE CASCADE` |
| `decision_id` | The decision id, copied for correlation queries that do not join |
| `ordinal` | Evaluation order, 1-based, in the catalogue's sorted alias order |
| `alias` / `provider` | Which candidate was evaluated |
| `selected` / `eligible` | Whether it was chosen; whether it satisfied every hard constraint |
| `eliminated_stage` | The stage that eliminated it; NULL when it was not eliminated |
| `reason_code` | The stable code of the outcome for this candidate |
| `reason` | The prose beside the code |
| `transient` | The elimination says nothing about the route itself (the observation was missing or stale) |
| `quota_state`, `quota_unknown_reason`, `quota_observed_at`, `quota_reset_at` | The quota values the decision acted on; NULL means "not observed" |
| `health_state`, `health_reason`, `health_observed_at`, `cooldown_until` | The health values the decision acted on; NULL means "not observed" |

The values are **copies of what the decision was made on**, not references to
observation rows: pruning a quota snapshot must not make a decision unreadable.
A value that was not observed stays NULL — never `0`, never `AVAILABLE`, never
`HEALTHY`. A stale observation is recorded as the `UNKNOWN` it actually was, with
its reason.

`eligible` means "passed every hard-eligibility stage". A candidate dropped by a
preference stage (a manual override, the session pin) was eligible and merely
not chosen — those carry the `not_selected_by_*` codes.

### The outcome (`usage_records`)

Outcomes live in the existing usage table rather than in a second accounting
implementation ([#14](https://github.com/gazon1/quota-router/issues/14)). The
correlation is `usage_records.decision_id`; the ingestion identity is still
`(request_id, attempt)`.

Migration 0007 added two columns and one outcome value:

- `latency_ms` — NULL when nobody measured it, never `0`.
- `error_class` — a closed normalized class (`TIMEOUT`, `RATE_LIMITED`,
  `AUTHENTICATION_OR_PERMISSION_FAILURE`, `PROVIDER_ERROR`, `UNREACHABLE`,
  `CLIENT_ERROR`, `UNKNOWN`). Never raw error text: a provider message can carry
  a credential.
- `outcome` gains `TIMEOUT`.

`decision_id` stays a **soft reference** — no foreign key to `routing_decisions`.
Usage outlives decisions: the decision row may be pruned by retention while the
attempt is still worth keeping. An id no decision claims is stored as reported.

## The reason-code contract

Every candidate carries a `reason_code` from a **closed vocabulary**
([internal/routing/reasons.go](../internal/routing/reasons.go)). The prose
`reason` beside it is written for a human and may be reworded; **the code is
the contract** and changes only together with the policy version.

| Code | Meaning |
|---|---|
| `selected_by_preference` | The candidate won the configured and request preference ranking |
| `selected_by_override` | A manual override chose the candidate |
| `selected_by_affinity` | The session's route pin kept the candidate |
| `eligible_not_selected` | Eligible, but lost the preference ranking |
| `not_selected_by_override` | A manual override chose another route |
| `not_selected_by_affinity` | The session's route pin chose another route |
| `model_disabled` | The alias is disabled in the catalogue |
| `capability_mismatch` | The model lacks a required capability or task class |
| `model_unavailable_window` | The model is inside its configured recurring unavailable window |
| `health_not_observed` | The provider carries no health observation |
| `health_stale` | The health observation is older than the freshness bound |
| `provider_unavailable` | The provider reported itself unavailable |
| `health_unknown` | The provider's health state is `UNKNOWN` |
| `quota_not_observed` | The provider carries no quota observation |
| `quota_unknown` | Quota could not be established; the candidate row carries the reason |
| `quota_exhausted` | The subscription quota is exhausted |
| `quota_reserved` | The remaining quota is reserved for another workload |
| `quota_ineligible` | The quota state is not an eligible one |
| `provider_cooldown` | Routing policy cooled the provider down |

A consumer must be able to treat an unknown code as "some other reason" without
guessing what it means. A code that is not in this list is a bug, not an
extension point.

`transient` marks an elimination that says nothing about the route itself: the
observation was missing or stale. It is not the same as a durable exclusion — a
provider cooldown is durable, because holding a session on a cooled provider
would send the conversation into the rate limiting the cooldown exists to avoid.

## Atomicity

The decision and its candidates are written in **one SQLite transaction**. A
partially persisted decision is not valid history, so a failure anywhere in the
write leaves the database exactly as it was: no decision row, no candidate rows.

Persistence failure is not silent. `POST /v1/route` answers `503` when the
decision cannot be stored — a computed decision is never returned as success
without its history.

## Retention

| Setting | Default | Bounds |
|---|---|---|
| `sqlite.retention.routing_decisions` | 30 days | Decision rows and, by cascade, their candidate rows |

Candidates are **not** a separate knob: they cascade with their decision
(`ON DELETE CASCADE`), so pruning can never orphan one. Outcome rows are bounded
by `sqlite.retention.usage_records` (90 days) and are pruned independently,
because a usage row may outlive its decision.

Each retention run is one transaction.

## Reading the history

- **In-process**: `Store.LatestRoutingDecisions(ctx, limit)` returns the newest
  decisions, each with its candidate set.
- **HTTP**: `GET /v1/routing/decisions` returns the same records. The candidate
  set is inside the stored decision payload, so no endpoint was added and the
  #18 DTOs were not extended. See [docs/api.md](./api.md).

There is no public history endpoint beyond that, and no endpoint that exposes
internal database rows.

## Boundaries

- **Routing never reads this history.** Selection is deterministic from the
  request, the catalogue, the quota snapshot, the provider health and the
  session's pin. A decision that could not be persisted fails the route rather
  than degrading the selection.
- **No learned scoring.** The history does not tune priorities, break ties or
  change a route.
- **No second token accounting.** Outcomes reuse the #14 identity and the
  existing usage table.
- **Not this issue:** a public history API, semantic explanations, embeddings,
  and any predictive routing.

## Tests

`internal/routing/candidates_test.go` covers the candidate set: evaluation
order, selected and rejected codes, an override that skips a preference stage
but not a hard constraint, UNKNOWN/stale observations, a provider with no
observation at all, the no-eligible-candidate case, and an invalid request that
evaluates nothing.

`internal/store/decision_test.go` covers persistence: the atomic commit (an
injected duplicate ordinal leaves no decision and no candidates), the read path,
NULL for unobserved values, retention pruning decisions and cascading their
candidates, restart persistence, the redaction boundary (a credential-looking
reason is refused and nothing is written), and the outcome side — latency and
error class persisted, a missing latency staying NULL, duplicate delivery,
conflicting reuse, and an unknown error class being refused.

`internal/application/route_test.go` covers the wiring: the persisted record
carries the candidate set and the selected reason code, and so does the payload.
