"""Fake OpenAI-shaped upstream for the LiteLLM contract test.

Stands in for a provider so the pinned LiteLLM image's *response serialization*
can be asserted without a real subscription key. It answers both a normal
completion and an SSE stream, and records the request body LiteLLM forwards so
the test can prove what the proxy actually sent upstream.

Run: python3 fake_upstream.py <port>
"""
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PROMPT_TOKENS = 11
COMPLETION_TOKENS = 7
RECORDED = []


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass  # keep stdout clean; the test reads RECORDED instead

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            body = json.loads(raw)
        except json.JSONDecodeError:
            body = {"raw": raw.decode("utf-8", "replace")}

        if self.path.rstrip("/").endswith("/chat/completions"):
            if body.get("stream"):
                self._stream(body)
            else:
                self._completion(body)
            return
        self._json(404, {"error": {"message": "unknown path " + self.path}})

    def _completion(self, body):
        RECORDED.append({"path": self.path, "body": body, "headers": dict(self.headers)})
        self._json(200, {
            "id": "chatcmpl-fake-1",
            "object": "chat.completion",
            "created": 1700000000,
            "model": body.get("model", "unknown"),
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "pong"},
                "finish_reason": "stop",
            }],
            "usage": {
                "prompt_tokens": PROMPT_TOKENS,
                "completion_tokens": COMPLETION_TOKENS,
                "total_tokens": PROMPT_TOKENS + COMPLETION_TOKENS,
            },
        })

    def _stream(self, body):
        RECORDED.append({"path": self.path, "body": body, "stream": True})
        include_usage = bool((body.get("stream_options") or {}).get("include_usage"))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        def chunk(delta, finish=None):
            payload = {
                "id": "chatcmpl-fake-1",
                "object": "chat.completion.chunk",
                "created": 1700000000,
                "model": body.get("model", "unknown"),
                "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
            }
            self.wfile.write(b"data: " + json.dumps(payload).encode() + b"\n\n")
            self.wfile.flush()

        chunk({"role": "assistant", "content": ""})
        for piece in ("po", "ng"):
            chunk({"content": piece})
        chunk({}, finish="stop")
        if include_usage:
            # The upstream's final usage chunk: choices empty, usage present.
            self.wfile.write(b"data: " + json.dumps({
                "id": "chatcmpl-fake-1",
                "object": "chat.completion.chunk",
                "created": 1700000000,
                "model": body.get("model", "unknown"),
                "choices": [],
                "usage": {
                    "prompt_tokens": PROMPT_TOKENS,
                    "completion_tokens": COMPLETION_TOKENS,
                    "total_tokens": PROMPT_TOKENS + COMPLETION_TOKENS,
                },
            }).encode() + b"\n\n")
            self.wfile.flush()
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def _json(self, status, obj):
        payload = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        # What the test reads back to prove what LiteLLM forwarded upstream.
        if self.path.rstrip("/").endswith("/_recorded"):
            self._json(200, {"recorded": RECORDED})
            return
        if self.path.rstrip("/").endswith("/models"):
            self._json(200, {"data": [{"id": "fake-verify-model", "object": "model"}]})
            return
        self._json(404, {"error": {"message": "unknown path " + self.path}})


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 18081
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    print("fake upstream on 127.0.0.1:%d" % port, flush=True)
    threading.Event().wait()


if __name__ == "__main__":
    main()
