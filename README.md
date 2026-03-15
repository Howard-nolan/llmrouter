# llmrouter

![Status](https://img.shields.io/badge/status-in%20progress-yellow)
![Go](https://img.shields.io/badge/Go-1.23-00ADD8?logo=go&logoColor=white)
![License](https://img.shields.io/badge/license-MIT-blue)

An LLM inference gateway in Go with semantic response caching, cost-aware model routing, and token-level streaming observability.

I document what I learn from each pull request in [**LEARNINGS.md**](./LEARNINGS.md).

**Status:** In progress. Core gateway, provider adapters, streaming, embedder, and semantic cache layer are implemented. Up next: cache integration into the request lifecycle, complexity classifier, and full Prometheus instrumentation.

---

## At a glance

- Unified `/v1/chat/completions` endpoint proxying Google Gemini and Anthropic Claude
- Semantic caching via in-process ONNX embeddings + Redis cosine similarity search
- Cost-aware routing: classifies prompt complexity, picks cheap or expensive model accordingly
- Full SSE streaming with tee-based cache write-through
- Prometheus metrics: request rates, TTFT, inter-token latency, cache hit ratio, cost tracking

---

## Architecture

```mermaid
flowchart TD
    Client([Client]) -->|POST /v1/chat/completions| Embedder
    Embedder[Embedder · ONNX] --> Cache
    Cache[(Redis Cache)] -->|hit| Response
    Cache -->|miss| Classifier
    Classifier[Complexity Classifier] -->|simple| Cheap[Cheap Model]
    Classifier -->|complex| Quality[Quality Model]
    Cheap --> Stream[SSE Stream]
    Quality --> Stream
    Stream -->|tokens| Response([Response])
    Stream -->|buffer| Cache
```

**Request lifecycle:**
1. Client sends a request to the unified `/v1/chat/completions` endpoint.
2. Embedder computes a 384-dim embedding of the prompt via in-process ONNX inference.
3. Cache layer searches Redis for semantically similar cached responses (SIMD-accelerated cosine similarity).
4. **Cache hit** → return stored response immediately.
5. **Cache miss** → complexity classifier scores the prompt and selects a cheap or expensive model within the target provider.
6. Provider adapter translates the request and streams the response to the client while buffering for cache write.
7. Metrics emitted at every stage.

## Quick Start

Start the local stack (Redis + Prometheus + Grafana):
```bash
docker-compose up -d
```

Run the gateway:
```bash
go run ./cmd/llmrouter
```

Send a request:
```bash
curl -N -X POST http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"auto","messages":[{"role":"user","content":"Explain TCP handshake"}],"stream":true}'
```

## API

| Method | Endpoint                | Description                          |
| ------ | ----------------------- | ------------------------------------ |
| POST   | /v1/chat/completions    | Chat completions (OpenAI-compatible) |
| GET    | /health                 | Liveness check + provider status     |
| GET    | /metrics                | Prometheus scrape target             |
| GET    | /cache/stats            | Cache hit rate, entry count          |
| POST   | /cache/flush            | Invalidate all cached entries        |

## Build & Test

```bash
go build ./cmd/llmrouter    # build
go test ./...               # test
golangci-lint run           # lint
```

## Observability

```bash
docker-compose up -d
```

- Gateway: http://localhost:8080
- Prometheus: http://localhost:9090
- Grafana: http://localhost:3000
