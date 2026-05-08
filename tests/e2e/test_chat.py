"""Scenario-level e2e tests. Matrix coverage lives in test_all_models.py."""

from __future__ import annotations

import time

import pytest
from openai import OpenAI

from conftest import (
    assert_streaming_progressive,
    discover_catalog_models,
    field,
    raw_consumer_key,
)


pytestmark = pytest.mark.timeout(120)

_openai = discover_catalog_models("openai")
_anthropic = discover_catalog_models("anthropic")
if not _openai or not _anthropic:
    pytest.skip("openai/anthropic protocol model missing from catalog", allow_module_level=True)
MODEL = _openai[0]
ANTHROPIC_MODEL = _anthropic[0]


@pytest.fixture
def client(gate_base_url: str) -> OpenAI:
    return OpenAI(base_url=f"{gate_base_url}/v1", api_key=raw_consumer_key())


def test_chat_non_stream(client: OpenAI) -> None:
    resp = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "Reply with just one word: pong"}],
        max_tokens=512,
    )
    assert resp.choices, f"no choices: {resp}"
    msg = resp.choices[0].message
    content = (msg.content or "").strip()
    reasoning = (field(msg, "reasoning_content") or "").strip()
    # Reasoning models can put output in reasoning_content instead.
    assert content or reasoning, f"both content and reasoning_content empty: {resp}"
    assert resp.usage is not None
    assert resp.usage.total_tokens > 0


def test_chat_anthropic_stream(client: OpenAI) -> None:
    chunks_with_content = 0
    finish_reason: str | None = None

    stream = client.chat.completions.create(
        model=ANTHROPIC_MODEL,
        messages=[{"role": "user", "content": "Count 1 to 3, one per line."}],
        stream=True,
        max_tokens=128,
    )
    for chunk in stream:
        if not chunk.choices:
            continue
        choice = chunk.choices[0]
        if choice.finish_reason:
            finish_reason = choice.finish_reason
        delta = choice.delta
        if delta is None:
            continue
        if delta.content or field(delta, "reasoning_content"):
            chunks_with_content += 1

    assert chunks_with_content >= 1
    assert finish_reason in ("stop", "length")


def test_chat_system_message_extraction(client: OpenAI) -> None:
    resp = client.chat.completions.create(
        model=ANTHROPIC_MODEL,
        messages=[
            {"role": "system", "content": "Answer in one short sentence."},
            {"role": "user", "content": "Say hello."},
        ],
        max_tokens=64,
    )
    assert resp.choices, f"no choices: {resp}"
    msg = resp.choices[0].message
    text = (msg.content or "").strip() or (field(msg, "reasoning_content") or "").strip()
    assert text, f"empty content and reasoning: {resp}"


def test_non_stream_preserves_vendor_fields(client: OpenAI) -> None:
    """Vendor extras (cost, prompt_cache_*) must ride through Response.Extra."""
    resp = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "say hi"}],
        max_tokens=128,
    )
    cost = field(resp, "cost")
    assert cost is not None, f"vendor 'cost' missing from response: {resp}"
    assert resp.usage is not None
    cache_miss = field(resp.usage, "prompt_cache_miss_tokens")
    assert cache_miss is not None, (
        f"vendor 'prompt_cache_miss_tokens' missing from usage: {resp.usage}"
    )


def test_chat_stream(client: OpenAI) -> None:
    """Stream deltas carry content/reasoning_content (ChoiceDelta/Delta nesting)."""
    request_start = time.monotonic()
    timestamps: list[float] = []
    chunks_with_payload = 0
    finish_reason: str | None = None

    stream = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "Count 1 to 5, one per line."}],
        stream=True,
        max_tokens=512,
    )
    for chunk in stream:
        timestamps.append(time.monotonic())
        if not chunk.choices:
            continue
        choice = chunk.choices[0]
        if choice.finish_reason:
            finish_reason = choice.finish_reason
        delta = choice.delta
        if delta is None:
            continue
        if delta.content or field(delta, "reasoning_content"):
            chunks_with_payload += 1

    assert len(timestamps) >= 2, f"too few chunks: {len(timestamps)}"
    assert finish_reason in ("stop", "length"), f"unexpected finish_reason: {finish_reason}"
    assert chunks_with_payload >= 1, (
        "no chunk carried content or reasoning_content — "
        "ChoiceDelta/Delta nesting regression"
    )

    first_byte = timestamps[0] - request_start
    total = timestamps[-1] - request_start
    assert first_byte < 10.0, f"first-byte too slow: {first_byte:.2f}s"
    assert total < 60.0, f"total stream too slow: {total:.2f}s"
    assert_streaming_progressive(timestamps, label="chat-stream")


def test_unregistered_client_key_is_rejected(gate_base_url: str) -> None:
    """Unregistered bearer token gets 401 (no bypass mode). ADR 003."""
    import openai as openai_pkg

    dummy = OpenAI(base_url=f"{gate_base_url}/v1", api_key="dummy-client-key")
    with pytest.raises(openai_pkg.AuthenticationError):
        dummy.chat.completions.create(
            model=MODEL,
            messages=[{"role": "user", "content": "hi"}],
            max_tokens=64,
        )


def test_unknown_model_fails(client: OpenAI) -> None:
    import openai as openai_pkg

    with pytest.raises(openai_pkg.BadRequestError):
        client.chat.completions.create(
            model="nonexistent-model-123",
            messages=[{"role": "user", "content": "hi"}],
            max_tokens=32,
        )
