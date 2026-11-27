# Agent-to-router request contract

Issue: [gazon1/quota-router#16](https://github.com/gazon1/quota-router/issues/16)
Code: [internal/routing/request.go](../internal/routing/request.go)

A routing request is an **explicit decision boundary**. The agent decides when
to ask for a route; the router chooses an eligible provider/model for that
request. It is not an instruction to switch models after every agent message,
not a per-message reclassification, and not a session migration.

## Request schema

`RouteRequest` is a small versioned JSON object (`version: 1`).

| Field | Required | Meaning |
|---|---|---|
| `version` | yes | Schema version. Current version is `1`. |
| `session_id` | yes | Agent session identifier. The router may use it to preserve session affinity where applicable. |
| `capabilities` | yes (object, fields optional) | Hard requirements the selected model/provider must satisfy: `streaming`, `tool_calling`, `structured_output`. The field must be present; an empty object `{}` means no capability requirements. A model that cannot satisfy a required capability is ineligible regardless of preference or override. |
| `task_class` | no | Agent-supplied task class: `routine`, `coding`, or `reasoning`. Include it only when routing policy has a concrete effect for it. The controller must not make an extra LLM call to infer it. |
| `preference` | no | Request-level preference, separate from operator-configured provider/model ordering (#53). It is not a hard constraint. |
| `override` | no | Explicit manual route selection. Visible, auditable, and bounded by hard constraints. |

### Preference

`preference.provider` and/or `preference.model_alias` name a preferred
configured provider or routing alias. A preference influences selection among
eligible candidates; it does not make an ineligible candidate eligible. An empty
preference object is invalid.

### Manual override

`override.model_alias` names the configured routing alias the agent explicitly
requests. The override is:

- **Explicit** — it appears in the request and must be recorded in the decision.
- **Bounded by hard constraints** — it cannot silently bypass capability
  constraints, select an unconfigured/disabled model, or bypass quota/health
  policy. Selection (#17) rejects an override target that is ineligible.
- **Auditable** — decision explanations include that an override was requested
  and whether it was applied or rejected.

A request-level preference that names a different `model_alias` than the
override is contradictory and rejected at validation. A provider preference is
not automatically contradictory with a model override; selection decides whether
the override target satisfies it.

## Validation

`RouteRequest.Validate()` rejects malformed, unsupported, and contradictory
values at the contract boundary:

- unsupported `version`;
- missing/blank `session_id`;
- missing `capabilities` (the field must be present; `{}` is valid);
- unsupported `task_class`;
- empty or blank preference/override fields;
- preference `model_alias` contradicting `override.model_alias`.

Contract validation checks shape, not catalogue membership or runtime
eligibility. Whether an alias exists, is enabled, has quota, is healthy, or
satisfies capabilities is decided by #17.

## Compatibility

`task_class` is a closed v1 vocabulary (`routine`, `coding`, `reasoning`).
Unknown values are rejected so routing policy cannot silently ignore a class it
does not understand. Adding a new class is a contract change that requires
coordinated agent/controller deployment and an explicit policy decision in #17.

## HTTP endpoints

`POST /v1/routing/select` evaluates a `RouteRequest` and returns a routing
decision. `GET /v1/routing/decisions` returns recent decision audit records.
Both endpoints require the configured bearer token (`QUOTA_ROUTER_AUTH__TOKEN` /
`auth.token`), same as `POST /v1/usage`; when no token is configured, requests
are refused.

`/v1/routing/select` returns `400` for malformed or invalid requests, `409`
when the request is valid but no eligible model can be selected, `503` when
routing state or decision audit persistence is unavailable, and `200` with the
decision otherwise. Decision audit persistence is mandatory: a computed decision
is not returned as success if it cannot be persisted.

The HTTP integration currently loads quota and health state only. Session
affinity and performance tie-breaks are implemented in the selector but not yet
wired into this endpoint.

## Boundaries

- **Not this issue:** eligibility, quota/health evaluation, preference ordering,
  deterministic selection, decision explanations, fallback, and in-flight turn
  safety — [#17](https://github.com/gazon1/quota-router/issues/17).
- **Not this issue:** operator-configured provider/model ordering and static
  preference defaults — [#53](https://github.com/gazon1/quota-router/issues/53).
- **Not this issue:** manual override rollout controls and shadow/active mode —
  [#29](https://github.com/gazon1/quota-router/issues/29).
- **Agent-owned:** when to request a route, checkpointing, context curation, and
  cross-model continuation. The router does not migrate partially produced
  context.
