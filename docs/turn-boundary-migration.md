# Turn-boundary migration and checkpoint contract

Issue: [gazon1/quota-router#21](https://github.com/gazon1/quota-router/issues/21)

Quota exhaustion or provider failure must not cost the work already done, and
must not buy the same input tokens twice. The contract lives in
[internal/turn](../internal/turn): `Stage`, `Decide`, `Checkpoint`, `Migration`
and `Budget`.

## A model switch is not a continuation

The next model inherits no context and no KV cache. Whatever it receives is
input tokens it processes from scratch, at that model's input price — the same
transcript sent twice is paid for twice, and the second payment buys nothing the
first did not already pay for. Migration is therefore a **context rebuild at a
boundary**, not a transparent retry.

## Where an attempt stopped decides what may follow

| Stage | What it means | Disposition |
|---|---|---|
| `NOT_DISPATCHED` | rejected before leaving this installation; nothing sent, delivered or executed | `START_OVER` — the same turn may pick another model |
| `AWAITING_FIRST_TOKEN` | sent, no output; the provider may have processed and charged the context | `SURFACE` |
| `STREAMING` | part of the answer reached the caller | `SURFACE` |
| `AFTER_TOOL_EXECUTION` | at least one tool ran | `SURFACE` |

`SURFACE` means: report the failure, keep this turn's route decision as it was,
and reconsider the route at the next turn boundary. Never auto-retry a partly
delivered response, and never replay a tool call. An undeclared stage is
`SURFACE` too: nothing proves it was free of side effects.

`START_OVER` is not a migration — no turn was in flight, so no state is lost and
no output can be delivered twice.

## The checkpoint carries the work across

A boundary hands over `Checkpoint`: goal, completed work, decisions, pending
work, references, and the tool results that already ran. That is what makes the
next model resume *after* an executed tool instead of at it.

- No provider payload, credential or whole file: `References` names material
  (path, id, URL) and the agent decides what re-reading it is worth.
- `ToolCompleted` and `ToolFailed` both mean the call ran. A failed call replayed
  is a second side effect, so it travels with the checkpoint, not through a retry.

### The boundary is enforced, not promised

The repository already enforces its trust boundaries in code rather than in
comments: `quota.Failed` replaces an undeclared reason with a safe code instead of
storing a raw provider error. The checkpoint does the same.

| Entry | Enforced by |
|---|---|
| a completed result | `CompletedResult` truncates past `maxResultRunes` (4096 runes, marker included) — a file dump is not a result |
| a failed result | `FailedResult` stores one of `TOOL_ERROR`, `DENIED`, `TIMEOUT`, `OUTCOME_UNKNOWN`; an undeclared value becomes `TOOL_ERROR` |
| a planned call | `PlannedResult`, which carries no result |
| the whole checkpoint | `Validate()` before it leaves for another model — it refuses an undeclared status or failure, raw error text in a failed result, a result on a call that never started, an over-long result and a nameless call |

`TOOL_ERROR` replaces the raw error on purpose: a provider error can carry a
credential in a URL or a header, and the next model would print it. The text stays
in the caller's own logs. `Validate` is the last gate — a checkpoint that skipped
the builders is refused rather than sent.

`OUTCOME_UNKNOWN` counts as executed: a call whose reply never arrived may have
run, and a retry could repeat its side effect.

## Rebuild and budget

`Migration` names the model, the rebuild and its estimated input tokens.

| Rebuild | Use |
|---|---|
| `CHECKPOINT` | default: the compact checkpoint plus only the relevant recent/extracted history and tool results — never the whole transcript |
| `FULL_TRANSCRIPT` | an explicit policy or agent decision, never automatic or a fallback on a failed migration |

`Migration.Allowed(budget)` fails closed. It refuses an unknown rebuild, an
unestimated cost (`InputTokens <= 0`), a model-less migration, and an unset
budget (`MaxInputTokens <= 0`) — a budget nobody configured must not authorize a
rebuild. Over the budget, no migration happens.

## What the contract does not do

It holds no provider client, no tokenizer and no store. `Migration.InputTokens`
is the caller's estimate, so `Allowed` can only compare an estimate with a budget:
it cannot tell whether the estimate describes the rebuild it is attached to, and
it cannot see what the failed route was actually charged. A caller that reuses a
checkpoint-sized estimate for a full replay passes the budget. Real token
accounting — what was charged, and therefore what a rebuild really costs — is
[#23](https://github.com/gazon1/quota-router/issues/23); this package only makes
the decision refusable when no cost is accountable at all.
