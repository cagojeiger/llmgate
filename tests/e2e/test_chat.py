"""End-to-end tests for /v1/chat/completions through the gateway.

These hit the real OpenCode upstream — costs tokens. Keep prompts short
and max_tokens reasonable. The point of these tests is regression: each
covers a class of bug we've already hit or could hit.
"""

from __future__ import annotations

import time

import pytest
from openai import OpenAI

from conftest import assert_streaming_progressive, field, raw_consumer_key


pytestmark = pytest.mark.timeout(120)

MODEL = "deepseek-v4-flash"


@pytest.fixture
def client(gate_base_url: str) -> OpenAI:
    # The gate validates the bearer token against consumers/ — a registered
    # raw key is required (no auth-bypass mode). Pull the documented key
    # from consumers/example.yaml so rotated keys flow through automatically.
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
    # DeepSeek is a reasoning model; on tight budgets content can be empty
    # while reasoning_content carries the thinking. Either is proof the
    # request reached upstream and a response came back through the gate.
    assert content or reasoning, f"both content and reasoning_content empty: {resp}"
    assert resp.usage is not None
    assert resp.usage.total_tokens > 0


@pytest.mark.parametrize("model", ["kimi-k2.6", "glm-5.1", "mimo-v2.5-pro"])
def test_chat_non_stream_other_models(client: OpenAI, model: str) -> None:
    resp = client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "say hi"}],
        max_tokens=64,
    )
    assert resp.choices, f"no choices from {model}: {resp}"
    msg = resp.choices[0].message
    text = (msg.content or "").strip() or (field(msg, "reasoning_content") or "").strip()
    assert text, f"{model}: empty content and reasoning"


@pytest.mark.parametrize("model", ["minimax-m2.5", "minimax-m2.7"])
def test_chat_anthropic_models_non_stream(client: OpenAI, model: str) -> None:
    # minimax can be a reasoning model — give enough budget that the
    # reply doesn't get truncated mid-thinking.
    resp = client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "say hi"}],
        max_tokens=256,
    )
    assert resp.choices, f"no choices from {model}: {resp}"
    msg = resp.choices[0].message
    text = (msg.content or "").strip() or (field(msg, "reasoning_content") or "").strip()
    assert text, f"{model}: empty content and reasoning"
    assert resp.usage is not None
    assert resp.usage.total_tokens > 0


def test_chat_anthropic_stream(client: OpenAI) -> None:
    chunks_with_content = 0
    finish_reason: str | None = None

    stream = client.chat.completions.create(
        model="minimax-m2.5",
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
        model="minimax-m2.5",
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
    """Regression: cost / prompt_cache_*_tokens must survive the typed gate.

    These are vendor-specific fields (OpenCode + DeepSeek) that we don't
    typed-model. They have to ride through Extra map[string]json.RawMessage
    on Response and Usage.
    """
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
    """Regression: stream deltas must carry content / reasoning_content.

    This is the bug we hit when ChoiceDelta was flat instead of nesting a
    Delta. If this test passes, the wire format and our typed model agree.
    """
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
    """Audit-always property: an unregistered bearer token gets 401.

    The gate validates Authorization against consumers/ — there is no
    bypass mode. ADR 003.
    """
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
