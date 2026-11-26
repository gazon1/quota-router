# Subscription Terms and Permitted API Access Verification

Issue: [gazon1/quota-router#2 — [P0] Verify subscription terms and permitted API access](https://github.com/gazon1/quota-router/issues/2)  
Date checked: **2026-11-26**  
Scope: Verify whether MiniMax and Command Code subscriptions may be used through documented programmatic interfaces and a local proxy/controller. Public/authoritative documentation only. No private endpoints, reverse engineering, browser-session extraction, or HTML scraping.

## Acceptance Criteria Coverage

| Criterion | Status | Where |
|---|---|---|
| Record authoritative documentation/terms and date checked for each provider | Done | Provider sections below |
| Distinguish subscription entitlement from separately billed API credits | Done | Provider sections below |
| Identify supported authentication and token/key lifecycle | Done (with gaps noted) | Provider sections below |
| Explicit go/no-go decision per provider | Done | Decision sections below |
| No private endpoints, reverse engineering, browser-session extraction, or HTML scraping | Done | Only public docs/pages/API metadata used |
| If a subscription cannot be used through a documented API, stop that provider integration and revisit product scope before proceeding | N/A — both providers have documented programmatic interfaces; decisions are conditional GO with constraints | Decision sections below |

## Evidence Collection Notes

- Sources were fetched from public provider documentation/pages on 2026-11-26 using `curl` and direct page reads.
- `web_search` was unavailable in this environment; research used known public/authoritative URLs.
- Command Code docs are JS-rendered; readable content was extracted from cached HTML/RSC payloads of public pages. The Provider API page (`https://commandcode.ai/docs/provider`) was readable; the `/docs/provider-api` path returned an empty body during this check.
- MiniMax Terms of Service / user agreement pages checked during this session were not readable in an authoritative text form (JS-rendered/login redirect or moved page). MiniMax findings below are based on publicly accessible MiniMax platform documentation pages that were readable.
- This document is an evidence record, not legal advice. Product/legal review should confirm any production or multi-user deployment against current provider terms.

---

## MiniMax

### Authoritative Sources Checked (2026-11-26)

| Source | URL | Notes |
|---|---|---|
| API quickstart / prerequisites | https://platform.minimax.io/docs/guides/quickstart-preparation.md | Readable markdown doc. Describes pay-as-you-go API Key vs M Plan Subscription Key, base URLs, env vars. |
| M Plan overview | https://platform.minimax.io/docs/m-plan/intro.md | Readable markdown doc. M Plan tiers/models; M Plan API key for other AI tools. |
| M Plan usage rules | https://platform.minimax.io/docs/m-plan/usage-rules.md | Readable markdown doc. Usage windows, reset, Credits, pay-as-you-go separation. |
| M Plan FAQ | https://platform.minimax.io/docs/m-plan/faq.md | Readable markdown doc. Key interchangeability, intended use, multi-tool usage. |
| Pricing overview | https://platform.minimax.io/docs/pricing/overview.md | Readable markdown doc. Pay-as-you-go vs packages/pricing structure. |
| Terms of Service page (attempted) | https://platform.minimax.io/user-center/payment/terms-of-service | JS-rendered console shell; readable terms text not extracted in this session. |
| Platform user agreement (attempted) | https://www.minimaxi.com/en/document/platform-user-agreement | 301/redirect to moved page; readable text not extracted in this session. |

### Subscription Entitlement vs Separately Billed API Credits

- **M Plan Subscription Key** is separate from pay-as-you-go **API Key**. The docs state they are not interchangeable.
  - Quickstart: “Pay-as-you-go：Visit API Keys > Create new secret key to get your **API Key** … M Plan：Visit Billing > M Plan to view your **Subscription Key** …”
  - M Plan FAQ: “M Plan API key vs standard API Key not interchangeable…”
  - M Plan usage rules: “For resources outside your plan or for production workloads, use a regular pay-as-you-go API Key. Charges are deducted from your Open Platform account balance. This Key is separate from your M Plan API key and cannot be used interchangeably with it.”
- **M Plan quota** is consumed by usage across supported tools connected with the M Plan API key. Usage rules: “Whether you use MiniMax Code or other AI tools connected with your M Plan API key, it all counts toward the same M Plan usage limits.”
- **Credits** are separately purchasable/available resources. Usage rules: “Use purchased Credits: For models and capabilities supported by Credits, the system uses your included M Plan quota first, then deducts any overage from your available Credits.”
- **Pay-as-you-go** is separately billed against Open Platform account balance and uses a different API key.

### Supported Authentication and Token/Key Lifecycle

- **Authentication method (documented):** API key / Subscription key passed via SDK/environment configuration.
  - Quickstart documents env vars: `ANTHROPIC_BASE_URL=https://api.minimax.io/anthropic`, `ANTHROPIC_API_KEY=<YOUR_API_KEY>`; `OPENAI_BASE_URL=https://api.minimax.io/v1`, `OPENAI_API_KEY=<YOUR_API_KEY>`; `MINIMAX_API_KEY=<YOUR_API_KEY>`. M Plan users use their Subscription Key as `<YOUR_API_KEY>` in those integrations.
- **Documented programmatic interfaces:** Anthropic-compatible and OpenAI-compatible API endpoints/base URLs are documented. M Plan FAQ states M Plan can be used in supported AI tools and usage is shared across tools.
- **Key lifecycle (partially documented):** Keys are obtained/managed in MiniMax platform user center (API Keys / Billing > M Plan). The checked public docs do not provide a complete public key-lifecycle spec (rotation, expiry, revocation semantics, per-key scopes) beyond platform management locations. **Gap:** verify key rotation/revocation/scope details in authoritative account/terms docs before productionizing key storage.

### Usage Restrictions, Rate/Quota Limits, and Billing/Consumption

- **Usage windows:** M Plan usage rules document 5-hour and weekly windows. Text/speech/other models require remaining quota in both windows; image/video subject only to weekly window. Unused quota does not carry over.
- **Intended use:** M Plan FAQ states M Plan is for individual interactive developer use; pay-as-you-go recommended for production. This is a material restriction for a local proxy/controller product.
- **Consumption:** Requests made through supported tools with M Plan API key count toward the same M Plan usage limits. Purchased Credits may cover supported overage after included quota; pay-as-you-go is separate.
- **Rate limits:** Specific numeric per-minute/per-request rate limits were not extracted from the checked public docs in this session. **Gap:** confirm current rate-limit/API throttling behavior in authoritative docs/console before implementation.

### Local Proxy/Controller Assessment

- **Technically compatible with documented interfaces:** A local proxy/controller can call MiniMax's documented Anthropic/OpenAI-compatible endpoints using a user-provided key. This is a documented programmatic interface pattern.
- **Policy/terms constraint:** M Plan is documented as “for individual interactive developer use”; pay-as-you-go is recommended for production. A local proxy/controller used by the same individual for interactive development may fit the documented tool-use model, but any production service, shared/multi-user access, resale, automation beyond individual interactive use, or workload requiring production billing/SLAs should use pay-as-you-go API keys and be reviewed against current MiniMax terms.
- **No private/undocumented access required:** No evidence suggests private endpoints, browser-session extraction, or reverse engineering are needed for the documented Anthropic/OpenAI-compatible API path.

### Decision

**GO WITH CONSTRAINTS — MiniMax (documented programmatic API path only).**

Rationale:
- MiniMax documents Anthropic/OpenAI-compatible programmatic endpoints and M Plan API key use in supported AI tools.
- M Plan requests consume M Plan quota; M Plan key and pay-as-you-go API key are explicitly separate and not interchangeable.
- The M Plan intended-use restriction (“individual interactive developer use; pay-as-you-go recommended for production”) means a local proxy/controller is acceptable only for personal/individual interactive developer use and must not be positioned as production/service-bureau/multi-user subscription pooling.

Required constraints before integration:
1. Use only documented MiniMax API endpoints/SDK auth (`api.minimax.io/anthropic`, `api.minimax.io/v1`) with user-provided keys.
2. Keep M Plan Subscription Key and pay-as-you-go API Key as distinct credential types; never substitute one for the other.
3. Do not market or implement M Plan routing as production, shared, or third-party service access. Route production/service workloads to pay-as-you-go keys.
4. Obtain and record readable MiniMax Terms of Service / platform agreement before launch; this check did not extract a readable authoritative ToS text.
5. Confirm current rate limits, key lifecycle (rotation/revocation/expiry/scope), and any automation restrictions in authoritative MiniMax terms/docs before productionizing.

---

## Command Code

### Authoritative Sources Checked (2026-11-26)

| Source | URL | Notes |
|---|---|---|
| Pricing | https://commandcode.ai/pricing | Public pricing page. API access/credits/plan comparison. |
| Terms & Conditions | https://commandcode.ai/terms | Public terms page; “Last updated: October 29 2026” (as displayed on page). JS-rendered; text extracted from HTML. |
| Pricing & Limits docs | https://commandcode.ai/docs/resources/pricing-limits | Public docs page. Plan limits, rolling windows, credits, API access notes. |
| Provider API docs | https://commandcode.ai/docs/provider | Public docs page. Endpoints, auth, key usage, plan/credit behavior, compatible formats. |
| GOAT plan docs | https://commandcode.ai/docs/plans/goat | Public docs page. GOAT plan API access/limits. |
| MOAT docs | https://commandcode.ai/docs/moat | Public docs page. MOAT membership/discounted credit use in CLI or OpenAI/Anthropic client. |
| Provider marketing page | https://commandcode.ai/provider | Public page. Provider API positioning/endpoints. |
| Provider API path attempt | https://commandcode.ai/docs/provider-api | Returned empty body during this check; used `/docs/provider` instead. |

### Subscription Entitlement vs Separately Billed API Credits

- **Plan subscription/allowance** is distinct from **on-demand/purchased credits**.
  - Pricing/limits docs: monthly subscription credits reset by cycle; on-demand credits/top-ups can be bought at model cost, roll over, never expire; rolling limits throttle included monthly credits, on-demand credits are not throttled in the same way.
  - Terms: “Our subscriptions are sold on the basis of underlying API costs…” and “Any additional credits, bonus credits, promotional credits, or top-up credits … (‘Additional Credits’) are provided strictly on a ‘while supplies last’ basis…” Additional Credits have no cash value and are non-refundable/non-transferable per terms.
- **Provider API billing/consumption:** Provider docs state “Pay as you go from credits. Buy them from Studio > Billing… Requests bill at the underlying API rates, no markup.” Also “Plans from GOAT up also include API access, and your plan allowance pays list price. Go uses it with credits: the Go allowance never pays an API request.”
- **Plan access:** Pricing states “Every plan except Go includes API access; Go uses it with credits.” Provider docs state “Plans from GOAT up also include API access, and your plan allowance pays list price. Go uses it with credits: the Go allowance never pays an API request.” Provider marketing/docs also describe Go as using API access with credits while higher plans include API access.

### Supported Authentication and Token/Key Lifecycle

- **Authentication method (documented):** Bearer API key.
  - Provider docs: `Authorization: Bearer <CMD_API_KEY>`.
  - Same key for CLI and API: “The same key authenticates the CLI and the API. Create one from your API keys page in Studio.”
- **Documented programmatic interfaces:** OpenAI-compatible and Anthropic-compatible endpoints.
  - `POST https://api.commandcode.ai/provider/v1/chat/completions` (OpenAI Chat Completions)
  - `POST https://api.commandcode.ai/provider/v1/responses` (OpenAI Responses)
  - `POST https://api.commandcode.ai/provider/v1/messages` (Anthropic Messages)
  - `GET https://api.commandcode.ai/provider/v1/models` (Models list)
  - `GET https://api.commandcode.ai/provider/v1/user/usage/limits` (usage/limits)
- **Key lifecycle (partially documented):** Keys are created in Studio API keys page and reused across CLI/API. The checked public pages did not expose a complete key lifecycle policy (expiry, rotation schedule, revocation SLA/scope, per-key permissions). **Gap:** confirm key revocation/rotation/scope/audit controls in Studio/authoritative docs before productionizing.

### Usage Restrictions, Rate/Quota Limits, and Billing/Consumption

- **Usage limits:** Pricing & Limits docs document rolling 5-hour and weekly limits on included monthly credits; monthly credits reset by billing cycle. On-demand credits are not throttled by those rolling window checks as described; requests over window may spend on-demand credits first.
- **Provider API rates:** Provider docs market “Best-in-class speed and performance, with rate limits built for production, not demos.” This is not a concrete numeric rate-limit contract in the checked docs. **Gap:** confirm concrete rate limits / quotas / burst policy for Provider API before productionizing.
- **Terms restrictions relevant to proxy/controller use:**
  - Acceptable Use: “Automated tools, such as bots or scripts, must not be used to create multiple accounts, perform automated searches or requests, or extract data from the Company's website.” (abuse/multi-account/scraping context)
  - Acceptable Use: “You must adhere to the Company's usage guidelines and limits. Each person may hold only one account, and creating or using multiple accounts—including to obtain extra plans, credits, or deals, or to circumvent these restrictions—is prohibited…” 
  - Acceptable Use: “You may not rent, lease, sell, or transfer access to the Company's website, software, or documentation to a third party.”
  - Usage Restrictions: “Sublicense, resell, rent, lease, transfer, or assign the services to any third party.” “Use the services for time sharing or service bureau purposes or for the benefit of a third party.”
  - Reverse engineering/disassembly/source-code discovery prohibited.
- **Data/ZDR caveat:** Provider page states ZDR can be requested via `x-cmd-zdr: 1` and that some models/providers may have different retention/training terms; request may fail when ZDR cannot be enforced depending on configuration. This is relevant for compliance but not a permission blocker for documented API use.

### Local Proxy/Controller Assessment

- **Documented programmatic access exists:** Command Code explicitly documents a Provider API with OpenAI/Anthropic-compatible endpoints and API key authentication. A local proxy/controller calling these documented endpoints with the user's own key is a normal API-client pattern.
- **No private/undocumented access required:** The documented Provider API covers the needed programmatic interface; no browser-session extraction, private endpoints, reverse engineering, or HTML scraping is required.
- **Terms boundary:** Terms prohibit service bureau/time-sharing/third-party benefit, resale/transfer/assignment, and circumventing usage limits or using multiple accounts. A local controller for the same authenticated user/account is not the same as a service bureau, but any shared proxy, multi-tenant routing, account pooling, resale, or limit circumvention would violate terms.
- **Plan/credit semantics:** Go plan can access API with credits; GOAT/Pro/Max/Team include API access metered against plan credits/access rules. The proxy/controller must preserve plan/credit attribution and not obscure account identity.

### Decision

**GO WITH CONSTRAINTS — Command Code (documented Provider API only).**

Rationale:
- Command Code provides a documented Provider API with Bearer key auth and OpenAI/Anthropic-compatible endpoints.
- Plan allowances and purchased/on-demand credits are distinguished in docs/terms; requests consume credits/allowances according to plan rules.
- Terms allow documented API use but prohibit resale/transfer/service bureau/time-sharing/third-party benefit, multiple accounts, and circumventing usage limits.

Required constraints before integration:
1. Use only documented Provider API endpoints and user-owned API keys created in Studio; do not scrape, reverse engineer, or extract browser sessions.
2. Keep the integration single-account, user-controlled. Do not provide shared proxying, multi-tenant account pooling, resale, service bureau, or third-party-benefit access.
3. Preserve plan/credit semantics: distinguish Go/API-with-credits, GOAT/Pro/Max/Team included access, and separately purchased/on-demand credits.
4. Enforce/observe Command Code usage windows and limits; confirm concrete Provider API rate-limit contract before productionizing.
5. Confirm key lifecycle controls (create/revoke/rotate/scope) and current terms before launch; terms were JS-rendered and extracted from public HTML on 2026-11-26.

---

## Final Go/No-Go Summary

| Provider | Decision | Documented programmatic API | Subscription vs credits separated | Auth/key lifecycle identified | Local proxy/controller | Blockers / required constraints |
|---|---|---|---|---|---|---|
| MiniMax | **GO WITH CONSTRAINTS** | Yes — Anthropic/OpenAI-compatible endpoints documented | Yes — M Plan Subscription Key/quota distinct from pay-as-you-go API Key and purchased Credits | Partially — key types/env vars/platform locations documented; lifecycle details incomplete publicly | Allowed only for personal/individual interactive developer use via documented APIs; not production/service/multi-user | Readable ToS not extracted; confirm rate limits, key lifecycle, automation/production terms before launch |
| Command Code | **GO WITH CONSTRAINTS** | Yes — Provider API with OpenAI/Anthropic-compatible endpoints | Yes — plan allowance vs on-demand/Additional Credits distinguished | Partially — Bearer key, same CLI/API key, Studio key creation documented; lifecycle details incomplete publicly | Allowed for same-user local controller via documented API; no service bureau/resale/third-party benefit/multi-account/circumvention | JS-rendered terms extracted; confirm concrete rate limits, key lifecycle, and any plan-specific API contract before launch |

## Recommended Next Step

Proceed only with a **user-owned, single-account, documented-API** integration design. Before any implementation that routes subscription traffic:

1. Add explicit credential-type modeling (`minimax_m_plan_key`, `minimax_payg_api_key`, `commandcode_api_key`, plan/credit metadata).
2. Add policy guards in product/docs: no account pooling, no resale/service bureau, no third-party benefit, no limit circumvention.
3. Capture refreshed authoritative terms snapshots/links during implementation and legal review, especially MiniMax readable ToS and Command Code current Terms.
4. Revisit product scope if either provider's current terms disallow the intended proxy/controller deployment model.
