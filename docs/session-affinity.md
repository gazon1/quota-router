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

A stale or missing observation says this installation cannot see the route right
now. That is not evidence the route got worse, and switching on it is exactly the
mid-turn move this issue forbids. Durable exclusions say something about the
route itself:

| Transient — the pin is held | Durable — the session moves |
|---|---|
| provider health stale, not observed, unknown | provider health `UNAVAILABLE` |
| quota not observed, or `QUOTA_UNKNOWN` with any reason | quota `EXHAUSTED` |
| | provider in a routing cooldown |
| | capability no longer satisfied |
| | alias disabled or absent from the catalogue |

The classification is per elimination (`RejectedCandidate.Transient`), not per
stage, because one stage can produce both kinds: `subscription_quota` yields an
exhausted provider (durable) next to an unobserved one (transient).

A stage that says nothing defaults to durable. Moving a session is the costlier
mistake to make for a reason nobody explained, so a new filter has to opt in to
being ignorable.

#### A cooldown is not missing information

A cooldown is an explicit decision that the provider should receive **no
traffic**, which is information about the route — arguably the strongest kind.
The controller is not continuing an already-running stream; it is choosing where
to send a *new* request. Holding a session on a cooled provider would route that
conversation's requests straight into the rate limiting the cooldown exists to
avoid, and would make the cooldown advisory for exactly the sessions that send the
most traffic.

So a pinned session moves off a cooled provider, and moves back only when ordinary
selection picks that route again after the cooldown expires. That is a real
trade-off rather than a conservative default: it spends one context rebuild to
respect an operator's exclusion. The alternative — pinning a session to a provider
that policy says must not receive traffic — is a routing failure that also happens
to cost the same rebuild when it eventually fails at the provider.

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

One session's whole routing operation — read the pin, decide, audit the decision,
write the pin — runs under a per-session lock, and release takes the same lock.
Without it, a release that lands between a selection's read and its write would be
silently undone by the write, and the one act documented as unconditional would
not be. The lock is per session, so unrelated sessions keep routing concurrently.

The pin is written **after** the decision audit record, and a failed pin write is
`503 unavailable`: a route that could not be recorded must not be returned as a
success. This is a deliberate two-write sequence, not one transaction, and it has a
visible consequence worth stating: an audit row can exist for a decision the caller
was told failed, because the audit log records *what was decided* while the pin
records *what will be used*. The two are not the same claim, and the audit record is
meant to capture decisions even when their routing state could not be committed.
Writing both in one SQLite transaction would remove the gap; it is not done here
because the audit store and the affinity store are separate seams, and the failure
mode chosen — refuse to report success — is the one that cannot mislead a caller.

A decision with no eligible route pins nothing: there is no route to keep.

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
