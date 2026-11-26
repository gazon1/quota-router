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
MODEL_OBJECT_KEYS = {"object", "owned_by", "context_length", "supported_endpoints",
                     "display_name", "created_at"}


def redact(obj):
    """Drops credentials and account identity, but keeps model ids: a model id is
    the evidence this file exists to record."""
    if isinstance(obj, dict):
        is_model = (obj.get("object") == "model" or bool(MODEL_OBJECT_KEYS & set(obj))
                    or ("id" in obj and isinstance(obj.get("id"), str)
                        and ("/" in obj["id"] or ":" in obj["id"])))
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
                parsed = raw.decode("utf-8", "replace")[:400]
            return {"ok": True, "status": resp.status,
                    "elapsed_ms": round((time.time() - started) * 1000),
                    "body": redact(parsed)}
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            parsed = json.loads(raw)
        except Exception:
            parsed = raw.decode("utf-8", "replace")[:400]
        return {"ok": False, "status": e.code,
                "elapsed_ms": round((time.time() - started) * 1000),
                "body": redact(parsed)}
    except Exception as e:  # DNS, TLS, timeout
        return {"ok": False, "status": None,
                "elapsed_ms": round((time.time() - started) * 1000),
                "error_type": type(e).__name__, "error": str(e)[:200]}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="docs/fixtures/smoke")
    ap.add_argument("--litellm", action="store_true",
                    help="also probe the local proxy at LITELLM_BASE_URL")
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
    failures: list[str] = []

    def record(name, payload, expect=None):
        results[name] = payload
        if expect is not None and payload.get("status") != expect:
            failures.append(f"{name}: status {payload.get('status')}, expected {expect}")

    def write(name):
        (out / f"{name}.json").write_text(
            json.dumps({"case": name, "response": results[name]}, indent=2) + "\n",
            encoding="utf-8")

    # --- MiniMax Token Plan: quota surface and inference are separate hosts.
    if mm_key:
        auth = {"Authorization": f"Bearer {mm_key}"}
        record("minimax_token_plan_remains", call("GET", MINIMAX_QUOTA, auth), expect=200)
        record("minimax_openai_models",
               call("GET", f"{MINIMAX_INFERENCE}/models", auth), expect=200)
        record("minimax_anthropic_models",
               call("GET", f"{MINIMAX_ANTHROPIC}/v1/models",
                    {"x-api-key": mm_key, "anthropic-version": "2023-06-01"}), expect=200)
        chat = {"model": MINIMAX_MODEL, "max_completion_tokens": 16,
                "messages": [{"role": "user", "content": "Reply with exactly: pong"}]}
        # 429 here is the documented plan-limit signal (2056), not a test bug.
        record("minimax_openai_chat", call("POST", f"{MINIMAX_INFERENCE}/chat/completions", auth, chat))
        record("minimax_openai_chat_stream",
               call("POST", f"{MINIMAX_INFERENCE}/chat/completions", auth, {**chat, "stream": True}, stream=True))
        record("minimax_anthropic_messages",
               call("POST", f"{MINIMAX_ANTHROPIC}/v1/messages",
                    {"x-api-key": mm_key, "anthropic-version": "2023-06-01"},
                    {"model": MINIMAX_MODEL, "max_tokens": 16, "messages": chat["messages"]}))
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
               call("POST", f"{COMMANDCODE_BASE}/chat/completions", auth, chat, timeout=90))
        record("commandcode_chat_stream",
               call("POST", f"{COMMANDCODE_BASE}/chat/completions", auth, {**chat, "stream": True},
                    timeout=90, stream=True))
        record("commandcode_chat_second",
               call("POST", f"{COMMANDCODE_BASE}/chat/completions", auth,
                    {**chat, "model": CC_OPENAI_MODEL_2}, timeout=90))
        record("commandcode_responses",
               call("POST", f"{COMMANDCODE_BASE}/responses", auth,
                    {"model": CC_OPENAI_MODEL, "input": "Reply with exactly: pong"}, timeout=90))
        record("commandcode_messages",
               call("POST", f"{COMMANDCODE_BASE}/messages",
                    {**auth, "anthropic-version": "2023-06-01"},
                    {"model": CC_ANTHROPIC_MODEL, "max_tokens": 16,
                     "messages": chat["messages"]}, timeout=90))
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
        for name, path in (
            ("commandcode_alpha_whoami", "/alpha/whoami?limits=1"),
            ("commandcode_alpha_credits", "/alpha/billing/credits"),
            ("commandcode_alpha_subscriptions", "/alpha/billing/subscriptions"),
            ("commandcode_alpha_usage", "/alpha/usage/summary"),
        ):
            record(name, call("GET", "https://api.commandcode.ai" + path, auth, timeout=30))
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
            record("litellm_models", call("GET", f"{litellm}/v1/models", h), expect=200)
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
            record("litellm_chat", call("POST", f"{litellm}/v1/chat/completions", h, chat, timeout=90))
            record("litellm_chat_stream",
                   call("POST", f"{litellm}/v1/chat/completions", h, {**chat, "stream": True},
                        timeout=90, stream=True))
            record("litellm_responses",
                   call("POST", f"{litellm}/v1/responses", h,
                        {"model": "commandcode/" + CC_OPENAI_MODEL, "input": "ping"}, timeout=90))
            record("litellm_messages",
                   call("POST", f"{litellm}/v1/messages", h,
                        {"model": CC_LITELLM_MESSAGES_MODEL, "max_tokens": 16,
                         "messages": chat["messages"]}, timeout=90))
            # The regression that reads as rate limiting: a rejected provider key
            # must stay 401 on every attempt, never become 429.
            bad = {"Authorization": f"Bearer {master}"}
            for i in (1, 2, 3):
                record(f"litellm_repeat_{i}",
                       call("POST", f"{litellm}/v1/chat/completions", bad,
                            {"model": "minimax/" + MINIMAX_MODEL,
                             "max_tokens": 8, "messages": chat["messages"]}, timeout=90))
            for name in list(results):
                write(name)
            codes = [results[f"litellm_repeat_{i}"].get("status") for i in (1, 2, 3)]
            bodies = [json.dumps(results[f"litellm_repeat_{i}"].get("body", {})) for i in (1, 2, 3)]
            # The provider's own verdict (401 rejected key, 429 exhausted plan) is
            # fine as long as it repeats unchanged; a cooldown replaces it with a
            # 429 "all deployments in cooldown" on the second attempt.
            if len(set(codes)) != 1:
                failures.append(f"litellm: repeated bad-key attempts returned {codes}; "
                                "the first failure is masking the next (cooldown)")
            if any("cooldown" in b for b in bodies):
                failures.append("litellm: a cooldown error masked the provider's real response")
            print("litellm repeated bad-key statuses:", codes)

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
    print("all expectations held")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
