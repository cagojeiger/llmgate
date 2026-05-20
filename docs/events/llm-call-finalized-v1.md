# llm.call.finalized v1

`llm.call.finalized` is emitted once per upstream-attempted LLM request after
llmgate has finalized the client-visible OpenAI-compatible result.

NATS:

```text
stream:  LLMGATE_LLM_RESULTS
subject: llmgate.llm.results.v1
```

Source of truth:

- `request.raw_json` is the OpenAI-compatible request received by llmgate.
- `response.raw_json` is the OpenAI-compatible final response produced by
  llmgate. For streaming requests this is reassembled from emitted chunks.
- `stream.events` contains the OpenAI-compatible chunks emitted to the client
  when the original request used `stream: true`.
- `timestamp` is the request start time; `completed_at` and `duration_ms` are
  the preferred fields for analysis-time ordering and latency calculations.
- Vendor-native request / response bodies are not part of this contract.

JetStream sequence is storage order, not global request-time order across
multiple llmgate processes. Downstream analysis should sort by `completed_at`
and then `request_id` when it needs a stable timeline.

Only the envelope fields are assumed present. `choices`, `content`, `usage`,
`model_used`, `vendor`, tool fields, and reasoning fields are optional because
they vary by provider, failure point, and model behavior.

Status values:

```text
success
error
partial
client_closed
```

Versioning:

- Additive optional fields stay on `schema_version: 1`.
- Field removals, renamed fields, or semantic changes require
  `schema_version: 2` and a new subject such as `llmgate.llm.results.v2`.
