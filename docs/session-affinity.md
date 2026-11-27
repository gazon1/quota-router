# Session affinity and the explicit route lease

Issue: [gazon1/quota-router#19](https://github.com/gazon1/quota-router/issues/19)
Code: [internal/routing/select.go](../internal/routing/select.go),
[internal/server/server.go](../internal/server/server.go),
[internal/store](../internal/store)
Schema: [0006_session_affinity.sql](../internal/store/migrations/0006_session_affinity.sql)

A session is an agent conversation. Switching its model mid-conversation is a
context rebuild, not a continuation: the next model inherits no KV cache and is
charged for the whole context it receives
([turn boundaries](./turn-boundary-migration.md)). So the route a session settled
on is held while it is still viable, and only an explicit act — expiry, release,
or a manual override — moves it.

## What is stored

One row per session in `session_affinity`:

| Column | Meaning |
|---|---|
| `session_id` | Primary key. The identity the routing request carries. |
| `alias` | The catalogue alias the session is pinned to. |
| `created_at` | When the session was first pinned. A re-pin keeps it. |
| `updated_at` | When the pin was last renewed. |
| `expires_at` | When the pin stops being affinity at all. |

The store sets the timestamps, not the caller. `expires_at` is required and must
be after `updated_at`: a pin with no future expiry is not affinity, because an
unbounded pin outlives the route it names and silently overrides every later
decision. `SaveSessionAffinity` refuses such a row rather than storing it.

The TTL is `24h`, measured from the **last decision**, and every decision with a
route renews it. An active session therefore keeps its route across pauses, and an
abandoned one stops being pinned without anyone releasing it.

## How a decision uses it

Affinity is applied after the durable stages (configuration, capabilities) and
before the transient ones. Only a pin for **this** session, naming an alias, with
an expiry still in the future, applies.

| Pin state | Effect |
|---|---|
| Viable route | Selected. `affinity_influenced: true`, every other candidate rejected at `session_affinity`. |
| Route excluded for a **transient** reason | Still selected, and the reason says so. |
| Route excluded for a **durable** reason | The pin is dropped for this decision: the session moves, and the new route is pinned. |
| Expired | Not affinity. The decision is re-evaluated from the full candidate set, and the stage note says the pin expired. |
| Absent, another session's, nameless, or without an expiry | Ignored. |

### Transient means "says nothing about the route"

A stale, missing or temporarily cooled observation says this installation cannot
see the route right now. That is not evidence the route got worse, and switching
on it is exactly the mid-turn move this issue forbids. Durable exclusions say the
route itself is gone:

| Transient — the pin is held | Durable — the session moves |
|---|---|
| provider health stale, not observed, unknown | provider health `UNAVAILABLE` |
| quota not observed, or `QUOTA_UNKNOWN` with any reason | quota `EXHAUSTED` |
| provider in a routing cooldown | capability no longer satisfied |
| | alias disabled or absent from the catalogue |

The classification is per elimination (`RejectedCandidate.Transient`), not per
stage, because one stage can produce both kinds: `subscription_quota` yields an
exhausted provider (durable) next to an unobserved one (transient).

A stage that says nothing defaults to durable. Moving a session is the costlier
mistake to make for a reason nobody explained, so a new filter has to opt in to
being ignorable.

**A held pin is not a bypass.** The hold only decides which *eligible* route a
session keeps; it never makes an ineligible candidate eligible. What changes is
that a route is not *dropped* because its last observation is missing.

## Moving on purpose

Two explicit acts move a session:

- `POST /v1/sessions/{id}/release` deletes the pin and returns
  `{"status":"released"}` plus `released_alias` when a pin was there. Repeating it
  succeeds and omits `released_alias`: a release that finds nothing to release is
  not an error, so a retried HTTP call is safe. It never touches quota or health.
  The next decision for that session starts from the full candidate set.
- `override.model_alias` in the request. The override is explicit and auditable,
  and it re-pins: the pin is written from the decision, so it replaces the route
  it was pinned to instead of being refused by it.

Overrides remain bounded by hard constraints — an override to an ineligible
candidate is rejected and the pin stays where it was.

## Failure behaviour

The pin is written **after** the decision audit record, and a failed write is
`503 unavailable`: a route that could not be recorded must not be returned as a
success, or the session would drift between what the caller was told and what the
next decision reads. A decision with no eligible route pins nothing.

Both the server and the selector refuse an expired pin, so the explanation and the
stored state agree on what a session is pinned to.

## Restart safety

Affinity is SQLite rows, not in-memory state, so a controller restart honours
every pin it already wrote. Both paths are tested: a pin written before a restart
is read back with its timestamps intact
(`TestSessionAffinitySurvivesRestart`), and the HTTP path keeps a session on its
route across decisions and releases it on request
(`TestSelectPinsAndKeepsTheRoute`, `TestSelectEndpointHonorsSessionRelease`).

Retention deletes pins whose `expires_at` has passed on every run, independently
of the configured retention ages: the TTL already says when a pin stopped meaning
anything, and readers refuse it either way. That is what stops the table from
growing with sessions nobody released.

## What this does not do

- **No in-flight migration.** This decides which route a *new* request uses. A
  turn already streaming is never re-routed or replayed; where an attempt stopped
  decides what may follow ([turn boundaries](./turn-boundary-migration.md)). The
  point of the hold is to not create such a boundary for no reason.
- **No turn tracking.** The controller does not know whether a turn is open. The
  "no switch mid-turn" guarantee comes from the decision boundary being explicit
  and from transient exclusions not pushing a session off its route — not from
  the controller observing a live turn.
- **No performance tie-breaks.** The selector supports them; no observation store
  feeds them, so they never influence an HTTP decision.
- **No configurable TTL.** `24h` is a reasoned constant, not a measured one.
