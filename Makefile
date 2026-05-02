.PHONY: test tidy build clean run e2e-probe-via-gate e2e-probe-via-gate-stream e2e

test:
	go test ./...

tidy:
	go mod tidy

build:
	go build -o bin/llmgate ./cmd/llmgate

run:
	go run ./cmd/llmgate

e2e-probe-via-gate:
	uv run scripts/probe_upstream.py --via-gate

e2e-probe-via-gate-stream:
	uv run scripts/probe_upstream.py --via-gate --stream

e2e:
	cd tests/e2e && uv run pytest

clean:
	rm -rf bin/
