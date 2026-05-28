# llmgate

OpenAI-wire-compatible LLM router. Callers keep using the OpenAI SDK
and send a logical alias (`smart`, `worker`, `cheap`); llmgate
resolves the alias to an ordered fallback chain, talks to the right
vendor, and emits one audit record per request.

```
caller ──┐                          ┌── openai
         │  POST /v1/chat/completions
         ▼                          ▼
     ┌────────┐  alias → chain   ┌──── anthropic
     │ llmgate│ ────────────────▶│
     └────────┘  per-vendor      ├──── deepseek
                 circuit breaker └── …
```

## Why

- **Vendor SDKs, keys, fallback, wire conversion, circuit breaking**
  are not part of the caller's task. llmgate absorbs them so caller
  code stays focused on the prompt and parameters it actually wants
  to send.
- **Model policy lives in YAML, not in caller code.** Operators
  rename / re-order an alias's chain in one place; caller code does
  not change.
- **Internal-only gateway.** llmgate is intentionally not a
  multi-tenant SaaS control plane — no per-user quotas, no model
  metadata service, no `/v1/models` discovery. See
  [docs/adr/000-identity.md](docs/adr/000-identity.md) for the
  scope decision and the explicit non-goals.

## Run locally

```bash
cp .env.example .env   # fill the provider API keys you use
make run               # listens on :8080
```

```bash
curl http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer example-key-001' \
  -H 'Content-Type: application/json' \
  -d '{"model":"smart","messages":[{"role":"user","content":"hi"}]}'
```

## Test

```bash
make test       # Go unit
make e2e-mock   # cassette replay (free, deterministic)
make e2e        # real upstream — costs vendor credits
make lint       # golangci-lint (same config CI runs)
```

## Deploy

Container images are published to
`ghcr.io/cagojeiger/llmgate:<version>` (and `:latest`) on every
release tag. A release is triggered by bumping `VERSION` in `main` —
see the [release workflow](.github/workflows/release.yml) for the
chain that builds `linux/amd64,linux/arm64`, tags the commit, and
publishes the GitHub Release.

## Observability

llmgate emits one **audit record per request** (success and error
paths alike) so the call graph is reconstructible from logs. Optional
JetStream emission carries the same record to a downstream consumer.
The [logs](docs/logs.md) and [metrics](docs/metrics.md) policy docs
describe the field shape; ADR
[005-timeout-authority](docs/adr/005-timeout-authority.md) explains
the timeout model that drives the call-event timing.

## Documentation map

- [`docs/adr/`](docs/adr/) — design decisions (start with
  [000-identity](docs/adr/000-identity.md), the parent of every
  other ADR).
- [`docs/config.md`](docs/config.md) — runtime environment contract.
- [`docs/data.md`](docs/data.md) — catalog and consumer file
  contract.
- [`docs/logs.md`](docs/logs.md),
  [`docs/metrics.md`](docs/metrics.md) — observability policy.

## Contributing

Patches and audit reports are welcome. Before opening a PR:

- `make test` and `make lint` must pass.
- Comments explain **WHY** (hidden constraints, gotchas), not what —
  the code is the WHAT.
- Decisions that change ADR-level scope go into a new `docs/adr/`
  entry; smaller changes stay in commit messages.

For **security issues**, follow
[`SECURITY.md`](SECURITY.md) — do **not** open a public issue.

## License

Apache 2.0 — see [`LICENSE`](LICENSE).
