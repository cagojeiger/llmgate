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

OpenAI SDK image input works through OpenAI-compatible providers such as
OpenRouter:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="example-key-001")
resp = client.chat.completions.create(
    model="google/gemini-3.1-flash-lite",
    messages=[{
        "role": "user",
        "content": [
            {"type": "text", "text": "What color is this image?"},
            {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}},
        ],
    }],
)
print(resp.choices[0].message.content)
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
