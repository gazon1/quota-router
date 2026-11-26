# Command Code quota adapter

Issue: [gazon1/quota-router#11](https://github.com/gazon1/quota-router/issues/11)
Gate: [gazon1/quota-router#4](https://github.com/gazon1/quota-router/issues/4) — **closed, negative on quota visibility**
Code: [internal/provider/commandcode](../internal/provider/commandcode)
Evidence: [compatibility matrix](./litellm-provider-compatibility.md#command-code-quota-alpha-routes)

## Outcome

Command Code subscription quota is **`UNKNOWN`**, reason
`UNSUPPORTED_NO_DOCUMENTED_QUOTA_API`. The adapter is a constant answer, not a
client: it holds no URL, no credential and no HTTP transport, and `Fetch` makes
no request.

This is the outcome #11 specifies for a negative or inconclusive gate:

> If no such API exists, the correct result is subscription quota UNKNOWN with
> reason `unsupported_no_documented_quota_api`. Do not estimate remaining
> subscription quota from API credits, billing-period usage, token totals, model
> listings, successful inference calls, or local usage.

## Why

The #4 spike established a documented **inference** surface only:

```
Base: https://api.commandcode.ai/provider/v1
POST /chat/completions   POST /responses   POST /messages   GET /models
```

Every one of those is a LiteLLM/inference integration endpoint. None reports
remaining subscription entitlement, so an adapter that called one and inferred
quota from the answer would be inventing the number that routing depends on.

The `/alpha` routes were probed as #4 evidence and did answer `200` with a quota
shape (`windowLimits.fiveHour` / `.weekly` with `used`, `cap`, `resetAt`). They
are recorded in [the matrix](./litellm-provider-compatibility.md#command-code-quota-alpha-routes)
and **deliberately not implemented**:

| Reason | Consequence |
|---|---|
| Undocumented — no published contract | may change or vanish without notice |
| Not confirmed as permitted for automated polling | polling them could breach the terms |
| Only the inference base is documented | nothing binds `/alpha` to a supported schema |

A cleaner response shape is not permission. Schema availability is an argument
for a new gate, not for shipping a client, so this package cannot be pointed at
`/alpha` by editing a config value — the endpoint is not a field on the adapter.

## What the adapter does

| Field | Value | Why |
|---|---|---|
| `Snapshot` | `UNKNOWN` + `UnsupportedNoQuotaAPI` | no documented source |
| `Windows` | `nil` | never zero remaining, never a partial set |
| `Confidence` | `NONE` | nothing was parsed |
| `Source` | `none: no documented Command Code subscription quota source` | not an inference endpoint, so it cannot read as one |
| `ProviderState` | `UNKNOWN` / `NOT_PROBED` | independence: nothing was probed, so nothing is claimed about reachability |
| `CooldownUntil` | zero | a quota refusal is not a routing exclusion |

Health stays `UNKNOWN`/`NOT_PROBED` on purpose. `UNAVAILABLE` would claim the
provider is down and `HEALTHY` would claim it is up; neither was observed.
Because no request is made, auth, 429, 5xx, timeout and malformed-response cases
the issue lists cannot arise in this adapter at all — they cannot be misreported
as exhausted quota because they cannot happen.

## Individual usage is separate

Command Code credits and usage stay available to the *operator* through Studio
Usage and the CLI's `/usage`. They are not part of this adapter and must not be
converted into quota here: credits are a billing balance, and "credits spent" is
a period total, not a remaining rolling entitlement. `quota.Usage` carries that
kind of locally observed accounting with its own `Origin`, and the two types are
never converted.

## When this changes

Reopen only if Command Code publishes a quota interface that #4 can verify:
exact host and path, auth requirements, response semantics, which field is the
remaining subscription entitlement, and its freshness/reset behaviour. Then
record all of it on #11 before writing a line of polling code, and map only that
verified field into the normalized model.

## Tests

`internal/provider/commandcode/quota_test.go`:

| Test | Asserts |
|---|---|
| `TestFetchReportsUnsupportedNotZero` | UNKNOWN + reason, no windows, `NONE` confidence, health `UNKNOWN`/`NOT_PROBED`, no cooldown; a `StateAt` freshness bound (positive, zero and negative) cannot convert the refusal into another diagnosis |
| `TestFetchMakesNoRequest` | a counting transport: any outbound request fails the test, so a speculative `/alpha` client cannot appear unnoticed |

No fixtures are needed and none exist: there is no response to parse. The `/alpha`
schemas already captured under `docs/fixtures/smoke/` stay as gate evidence and
are not test input for this package.
