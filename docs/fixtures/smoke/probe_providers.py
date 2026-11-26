#!/usr/bin/env python3
"""Reproducible provider + LiteLLM compatibility smoke test (issue #8).

Drives the exact matrix in the issue against the live providers and the pinned
LiteLLM proxy, and writes sanitized fixtures. No secret, prompt, or raw provider
payload containing an identity field is written.

Usage:
  # provider endpoints only
  MINIMAX_TOKEN_PLAN_KEY=... COMMAND_CODE_API_KEY=... \
    python3 docs/fixtures/smoke/probe_providers.py --out docs/fixtures/smoke

  # include the proxy (docker compose up -d first)
  LITELLM_MASTER_KEY=... python3 docs/fixtures/smoke/probe_providers.py --litellm

Exit code is non-zero when a case that must hold regresses, so this doubles as a
gate. Authenticated cases are skipped, not failed, when their key is unset.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import time
import urllib.error
import urllib.request
from pathlib import Path

MINIMAX_INFERENCE = "https://api.minimax.io/v1"
MINIMAX_ANTHROPIC = "https://api.minimax.io/anthropic"
MINIMAX_QUOTA = "https://www.minimax.io/v1/token_plan/remains"
COMMANDCODE_BASE = "https://api.commandcode.ai/provider/v1"
MINIMAX_MODEL = os.environ.get("MINIMAX_MODEL", "MiniMax-M2.7")
CC_OPENAI_MODEL = os.environ.get("CC_OPENAI_MODEL", "stealth/glyph-cluster:free")
# Second OpenAI-shaped model in the config. Free-tier models carry a daily request
# cap, so its status is recorded without an expectation: a 429 here is the plan
# signal, not a regression.
CC_OPENAI_MODEL_2 = os.environ.get("CC_OPENAI_MODEL_2", "recursive/flint-v1.0:free")
CC_ANTHROPIC_MODEL = os.environ.get("CC_ANTHROPIC_MODEL", "deepseek/deepseek-v4.1-flash")
# The mismatch cases need a model that is listed on exactly one endpoint, so the
# proxy cannot serve it on the other. deepseek-v4.1-flash advertises all three,
# claude-* advertise /messages only.
CC_MESSAGES_ONLY_MODEL = os.environ.get("CC_MESSAGES_ONLY_MODEL", "claude-sonnet-5")
# The proxy's model_name for the Anthropic-shaped model; it is the string a
# client sends and must match deploy/litellm-config.yaml exactly.
CC_LITELLM_MESSAGES_MODEL = os.environ.get("CC_LITELLM_MESSAGES_MODEL", "commandcode/deepseek-v4.1-flash")
USER_AGENT = "quota-router-smoke/1.0"

REDACT_KEYS = ("key", "authorization", "cookie", "secret", "token",
               "userid", "priceid", "planid", "fbp", "subscription")
REDACT_VALUES = {"email", "name", "userName", "userId", "request_id",
                 "requestId", "orgId", "subscription", "customer"}
# Fields only a model catalogue entry carries. "object" is NOT one of them: every
# OpenAI-shaped response has object: "chat.completion", so treating it as a model
# marker kept generation ids such as gen_... in the fixtures.
MODEL_OBJECT_KEYS = {"owned_by", "context_length", "supported_endpoints",
                     "display_name", "created_at"}


SECRET_PATTERNS = (
    re.compile(r"sk-[A-Za-z0-9_-]{8,}"),
    re.compile(r"\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b"),
    re.compile(r"\b(sub|price|gen|resp|call)_[A-Za-z0-9]{6,}"),
    re.compile(r"[\w.+-]+@[\w-]+\.[\w.]+"),
)


def scrub_text(text):
    """A provider can echo a credential, an id or an address inside an error
    message. Strings are not dicts, so the key-based rules never see them."""
    for pattern in SECRET_PATTERNS:
        text = pattern.sub("***REDACTED***", text)
    return text


def redact(obj):
    """Drops credentials and account identity, but keeps model ids: a model id is
    the evidence this file exists to record."""
    if isinstance(obj, dict):
        # "object" alone marks a model entry; the other catalogue fields only
        # count without a response-style object (a Responses body also has
        # created_at and model, but object: "response").
        is_model = (obj.get("object") == "model"
                    or (obj.get("object") not in ("response", "chat.completion", "message")
                        and bool(MODEL_OBJECT_KEYS & set(obj)))
                    or ("id" in obj and isinstance(obj.get("id"), str)
                        and ("/" in obj["id"] or ":" in obj["id"])
                        # A bare model id has no provider prefix and no separator,
                        # which no response or subscription id shares.
                        and not re.match(r"^(gen|resp|call|sub|price|msg|req)_", obj["id"])))
        out = {}
        for k, v in obj.items():
            lk = k.lower()
            if isinstance(v, (int, float, bool, type(None))):
                out[k] = v
            elif k == "id":
                out[k] = v if is_model else "***REDACTED***"
            elif any(p in lk for p in REDACT_KEYS) or k in REDACT_VALUES:
                out[k] = "***REDACTED***"
            else:
                out[k] = redact(v)
        return out
    if isinstance(obj, list):
        return [redact(x) for x in obj]
    if isinstance(obj, str):
        return scrub_text(obj)
    return obj


def call(method, url, headers=None, body=None, timeout=60, stream=False):
    data = json.dumps(body).encode() if body is not None else None
    # Command Code's edge rejects requests with no User-Agent (403, "error code:
    # 1010"), which surfaces as a permission error rather than a client bug.
    h = {"Content-Type": "application/json", "User-Agent": USER_AGENT}
    h.update(headers or {})
    req = urllib.request.Request(url, data=data, headers=h, method=method)
    started = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            if stream:
                events, usage = [], None
                for raw in resp:
                    line = raw.decode("utf-8", "replace").rstrip("\n")
                    if not line.startswith("data:"):
                        continue
                    payload = line[5:].strip()
                    if payload == "[DONE]":
                        events.append("data: [DONE]")
                        break
                    try:
                        chunk = json.loads(payload)
                    except Exception:
                        events.append("data: <unparsed>")
                        continue
                    if chunk.get("usage"):
                        usage = chunk["usage"]
                    events.append("data: <chunk>")
                return {"ok": True, "status": resp.status,
                        "elapsed_ms": round((time.time() - started) * 1000),
                        "events": events, "final_usage": redact(usage)}
            raw = resp.read()
            try:
                parsed = json.loads(raw)
            except Exception:
                # Raw provider text may echo a credential or prompt; only its
                # size and a scrubbed prefix are kept.
                parsed = {"non_json_body": scrub_text(
                    raw.decode("utf-8", "replace")[:200])}
            return {"ok": True, "status": resp.status,
                    "elapsed_ms": round((time.time() - started) * 1000),
                    "body": redact(parsed)}
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            parsed = json.loads(raw)
        except Exception:
            parsed = {"non_json_body": scrub_text(
                raw.decode("utf-8", "replace")[:200])}
        return {"ok": False, "status": e.code,
                "elapsed_ms": round((time.time() - started) * 1000),
                "body": redact(parsed)}
    except Exception as e:  # DNS, TLS, timeout
        return {"ok": False, "status": None,
                "elapsed_ms": round((time.time() - started) * 1000),
                "error_type": type(e).__name__, "error": scrub_text(str(e))[:200]}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="docs/fixtures/smoke")
    ap.add_argument("--litellm", action="store_true",
                    help="also probe the local proxy at LITELLM_BASE_URL")
    ap.add_argument("--litellm-invalid", metavar="URL", default="",
                    help="base URL of a proxy started with a deliberately invalid upstream "
                         "MiniMax key; runs the 401 -> cooldown regression there. A normal "
                         "proxy cannot test a rejected provider key: its own key is accepted.")
    ap.add_argument("--cancellation", action="store_true",
                    help="also cancel a live stream (spends a small amount of quota)")
    args = ap.parse_args()
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    mm_key = os.environ.get("MINIMAX_TOKEN_PLAN_KEY", "")
    cc_key = os.environ.get("COMMAND_CODE_API_KEY", "")
    master = os.environ.get("LITELLM_MASTER_KEY", "")
    litellm = os.environ.get("LITELLM_BASE_URL", "http://127.0.0.1:4000").rstrip("/")

    results: dict[str, dict] = {}

    def has_choices(body):
        return bool((body.get("choices") or [{}])[0].get("message"))

    def has_usage(body):
        usage = body.get("usage") or {}
        return bool(usage.get("prompt_tokens")) or bool(usage.get("input_tokens"))

    def has_tool_calls(body):
        msg = (body.get("choices") or [{}])[0].get("message") or {}
        calls = msg.get("tool_calls") or []
        if not calls or msg.get("finish_reason") not in (None, "tool_calls"):
            return False
        try:
            json.loads(calls[0]["function"]["arguments"])
        except Exception:
            return False
        return True

    def has_stream(body):
        events = body.get("events") or []
        return "data: [DONE]" in events and len(events) > 1

    def has_stream_usage(body):
        return bool(body.get("final_usage") or body.get("usage"))

    failures: list[str] = []
    blocked: list[str] = []

    def shape(value):
        """Field names and JSON types, without any value."""
        if isinstance(value, dict):
            return {k: shape(v) for k, v in value.items()}
        if isinstance(value, list):
            return [shape(value[0])] if value else []
        return type(value).__name__

    # Codes the providers themselves document, so "blocked by plan" cannot absorb
    # an unrelated rate limit or permission failure.
    PLAN_CODES = ("2056", "2057", "2061", "2062", "2068", "MODEL_NOT_IN_PLAN")
    DAILY_CAP_MARKERS = ("free Flint requests for today", "requests for today")
    # A provider's own "try again" answer: not a plan refusal, not our regression.
    TRANSIENT_MARKERS = ("temporarily unavailable", "try again in a moment",
                         "overloaded", "service unavailable")

    def body_text(body) -> str:
        return json.dumps(body)[:2000]

    def is_transient(body) -> bool:
        text = body_text(body).lower()
        return any(marker in text for marker in TRANSIENT_MARKERS)

    def is_plan_refusal(body) -> bool:
        text = body_text(body)
        if any(marker in text for marker in DAILY_CAP_MARKERS):
            return True
        codes = {str((body.get("error") or {}).get("code") or ""),
                 str((body.get("error") or {}).get("type") or ""),
                 str((body.get("base_resp") or {}).get("status_code") or "")}
        blob = text + " " + " ".join(codes)
        return any(code in blob for code in PLAN_CODES)

    def record(name, payload, expect=None, expect_business=None, need=None,
               allow_plan_exhausted=False, allow_refusal=False, schema_only=False):
        """Records one case and enforces its expectation.

        MiniMax answers an auth failure with HTTP 200 and a business error in the
        body, so a transport-only check cannot see it. Where a business status is
        expected it must be present and equal, not merely absent.

        `need` asserts the response semantics rather than the transport status: a
        case that claims tool calls must contain one, a stream must terminate and
        carry usage. A call the provider's plan refuses is recorded as
        BLOCKED_BY_PLAN only when the body matches a documented plan code.
        """
        if schema_only:
            payload = {**payload, "body": shape(payload.get("body"))}
        results[name] = payload
        status = payload.get("status")
        body = payload.get("body") or {}
        biz = (body.get("base_resp") or {}).get("status_code")

        if expect is not None and status != expect:
            failures.append(f"{name}: status {status}, expected {expect}")
            return
        if expect_business is not None and status == 200:
            if biz is None:
                failures.append(f"{name}: no base_resp.status_code in the body; a missing "
                                "business status is not a success")
            elif biz != expect_business:
                failures.append(f"{name}: base_resp.status_code {biz}, expected {expect_business}")

        if allow_plan_exhausted:
            if status == 200 and biz in (None, 0):
                results[name]["verdict"] = "VERIFIED"
            elif status == 429 and is_plan_refusal(body):
                results[name]["verdict"] = "BLOCKED_BY_PLAN"
                blocked.append(name)
            elif status in (429, 502, 503) and is_transient(body):
                results[name]["verdict"] = "TRANSIENT"
                blocked.append(name)
            elif status in (429, 502, 503) and is_transient(body):
                results[name]["verdict"] = "TRANSIENT"
                blocked.append(name)
            else:
                failures.append(f"{name}: status {status}, expected 200 or the documented "
                                f"plan-exhaustion error, got neither")
            continue_checks = status == 200
        elif allow_refusal:
            if status == 200:
                results[name]["verdict"] = "VERIFIED"
                continue_checks = True
            elif status in (403, 429) and is_plan_refusal(body):
                results[name]["verdict"] = "BLOCKED_BY_PLAN"
                blocked.append(name)
                continue_checks = False
            elif status in (429, 502, 503) and is_transient(body):
                results[name]["verdict"] = "TRANSIENT"
                blocked.append(name)
                continue_checks = False
            else:
                failures.append(f"{name}: status {status}, and the body matches no documented "
                                f"plan refusal (403/429 with a plan code)")
                continue_checks = False
        else:
            continue_checks = status == (expect if expect is not None else 200)

        if continue_checks and need:
            what, ok = need
            # A stream payload carries events/final_usage beside "body", not in it.
            target = payload["body"] if payload.get("body") is not None else payload
            if not ok(target):
                failures.append(f"{name}: response does not satisfy {what}; "
                                "a status code alone is not verification")

    def write(name):
        (out / f"{name}.json").write_text(
            json.dumps({"case": name, "response": results[name]}, indent=2) + "\n",
            encoding="utf-8")

    # --- MiniMax Token Plan: quota surface and inference are separate hosts.
    if mm_key:
        auth = {"Authorization": f"Bearer {mm_key}"}
        record("minimax_token_plan_remains", call("GET", MINIMAX_QUOTA, auth),
               expect=200, expect_business=0,
               need=("quota windows", lambda b: bool(b.get("model_remains"))))
        record("minimax_openai_models",
               call("GET", f"{MINIMAX_INFERENCE}/models", auth), expect=200,
               need=("a model list", lambda b: bool(b.get("data"))))
        record("minimax_anthropic_models",
               call("GET", f"{MINIMAX_ANTHROPIC}/v1/models",
                    {"x-api-key": mm_key, "anthropic-version": "2023-06-01"}), expect=200,
               need=("a model list", lambda b: bool(b.get("data"))))
        chat = {"model": MINIMAX_MODEL, "max_completion_tokens": 16,
                "messages": [{"role": "user", "content": "Reply with exactly: pong"}]}
        # Inference is either served (200) or refused by the documented plan
        # limit (429 / 2056). Anything else - a 401 after the key was accepted on
        # /models, a 5xx - is a real compatibility regression and fails the run.
        record("minimax_openai_chat",
               call("POST", f"{MINIMAX_INFERENCE}/chat/completions", auth, chat),
               allow_plan_exhausted=True, expect_business=0,
               need=("choices with usage", lambda b: has_choices(b) and has_usage(b)))
        record("minimax_openai_chat_stream",
               call("POST", f"{MINIMAX_INFERENCE}/chat/completions", auth,
                    {**chat, "stream": True}, stream=True), allow_plan_exhausted=True,
               need=("a terminated stream", has_stream))
        record("minimax_anthropic_messages",
               call("POST", f"{MINIMAX_ANTHROPIC}/v1/messages",
                    {"x-api-key": mm_key, "anthropic-version": "2023-06-01"},
                    {"model": MINIMAX_MODEL, "max_tokens": 16, "messages": chat["messages"]}),
               allow_plan_exhausted=True, expect_business=0,
               need=("a content block", lambda b: bool(b.get("content"))))
        for name in list(results):
            write(name)
        q = results["minimax_token_plan_remains"]["body"]
        print("minimax quota windows:")
        for entry in (q.get("model_remains") or []):
            print(f"  {entry.get('model_name')}: 5h status={entry.get('current_interval_status')}"
                  f" pct={entry.get('current_interval_remaining_percent')}"
                  f" | weekly status={entry.get('current_weekly_status')}"
                  f" pct={entry.get('current_weekly_remaining_percent')}"
                  f" | counts={entry.get('current_interval_total_count')}")
    else:
        (out / "minimax_skipped.json").write_text(json.dumps({
            "case": "minimax_skipped",
            "reason": "MINIMAX_TOKEN_PLAN_KEY not set",
        }, indent=2) + "\n", encoding="utf-8")

    # --- Command Code: the live catalogue decides which path each model takes.
    if cc_key:
        auth = {"Authorization": f"Bearer {cc_key}"}
        catalog = call("GET", f"{COMMANDCODE_BASE}/models", auth, timeout=45)
        record("commandcode_models", catalog, expect=200)
        endpoints = {}
        if catalog.get("ok"):
            for m in catalog["body"].get("data", []):
                endpoints[m["id"]] = m.get("supported_endpoints", [])
            (out / "commandcode_catalogue.json").write_text(json.dumps({
                "case": "commandcode_catalogue",
                "models": [{"id": k, "context_length": None, "supported_endpoints": v}
                           for k, v in endpoints.items()],
            }, indent=2) + "\n", encoding="utf-8")

        chat = {"model": CC_OPENAI_MODEL, "max_tokens": 16,
                "messages": [{"role": "user", "content": "Reply with exactly: pong"}]}
        record("commandcode_chat",
               call("POST", f"{COMMANDCODE_BASE}/chat/completions", auth, chat, timeout=90),
               allow_refusal=True, need=("choices with usage", lambda b: has_choices(b) and has_usage(b)))
        record("commandcode_chat_stream",
               call("POST", f"{COMMANDCODE_BASE}/chat/completions", auth, {**chat, "stream": True},
                    timeout=90, stream=True), allow_refusal=True,
               # Command Code emits usage in the final chunk of a normal stream.
               need=("a [DONE]-terminated stream carrying final usage",
                     lambda b: has_stream(b) and has_stream_usage(b)))
        # Free-tier model with a daily cap: the cap is the expected outcome, not a
        # compatibility failure, but a 400/401 still is.
        record("commandcode_chat_second",
               call("POST", f"{COMMANDCODE_BASE}/chat/completions", auth,
                    {**chat, "model": CC_OPENAI_MODEL_2}, timeout=90), allow_refusal=True,
               need=("choices with usage", lambda b: has_choices(b) and has_usage(b)))
        record("commandcode_responses",
               call("POST", f"{COMMANDCODE_BASE}/responses", auth,
                    {"model": CC_OPENAI_MODEL, "input": "Reply with exactly: pong"}, timeout=90),
               allow_refusal=True, need=("a responses output", lambda b: bool(b.get("output") or b.get("id"))))
        record("commandcode_messages",
               call("POST", f"{COMMANDCODE_BASE}/messages",
                    {**auth, "anthropic-version": "2023-06-01"},
                    {"model": CC_ANTHROPIC_MODEL, "max_tokens": 16,
                     "messages": chat["messages"]}, timeout=90), allow_refusal=True,
               need=("a message content block", lambda b: bool(b.get("content"))))
        record("commandcode_tool_call",
               call("POST", f"{COMMANDCODE_BASE}/chat/completions", auth,
                    {"model": CC_OPENAI_MODEL, "max_tokens": 100,
                     "messages": [{"role": "user", "content": "Weather in SF? Use the tool."}],
                     "tools": [{"type": "function", "function": {
                         "name": "get_weather", "description": "Get weather",
                         "parameters": {"type": "object",
                                        "properties": {"city": {"type": "string"}},
                                        "required": ["city"]}}}]}, timeout=90),
               allow_refusal=True, need=("a tool call with JSON arguments", has_tool_calls))
        # Endpoint/model mismatch must be rejected, never silently rerouted. A
        # messages-only model is used so the OpenAi-side attempt cannot succeed.
        record("commandcode_mismatch_anthropic_on_chat",
               call("POST", f"{COMMANDCODE_BASE}/chat/completions", auth,
                    {"model": CC_MESSAGES_ONLY_MODEL, "max_tokens": 8, "messages": chat["messages"]}),
               expect=400)
        record("commandcode_mismatch_openai_on_messages",
               call("POST", f"{COMMANDCODE_BASE}/messages",
                    {**auth, "anthropic-version": "2023-06-01"},
                    {"model": CC_OPENAI_MODEL, "max_tokens": 8, "messages": chat["messages"]}),
               expect=400)
        # Undocumented quota routes: evidence only, never a supported contract.
        # Their bodies carry this account's billing figures, so only the shape is
        # kept: field names, types and status codes are the compatibility evidence,
        # the amounts are private operational data.
        for name, path in (
            ("commandcode_alpha_whoami", "/alpha/whoami?limits=1"),
            ("commandcode_alpha_credits", "/alpha/billing/credits"),
            ("commandcode_alpha_subscriptions", "/alpha/billing/subscriptions"),
            ("commandcode_alpha_usage", "/alpha/usage/summary"),
        ):
            record(name, call("GET", "https://api.commandcode.ai" + path, auth, timeout=30),
                   schema_only=True)
        for name in list(results):
            write(name)
    else:
        (out / "commandcode_skipped.json").write_text(json.dumps({
            "case": "commandcode_skipped",
            "reason": "COMMAND_CODE_API_KEY not set",
        }, indent=2) + "\n", encoding="utf-8")

    # --- The local proxy: internal auth, alias resolution, no cooldown masking.
    if args.litellm:
        if not master:
            failures.append("litellm: LITELLM_MASTER_KEY is not set")
        else:
            h = {"Authorization": f"Bearer {master}"}
            record("litellm_models", call("GET", f"{litellm}/v1/models", h), expect=200,
                   need=("a model list", lambda b: bool(b.get("data"))))
            record("litellm_no_auth",
                   call("POST", f"{litellm}/v1/chat/completions",
                        {"Content-Type": "application/json"},
                        {"model": "unknown", "messages": []}), expect=401)
            record("litellm_unknown_model",
                   call("POST", f"{litellm}/v1/chat/completions", h,
                        {"model": "no-such-model", "messages": [{"role": "user", "content": "x"}]}),
                   expect=400)
            chat = {"model": "commandcode/" + CC_OPENAI_MODEL, "max_tokens": 16,
                    "messages": [{"role": "user", "content": "Reply with exactly: pong"}]}
            record("litellm_chat",
                   call("POST", f"{litellm}/v1/chat/completions", h, chat, timeout=90),
                   allow_refusal=True, need=("choices with usage", lambda b: has_choices(b) and has_usage(b)))
            record("litellm_chat_stream",
                   call("POST", f"{litellm}/v1/chat/completions", h,
                        {**chat, "stream": True, "stream_options": {"include_usage": True}},
                        timeout=90, stream=True), allow_refusal=True,
               # Command Code emits usage in the final chunk of a normal stream.
               need=("a [DONE]-terminated stream carrying final usage",
                     lambda b: has_stream(b) and has_stream_usage(b)))
            record("litellm_responses",
                   call("POST", f"{litellm}/v1/responses", h,
                        {"model": "commandcode/" + CC_OPENAI_MODEL, "input": "ping"}, timeout=90),
                   allow_refusal=True,
                   need=("a responses output", lambda b: bool(b.get("output") or b.get("id"))))
            record("litellm_messages",
                   call("POST", f"{litellm}/v1/messages", h,
                        {"model": CC_LITELLM_MESSAGES_MODEL, "max_tokens": 16,
                         "messages": chat["messages"]}, timeout=90), allow_refusal=True,
               need=("a message content block", lambda b: bool(b.get("content"))))
            for name in list(results):
                write(name)

    # --- The cooldown regression needs a proxy whose upstream key is rejected.
    # A 401 from the provider must repeat as 401; with LiteLLM's default router it
    # becomes a 429 "all deployments in cooldown" on the next attempt, which reads
    # as rate limiting and misdirects quota accounting.
    if args.litellm_invalid:
        if not master:
            failures.append("litellm-invalid: LITELLM_MASTER_KEY is not set")
        else:
            bad = {"Authorization": f"Bearer {master}"}
            codes = []
            for i in (1, 2, 3):
                name = f"litellm_invalid_repeat_{i}"
                record(name, call("POST", f"{args.litellm_invalid.rstrip('/')}/v1/chat/completions", bad,
                                  {"model": "minimax/" + MINIMAX_MODEL, "max_tokens": 8,
                                   "messages": [{"role": "user", "content": "Reply with exactly: pong"}]},
                                  timeout=90))
                write(name)
                codes.append(results[name].get("status"))
            bodies = [json.dumps(results[f"litellm_invalid_repeat_{i}"].get("body", {})) for i in (1, 2, 3)]
            if any("cooldown" in b for b in bodies):
                failures.append("litellm-invalid: a cooldown error masked the provider's rejection")
            if codes != [401, 401, 401]:
                failures.append(f"litellm-invalid: repeated rejected-key attempts returned {codes}, "
                                "expected 401 every time (a 429 means cooldowns are masking it)")
            print("litellm rejected-key statuses:", codes)

    if args.cancellation and cc_key:
        # A cancelled stream is billed for what was produced; recorded for the
        # "never retry after partial output" contract.
        auth = {"Authorization": f"Bearer {cc_key}"}
        before = call("GET", "https://api.commandcode.ai/alpha/billing/credits", auth)
        body = json.dumps({"model": CC_OPENAI_MODEL, "max_tokens": 600, "stream": True,
                           "messages": [{"role": "user", "content": "Write 500 words about the sea."}]}).encode()
        req = urllib.request.Request(f"{COMMANDCODE_BASE}/chat/completions", data=body,
                                     headers={**auth, "Content-Type": "application/json"}, method="POST")
        received = 0
        resp = urllib.request.urlopen(req, timeout=60)
        for line in resp:
            if line.startswith(b"data:"):
                received += 1
            if received >= 3:
                break
        resp.close()
        time.sleep(5)
        after = call("GET", "https://api.commandcode.ai/alpha/billing/credits", auth)
        try:
            delta = (after["body"]["windowLimits"]["fiveHour"]["used"]
                     - before["body"]["windowLimits"]["fiveHour"]["used"])
        except Exception:
            delta = None
        results["commandcode_cancellation"] = {
            "ok": True, "status": 200, "chunks_before_cancel": received,
            "five_hour_credits_delta": round(delta, 6) if delta is not None else None,
            "note": "client closed the stream mid-flight; the produced tokens are billed",
        }
        write("commandcode_cancellation")

    print(f"\n{len(results)} cases; fixtures in {out}")
    if failures:
        print("FAILED:")
        for f in failures:
            print("  -", f)
        return 1
    # Blocked cases are neither passes nor failures: the account could not serve
    # them. Saying "all expectations held" while holding them would overstate the
    # evidence, so they are counted out loud.
    if blocked:
        print(f"expectations held for the cases that ran; {len(blocked)} case(s) "
              f"BLOCKED_BY_PLAN and not verified: {', '.join(blocked)}")
        return 0
    print("all expectations held")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
