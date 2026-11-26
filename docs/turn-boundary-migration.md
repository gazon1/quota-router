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
storing a raw provider error, and `config` rejects a URL with embedded
credentials. The checkpoint follows the same rule, over **every** field, not only
the tool results.

| Entry | Enforced by |
|---|---|
| a completed result | `CompletedResult` truncates past `maxFieldRunes` (4096 runes, marker included) and marks the cut — a file dump is not a result, and the next model knows to re-read the source |
| a failed result | `FailedResult` stores one of `TOOL_ERROR`, `DENIED`, `TIMEOUT`, `OUTCOME_UNKNOWN`; an undeclared value becomes `TOOL_ERROR` |
| a planned call | `PlannedResult`, which carries no result |
| every string | `Validate()` refuses one over 4096 runes (a file, not a summary) or one carrying a credential shape — an `Authorization:` header, a bearer token, `api_key=`/`token=`/`password=`, or `user:password@host` in a URL |
| every tool entry | `Validate()` refuses an undeclared status or failure, raw error text in a failed result, a result on a call that never started, and a nameless call |

`TOOL_ERROR` replaces the raw error on purpose: a provider error can carry a
credential in a URL or a header, and the next model would print it. The text stays
in the caller's own logs. `Validate` is the last gate — a checkpoint that skipped
the builders is refused rather than sent.

The URL rule is applied to URLs *extracted* from the text, because a credentialed
URL inside a sentence does not parse as a URL and a parse error must not read as
"no credential". A bare JWT is caught by its shape (`eyJ…`, two dots).
`authorization:`, `token=`, `api_key=` and `password=` count only when a value
that is not a placeholder (`none`, `unset`, `empty`, `optional`, `required`,
`redacted`, `***`) follows — at **any** occurrence, so
`token= unset; token=sk-live-abc` is still refused — and "authorization: none
required" stays valid. For `Bearer`, the side fails closed and the scheme is
matched **case-insensitively** (RFC 7235 §2.1), so a credential after either
spelling is refused, including inside diagnostic text
(`bearer abcdefghijklmnop was rejected`). The only escape is grammatical: an
article **before** the word and a function word after it, as in "the bearer of this
report" — so `Bearer the` and `Bearer of sk-live-1` stay refused, since neither
reads as a sentence. A value spelled like a function word is still a value.
Surrounding punctuation is ignored (`none!`, `(unset)`, `unset?`), so a placeholder
is a placeholder in any sentence. No character or vowel heuristic tries to read a value
as a word — those could not separate `abcdefghijklmnop` from prose. The cost is that a summary mentioning the
scheme before any other word rephrases it; that cost is far below sending a live
credential to the next model.

What `Validate` cannot do is read prose. It catches the shapes this installation
itself hands around, not arbitrary text: a caller that pastes a raw provider body
into `Goal` has bypassed the contract, and the fix there is to summarize, not to
forward. The narrowness is deliberate: the guard refuses rather than guesses, and
the one allowance it grants is a fixed list of function words.

`OUTCOME_UNKNOWN` counts as executed: a call whose reply never arrived may have
run, and a retry could repeat its side effect.

## Rebuild and budget

`Migration` names the model and the rebuild. `Rebuild` pairs its kind with its
**own** input-token estimate, so a number measured for a checkpoint rebuild cannot
be lent to a full replay by a field mix-up. The estimate is still the caller's:
`Rebuild{Kind: FULL_TRANSCRIPT, InputTokens: 6000}` is representable and passes
the budget. Only accounting (#23) can tell what the context really costs.

| Rebuild kind | Use |
|---|---|
| `CHECKPOINT` | default: the compact checkpoint plus only the relevant recent/extracted history and tool results — never the whole transcript |
| `FULL_TRANSCRIPT` | a deliberate replay. It requires `AuthorizedBy`, naming the policy or agent that decided on it, and is never a fallback on a failed migration |

`Migration.Allowed(budget)` fails closed. It refuses an unknown rebuild, an
unestimated cost (`Rebuild.InputTokens <= 0`), a model-less migration, an unset
budget (`MaxInputTokens <= 0`) and a `FULL_TRANSCRIPT` with a blank `AuthorizedBy`
— a budget nobody configured must not authorize a rebuild, and neither must a bare
struct literal authorize a full replay. Over the budget, no migration happens.

## What the contract does not do

It holds no provider client, no tokenizer and no store. `Rebuild.InputTokens` is
still the caller's number: `Allowed` compares it with a budget, but it cannot
measure the context, and it cannot see what the failed route was actually charged.
A caller who writes a checkpoint-sized estimate inside a `FULL_TRANSCRIPT` rebuild
passes the budget. Real token accounting — what was charged, and therefore what a
rebuild really costs — is
[#23](https://github.com/gazon1/quota-router/issues/23); this package only makes
the decision refusable when no cost is accountable at all.
