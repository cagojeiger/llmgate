# Event Contracts

Runtime event schemas live here because NATS subjects are external contracts.
Code constants, docs, and JSON Schema must move together.

Rules:

- Backward-compatible additions keep the same `schema_version` and subject.
- Breaking changes create a new schema file and a new subject suffix.
- Raw LLM result events are not fine-tuning JSONL. Dataset builders transform
  these events into training formats downstream.

Current contracts:

| Event | Subject | Schema |
|---|---|---|
| `llm.call.finalized` | `llmgate.llm.results.v1` | [`schemas/llm.call.finalized.v1.schema.json`](schemas/llm.call.finalized.v1.schema.json) |
