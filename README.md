# llmgate (dev-v2)

OpenAI-wire-compatible LLM gateway. Logical model names resolve through a
catalog to ordered fallback chains; per-process circuit breakers suppress
dead upstreams.

## Layout

```
cmd/llmgate/                  HTTP gateway entrypoint
internal/config/              env-driven Server config
internal/catalog/             catalog.yaml: endpoints / models / aliases / fallback
internal/provider/            Provider interface + OpenAI-shaped types
internal/provider/openai/     OpenAI-protocol adapter
internal/provider/anthropic/  Anthropic-protocol adapter (response normalized to OpenAI wire)
internal/server/              HTTP handler, middleware, router server
internal/audit/               per-request audit Record + slog recorder
```

## Quick Start

```bash
cp .env.example .env
$EDITOR .env  # fill LLMGATE_OPENCODE_API_KEY

make test     # unit tests
make run      # start the gateway on :8080
```

Issue an OpenAI-compatible request:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

For end-to-end checks against upstream via the gateway:

```bash
make e2e-probe-via-gate
make e2e
```
