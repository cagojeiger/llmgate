"""Reusable agent runtime — @tool decorator, Agent (ReAct loop), Trace."""

from __future__ import annotations

import inspect
import json
import typing
from dataclasses import dataclass, field
from typing import Any, Callable

_PRIMITIVES = {
    str:   {"type": "string"},
    int:   {"type": "integer"},
    float: {"type": "number"},
    bool:  {"type": "boolean"},
}


def _annotation_to_schema(annotation: Any) -> dict:
    if annotation in _PRIMITIVES:
        return _PRIMITIVES[annotation]
    if annotation is dict:
        return {"type": "object"}
    origin = typing.get_origin(annotation)
    if origin is list:
        args = typing.get_args(annotation)
        inner = _annotation_to_schema(args[0]) if args else {"type": "string"}
        return {"type": "array", "items": inner}
    if origin is dict:
        return {"type": "object"}
    raise TypeError(f"unsupported tool annotation: {annotation!r}")


def tool(fn: Callable) -> Callable:
    sig = inspect.signature(fn)
    hints = typing.get_type_hints(fn)
    properties: dict[str, dict] = {}
    required: list[str] = []
    for name, p in sig.parameters.items():
        annotation = hints.get(name)
        if annotation is None:
            raise TypeError(f"{fn.__name__}({name}) missing type annotation")
        properties[name] = _annotation_to_schema(annotation)
        if p.default is inspect.Parameter.empty:
            required.append(name)
    description = (inspect.getdoc(fn) or "").split("\n\n", 1)[0].replace("\n", " ").strip()
    fn.spec = {
        "type": "function",
        "function": {
            "name": fn.__name__,
            "description": description,
            "parameters": {"type": "object", "properties": properties, "required": required},
        },
    }
    return fn


@dataclass
class Trace:
    sink: Callable[[dict], None] = field(default=lambda _: None)

    def step(self, kind: str, **payload: Any) -> None:
        self.sink({"kind": kind, **payload})


@dataclass
class Agent:
    client: Any
    system_prompt: str
    tools: list[Callable]
    model: str = "cheap"
    max_steps: int = 8

    def run(self, user_message: str, trace: Trace | None = None) -> dict:
        trace = trace or Trace()
        specs = [t.spec for t in self.tools]
        dispatch = {t.__name__: t for t in self.tools}
        messages: list[dict] = [
            {"role": "system", "content": self.system_prompt},
            {"role": "user",   "content": user_message},
        ]
        final_payload: dict | None = None

        for step in range(self.max_steps):
            trace.step("llm_request", turn=step + 1, messages_len=len(messages))
            resp = self.client.chat.completions.create(
                model=self.model, messages=messages, tools=specs, tool_choice="auto",
            )
            msg = resp.choices[0].message
            messages.append(_assistant_entry(msg))
            trace.step(
                "llm_response", turn=step + 1, content=msg.content,
                tool_calls=[{"name": tc.function.name, "args": tc.function.arguments}
                            for tc in (msg.tool_calls or [])],
            )
            if not msg.tool_calls:
                trace.step("done", reason="no_tool_calls")
                break

            for call in msg.tool_calls:
                name = call.function.name
                if name not in dispatch:
                    raise RuntimeError(f"unknown tool: {name!r}")
                args = json.loads(call.function.arguments or "{}")
                trace.step("tool_call", name=name, args=args)
                result = dispatch[name](**args)
                trace.step("tool_result", name=name,
                           result_keys=list(result.keys()) if isinstance(result, dict) else None)
                if isinstance(result, dict) and result.get("final"):
                    final_payload = result
                messages.append({"role": "tool", "tool_call_id": call.id,
                                 "content": json.dumps(result, ensure_ascii=False)})
            if final_payload is not None:
                trace.step("done", reason="final_flag")
                break
        else:
            raise RuntimeError(f"agent exceeded {self.max_steps} steps")

        if final_payload is None:
            raise RuntimeError("agent finished without final payload")
        return final_payload


def _assistant_entry(msg: Any) -> dict:
    # DeepSeek thinking mode rejects subsequent turns unless its
    # reasoning_content is echoed — model_dump preserves vendor fields.
    entry = msg.model_dump(exclude_unset=True, exclude_none=True)
    entry["role"] = "assistant"
    return entry
