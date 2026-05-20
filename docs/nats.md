# Local NATS JetStream

This compose setup is for learning and local integration testing before
llmgate publishes training examples itself.

## Components

- `nats` — the NATS server. JetStream is enabled with `--jetstream`, so it can
  persist messages to `/data`.
- `nats-box` — an optional CLI container for inspecting streams, publishing test
  messages, and creating consumers. It is under the `tools` compose profile so
  it does not run with the normal app stack.
- `nats-data` — the Docker volume that keeps JetStream files across container
  restarts.

Ports:

- `4222` — NATS client port. Applications and CLI tools connect here.
- `8222` — HTTP monitoring port. Useful endpoints include `/varz` and `/jsz`.

## Start

```bash
docker compose up -d nats
curl http://localhost:8222/varz
```

## Create A Training Stream

When `LLMGATE_NATS_URL` is configured, llmgate creates or updates this stream
on startup. The CLI command is still useful for learning or inspecting the
local server by hand.

```bash
docker compose run --rm nats-box \
  nats stream add LLMGATE_LLM_RESULTS \
  --subjects llmgate.llm.results.v1 \
  --storage file \
  --retention limits \
  --defaults
```

## Publish A Sample

Use JetStream publish so the CLI waits for the server's storage acknowledgment.

```bash
docker compose run --rm nats-box \
  nats pub llmgate.llm.results.v1 \
  '{"schema_version":1,"event_type":"llm.call.finalized","event_id":"demo-1","request_id":"demo-1","timestamp":"2026-05-20T00:00:00Z","completed_at":"2026-05-20T00:00:01Z","duration_ms":1000,"status":"success","operation":"chat.completions","wire_format":"openai.chat.completions","service":{},"request":{"available":true,"raw_json":{"model":"demo","messages":[{"role":"user","content":"hi"}]}},"response":{"available":true,"raw_json":{"choices":[{"message":{"role":"assistant","content":"hello"}}]}},"stream":{"enabled":false}}' \
  --jetstream
```

## Inspect

```bash
docker compose run --rm nats-box nats stream info LLMGATE_LLM_RESULTS
docker compose run --rm nats-box nats stream get LLMGATE_LLM_RESULTS 1 --json
```

For llmgate's training-data use case, publish one `llm.call.finalized` event per
request after the request finishes. Streaming chunks are reassembled into the
final OpenAI-compatible response before publishing. The producer may batch
publish calls for throughput, but each JetStream message still contains exactly
one finalized event.

Default producer buffering:

```text
workers:        1 per llmgate process
batch size:     100 events
batch max wait: 1s
```

JetStream sequence is storage order. If multiple llmgate processes publish to
the same stream, sort downstream datasets by `completed_at` and then
`request_id`, not by stream sequence alone.
