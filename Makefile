.PHONY: test tidy build clean run e2e e2e-mock lint

VERSION := $(shell cat VERSION)
LDFLAGS := -X main.version=$(VERSION)

test:
	go test ./...

tidy:
	go mod tidy

# Static analysis. Config lives in .golangci.yml; CI runs the same command.
# Requires `golangci-lint` on PATH (install via `go install
# github.com/golangci/golangci-lint/cmd/golangci-lint@latest`).
lint:
	golangci-lint run ./...

build:
	go build -ldflags "$(LDFLAGS)" -o bin/llmgate ./cmd/llmgate

run:
	go run ./cmd/llmgate

e2e:
	cd tests/e2e && uv run pytest

# Cassette mode: replays canned vendor responses from tests/e2e/fixtures/.
# No vendor credits, deterministic. Skips tests marked @pytest.mark.live_only.
# Decision rationale: docs/adr/006-cassette-e2e.md.
e2e-mock:
	cd tests/e2e && LLMGATE_E2E_MODE=cassette uv run pytest

clean:
	rm -rf bin/
