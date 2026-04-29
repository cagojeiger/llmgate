# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "openai>=1.50",
#   "python-dotenv>=1.0",
# ]
# ///
"""Sanity check: does Python's openai SDK talk to OpenCode Zen Go directly?

Bypasses llmgate entirely so we know whether upstream is genuinely
OpenAI-compatible before we build the gateway around that assumption.

Usage:
    uv run scripts/probe_upstream.py                    # non-stream
    uv run scripts/probe_upstream.py --stream           # SSE
    uv run scripts/probe_upstream.py --via-gate         # through llmgate
    uv run scripts/probe_upstream.py --prompt "say hi"  # custom prompt
"""

from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path

from dotenv import load_dotenv
from openai import OpenAI


def field(obj, name):
    """Read a possibly-vendor-extension field off an openai SDK Pydantic model.

    OpenAI's models declare their schemas explicitly; vendor fields like
    ``reasoning_content`` and ``cost`` end up in ``model_extra`` rather than
    as typed attributes.
    """
    val = getattr(obj, name, None)
    if val is not None:
        return val
    extra = getattr(obj, "model_extra", None) or {}
    return extra.get(name)


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--stream", action="store_true")
    p.add_argument("--via-gate", action="store_true")
    p.add_argument("--gate-url", default="http://localhost:8080/v1")
    p.add_argument("--model", default=None)
    p.add_argument("--max-tokens", type=int, default=256)
    p.add_argument(
        "--prompt",
        default="Reply with just one word: pong",
    )
    args = p.parse_args()

    repo_root = Path(__file__).resolve().parent.parent
    load_dotenv(repo_root / ".env")

    if args.via_gate:
        api_key = "dummy-client-key"
        base_url = args.gate_url
    else:
        api_key = os.environ.get("LLMGATE_OPENCODE_API_KEY")
        base_url = os.environ.get(
            "LLMGATE_OPENCODE_BASE_URL", "https://opencode.ai/zen/go/v1"
        )
    model = args.model or os.environ.get(
        "LLMGATE_DEFAULT_MODEL", "deepseek-v4-flash"
    )

    if not api_key:
        print("ERROR: LLMGATE_OPENCODE_API_KEY not set", file=sys.stderr)
        return 1

    client = OpenAI(api_key=api_key, base_url=base_url)
    if args.via_gate:
        print("mode:     via-gate")
    else:
        print(f"base_url: {base_url}")
    print(f"model:    {model}")
    print(f"prompt:   {args.prompt!r}")
    if args.via_gate:
        print(f"request:  {'stream' if args.stream else 'non-stream'}")
    else:
        print(f"mode:     {'stream' if args.stream else 'non-stream'}")
    print("---")

    if args.stream:
        return run_stream(client, model, args.prompt, args.max_tokens)
    return run_non_stream(client, model, args.prompt, args.max_tokens)


def run_non_stream(client: OpenAI, model: str, prompt: str, max_tokens: int) -> int:
    resp = client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": prompt}],
        max_tokens=max_tokens,
    )
    msg = resp.choices[0].message
    content = (msg.content or "").strip()
    reasoning = (field(msg, "reasoning_content") or "").strip()

    print(f"id:            {resp.id}")
    print(f"model:         {resp.model}")
    print(f"finish_reason: {resp.choices[0].finish_reason}")
    print(f"content:       {content!r}")
    if reasoning:
        truncated = reasoning if len(reasoning) <= 400 else reasoning[:400] + "..."
        print(f"reasoning:     {truncated!r}")
    print(f"usage:         {resp.usage}")

    cost = field(resp, "cost")
    if cost is not None:
        print(f"cost:          {cost!r}  (vendor extension)")

    if not content and not reasoning:
        print("WARN: both content and reasoning_content empty", file=sys.stderr)
        return 2
    return 0


def run_stream(client: OpenAI, model: str, prompt: str, max_tokens: int) -> int:
    stream = client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": prompt}],
        max_tokens=max_tokens,
        stream=True,
    )

    n_chunks = 0
    n_with_content = 0
    n_with_reasoning = 0
    finish_reason = None
    final_content_buf: list[str] = []
    final_reasoning_buf: list[str] = []

    for chunk in stream:
        n_chunks += 1
        if not chunk.choices:
            continue
        choice = chunk.choices[0]
        if choice.finish_reason:
            finish_reason = choice.finish_reason
        delta = choice.delta
        if delta is None:
            continue
        c = getattr(delta, "content", None)
        if c:
            n_with_content += 1
            final_content_buf.append(c)
            sys.stdout.write(c)
            sys.stdout.flush()
        r = field(delta, "reasoning_content")
        if r:
            n_with_reasoning += 1
            final_reasoning_buf.append(r)

    print()
    print("---")
    print(f"chunks:                 {n_chunks}")
    print(f"chunks w/ content:      {n_with_content}")
    print(f"chunks w/ reasoning:    {n_with_reasoning}")
    print(f"finish_reason:          {finish_reason}")
    if final_reasoning_buf:
        joined = "".join(final_reasoning_buf).strip()
        truncated = joined if len(joined) <= 400 else joined[:400] + "..."
        print(f"reasoning_total:        {truncated!r}")
    if final_content_buf:
        print(f"content_total:          {''.join(final_content_buf)!r}")

    if n_chunks == 0:
        print("FAIL: zero chunks received", file=sys.stderr)
        return 2
    if not final_content_buf and not final_reasoning_buf:
        print("WARN: stream produced no content or reasoning", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
