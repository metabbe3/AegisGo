BINARY_AGENT := aegis-agent
BINARY_SERVE := aegis-serve

.PHONY: build test vet lint run-agent run-serve clone-refs tidy clean

## build: compile both binaries into ./bin
build:
	go build -o bin/$(BINARY_AGENT) ./cmd/aegis-agent
	go build -o bin/$(BINARY_SERVE) ./cmd/aegis-serve

## test: run all unit tests
test:
	go test ./...

## vet: static analysis (run alongside test before every commit)
vet:
	go vet ./...

## lint: convenience alias; install staticcheck separately if wanted
lint: vet

## run-agent: run the CLI agent against testdata (needs provider env vars)
run-agent:
	go run ./cmd/aegis-agent

## run-serve: run the HTTP agent service locally on :8080
run-serve:
	go run ./cmd/aegis-serve

## clone-refs: shallow-clone upstream libraries into reference/ for offline reading
clone-refs:
	git clone --depth 1 https://github.com/mark3labs/mcp-go reference/mcp-go || true
	git clone --depth 1 https://github.com/microsoft/agent-framework-go reference/agent-framework-go || true

## tidy: prune and refresh go.sum
tidy:
	go mod tidy

clean:
	rm -rf bin
