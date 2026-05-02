# llmgate (dev-v2)

OpenAI-wire-compatible LLM gateway. Logical model names resolve through a
catalog to ordered fallback chains; per-process circuit breakers suppress
dead upstreams.

## Layout

```
cmd/llmgate/                  HTTP gateway entrypoint
catalog/                      data only — operator-facing yaml directory
  models/<id>.yaml            one yaml per endpoint (id + vendor + type +
                              base_url + auth_env)
  aliases/<name>.yaml         one yaml per alias (alias + chain)
internal/catalog/             loader package (yaml -> Catalog struct)
internal/config/              env-driven Server config (incl. router tuning)
internal/provider/            Provider interface + OpenAI-shaped types
internal/provider/openai/     OpenAI-protocol adapter
internal/provider/anthropic/  Anthropic-protocol adapter (response normalized to OpenAI wire)
internal/router/              alias→chain dispatch + fallback + circuit breaker
internal/server/              chi handler, sseWriter, error envelope, middleware
internal/audit/               per-request audit Record + slog recorder
docs/adr/                     accepted decisions
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

Aliases work the same way (`coder`, `reasoning`, `cheap-fast`):

```bash
curl http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"coder","messages":[{"role":"user","content":"hi"}]}'
```

The gateway resolves the alias via `catalog/default/fallback/coder.yaml` and
walks the chain on fallback-eligible errors.

## Catalog overrides

`make run` reads from `./catalog` by default (relative to cwd, so always
run from the repo root). Set `LLMGATE_CATALOG=/path/to/dir` to point at
an external directory instead. The directory must contain `models/` (one
yaml per endpoint) and may contain `aliases/`. Router policy
(`LLMGATE_FALLBACK_ON`, `LLMGATE_CIRCUIT_FAILURES`,
`LLMGATE_CIRCUIT_OPEN_DURATION`) lives in env, not yaml.
Hot-reload is not supported — change the catalog and restart.

## Run in a container

`compose.yaml` bind-mounts `./catalog` read-only into the container and
reads `LLMGATE_OPENCODE_API_KEY` from `.env`, so editing yaml on the
host flows through without rebuilding the image:

```bash
docker compose up --build
# (in another shell)
curl http://localhost:8080/healthz
docker compose down
```

The image is distroless static, ~12MB, runs as nonroot. Suitable as a
starting point for k8s manifests / configMap mounts.

## End-to-end checks against upstream

```bash
make e2e
```
