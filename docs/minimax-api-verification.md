# MiniMax API / Model / Subscription Quota Verification

Issue: [gazon1/quota-router#3 — [P0] Verify MiniMax API, models, and subscription quota visibility](https://github.com/gazon1/quota-router/issues/3)  
Date checked: **2026-11-26**  
Scope: Focused compatibility spike for MiniMax. Public/authoritative documentation and documented API probes only. No undocumented endpoints, no private endpoints, no reverse engineering, no browser-session extraction, no HTML scraping.

## Status Summary

| Acceptance criterion | Status | Evidence |
|---|---|---|
| Prove whether API calls consume intended MiniMax subscription quota rather than separate API billing | **NOT EMPIRICALLY PROVEN** (docs indicate separation; no live key in environment) | [Docs evidence](#subscription-quota-vs-api-billing), [Authenticated tests skipped](fixtures/minimax/authenticated_tests_skipped.json) |
| Identify documented quota endpoints/fields, quota windows, reset semantics, polling limits | **PARTIAL** (quota endpoint/windows/reset documented; fields/polling limits not fully documented publicly) | [Quota visibility](#quota-usage-visibility) |
| Save reproducible request/response fixtures with secrets removed | **DONE** (unauth/bad-auth/timeout fixtures + probe script) | [Fixtures](fixtures/minimax/) |
| Test 429, auth failure, timeout, streaming, and tool-call response | **PARTIAL** (auth failure, client timeout, bad-auth streaming request observed; 429 and authenticated streaming/tool-call not empirically reproduced) | [Error/behavior tests](#observed-behavior-tests) |
| Document supported model aliases and capabilities | **DONE** (from public docs/model endpoints) | [Models](#models-and-capabilities) |
| No undocumented endpoints | **DONE** | Only documented platform/API endpoints used; probe script lists endpoints |

## Environment / Constraints

- Checked from this workspace on 2026-11-26.
- No MiniMax credentials were present in environment variables (`MINIMAX_API_KEY` / `MINIMAX_ANTHROPIC_KEY` not set).
- `web_search` was unavailable; research used public MiniMax docs via direct HTTPS fetches to documented pages and `platform.minimax.io/docs/llms.txt`.
- Live probes used only documented endpoints and invalid/missing credentials, plus a client-side timeout case. No secrets were recorded.

## Authoritative Sources Checked (2026-11-26)

| Source | URL | Notes |
|---|---|---|
| Docs index | https://platform.minimax.io/docs/llms.txt | Discovery index for public docs/API reference |
| Prerequisites | https://platform.minimax.io/docs/guides/quickstart-preparation.md | Base URLs, env vars, key types |
| Models intro | https://platform.minimax.io/docs/guides/models-intro.md | Model capabilities/context/features |
| Model list FAQ | https://platform.minimax.io/docs/faq/models.md | Available/retired models |
| OpenAI-compatible SDK/API | https://platform.minimax.io/docs/api-reference/text-openai-api.md | OpenAI base URL, auth, chat params, streaming, tool use, multimodal |
| Anthropic-compatible SDK/API | https://platform.minimax.io/docs/api-reference/text-anthropic-api.md | Anthropic base URL, auth, messages params, streaming, tool use |
| Chat Completions reference | https://platform.minimax.io/docs/api-reference/text-chat-openai.md | OpenAI chat endpoint behavior |
| Tool use / interleaved thinking | https://platform.minimax.io/docs/guides/text-m3-function-call.md | Tool-call flow, reasoning/tool_calls history requirements |
| Rate limits | https://platform.minimax.io/docs/guides/rate-limits.md | RPM/TPM/CONN/IPM limits |
| Error codes | https://platform.minimax.io/docs/api-reference/errorcode.md | Error semantics including auth/rate/quota/invalid params |
| M Plan intro | https://platform.minimax.io/docs/m-plan/intro.md | Plan tiers/models/usage framing |
| M Plan usage rules | https://platform.minimax.io/docs/m-plan/usage-rules.md | Quota windows, reset semantics, Credits/payg separation |
| M Plan FAQ | https://platform.minimax.io/docs/m-plan/faq.md | `token_plan/remains` endpoint, key separation, intended use, rate-limit vs usage limit |
| Pricing overview | https://platform.minimax.io/docs/pricing/overview.md | Pay-as-you-go pricing / priority tier |
| Model releases | https://platform.minimax.io/docs/release-notes/models.md | Recent model status/releases |
| OpenAI models list API | https://platform.minimax.io/docs/api-reference/models/openai/list-models.md | `GET /v1/models` |
| Anthropic models list API | https://platform.minimax.io/docs/api-reference/models/anthropic/list-models.md | `GET /anthropic/v1/models` |

## API Surface (Documented)

### Base URLs / Auth

| Interface | Base URL | Auth | Notes |
|---|---|---|---|
| OpenAI-compatible | `https://api.minimax.io/v1` | `Authorization: Bearer <API_KEY>` | Chat Completions, models list, OpenAI SDK compatible |
| Anthropic-compatible | `https://api.minimax.io/anthropic` | `x-api-key: <API_KEY>` (Anthropic SDK uses `ANTHROPIC_API_KEY`) | Messages API, models list, Anthropic SDK compatible |
| M Plan usage remains (documented in M Plan FAQ) | `https://www.minimax.io/v1/token_plan/remains` | `Authorization: Bearer <M Plan API key>` | Documented quota/usage endpoint; response schema not fully documented publicly |

Key types are documented as separate:
- **Pay-as-you-go API Key**: `platform.minimax.io` API Keys; billed against Open Platform balance.
- **M Plan Subscription Key**: Billing > M Plan; used for M Plan subscription/Credits in supported tools. Docs state M Plan key and pay-as-you-go API key are not interchangeable.

### Models and Capabilities

| Model ID | Context window | Modalities / capabilities (documented) | Thinking controls | Notes |
|---|---:|---|---|---|
| `MiniMax-M3.1-Flash` | 1,000,000 | Text, image input, video input, tool use, streaming; frontier multimodal coding model | `thinking.type`: `adaptive` default/on; `disabled` returns 400. `reasoning_effort`: `low/medium/high/xhigh/max`, default `max`; `none` unsupported. OpenAI: `reasoning_content`; Anthropic: thinking blocks. | Current flagship LLM in public docs; OpenAI/Anthropic compatible docs focus on M3.1-Flash/M3. |
| `MiniMax-M3` | 1,000,000 | Text, image input, video input, tool use, streaming; frontier multimodal coding model | `thinking` can be omitted/adaptive/disabled (disabled skips thinking for M3). | Documented alongside M3.1-Flash. |

Observed from OpenAI/Anthropic docs:
- **Streaming**: supported (`stream=true`).
- **Tool calling**: supported (`tools`, `tool_calls` / `tool_use`); full assistant message including thinking/reasoning content must be preserved in multi-turn tool loops.
- **Multimodal input limits** (M3.1-Flash/M3): images JPEG/PNG/GIF/WEBP up to 10MB URL/base64; videos MP4/AVI/MOV/MKV up to 50MB URL/base64, 512MB via Files API; up to 200 images and 20 videos per request; video duration >0 and <=30 min; sampled-frame and total-video-pixel limits apply.
- **Unsupported/ignored params**: `temperature` and `top_p` not effective (M3.1-Flash/M3 fixed behavior documented); OpenAI `n` only 1; some Anthropic/OpenAI params ignored as documented.

### Rate Limits (Documented)

From Rate Limits page (2026-11-26):

| Scope | Model/API | RPM | TPM | Other |
|---|---|---:|---:|---|
| LLM | `MiniMax-M3.1-Flash`, `MiniMax-M3` | 200 | 10,000,000 | - |

Additional rate-limit concepts: RPM and TPM; dynamic throttling may occur during peak traffic; business can contact `api@minimax.io` for higher limits.

### Error Semantics (Documented)

Selected relevant codes from Error Codes page:

| Code | Meaning | Relevance |
|---:|---|---|
| 1002 | rate limit | 429/rate-limit style condition |
| 1004 | not authorized / token not match group / cookie missing | auth failure |
| 1008 | insufficient balance | payg balance/billing |
| 1026/1027 | input/output sensitive | content policy |
| 1039 | token limit | context/token limit |
| 2013 | invalid parameters | request validation |
| 2049 | invalid API Key | auth/key invalid |
| 2056 | usage limit exceeded | plan/usage exhaustion |
| 2057 | not token plan token | wrong key type for token-plan endpoint |
| 2061 | current token plan not support model | plan/model entitlement |
| 2062 | M Plan rate limit reached; when querying usage may also mean no active M Plan subscription | M Plan rate/usage/subscription state |
| 2067 | usage limit reached and credit auto consumption off | M Plan/Credits behavior |
| 2068 | current plan tier does not support model | plan tier capability |

## Subscription Quota vs API Billing

### What docs say

- M Plan FAQ: M Plan key and standard API key are not interchangeable. M Plan key uses included M Plan quota and Credits; standard API key uses pay-as-you-go billing against account balance.
- M Plan usage rules: usage across MiniMax Code or other AI tools connected with M Plan API key counts toward same M Plan usage limits. 5-hour and weekly windows; image/video only weekly. Credits can cover supported overage after included quota; pay-as-you-go is separate and uses regular API key.
- M Plan FAQ: "Will my account balance be charged when I reach my usage limit? No..." for M Plan API key; balance charged only when using pay-as-you-go API key.
- M Plan intro: M Plan is subscription for individuals; tiers Go/Explore/Build.

### Assessment

**Docs strongly indicate** M Plan subscription quota and pay-as-you-go API billing are separate key/billing paths. **Not empirically proven here** because no M Plan subscription key/API key was available in environment, so no authenticated request could be issued and no before/after quota consumption could be measured.

### Required empirical proof (not executed)

Run with a user-owned M Plan key and record sanitized fixtures:

1. `GET https://www.minimax.io/v1/token_plan/remains` before request (M Plan key).
2. `POST https://api.minimax.io/v1/chat/completions` with minimal prompt/model allowed by plan.
3. `GET /v1/token_plan/remains` after request; compare quota/usage fields.
4. Repeat with pay-as-you-go key (separate credential) only if explicitly authorized; confirm balance/billing path differs and no M Plan quota mutation is assumed.

Repro command in this repo:

```bash
MINIMAX_API_KEY=*** python3 docs/fixtures/minimax/probe_minimax.py --out docs/fixtures/minimax
```

The probe deliberately skips authenticated tests when `MINIMAX_API_KEY` is unset and writes `authenticated_tests_skipped.json`.

## Quota / Usage Visibility

| Item | Documented evidence | Status |
|---|---|---|
| Quota endpoint | `GET https://www.minimax.io/v1/token_plan/remains` with `Authorization: Bearer <M Plan API key>` (M Plan FAQ) | Documented |
| CLI visibility | `mmx quota` (MiniMax CLI doc) | Documented |
| Console visibility | Plan usage page `https://platform.minimax.io/console/usage` | Documented |
| Quota windows | 5-hour + weekly for text/speech/other; image/video weekly only | Documented |
| Reset semantics | Window starts at first use; 5-hour lasts 5h, weekly 7d; full quota available after window ends; windows reset independently; unused quota does not carry over to next window/billing period | Documented |
| Credits | Included M Plan quota used first, then Credits for supported overage; Credits valid 1 year from purchase | Documented |
| Response fields for `token_plan/remains` | Not fully specified in public docs checked | **UNKNOWN** from docs |
| Polling limits / recommended poll interval | Not found in checked public docs | **UNKNOWN** |
| Empirical remains response | Not captured (no M Plan key) | **UNKNOWN** |

Observed bad-auth behavior against `token_plan/remains`: HTTP 200 with business error body `base_resp.status_code=1004` and login-fail message; see fixture. This is an observed behavior, not a documented schema contract.

## Observed Behavior Tests

Fixtures were generated on 2026-11-26 by `docs/fixtures/minimax/probe_minimax.py`. Secrets/volatile headers/request IDs are redacted by the probe.

| Test | Result | Fixture | Notes |
|---|---|---|---|
| Auth failure - OpenAI models, no auth | HTTP 401, `authorized_error`, message includes `(1004)` | [`openai_models_no_auth.json`](fixtures/minimax/openai_models_no_auth.json) | Documented endpoint requires auth. |
| Auth failure - OpenAI models, bad bearer | HTTP 401, `authorized_error`, `(1004)` | [`openai_models_bad_auth.json`](fixtures/minimax/openai_models_bad_auth.json) | Invalid key rejected. |
| Auth failure - Anthropic models, bad key | HTTP 401, `authentication_error` | [`anthropic_models_bad_auth.json`](fixtures/minimax/anthropic_models_bad_auth.json) | Anthropic-compatible endpoint expects `x-api-key`. |
| Auth failure - OpenAI chat completion, bad bearer | HTTP 401, `authorized_error`, `(1004)` | [`openai_chat_bad_auth.json`](fixtures/minimax/openai_chat_bad_auth.json) | Non-streaming request rejected before model processing. |
| Auth failure - Anthropic messages, bad key | HTTP 401, `authentication_error` | [`anthropic_messages_bad_auth.json`](fixtures/minimax/anthropic_messages_bad_auth.json) |  |
| Streaming request - OpenAI chat, bad bearer, `stream=true` | HTTP 401 JSON error (not SSE) | [`openai_chat_bad_auth_stream.json`](fixtures/minimax/openai_chat_bad_auth_stream.json) | **Authenticated successful streaming not tested**; bad-auth path returns JSON error. Doc example: [`openai_chat_stream_doc_example.json`](fixtures/minimax/openai_chat_stream_doc_example.json). |
| Timeout | Client-side timeout observed (`URLError timed out`, 0.001s socket timeout) | [`openai_chat_timeout.json`](fixtures/minimax/openai_chat_timeout.json) | Simulates local client timeout handling; not a provider-side timeout. |
| 429 / rate limit | **NOT REPRODUCED** | none | Would require valid credentials and rate pressure; docs list 1002/2062/1041/2045-2047 semantics. |
| Tool-call response | **NOT REPRODUCED with live API** | [`openai_tool_call_doc_example.json`](fixtures/minimax/openai_tool_call_doc_example.json) | Docs include tool-use schema/examples; authenticated execution not run. Fixture captures documented request/expected shape only. |
| Authenticated chat/stream/tool/models/remains | **SKIPPED** | [`authenticated_tests_skipped.json`](fixtures/minimax/authenticated_tests_skipped.json) | No `MINIMAX_API_KEY` in environment. |

## Gaps / Risks

- **No empirical subscription-quota consumption proof.** This is the main unresolved acceptance criterion; it is blocked on a real M Plan key and safe test procedure.
- **`token_plan/remains` response schema undocumented publicly.** Fields, units, and polling limits are unknown; treat as unstable until captured from authorized tests.
- **429 not observed.** Rate-limit behavior is documented but not empirically reproduced here.
- **Authenticated streaming/tool-call not observed.** Only doc examples and bad-auth behavior captured.
- **Models listing required auth in observed probes.** Both unauth and bad-auth OpenAI/Anthropic model list calls returned 401; docs do not prominently state this but behavior is consistent.
- **M Plan intended use constraint remains relevant.** Prior issue #2 recorded M Plan as individual interactive developer use; pay-as-you-go recommended for production. Any quota-router integration must respect that boundary.

## Reproduction

```bash
# From repo root
python3 docs/fixtures/minimax/probe_minimax.py --out docs/fixtures/minimax

# With credentials (user-owned; do not commit secrets)
MINIMAX_API_KEY=*** python3 docs/fixtures/minimax/probe_minimax.py --out docs/fixtures/minimax
```

The probe writes sanitized JSON fixtures and redacts secret-like keys, volatile headers, and request/trace IDs.

## Recommendation

Treat MiniMax as **GO WITH CONSTRAINTS for documented API integration**, but keep issue #3 acceptance partially open until an authorized M Plan credential is available for empirical quota-consumption proof. Do not claim subscription quota consumption without before/after `token_plan/remains` (or equivalent authoritative) evidence.
