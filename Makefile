.PHONY: build test lint run docker-up docker-down

# Tell the linker where to find libtokenizers.a (CGo static library for the
# HuggingFace tokenizer). This is needed at compile time for any target that
# builds the embedder package.
export CGO_LDFLAGS := -L$(CURDIR)/lib

## Build the gateway binary into the repo root.
build:
	go build ./cmd/llmrouter

## Run all tests with the race detector enabled.
test:
	go test -race ./...

## Run golangci-lint (must be installed: https://golangci-lint.run/usage/install/).
lint:
	golangci-lint run

## Compile and run the gateway in one step (no binary produced).
run:
	go run ./cmd/llmrouter

## Start the local dev stack (Redis + Prometheus).
docker-up:
	docker compose up -d

## Stop the local dev stack.
docker-down:
	docker compose down
