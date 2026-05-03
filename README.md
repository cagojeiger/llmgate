# llmgate (dev-v2)

OpenAI-wire-compatible LLM gateway. Logical model names resolve through a
catalog to ordered fallback chains; per-process circuit breakers suppress
dead upstreams.

## Layout

```
cmd/llmgate/                  HTTP gateway entrypoint
catalog/                      vendor-side yaml directory (operator-facing)
  models/<id>.yaml            one yaml per model (id + vendor + protocol +
                              base_url + auth_env + auth_scheme)
  aliases/<name>.yaml         one yaml per alias (alias + chain)
clients/                      caller-side yaml directory (operator-facing)
  <name>.yaml                 one yaml per caller (name + sha256 key_hashes;
                              raw keys never live on disk)
scripts/gen-client.sh         helper to issue one caller (raw key + sha256 yaml)
internal/catalog/             vendor catalog loader (yaml -> Catalog struct)
internal/clients/             caller registry loader (yaml -> Store, sha256 lookup)
internal/config/              env-driven Server config (incl. router tuning)
internal/provider/            Provider interface + OpenAI-shaped types
internal/provider/openai/     OpenAI-protocol adapter
internal/provider/anthropic/  Anthropic-protocol adapter (response normalized to OpenAI wire,
                              tools / tool_calls / tool_use translation in both directions)
internal/router/              alias→chain dispatch + fallback + circuit breaker
internal/server/              chi handler, auth middleware, streamResponder, sseWriter, errors
internal/audit/               per-request audit Record (incl. client_name / client_key_id)
docs/adr/                     accepted decisions
```

## Quick Start

```bash
cp .env.example .env
$EDITOR .env  # fill LLMGATE_OPENCODE_API_KEY

make test     # unit tests
make run      # start the gateway on :8080
```

The gateway requires at least one registered caller — the `clients/`
directory ships with `example.yaml` (raw keys `example-key-001` /
`example-key-002` documented in its comments) so `make run` boots
straight away. For real deployments register your own caller:

```bash
./scripts/gen-client.sh acme-prod
# prints the raw key once — store it in your secret manager.
```

Issue an OpenAI-compatible request (note the `Authorization: Bearer`
header — without it the request is rejected with 401, and an audit
record is still emitted):

```bash
curl http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer example-key-001' \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

Aliases work the same way (`cheap`, `worker`, `smart`, `planner`):

```bash
curl http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer example-key-001' \
  -H 'Content-Type: application/json' \
  -d '{"model":"smart","messages":[{"role":"user","content":"hi"}]}'
```

The gateway resolves the alias via `catalog/aliases/smart.yaml` and walks
the chain on fallback-eligible errors. Alias intent / chain rationale lives
in yaml comments — `description` is not a data field. See
`catalog/aliases/example.yaml.example` for the template.

## Caller (client) registration

Every request to `/v1/chat/completions` must carry
`Authorization: Bearer <raw-key>`. `/healthz` stays public so liveness
probes work without a key. Raw keys never live on disk — only their
sha256 hashes do — so a caught `clients/` directory leak does not
expose the keys themselves.

Register a new caller:

```bash
./scripts/gen-client.sh acme-prod
# wrote clients/acme-prod.yaml
# raw key (give to caller, gateway never sees it again):
#   <64 hex chars>
```

Rotate a key by editing the `key_hashes` array — add the new sha256,
deploy, switch the caller over, then remove the old hash on the next
deploy. Multiple hashes per caller are valid; both keys authenticate
during the rotation window. The audit record's `client_key_id` field
(first 8 hex of the matched hash) tracks which key each call used.

`./clients` is the default; override with `LLMGATE_CLIENTS=/path/to/dir`.
A missing or empty directory fails boot — there is no anonymous mode.
Decisions and trade-offs in `docs/adr/008-clients.md`.

## Catalog overrides

`make run` reads from `./catalog` by default (relative to cwd, so always
run from the repo root). Set `LLMGATE_CATALOG=/path/to/dir` to point at
an external directory instead. The directory must contain `models/` (one
yaml per model) and may contain `aliases/`. Yaml is parsed strictly —
unknown fields (typos, stale `type:` / `specs:` / `notes:` blocks) fail
boot. Use `models/example.yaml.example` and `aliases/example.yaml.example`
as templates. Router/server policy (`LLMGATE_FALLBACK_ON`, circuit breaker
settings, `LLMGATE_REQUEST_TIMEOUT`, `LLMGATE_COMPLETE_TIMEOUT`,
`LLMGATE_STREAM_IDLE_TIMEOUT`) lives in env, not yaml. Hot-reload is not
supported — change the catalog and restart.

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
