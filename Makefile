.PHONY: build test lint run docker-up docker-down bench bench-collect bench-quality

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
## Starts Docker services (Redis, Prometheus) if not already running.
run: docker-up
	go run ./cmd/llmrouter

## Start the local dev stack (Redis + Prometheus).
docker-up:
	docker compose up -d

## Stop the local dev stack.
docker-down:
	docker compose down

## Run the cache benchmark harness against a running gateway. Prints
## headline numbers (hit rate, cost saved, latency/TTFT percentiles).
bench:
	go test -tags bench -v -run TestCacheBenchmark -timeout 30m ./benchmarks/

## Run the bench harness AND collect a JSONL log for quality eval.
## Issues baseline calls on hits and cheap-routed misses. ~$1.50-3 in API calls.
bench-collect:
	LLMROUTER_LOG_PATH=data/realistic_records.jsonl \
	LLMROUTER_BASELINE_MODEL=claude-sonnet-4-5-20250929 \
	go test -tags bench -v -run TestCacheBenchmark -timeout 30m ./benchmarks/

## Judge collected records with Gemini 2.5 Pro and report per-path quality
## + overall preservation. Requires bench-collect to have run first.
bench-quality:
	cd benchmarks/python && uv run python label_eval.py
	cd benchmarks/python && uv run python eval_quality.py
