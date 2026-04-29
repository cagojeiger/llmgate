.PHONY: test tidy probe build clean

test:
	go test ./...

tidy:
	go mod tidy

build:
	go build -o bin/llmgate-probe ./cmd/llmgate-probe

probe:
	go run ./cmd/llmgate-probe -prompt "ping" -raw

clean:
	rm -rf bin/
