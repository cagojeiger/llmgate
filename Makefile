.PHONY: test tidy build clean run e2e

test:
	go test ./...

tidy:
	go mod tidy

build:
	go build -o bin/llmgate ./cmd/llmgate

run:
	go run ./cmd/llmgate

e2e:
	cd tests/e2e && uv run pytest

clean:
	rm -rf bin/
