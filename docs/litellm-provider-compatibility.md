# LiteLLM API compatibility and provider smoke tests

Issue: [gazon1/quota-router#8](https://github.com/gazon1/quota-router/issues/8)
Date checked: **2026-11-26**
Scope: Verified the pinned LiteLLM deployment and both provider integrations
against the live Token Plan endpoints, with the account keys supplied by the
operator. No undocumented provider endpoint was used for inference, and no
Studio page was scraped.

Reproduce with `docs/fixtures/smoke/probe_providers.py` (see
[Reproduction](#reproduction)). Fixtures in [docs/fixtures/smoke](./fixtures/smoke/)
are sanitized: keys, request IDs, user/org identifiers and subscription IDs are
`***REDACTED***`.

## Status summary

| Acceptance criterion | Status | Evidence |
|---|---|---|
| Pin the LiteLLM image tag/digest that passes tests | **DONE** — `v1.110.0@sha256:4295b06…b883c3`, tag resolves to that digest | [Pin verification](#pinned-image) |
| Separate provider credentials and base URLs; never reuse quota host as inference `api_base` | **DONE** — and one shipped example violated it (fixed) | [Defect 3](#defect-3-controller-example-polled-the-inference-host) |
| MiniMax: non-streaming + streaming with the Token Plan key | **BLOCKED ON QUOTA** — key authenticates, plan exhausted (`2056`) | [MiniMax inference](#minimax-inference) |
| MiniMax: quota endpoint with the same credential, 5-hour and weekly mapping | **DONE** — captured and decoded, including how it disagrees with inference | [MiniMax quota](#minimax-token-plan-quota) |
| Command Code: live catalogue, then correct path per model | **DONE** — 105 models with per-model `supported_endpoints` | [Command Code catalogue](#command-code-catalogue) |
| Command Code: streaming, tool calls, final usage, errors | **DONE, reproducible** — each case is asserted by `probe_providers.py` and fails the run on regression | [Command Code behavior](#command-code-behavior) |
| Command Code: cancellation | **MANUALLY VERIFIED only** — measured once, not part of the committed matrix (`--cancellation`, spends quota) | [Cancellation](#command-code-behavior) |
| Command Code: investigate `/alpha` quota routes as the #4 gate | **DONE — NOT a supported contract**; response schemas recorded | [Command Code quota](#command-code-quota-alpha-routes) |
| Invalid, wrong-type, revoked, insufficient-plan credentials | **PARTIALLY reproducible** — the probe asserts invalid-key, wrong-master-key, plan-refusal and the 401-not-429 cooldown case; revoked-key and 5xx were recorded manually | [Credential matrix](#credential-matrix) |
| 429/5xx/timeouts/malformed payloads, bounded retry | **PARTIAL** — 429 and malformed covered; 5xx not observed | [Error semantics](#error-semantics) |
| Model aliases map to exact upstream IDs, no silent fallback | **DONE** — plus one shipped alias was rejected by LiteLLM (fixed) | [Defect 1](#defect-1-the-shipped-model_name-was-not-a-liteLLM-model-id) |
| Internal LiteLLM auth; no secrets/prompts in logs or artifacts | **DONE** — asserted by the probe; fixtures keep model ids and schema only, never billing figures or identity | [Internal auth](#internal-litellm-auth) |
| Unsupported quota surfaces documented as `UNKNOWN`, not zero/AVAILABLE | **DONE** | [UNKNOWN surfaces](#unsupported-or-unverified-surfaces) |

## Pinned image

`deploy/compose.yaml` pins `ghcr.io/berriai/litellm:v1.110.0@sha256:4295b064b22eb88f9530e616afc812edd47048891ba64896bfc3fa74e2b883c3`.
The digest was re-resolved from the tag on 2026-11-26 and matches:

```
$ docker buildx imagetools inspect ghcr.io/berriai/litellm:v1.110.0
Digest: sha256:4295b064b22eb88f9530e616afc812edd47048891ba64896bfc3fa74e2b883c3
```

All proxy results below were produced by this exact image, started the way
`compose.yaml` starts it (`--config /app/config.yaml`, config mounted read-only).

## Defects found and fixed

Three shipped defects were found by testing, not review. Each now has a
regression test in `deploy/compose_test.go`.

### Defect 1: the shipped `model_name` was not a LiteLLM model id

`deploy/litellm-config.yaml` declared `model_name: minimax-m3.1-flash` while the
controller's `litellm.model_aliases` sent `minimax/MiniMax-M3.1-Flash`. Measured
on the pinned image:

```
GET /v1/models                      -> {"id":"minimax-m3.1-flash", ...}
POST /v1/chat/completions {"model":"minimax/MiniMax-M3.1-Flash"}
                                    -> 400 Invalid model name passed in model=minimax/MiniMax-M3.1-Flash
```

LiteLLM resolves requests by `model_name`, so the alias documented in
`config.example.yaml` could never route. Fixed: `model_name` is the exact
`<provider>/<model>` string, verified to resolve:

```
GET /v1/models -> minimax/MiniMax-M2.7, commandcode/recursive/flint-v1.0:free,
                  commandcode/stealth/glyph-cluster:free, commandcode/deepseek-v4.1-flash
```

### Defect 2: a rejected provider key resurfaced as `429` on the next request

With default router settings, one provider `401` marks the deployment unhealthy.
The **next** request then fails with a cooldown error that is not about the
caller's request at all. Measured, same key, three attempts, no config change
between them:

```
attempt 1 -> 401  authentication_error
attempt 2 -> 429  {"type":"all_deployments_in_cooldown",
                   "message":"No deployments available for selected model, Try again in 5 seconds.
                              All deployments for selected model are in cooldown."}
```

That is the dangerous shape: it tells a client to back off and retry when the
credential is simply wrong, and it would make quota accounting blame the wrong
cause. `router_settings.disable_cooldowns: true` removes it; re-measured with the
fix in place, and with a genuinely exhausted plan, the provider's own status
repeats unchanged:

```
attempt 1 -> 429   (provider's own 2056 rate_limit_error)
attempt 2 -> 429   (same provider error, not a cooldown)
attempt 3 -> 429   (same)
```

`num_retries: 0` is set alongside it: a retry may follow a tool call that already
executed, so the client decides whether to repeat, not the proxy.

### Defect 3: controller example polled the inference host

`config.example.yaml` set `providers.minimax.base_url: https://api.minimax.io/v1`.
MiniMax serves Token Plan quota from a **different** host,
`www.minimax.io`. A Controller pointed at `api.minimax.io` would poll inference
and never see quota. Fixed to `https://www.minimax.io/v1`; LiteLLM keeps
`api.minimax.io` for inference only. `deploy/compose_test.go` fails if a
`www.minimax.io` host appears as a LiteLLM `api_base`.

## MiniMax Token Plan

Credential: the operator's Token Plan subscription key (`sk-cp-…`). The pay-as-you-go Open Platform key was **not** used and must not be substituted: they
are different billing paths.

### Endpoints

| Purpose | Method | URL | Measured |
|---|---|---|---|
| OpenAI-compatible inference | POST | `https://api.minimax.io/v1/chat/completions` | reachable, auth enforced |
| OpenAI Responses | POST | `https://api.minimax.io/v1/responses` | path exists (401 on bad key, 404 on a bogus sibling path) |
| OpenAI model list | GET | `https://api.minimax.io/v1/models` | **200**, live list |
| Anthropic-compatible inference | POST | `https://api.minimax.io/anthropic/v1/messages` | reachable, auth enforced |
| Anthropic model list | GET | `https://api.minimax.io/anthropic/v1/models` | **200**, live list |
| Token Plan quota | GET | `https://www.minimax.io/v1/token_plan/remains` | **200**, schema captured |

The `/models` route does exist and is complete, contrary to the issue's warning
that it might not:

```
openai:    MiniMax-M3.1-Flash, MiniMax-M3, MiniMax-M2.7, MiniMax-M2.7-highspeed, MiniMax-M2.5,
           MiniMax-M2.5-highspeed, MiniMax-M2.1, MiniMax-M2.1-highspeed, MiniMax-M2
anthropic: identical list
```

Both protocols agree, so `MiniMax-M2.7` is a verified model ID, not a guess. It is the
model configured in `deploy/litellm-config.yaml`.

### MiniMax inference

Inference is **blocked by plan exhaustion**, not by a compatibility problem. Every
model on both protocols, with the valid key, returns the same documented
business error:

```
POST /v1/chat/completions   MiniMax-M2.7      -> 429  rate_limit_error
                                                     "Token Plan usage limit reached: Upgrade your
                                                      Token Plan or purchase Credits for more usage. (2056)"
POST /v1/chat/completions   MiniMax-M3, -M2.7    -> 429  (same 2056)
POST /v1/chat/completions   MiniMax-M3.1-Flash -> 429  (same 2056)
POST /anthropic/v1/messages MiniMax-M3.1-Flash   -> 429  (same 2056)
```

Every probed model returns the same business error, so the quota is exhausted account-wide
rather than per-model. So **authenticated successful streaming and tool calls were not
reproduced on MiniMax** and remain unverified for this issue. The key itself is valid — the
same key returns `200` on `/models` and on the quota endpoint — so this
distinguishes *plan/usage exhaustion* (`2056`) from *authentication failure*
(`1004`/`2049`), which the issue asks for. Nothing here indicates that the
OpenAI-compatible or Anthropic-compatible request shapes are wrong; it indicates
no remaining quota on this account.

### MiniMax Token Plan quota

`GET https://www.minimax.io/v1/token_plan/remains` with the Token Plan key,
captured response ([fixture](./fixtures/smoke/minimax_token_plan_remains.json)):

```json
{"model_remains":[
  {"model_name":"general",
   "start_time":1795687200000,"end_time":1795705200000,"remains_time":126683,
   "current_interval_total_count":0,"current_interval_usage_count":0,
   "current_interval_status":1,"current_interval_remaining_percent":100,
   "weekly_start_time":1795392000000,"weekly_end_time":1795996800000,
   "weekly_remains_time":291726683,
   "current_weekly_total_count":0,"current_weekly_usage_count":0,
   "current_weekly_status":2,"current_weekly_remaining_percent":0},
  {"model_name":"video", ... "current_interval_status":3, "current_weekly_status":3, ...}],
 "base_resp":{"status_code":0,"status_msg":"success"}}
```

Mapped (timestamps are epoch **milliseconds**, `remains_time` is milliseconds
remaining, not a timestamp and not a percentage):

| Field group | Meaning as measured |
|---|---|
| `start_time`/`end_time` | 5-hour window, midnight-anchored UTC. Observed `general` 10:00→15:00Z, `video` 00:00→24:00Z. |
| `weekly_start_time`/`weekly_end_time` | Weekly window, Monday 00:00Z → next Monday 00:00Z. |
| `remains_time` / `weekly_remains_time` | Milliseconds left in each window. |
| `current_interval_total_count` / `_usage_count` | **Both 0 on this account.** Not a quota numerator. |
| `current_interval_remaining_percent` / `current_weekly_remaining_percent` | Percentage, in `0..100`. |
| `current_interval_status` / `current_weekly_status` | Small integer enum, observed `1`, `2`, `3`. Not documented publicly; treated as `UNKNOWN` (see below). |

**The quota response does not agree with the inference result, and this is the
most important finding of this issue.** After every inference call above returned
`429 2056 usage limit reached`, the quota endpoint still reported:

```
general: 5h status=1 pct=100 | weekly status=2 pct=0
```

The 5-hour window claims 100% remaining while inference refuses with an exhausted
plan. Any adapter that maps `current_interval_remaining_percent` to "quota
available" would therefore report a healthy provider that cannot serve a single
request. Per the issue's rule, the **counts are absent/zero and must not be read
as a quota figure**, and the discrepancy must surface as `UNKNOWN` rather than
`AVAILABLE`. Provider quota parsing is explicitly out of scope here (issues
#9–#11); this is the evidence they need.

## Command Code Token Plan

Credential: the operator's Command Code Provider API key.

### Command Code catalogue

`GET https://api.commandcode.ai/provider/v1/models` → **200**, 105 models, each
with a `supported_endpoints` array. The live catalogue, not documentation, decides
the path. Representative rows ([full snapshot](./fixtures/smoke/commandcode_catalogue.json)):

| Model id | `supported_endpoints` |
|---|---|
| `stealth/glyph-cluster:free` — configured | `/chat/completions`, `/responses` |
| `recursive/flint-v1.0:free` — configured | `/chat/completions`, `/responses` |
| `gpt-5.6-sol`, `gpt-6.2-astra`, `deepseek/deepseek-v4-pro` | `/chat/completions`, `/responses` |
| `deepseek/deepseek-v4.1-flash` | `/chat/completions`, `/responses`, `/messages` — the model configured for the Anthropic-shaped route |
| `MiniMaxAI/MiniMax-M3` | `/chat/completions`, `/responses` |
| `minimax/minimax-m3.1-flash` | `/chat/completions` only |
| `claude-sonnet-5`, `claude-opus-5-5`, `claude-haiku-4-5-20251001` | `/messages` only — also the model used to prove endpoint/model mismatch |

The catalogue is genuinely per-model: Command Code does **not** assume every model
supports every protocol, and a model's presence in the list does not mean the
account's plan can call it (see `MODEL_NOT_IN_PLAN` below).

### Command Code behavior

All measured through LiteLLM and directly.

| Behavior | Result |
|---|---|
| Non-streaming Chat Completions | `200`; `choices[0].message.content == "pong"`; `usage.prompt_tokens/completion_tokens/total_tokens` present |
| Streaming Chat Completions | `200`, SSE; `data: [DONE]` terminator; **final chunk carries `usage`** |
| Responses API | `200` through LiteLLM `/v1/responses` |
| Anthropic Messages | `200` via `deepseek/deepseek-v4.1-flash`, directly and through LiteLLM `/v1/messages`. Claude models are `403 MODEL_NOT_IN_PLAN` on this account. |
| Tool calls | `200`, `finish_reason: "tool_calls"`, well-formed `tool_calls[0].function.arguments` JSON |
| Cancellation | Client closing the stream mid-flight is billed for produced tokens — measured `+0.001409` five-hour credits after aborting 3 chunks in |
| Final usage metadata | Streaming usage arrives in the last chunk, so a cancelled stream yields **no** usage object |
| Insufficient plan | `403 permission_error` `MODEL_NOT_IN_PLAN: … available in Pro and above plans or extra on demand usage` |
| Free-tier daily cap | `429`, `"You've used all 1000 free Flint requests for today. Your quota resets at <ISO-8601>"` — replicated on `/chat/completions`, `/responses` and through LiteLLM |

Cancellation matters for the issue's retry rule: a cancelled stream has already
consumed quota and possibly executed a tool, so a retry is not free. This is why
`num_retries: 0` is pinned rather than left at LiteLLM's default.

### Command Code quota (`/alpha` routes)

These are **not a supported public contract**. They were probed only as the #4
evidence gate. All four responded `200` to the Provider API key, with no `orgId`
scoping needed on this account (`/alpha/whoami` → `"org": null`, so `orgId=<orgId>`
is unnecessary here but remains part of the documented candidate shape).

| Route | Status | Schema as measured |
|---|---|---|
| `GET /alpha/whoami?limits=1` | `200` | `{success, user:{id,name,email,userName}, org}` |
| `GET /alpha/billing/credits` | `200` | `credits:{monthlyCredits,purchasedCredits,freeCredits,belowThreshold,creditThreshold}`, `windowLimits:{fiveHour:{used,cap,exceeded,resetAt}, weekly:{…}, limited, exceeded}`, `sandboxAccess`, `sandboxMinutes:{usedMinutes,limitMinutes,overageUsd,resetAt}` |
| `GET /alpha/billing/subscriptions` | `200` | `data:{id,status,userId,orgId,priceId,planId,quantity,currentPeriodStart,currentPeriodEnd,cancelAtPeriodEnd,…}` |
| `GET /alpha/usage/summary` | `200` | `totalCount,totalCost,averageCost,successRate,completedCount,failedCount,totalTokensIn,totalTokensOut,totalTokens,totalCredits,totalFreeCredits` |

Observed window values: 5-hour `cap` `14` credits, weekly `cap` `35`,
`resetAt` epoch milliseconds, `exceeded: false`. This is a *cleaner* quota shape
than MiniMax's, but the issue is explicit: **responding routes are not a supported
contract**. Verdict for this issue:

> **Command Code subscription quota: `UNKNOWN` as a supported API surface.**
> The `/alpha` routes work today and their schemas are recorded above, but they
> are undocumented, may change without notice, and were not confirmed as permitted
> for automated polling. No production adapter should rely on them until #4
> returns a documented/permitted decision.

## Credential matrix

| Case | Provider | Result |
|---|---|---|
| No credential | MiniMax inference | `401 authorized_error (1004)` |
| Bad bearer | MiniMax inference, chat + streaming + tool call | `401 authorized_error (1004)` |
| Bad `x-api-key` | MiniMax Anthropic Messages / models | `401 authentication_error` |
| **Wrong key type** (Command Code key on MiniMax quota host) | MiniMax quota | `200` with `base_resp.status_code 1004` business error — note the transport status is **200**, so only the body says it failed |
| Valid Token Plan key, exhausted plan | MiniMax inference | `429` business error `2056`, all models, both protocols |
| Valid key, model not in plan | Command Code Messages | `403 permission_error MODEL_NOT_IN_PLAN` |
| Wrong internal master key | LiteLLM | `400 no_db_connection` (**not** `401` — see below) |
| No internal key | LiteLLM | `401 auth_error "No api key passed in."` |
| Model/endpoint mismatch | Command Code | `400 unsupported_model`, exact expected path named |

Two of these are easy to mishandle:

- MiniMax's quota host returns HTTP **200** for an authentication failure. An
  adapter that only checks `status == 200` treats a rejected key as success.
- LiteLLM answers a wrong internal master key with `400 no_db_connection` rather
  than `401`, because this deployment runs LiteLLM stateless with no `database_url`.
  A client must not interpret that as a provider problem.

## Error semantics

| Case | Result |
|---|---|---|
| MiniMax 429 / usage limit | `429`, `type: rate_limit_error`, message carries `(2056)` |
| Command Code endpoint mismatch | `400`, `type: invalid_request_error`, `code: unsupported_model`, and `"must be called via /provider/v1/messages"` |
| LiteLLM malformed JSON body | `400 bad_request_error` / `invalid_request_error`, `param: request_body` |
| LiteLLM `model` not a string | `400`, `"'model' must be a string."` |
| LiteLLM unknown model | `400`, `"Invalid model name passed in model=… Call /v1/models to view available models for your key."` with `provider_specific_fields` |
| Command Code edge, no `User-Agent` | `403`, body `error code: 1010` (Cloudflare bot rule). Not an auth failure; a Python default `urllib` agent is rejected |
| 5xx | **Not observed** in any authenticated case |

Errors never echo the credential: `probe_providers.py` and the fixtures were
scanned for the key prefixes, user identity fields and subscription IDs, and none
appear. LiteLLM's container logs for the same run contained no key material.

## Internal LiteLLM auth

- Without `Authorization`, `/v1/chat/completions` and `/v1/models` both return
  `401 auth_error`, so the proxy is not open.
- `.env.example` ships `LITELLM_MASTER_KEY=` empty on purpose: LiteLLM refuses to
  start without it rather than accept unauthenticated traffic.
- LiteLLM logs requests but the measured log output contained no provider key; the
  config passes every credential as `os.environ/…`, and a literal `sk-` in the
  config fails the new contract test.

## Unsupported or unverified surfaces

| Surface | Status | Why |
|---|---|---|
| MiniMax authenticated streaming, tool calls, 429-vs-quota detail | `UNKNOWN` | Plan exhausted (`2056`); cannot consume quota to verify. Re-run the probe once quota resets. |
| MiniMax `current_interval_status` / `current_weekly_status` enum values | `UNKNOWN` | Not documented publicly; `1`/`2`/`3` observed, meaning unconfirmed. |
| MiniMax quota vs inference consistency | **INCONSISTENT, `UNKNOWN`** | Quota reports 100% on the 5-hour window while inference is exhausted. Adapters must not treat the percentage as availability. |
| MiniMax Responses API support | `UNKNOWN` | Path exists; support for the selected model could not be tested with no quota. |
| Command Code 5xx / timeout behavior | `UNKNOWN` | Not reproducible on demand. |
| Command Code `/alpha` quota as a contract | `UNKNOWN` / not supported | Undocumented; evidence above, decision deferred to #4. |

## Reproduction

```sh
# Provider endpoints (skips a provider whose key is unset, never fails on it)
MINIMAX_TOKEN_PLAN_KEY=… COMMAND_CODE_API_KEY=… \
  python3 docs/fixtures/smoke/probe_providers.py --out docs/fixtures/smoke

# Plus the pinned proxy, started the way compose.yaml starts it
cd deploy && docker compose up -d --wait && cd ..
LITELLM_MASTER_KEY=… MINIMAX_TOKEN_PLAN_KEY=… COMMAND_CODE_API_KEY=… \
  python3 docs/fixtures/smoke/probe_providers.py --out docs/fixtures/smoke --litellm

# The 401 -> 429 cooldown regression needs a proxy whose upstream key is
# rejected. Run a second proxy from the same config with a bogus MiniMax key:
docker run -d --name qr-bad -p 127.0.0.1:4002:4000 \
  -e LITELLM_MASTER_KEY="$LITELLM_MASTER_KEY" \
  -e MINIMAX_TOKEN_PLAN_KEY=sk-invalid-probe-key \
  -e COMMAND_CODE_API_KEY="$COMMAND_CODE_API_KEY" \
  -v "$PWD/deploy/litellm-config.yaml:/app/config.yaml:ro" \
  ghcr.io/berriai/litellm:v1.110.0@sha256:4295b064…b883c3 --config /app/config.yaml

MINIMAX_TOKEN_PLAN_KEY=… COMMAND_CODE_API_KEY=… LITELLM_MASTER_KEY=… \
  python3 docs/fixtures/smoke/probe_providers.py --out /tmp/smoke \
  --litellm --litellm-invalid http://127.0.0.1:4002

# Add the cancellation/billing measurement (spends a small amount of quota)
python3 docs/fixtures/smoke/probe_providers.py --cancellation
```

The script exits non-zero when an expectation that must hold regresses — for
example an alias LiteLLM rejects, a cooldown-masked provider error, a
model/endpoint mismatch that stops being rejected, or a 200 from the MiniMax
quota endpoint whose `base_resp.status_code` is an error. Cases the plan refuses
are printed as `BLOCKED_BY_PLAN`, never folded into a bare success.

`--out` points at a temporary directory in the commands above on purpose: running
the gate must not rewrite the committed fixtures as a side effect. Copy them in
deliberately, then check the diff before committing.

## Reproducible vs manually measured

`probe_providers.py` is the gate: a case it asserts fails the run when the
behaviour regresses, and its exit status is the verdict. Everything below is one
of these, and the table above is not allowed to blur them:

| Kind | Meaning |
|---|---|
| **reproducible** | asserted by `probe_providers.py`, checked by its exit status |
| **manually measured** | observed once with the same credentials; the fixture is the record, there is no assertion |
| **BLOCKED_BY_PLAN** | the call reached the provider and was refused by the account's plan; recorded, never counted as a pass |
| **UNKNOWN** | not observed at all |

A run where a case is blocked says so instead of printing a bare success, and the
`--litellm-invalid` proxy is required for the cooldown case: a proxy holding a
valid provider key cannot reproduce a rejected one.

## Boundaries honored

- LiteLLM serves inference; the Controller is not a proxy and no inference traffic
  is routed through it. The only Controller change here is the corrected example
  `providers.minimax.base_url`.
- Provider quota *parsing and state semantics* are not implemented here; issues
  #9–#11 own that. This document supplies the measured schemas they need.
- A Command Code quota no-go blocks neither the MiniMax integration nor the shared
  Controller/LiteLLM path: the LiteLLM model list routes Command Code inference
  regardless.
