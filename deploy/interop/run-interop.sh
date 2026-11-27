#!/bin/sh
# Contract test the agent against the pinned LiteLLM image.
#
# Starts a recording OpenAI-shaped upstream, the pinned proxy in front of it, and
# drives both hops. Asserts the usage field names internal/agent depends on, then
# rewrites internal/agent/testdata/ so the Go tests assert measured bytes.
#
# No provider subscription key is needed: the upstream is local, so this measures
# LiteLLM's own response serialization — which is what the agent parses. It does
# not prove anything about a real provider's token accounting.
#
# Usage: deploy/interop/run-interop.sh [--keep]
set -eu

REPO=$(cd "$(dirname "$0")/../.." && pwd)
IMAGE="ghcr.io/berriai/litellm:v1.110.0@sha256:4295b064b22eb88f9530e616afc812edd47048891ba64896bfc3fa74e2b883c3"
UPSTREAM_PORT=18081
PROXY_PORT=13182
MASTER_KEY=sk-interop-master
CONTAINER=litellm-interop
WORKDIR=$(mktemp -d)
TESTDATA="$REPO/internal/agent/testdata"
KEEP=${1:-}

cleanup() {
	[ -n "${UPSTREAM_PID:-}" ] && kill "$UPSTREAM_PID" 2>/dev/null || true
	if [ "$KEEP" != "--keep" ]; then
		docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
		rm -rf "$WORKDIR"
	fi
}
trap cleanup EXIT INT TERM

echo "==> starting recording upstream on :$UPSTREAM_PORT"
python3 "$REPO/deploy/interop/fake_upstream.py" "$UPSTREAM_PORT" >"$WORKDIR/upstream.log" 2>&1 &
UPSTREAM_PID=$!
sleep 2
curl -fsS -o /dev/null -X POST "http://127.0.0.1:$UPSTREAM_PORT/v1/chat/completions" \
	-d '{"model":"probe","messages":[]}' || { echo "upstream failed to start"; exit 1; }

# Same model_list shape as deploy/litellm-config.yaml; only api_base changes.
cat >"$WORKDIR/config.yaml" <<EOF
model_list:
  - model_name: verify/fake-model
    litellm_params:
      model: openai/fake-verify-model
      api_base: http://127.0.0.1:$UPSTREAM_PORT/v1
      api_key: os.environ/UPSTREAM_FAKE_KEY
router_settings:
  disable_cooldowns: true
  num_retries: 0
general_settings:
  master_key: os.environ/LITELLM_MASTER_KEY
EOF

echo "==> starting pinned LiteLLM ($IMAGE)"
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
# --port is explicit, not inherited: the image's CMD is "--port 4000", and
# overriding command drops it, so the CLI picks a free port instead of failing
# when one is taken. The harness must know which port it is talking to.
docker run -d --name "$CONTAINER" --network host \
	-e LITELLM_MASTER_KEY="$MASTER_KEY" \
	-e UPSTREAM_FAKE_KEY=sk-fake-upstream \
	-v "$WORKDIR/config.yaml:/app/config.yaml:ro" \
	"$IMAGE" --config /app/config.yaml --port "$PROXY_PORT" >/dev/null

echo -n "==> waiting for the proxy"
i=0
while [ $i -lt 60 ]; do
	if curl -fsS "http://127.0.0.1:$PROXY_PORT/health/liveliness" >/dev/null 2>&1; then break; fi
	i=$((i + 1)); sleep 1; echo -n "."
done
echo
curl -fsS "http://127.0.0.1:$PROXY_PORT/health/liveliness" >/dev/null || {
	echo "proxy did not become ready; logs:"; docker logs "$CONTAINER" | tail -20; exit 1; }

# The proxy's port is not the compose default here because the harness runs on
# the host network; assert the version so a stray proxy on the same port cannot
# pass for the pinned image.
VERSION=$(curl -fsS "http://127.0.0.1:$PROXY_PORT/health/liveliness" >/dev/null 2>&1 &&
	curl -sS -o /dev/null -D- -X POST "http://127.0.0.1:$PROXY_PORT/v1/chat/completions" \
		-H "Authorization: Bearer $MASTER_KEY" -H 'Content-Type: application/json' \
		-d '{"model":"verify/fake-model","messages":[{"role":"user","content":"ping"}]}' |
	sed -n 's/^x-litellm-version: *//p' | tr -d '\r')
echo "==> pinned proxy reports version ${VERSION:-unknown}"
[ "$VERSION" = "1.110.0" ] || { echo "expected 1.110.0, got '${VERSION:-none}'"; exit 1; }

echo "==> capturing a non-streaming completion"
curl -fsS -X POST "http://127.0.0.1:$PROXY_PORT/v1/chat/completions" \
	-H "Authorization: Bearer $MASTER_KEY" -H 'Content-Type: application/json' \
	-d '{"model":"verify/fake-model","messages":[{"role":"user","content":"ping"}]}' \
	>"$TESTDATA/litellm_v1.110.0_chat_completion.json"

echo "==> capturing an SSE stream with include_usage"
curl -fsS -X POST "http://127.0.0.1:$PROXY_PORT/v1/chat/completions" \
	-H "Authorization: Bearer $MASTER_KEY" -H 'Content-Type: application/json' \
	-d '{"model":"verify/fake-model","messages":[{"role":"user","content":"ping"}],"stream":true,"stream_options":{"include_usage":true}}' \
	>"$TESTDATA/litellm_v1.110.0_chat_stream.sse"

echo "==> asserting the usage fields the agent parses"
python3 - "$TESTDATA" <<'PY'
import json, sys, pathlib
data = pathlib.Path(sys.argv[1])
completion = json.loads((data / "litellm_v1.110.0_chat_completion.json").read_text())
usage = completion.get("usage") or {}
assert usage.get("prompt_tokens") == 11, f"prompt_tokens wrong: {usage}"
assert usage.get("completion_tokens") == 7, f"completion_tokens wrong: {usage}"
print("   non-streaming: usage.prompt_tokens=%d usage.completion_tokens=%d"
      % (usage["prompt_tokens"], usage["completion_tokens"]))

chunks = [json.loads(line[6:]) for line in
          (data / "litellm_v1.110.0_chat_stream.sse").read_text().splitlines()
          if line.startswith("data: ") and line[6:].strip() != "[DONE]"]
final = [c for c in chunks if c.get("usage")]
assert final, "no stream chunk carried usage; include_usage was not honoured"
u = final[-1]["usage"]
assert u.get("prompt_tokens") == 11 and u.get("completion_tokens") == 7, f"stream usage wrong: {u}"
assert final[-1].get("choices") == [], "the usage chunk should carry no choices"
print("   streaming:     usage.prompt_tokens=%d usage.completion_tokens=%d on a choices-less chunk"
      % (u["prompt_tokens"], u["completion_tokens"]))
PY

echo "==> running the Go contract tests against the captured bytes"
cd "$REPO" && go test -count=1 -run 'TestUsageFields' ./internal/agent/

echo
echo "PASS: the pinned image returns usage.prompt_tokens / usage.completion_tokens"
echo "      (non-streaming and on the final stream chunk)."
