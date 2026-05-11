#!/usr/bin/env sh
# probe.sh — smoke test the deployed llmgate.
#
# Reads LLMGATE_DEV_KEY (and optional LLMGATE_URL) from ./.dev so credentials
# stay out of shell history. .dev is gitignored.
#
# Usage:
#   scripts/probe.sh [model]      # default: smart
#
# Examples:
#   scripts/probe.sh
#   scripts/probe.sh deepseek-v4-flash

set -eu

cd "$(dirname "$0")/.."
if [ -f .dev ]; then
    # shellcheck disable=SC1091
    . ./.dev
fi

URL="${LLMGATE_URL:-https://llmgate.project-jelly.io}"
MODEL="${1:-smart}"

if [ -z "${LLMGATE_DEV_KEY:-}" ]; then
    echo "error: LLMGATE_DEV_KEY missing — populate .dev (e.g. LLMGATE_DEV_KEY=<raw>)" >&2
    exit 1
fi

echo "→ $URL  model=$MODEL"

echo
echo "[1] /healthz/ready"
curl -fsS "$URL/healthz/ready" && echo

echo
echo "[2] /v1/chat/completions"
resp=$(curl -fsS "$URL/v1/chat/completions" \
    -H "Authorization: Bearer $LLMGATE_DEV_KEY" \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with one word: pong\"}],\"max_tokens\":256}")

printf '%s\n' "$resp" | python3 -c '
import json, sys
d = json.load(sys.stdin)
choice = d["choices"][0]
msg = choice.get("message", {})
content = (msg.get("content") or "").strip()
reasoning = (msg.get("reasoning_content") or "").strip()
print("  model_used   :", d.get("model"))
print("  finish       :", choice.get("finish_reason"))
print("  content      :", repr(content))
if reasoning:
    short = reasoning[:80] + ("..." if len(reasoning) > 80 else "")
    print("  reasoning    :", short)
usage = d.get("usage") or {}
if usage:
    print("  tokens       : prompt={} completion={} total={}".format(
        usage.get("prompt_tokens"), usage.get("completion_tokens"), usage.get("total_tokens")))
'

echo
echo "✓ passed"
