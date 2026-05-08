# llmgate — agent context

OpenAI-wire-compatible LLM router with alias/fallback chains and per-vendor
circuit breakers. See `docs/architecture.md` for the request flow.

## Build / test

```bash
make build       # local binary (injects VERSION via ldflags)
make test        # Go unit
make e2e-mock    # cassette replay (free, deterministic)
make e2e         # real upstream (costs vendor credits)
```

## Versioning

SemVer in `VERSION` (single line). The cmd binary embeds it at build time
(`-X main.version=...`) and `llmgate --version` prints it.

## Release

1. Open PR titled `chore: release vX.Y.Z`.
2. Bump `VERSION`.
3. Merge.

`.github/workflows/release.yml` triggers on `VERSION` change and:
- Builds `linux/amd64,linux/arm64` image, pushes to
  `ghcr.io/cagojeiger/llmgate:<version>` + `:latest`.
- Tags the commit `vX.Y.Z` and creates a GitHub Release with
  auto-generated notes.
- Posts the result to Slack (`SLACK_WEBHOOK_URL` secret).

If the tag already exists, the workflow fails — bump VERSION first.

## Conventions

- Decisions: `docs/adr/`.
- Audit: one record per request (success and error paths).
- Comments explain WHY (hidden constraints, gotchas), not WHAT.
- Cassette fixtures live at
  `tests/e2e/fixtures/models/<id>/chat-completion.{json,sse}`, captured by
  `./scripts/refresh-fixtures.sh --record`.
