"""Auto-discovered matrix: every catalog model × non-stream / stream / function-call.
Function-call is live_only (cassette V1 doesn't cover multi-turn tool calls)."""

from __future__ import annotations

import pytest
from openai import OpenAI

from conftest import discover_catalog_models, field, raw_consumer_key


pytestmark = pytest.mark.timeout(180)

MODELS = discover_catalog_models()

WEATHER_TOOL = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "Get the current weather for a location",
        "parameters": {
            "type": "object",
            "properties": {
                "location": {"type": "string", "description": "City name"},
            },
            "required": ["location"],
        },
    },
}


@pytest.fixture
def client(gate_base_url: str) -> OpenAI:
    return OpenAI(base_url=f"{gate_base_url}/v1", api_key=raw_consumer_key())


@pytest.mark.parametrize("model", MODELS)
def test_nonstream(client: OpenAI, model: str) -> None:
    resp = client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with just one word: pong"}],
        max_tokens=128,
    )
    assert resp.choices, f"{model}: no choices in response"
    msg = resp.choices[0].message
    text = (msg.content or "").strip() or (field(msg, "reasoning_content") or "").strip()
    assert text, f"{model}: empty content and reasoning_content"


@pytest.mark.parametrize("model", MODELS)
def test_stream(client: OpenAI, model: str) -> None:
    chunks_with_payload = 0
    finish_reason: str | None = None
    stream = client.chat.completions.create(
        model=model,
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
            chunks_with_payload += 1
    assert chunks_with_payload >= 1, f"{model}: no payload chunks delivered"
    assert finish_reason in ("stop", "length"), f"{model}: finish_reason={finish_reason}"


@pytest.mark.live_only
@pytest.mark.parametrize("model", MODELS)
def test_function_call(client: OpenAI, model: str) -> None:
    resp = client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "What's the weather in Seoul? Use the tool."}],
        tools=[WEATHER_TOOL],
        tool_choice="auto",
        max_tokens=256,
    )
    assert resp.choices, f"{model}: no choices in response"
    msg = resp.choices[0].message
    tool_calls = msg.tool_calls or []
    assert tool_calls, f"{model}: no tool_calls returned (content={msg.content!r})"
    call = tool_calls[0]
    assert call.function.name == "get_weather", f"{model}: wrong fn {call.function.name!r}"
    args = call.function.arguments or ""
    assert "seoul" in args.lower(), f"{model}: arguments did not include Seoul ({args!r})"
