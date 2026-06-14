#!/usr/bin/env python3
"""Run the catalog-diff agent — emits a change manifest YAML to stdout.

Exit codes:
    0  no changes
    1  actions present, no warnings (safe to auto-apply)
    2  runtime error
    3  actions present + warnings (manual review required)
"""

import argparse
import json
import os
import pathlib
import sys

_HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(_HERE))

from dotenv import load_dotenv
from openai import OpenAI
import yaml

from runtime import Agent, Trace
from tools import (
    OPENCODE_DOC,
    analyze_and_build_manifest,
    extract_region,
    list_page_regions,
)

SYSTEM_PROMPT = """\
You are a catalog-diff agent. Locate the OpenCode Go endpoint table on
the given documentation URL, extract each row's model id AND protocol,
then hand the result to analyze_and_build_manifest. That final tool
fetches the OpenCode Go /v1/models API as the authoritative add/delete
source, fetches models.dev metadata for generated specs, and compares
only local OpenCode Go provider models.

Procedure:
1. list_page_regions(url).
2. Pick the region whose label/headers indicate the OpenCode Go endpoint
   table with Model ID and AI SDK Package columns (NOT a
   rate-limit, pricing, or navigation table). Call extract_region.
3. For each row identify:
   - model id (e.g. deepseek-v4-pro, kimi-k2.6)
   - protocol — from the "AI SDK Package" column:
       "@ai-sdk/anthropic"          -> "anthropic"
       "@ai-sdk/openai-compatible"  -> "openai"
       "@ai-sdk/alibaba"            -> "openai"  (alibaba is openai-wire compatible)
4. analyze_and_build_manifest(source_url=<url>,
                              remote=[{"id":..., "protocol":...}, ...]).
   Then stop.
"""


def _human_trace_sink(event: dict) -> None:
    kind = event.get("kind")
    if kind == "llm_request":
        print(f"T{event['turn']} → LLM", file=sys.stderr, flush=True)
    elif kind == "llm_response":
        calls = event.get("tool_calls") or []
        for c in calls:
            args = c.get("args") or ""
            args_str = args if len(args) < 100 else args[:97] + "..."
            print(f"     · pick: {c['name']}({args_str})", file=sys.stderr, flush=True)
        if not calls:
            content = (event.get("content") or "").strip() or "(no content)"
            print(f"     · agent done speaking: {content[:120]!r}", file=sys.stderr, flush=True)
    elif kind == "tool_result":
        keys = event.get("result_keys") or []
        print(f"     · {event['name']} ← {keys}", file=sys.stderr, flush=True)
    elif kind == "done":
        print(f"✓ done ({event['reason']})", file=sys.stderr, flush=True)


def main() -> int:
    load_dotenv(_HERE.parents[1] / ".env")
    key = os.environ.get("LLMGATE_CONSUMER_KEY")
    if not key:
        print("error: LLMGATE_CONSUMER_KEY is unset (see .env.example)", file=sys.stderr)
        return 2

    parser = argparse.ArgumentParser()
    parser.add_argument("-v", "--verbose", action="store_true",
                        help="emit a human-readable ReAct progress log to stderr")
    parser.add_argument("--trace-json", action="store_true",
                        help="emit JSON-lines ReAct trace to stderr (machine-readable)")
    args = parser.parse_args()

    if args.trace_json:
        trace = Trace(sink=lambda e: print(json.dumps(e, ensure_ascii=False), file=sys.stderr, flush=True))
    elif args.verbose:
        trace = Trace(sink=_human_trace_sink)
        print(f"agent: starting (url={OPENCODE_DOC})", file=sys.stderr, flush=True)
    else:
        trace = Trace()

    client = OpenAI(
        base_url=os.environ.get("LLMGATE_BASE_URL", "https://llmgate.project-jelly.io/v1"),
        api_key=key,
    )
    agent = Agent(
        client=client,
        system_prompt=SYSTEM_PROMPT,
        tools=[list_page_regions, extract_region, analyze_and_build_manifest],
    )

    try:
        result = agent.run(
            f"Diff the catalog at {OPENCODE_DOC} against the local one.",
            trace=trace,
        )
    except Exception as e:
        print(f"error: {e}", file=sys.stderr)
        return 2

    print(yaml.safe_dump(result, sort_keys=False, allow_unicode=True, default_flow_style=False))

    if args.verbose:
        s = result.get("summary", {})
        print(
            f"agent: manifest emitted — {s.get('action_count', 0)} actions, "
            f"{s.get('warning_count', 0)} warnings",
            file=sys.stderr, flush=True,
        )

    if result.get("warnings"):
        return 3
    if result.get("actions"):
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
