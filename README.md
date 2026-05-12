# llmgate

OpenAI-wire-compatible LLM router. Logical model names resolve to ordered
fallback chains; per-vendor circuit breakers suppress dead upstreams.
Bearer-key auth, one audit record per request.

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
```

## Docs

- `docs/adr/` — design decisions
- `docs/architecture.md` — request flow + components

## License

Apache 2.0 — see `LICENSE`.
