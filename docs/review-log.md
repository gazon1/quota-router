# Review log — PR #55

Reviews of this pull request by an independent model (ChatGPT, via `10x-chat`),
in two isolated roles per round. This file records what each round found and what
the coding agent did about it, so the adjudication is auditable rather than
implied. It is not a substitute for the review: the raw answers are quoted or
summarised with their severity, and a finding is only closed when the current
code proves it.

Roles: **A** = correctness, regressions, security, edge cases. **B** =
architecture, maintainability, compatibility, scope discipline. Each role runs in
a fresh conversation and never sees the other's answers.

## Round 1 — head `7e9c121`

| Role | Verdict | Findings |
|---|---|---|
| A | changes requested | 8 (4 MAJOR, 3 MINOR, 1 INFO) |
| B | no blocking issues | 4 (3 MAJOR, 1 MINOR) |

Both roles converged on the same core defect: the probe recorded outcomes instead
of asserting them, so it could exit 0 without verifying most of what the document
claimed.

| Finding | Roles | Resolution |
|---|---|---|
| MiniMax quota "success" without checking `base_resp.status_code` | A1, B2 | fixed — business status asserted | 
| Most inference cases had no expected outcome | A2, B1 | fixed — every case states its outcome |
| Redaction left arbitrary strings and non-JSON bodies raw | A3 | fixed — strings scrubbed, bodies bounded |
| `/alpha` fixtures carried real billing figures | A4, A8 | fixed — field names and types only |
| Cooldown test used a valid upstream key | A5, B3 | fixed — needs `--litellm-invalid` proxy, asserts 401,401,401 |
| Document marked cases DONE that the probe does not reproduce | A6 | fixed — reproducible vs manually measured split |
| Go contract test matched YAML substrings | A7, B4 | fixed — decoded YAML, full tuple asserted |

## Round 2 — head `c13520d`

| Finding | Status reported | Resolution |
|---|---|---|
| Business status still optional (`biz is not None`) | STILL OPEN | fixed — required on a 200 |
| `allow_plan_exhausted` marked any 200 VERIFIED | STILL OPEN | fixed — inspected |
| Any 403/429 labelled `BLOCKED_BY_PLAN` | new, MINOR | fixed — documented plan codes only; transient classified separately |
| Tool-call / stream assertions weaker than documented | new, MAJOR | fixed — payload asserted, `stream_options.include_usage` requested |
| `gen_...` ids escaping redaction | new, MINOR | fixed — `object` alone is not a model marker |

One finding from round 2 was **rejected as not actionable**: the reviewer asked
that the probe "independently establish that the target proxy accepted its master
key". The test's subject is whether a rejected *upstream* key stays 401; a wrong
master key already has its own case (`litellm_no_auth`). The cooldown case now
asserts the observed upstream error shape instead, which is the behaviour under
test.

## Round 3 — head `fcaa2dc`

Verdict: **REQUEST CHANGES**, no BLOCKING findings, 5 items to close.

| Finding | Status reported | Resolution |
|---|---|---|
| MiniMax streaming verified without inspecting business status | MAJOR, open | fixed — the stream parser keeps `stream_error` and an SSE error event on a 200 is no longer a pass |
| Streaming assertions accepted empty streams | MAJOR, open | fixed — a stream must terminate, carry no error and produce assistant deltas |
| Failed assertions still wrote `VERIFIED` fixtures | MINOR | fixed — one failing assertion sets `verdict: FAILED`; `VERIFIED` is only assigned after every check passes |
| Fixtures published raw `reasoning` / encrypted reasoning payloads | MAJOR, security | fixed — reasoning is dropped wholesale, key- and type-based, verified on chat and Responses shapes |
| Plan codes matched as substrings of the whole body | MINOR | fixed — structured error fields only, with a message fallback; checked against six bodies |

Role A also confirmed as fixed: the Go contract test (both prior defects), the
`/alpha` billing figures, and the rejected-upstream-key test.

## Round 4 — head `75a215e`

Verdict: **REQUEST CHANGES**, no BLOCKING findings, 5 targeted items. Role A also
re-audited the config and found no incorrect provider prefix, base URL or leaked
credential, and confirmed the round-3 reasoning exposure is gone.

| Finding | Resolution |
|---|---|
| Streaming checked without a business status | fixed — a stream has no JSON body, so its business outcome is the error event and the assistant text; the case now asserts both and says which it checks |
| External assertions left fixtures `VERIFIED` while the run failed | fixed — the cooldown assertion now writes `verdict: FAILED` into all three fixtures it judges |
| Plan-refusal classification accepted any `permission_error` | fixed — a `permission_error` must name the plan; a bare 403 is a failure |
| A duplicate model could offset a missing one in the count check | fixed — each configured model must appear exactly once; mutation-checked |
| The catalogue was never asserted, only fetched | fixed — every configured model must be listed with the endpoint the config routes to; verified it fails when a model or an endpoint is removed |

## Round 5 — head `8b18f47`

Verdict: **APPROVE** — no blocking or major findings; all five round-4 items
confirmed FIXED at that head. One MINOR remained: the document claimed the
fixtures contain "schema only, never billing figures", while they do retain the
token counts and quota percentages the endpoints exist to return. Corrected: the
document now states which fields are kept and why, which are dropped and why, and
that `/alpha` magnitudes are replaced by their types. The reviewer also confirmed
the blocked MiniMax inference is recorded honestly and is not presented as passed.

The reviewer listed six decisions for the repository owner to confirm (MiniMax
model switch, free-tier Command Code models, global cooldown and retry policy,
shared Token Plan credential, and the fixture contents). They are recorded as
implicit decisions in the PR body and are not defects.

## What the reviews did not change

Both roles accepted several decisions as intended rather than defective, and they
stay: exact `<provider>/<model>` ids as `model_name`, `disable_cooldowns: true`,
`num_retries: 0`, the free-tier model selection, and treating the Command Code
`/alpha` quota routes as evidence only. Each is recorded as an implicit decision
in the PR body so a human can confirm or overrule it.
