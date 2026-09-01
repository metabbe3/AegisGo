BINARY_AGENT := aegis-agent
BINARY_SERVE := aegis-serve

.PHONY: build test vet lint proto run-agent run-serve clone-refs tidy clean

## proto: regenerate gRPC stubs (requires protoc + protoc-gen-go[-grpc])
proto:
	PATH="$$PATH:$$HOME/go/bin" protoc \
		--go_out=./internal/pb --go_opt=paths=source_relative \
		--go-grpc_out=./internal/pb --go-grpc_opt=paths=source_relative \
		internal/pb/agent.proto

## build: compile both binaries into ./bin
build:
	go build -o bin/$(BINARY_AGENT) ./cmd/aegis-agent
	go build -o bin/$(BINARY_SERVE) ./cmd/aegis-serve
	go build -o bin/mcp-echo-server ./examples/mcp-echo-server

## test: run all unit tests
test:
	go test ./...

## vet: static analysis (run alongside test before every commit)
vet:
	go vet ./...

## lint: convenience alias; install staticcheck separately if wanted
lint: vet

# Non-generated packages: internal/pb is committed protoc output (make proto),
# exercised behaviorally through internal/grpcapi's in-process client.
# The gate counts TestGoFiles OR XTestGoFiles: a package whose tests live in
# the external foo_test package (internal/app does) is still tested.
PKGS := $(shell go list ./... | grep -v /internal/pb)

.PHONY: cover cover-html check e2e

## check: prove no escape hatches — no TODO/FIXME, no skipped tests, and
## every non-generated package has at least one test file.
check:
	@files=$$(find . -name '*.go' -not -path './reference/*' -not -path './bin/*'); \
	if grep -nE 'TODO|FIXME' $$files 2>/dev/null; then \
		echo "check: TODO/FIXME found — project law forbids them"; exit 1; \
	fi; \
	if grep -nE '\.Skip\(|\.Skipf\(|testing\.Short' $$files 2>/dev/null; then \
		echo "check: skipped tests found — project law forbids them"; exit 1; \
	fi; \
	bad=""; for p in $(PKGS); do \
		if [ "$$(go list -f '{{if or .TestGoFiles .XTestGoFiles}}y{{end}}' $$p)" != "y" ]; then bad="$$bad $$p"; fi; \
	done; \
	if [ -n "$$bad" ]; then echo "check: packages without tests:$$bad"; exit 1; fi; \
	echo "check: no TODO/FIXME, no skipped tests, every non-generated package tested"

## cover: unit tests with merged coverage profile; FAILS below COVER_MIN%.
COVER_OUT := coverage.out
COVER_MIN := 90
cover: check
	go test -covermode=atomic -coverprofile=$(COVER_OUT) $(PKGS)
	go tool cover -func=$(COVER_OUT) | tail -n 1
	@pct=$$(go tool cover -func=$(COVER_OUT) | awk '/^total:/ {sub(/%/,"",$$3); print $$3}'); \
	awk -v p=$$pct -v m=$(COVER_MIN) 'BEGIN { if (p+0 < m) { printf "FAIL: coverage %.1f%% < %d%%\n", p, m; exit 1 } printf "OK: coverage %.1f%% >= %d%%\n", p, m }'

cover-html: cover
	go tool cover -html=$(COVER_OUT)

## e2e: staged macOS end-to-end run against the real binaries (scripts/e2e.sh)
e2e:
	./scripts/e2e.sh

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
