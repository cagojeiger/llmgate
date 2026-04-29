# llmgate (dev-v2)

Provider-first LLM gateway. The `Provider` interface is the spine; both the
HTTP server and a CLI probe consume it through the exact same contract.

V1 ships a single OpenCode Go adapter targeting `deepseek-v4-flash` and a
non-streaming text-only request shape.

## Layout

```
cmd/llmgate-probe/    CLI: stdin OpenAI request -> stdout OpenAI response
internal/config/      env-driven Config
internal/provider/    Provider interface + OpenAI-shaped types
internal/provider/opencode/   OpenCode Go HTTP adapter
```

## Quick Start

```bash
cp .env.example .env
$EDITOR .env  # fill LLMGATE_OPENCODE_API_KEY

make test     # unit tests (httptest mocks)
make probe    # real upstream call: POST /chat/completions -> deepseek-v4-flash
```

Custom prompt or full OpenAI request:

```bash
go run ./cmd/llmgate-probe -prompt "say pong" -max-tokens 32
echo '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}' \
  | go run ./cmd/llmgate-probe
```
