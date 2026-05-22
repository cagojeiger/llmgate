# Security policy

## Reporting a vulnerability

**Do not open a public GitHub issue for a security report.** A public
issue makes the disclosure visible before a fix is available and would
put every operator on this version at risk.

Use **GitHub's private vulnerability reporting** instead:

1. Open <https://github.com/cagojeiger/llmgate/security/advisories/new>.
2. Describe the issue with enough detail to reproduce (request shape,
   environment variables, observed vs. expected behavior).
3. Include any logs / stack traces / payloads that helped you find it
   — feel free to redact prompts and consumer keys.

A confirmation is sent within **3 business days**. Initial assessment
follows within **7 business days**; for a confirmed issue a fix
timeline is communicated in that response.

## Supported versions

Only the **latest released tag** receives security fixes. Older tags
are not patched — pull `ghcr.io/cagojeiger/llmgate:latest` (or the
specific newer `vX.Y.Z`) to roll up to the fix.

| Version | Supported |
|---------|-----------|
| latest  | ✅        |
| < latest | ❌        |

## Scope

In scope:

- The Go code in this repository (`internal/`, `cmd/`).
- The container image published to `ghcr.io/cagojeiger/llmgate`.
- The configuration formats (`catalog/*.yaml`, `consumers/*.yaml`)
  insofar as they accept untrusted input from the operator.

Out of scope:

- Findings against vendor APIs upstream of llmgate (OpenAI,
  Anthropic, DeepSeek…). Report those to the vendor.
- Issues that require trusted-operator access (e.g. "I edited
  `consumers/*.yaml` to add a key and now my key works"). Operators
  are inside the trust boundary; the consumers file is a private
  config surface.
- Social-engineering against the maintainer or against the GitHub
  org itself.

## What we ask of reporters

- Give us a reasonable window (usually 90 days) before public
  disclosure. Earlier if we miss the SLA above.
- Don't run automated scanning against any deployment you don't own.
  llmgate is operator-deployed software — every running instance is
  someone else's production.

## Hall of fame

Reporters who follow this policy are credited in the release notes
of the fix, unless they ask to stay anonymous.
