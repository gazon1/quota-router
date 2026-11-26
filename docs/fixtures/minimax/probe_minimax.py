#!/usr/bin/env python3
"""Reproducible MiniMax API compatibility probe.

Usage:
  python3 docs/fixtures/minimax/probe_minimax.py --out docs/fixtures/minimax

Environment (optional, for authenticated tests):
  MINIMAX_API_KEY        OpenAI-compatible key / M Plan subscription key
  MINIMAX_ANTHROPIC_KEY  Anthropic-compatible key (defaults to MINIMAX_API_KEY)
  MINIMAX_BASE_URL       default https://api.minimax.io/v1
  MINIMAX_ANTHROPIC_BASE default https://api.minimax.io/anthropic

No secrets are written to fixtures. Only documented endpoints are used.
"""
from __future__ import annotations

import argparse
import json
import os
import socket
import time
import urllib.error
import urllib.request
from pathlib import Path

REDACT_KEY_PARTS = ("key", "auth", "token", "cookie", "secret", "authorization", "set-cookie")
VOLATILE_HEADERS = {
    "date", "expires", "trace-id", "minimax-request-id", "alb_receive_time",
    "alb_request_id", "akamai-grn", "set-cookie", "x-from",
}
REDACT_BODY_KEYS = {"request_id", "trace_id", "minimax_request_id"}


def redact(obj):
    if isinstance(obj, dict):
        out = {}
        for k, v in obj.items():
            lk = k.lower()
            if any(p in lk for p in REDACT_KEY_PARTS) or lk in REDACT_BODY_KEYS:
                out[k] = "***REDACTED***"
            else:
                out[k] = redact(v)
        return out
    if isinstance(obj, list):
        return [redact(x) for x in obj]
    return obj


def sanitize_headers(headers):
    out = {}
    for k, v in dict(headers).items():
        if k.lower() in VOLATILE_HEADERS or any(p in k.lower() for p in REDACT_KEY_PARTS):
            out[k] = "***REDACTED***"
        else:
            out[k] = v
    return out


def request(method, url, headers=None, body=None, timeout=20):
    data = json.dumps(body).encode() if body is not None else None
    h = {"Content-Type": "application/json", "Accept": "application/json"}
    if headers:
        h.update(headers)
    req = urllib.request.Request(url, data=data, headers=h, method=method)
    started = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            elapsed_ms = round((time.time() - started) * 1000)
            try:
                parsed = json.loads(raw)
            except Exception:
                parsed = raw.decode("utf-8", errors="replace")
            return {
                "ok": True,
                "status": resp.status,
                "elapsed_ms": elapsed_ms,
                "headers": sanitize_headers(resp.headers),
                "body": redact(parsed),
            }
    except urllib.error.HTTPError as e:
        raw = e.read()
        elapsed_ms = round((time.time() - started) * 1000)
        try:
            parsed = json.loads(raw)
        except Exception:
            parsed = raw.decode("utf-8", errors="replace")
        return {
            "ok": False,
            "status": e.code,
            "elapsed_ms": elapsed_ms,
            "headers": sanitize_headers(e.headers),
            "body": redact(parsed),
            "error_type": "HTTPError",
        }
    except Exception as e:
        elapsed_ms = round((time.time() - started) * 1000)
        return {
            "ok": False,
            "elapsed_ms": elapsed_ms,
            "error_type": type(e).__name__,
            "error": str(e),
        }


def stream_request(url, headers, body, timeout=20):
    data = json.dumps(body).encode()
    h = {"Content-Type": "application/json", "Accept": "text/event-stream"}
    h.update(headers)
    req = urllib.request.Request(url, data=data, headers=h, method="POST")
    started = time.time()
    events = []
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            for raw_line in resp:
                elapsed_ms = round((time.time() - started) * 1000)
                line = raw_line.decode("utf-8", errors="replace").rstrip("\n")
                if line.startswith("data:"):
                    payload = line[5:].strip()
                    if payload == "[DONE]":
                        events.append({"elapsed_ms": elapsed_ms, "data": "[DONE]"})
                        break
                    try:
                        events.append({"elapsed_ms": elapsed_ms, "data": redact(json.loads(payload))})
                    except Exception:
                        events.append({"elapsed_ms": elapsed_ms, "data": payload})
                if len(events) >= 20:
                    events.append({"truncated": True})
                    break
            return {"ok": True, "status": resp.status, "headers": sanitize_headers(resp.headers), "events": events}
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            parsed = json.loads(raw)
        except Exception:
            parsed = raw.decode("utf-8", errors="replace")
        return {"ok": False, "status": e.code, "headers": sanitize_headers(e.headers), "body": redact(parsed), "error_type": "HTTPError"}
    except Exception as e:
        return {"ok": False, "error_type": type(e).__name__, "error": str(e)}


def write_fixture(out_dir: Path, name: str, payload):
    path = out_dir / f"{name}.json"
    path.write_text(json.dumps(payload, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"wrote {path}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="docs/fixtures/minimax")
    ap.add_argument("--timeout", type=float, default=20)
    args = ap.parse_args()
    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)

    base = os.environ.get("MINIMAX_BASE_URL", "https://api.minimax.io/v1").rstrip("/")
    anth_base = os.environ.get("MINIMAX_ANTHROPIC_BASE", "https://api.minimax.io/anthropic").rstrip("/")
    key = os.environ.get("MINIMAX_API_KEY")
    anth_key = os.environ.get("MINIMAX_ANTHROPIC_KEY") or key
    bad_key = "sk-invalid-probe-key"

    cases = []

    # Auth failures / documented endpoint behavior (no real key required)
    cases.append(("openai_models_no_auth", lambda: request("GET", f"{base}/models", timeout=args.timeout)))
    cases.append(("openai_models_bad_auth", lambda: request("GET", f"{base}/models", headers={"Authorization": f"Bearer {bad_key}"}, timeout=args.timeout)))
    cases.append(("anthropic_models_bad_auth", lambda: request("GET", f"{anth_base}/v1/models", headers={"x-api-key": bad_key, "anthropic-version": "2023-06-01"}, timeout=args.timeout)))
    cases.append(("openai_chat_bad_auth", lambda: request("POST", f"{base}/chat/completions", headers={"Authorization": f"Bearer {bad_key}"}, body={
        "model": "MiniMax-M3.1-Flash", "messages": [{"role": "user", "content": "ping"}], "max_completion_tokens": 8
    }, timeout=args.timeout)))
    cases.append(("anthropic_messages_bad_auth", lambda: request("POST", f"{anth_base}/v1/messages", headers={"x-api-key": bad_key, "anthropic-version": "2023-06-01"}, body={
        "model": "MiniMax-M3.1-Flash", "max_tokens": 8, "messages": [{"role": "user", "content": "ping"}]
    }, timeout=args.timeout)))
    cases.append(("openai_chat_bad_auth_stream", lambda: stream_request(f"{base}/chat/completions", headers={"Authorization": f"Bearer {bad_key}"}, body={
        "model": "MiniMax-M3.1-Flash", "messages": [{"role": "user", "content": "ping"}], "max_completion_tokens": 8, "stream": True
    }, timeout=args.timeout)))
    cases.append(("token_plan_remains_bad_auth", lambda: request("GET", "https://www.minimax.io/v1/token_plan/remains", headers={"Authorization": f"Bearer {bad_key}"}, timeout=args.timeout)))
    cases.append(("openai_chat_timeout", lambda: request("POST", f"{base}/chat/completions", headers={"Authorization": f"Bearer {bad_key}"}, body={
        "model": "MiniMax-M3.1-Flash", "messages": [{"role": "user", "content": "ping"}], "max_completion_tokens": 8
    }, timeout=0.001)))

    for name, fn in cases:
        payload = {"case": name, "request": {"endpoint": name, "auth": "none/bad-key as named"}, "response": fn()}
        write_fixture(out_dir, name, payload)

    if not key:
        write_fixture(out_dir, "authenticated_tests_skipped", {
            "case": "authenticated_tests_skipped",
            "reason": "MINIMAX_API_KEY not set; authenticated, streaming-success, tool-call, and quota-consumption tests were not run.",
            "required_env": ["MINIMAX_API_KEY"],
            "optional_env": ["MINIMAX_ANTHROPIC_KEY", "MINIMAX_BASE_URL", "MINIMAX_ANTHROPIC_BASE"],
        })
        return

    auth = {"Authorization": f"Bearer {key}"}
    anth_auth = {"x-api-key": anth_key, "anthropic-version": "2023-06-01"}

    write_fixture(out_dir, "openai_models_auth", {"case": "openai_models_auth", "response": request("GET", f"{base}/models", headers=auth, timeout=args.timeout)})
    write_fixture(out_dir, "anthropic_models_auth", {"case": "anthropic_models_auth", "response": request("GET", f"{anth_base}/v1/models", headers=anth_auth, timeout=args.timeout)})

    chat_body = {"model": "MiniMax-M3.1-Flash", "messages": [{"role": "user", "content": "Reply with the word pong only."}], "max_completion_tokens": 16}
    write_fixture(out_dir, "openai_chat_auth", {"case": "openai_chat_auth", "request_body": redact(chat_body), "response": request("POST", f"{base}/chat/completions", headers=auth, body=chat_body, timeout=args.timeout)})

    stream_body = {**chat_body, "stream": True}
    write_fixture(out_dir, "openai_chat_stream_auth", {"case": "openai_chat_stream_auth", "request_body": redact(stream_body), "response": stream_request(f"{base}/chat/completions", headers=auth, body=stream_body, timeout=args.timeout)})

    tool_body = {
        "model": "MiniMax-M3.1-Flash",
        "messages": [{"role": "user", "content": "What is the weather in San Francisco? Use the tool."}],
        "tools": [{"type": "function", "function": {"name": "get_weather", "description": "Get weather", "parameters": {"type": "object", "properties": {"location": {"type": "string"}}, "required": ["location"]}}}],
        "max_completion_tokens": 64,
    }
    write_fixture(out_dir, "openai_tool_call_auth", {"case": "openai_tool_call_auth", "request_body": redact(tool_body), "response": request("POST", f"{base}/chat/completions", headers=auth, body=tool_body, timeout=args.timeout)})

    write_fixture(out_dir, "token_plan_remains_auth", {"case": "token_plan_remains_auth", "response": request("GET", "https://www.minimax.io/v1/token_plan/remains", headers=auth, timeout=args.timeout)})


if __name__ == "__main__":
    main()
