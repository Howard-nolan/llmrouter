.PHONY: build test lint run docker-up docker-down

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
