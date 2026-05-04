<div align="center">

![Cost reduction](https://img.shields.io/badge/cost%20reduction-20.4%25-brightgreen)
![Quality preserved](https://img.shields.io/badge/quality%20preserved-94.5%25-brightgreen)
![Go](https://img.shields.io/badge/Go-1.23-00ADD8?logo=go&logoColor=white)
![License](https://img.shields.io/badge/license-MIT-blue)

# llmrouter

---

**LLM inference gateway with semantic caching and cost-aware routing**

An LLM inference gateway in Go with semantic response caching, cost-aware model routing, streaming support, and a full observability suite.

</div>

I document what I learn from each pull request in [**LEARNINGS.md**](./LEARNINGS.md).

---

## What llmrouter does

**Cuts LLM API cost by 20.4% while preserving 94.5% of end-user response quality** on a 199-prompt realistic-distribution benchmark. llmrouter sits in front of Anthropic Claude and Google Gemini behind a unified OpenAI-compatible endpoint, and:

- **Routes by prompt complexity** — a gradient-boosted classifier scores each prompt and sends easy ones to a cheap model, hard ones to the expensive model.
- **Caches semantically similar responses** — in-process ONNX embeddings + Redis cosine similarity search. Paraphrased and repeat prompts return in 52ms p50, 28× faster than a fresh model call.
- **Streams tokens end-to-end** — tee pattern writes through to cache while delivering SSE to the client.
- **Emits full observability** — 17 Prometheus collectors covering request rate, TTFT, inter-token latency, cache hit ratio, and per-model cost, with a 13-panel Grafana dashboard out of the box.

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

## Benchmarks

> **20.4% cost reduction at 94.5% quality preservation** on a 199-prompt realistic-distribution corpus.

### Setup

| | |
|---|---|
| Corpus | 199 prompts across 104 clusters; QQP-derived paraphrases with a power-law cluster-size distribution to mimic real workloads (some questions repeat heavily, others are unique) |
| Cache threshold | Cosine similarity `T = 0.92` (chosen via [Parameter tuning](./TUNING.md#cache-similarity-threshold)) |
| Complexity threshold | `0.28`, F2-tuned (chosen via [Parameter tuning](./TUNING.md#complexity-classifier)) |
| Run | `model="auto"`, `concurrency=3`, single 199-request pass via `make bench-collect`, gateway running locally with Redis |

### Cost savings

| Metric | Value |
|---|--:|
| Actual cost (199 requests) | $0.69 |
| Saved by cache | $0.12 |
| Saved by cheap-routing | $0.06 |
| **Total saved** | **$0.18** |
| **Savings rate (vs naive baseline)** | **20.4%** |

Naive baseline = every request routed to the quality model with no cache. Sonnet handled 148 misses ($0.66), Haiku handled 22 ($0.03) — cheap-routing absorbed 13% of misses.

### Quality preservation

Methodology: Gemini 2.5 Pro judges each cache-hit and cheap-routed-miss response against a freshly-generated baseline from the quality model (Sonnet). Quality-routed misses are skipped — same model as baseline, so judging them would just measure LLM stochasticity.

| Path | Count | Adequate | Rate |
|---|--:|--:|--:|
| Cache hit | 29 | 24 | 82.8% |
| Cheap-routed miss | 22 | 16 | 72.7% |
| Quality-routed miss | 148 | 148 | 100%* |
| **Total** | **199** | **188** | **94.5%** |

*\*Quality-routed misses are preserved by definition — the gateway routed to the baseline model.*

Full methodology behind both thresholds: [TUNING.md](./TUNING.md).

## Observability

llmrouter ships with a 17-collector Prometheus suite and a 13-panel Grafana dashboard preprovisioned via `docker-compose`. Bring up the local stack and the dashboard is live with no extra setup.

```bash
docker-compose up -d
```

| Service    | URL                   |
|------------|-----------------------|
| Gateway    | http://localhost:8080 |
| Prometheus | http://localhost:9090 |
| Grafana    | http://localhost:3000 |

**Metric coverage:**

- **Request flow** — request rate, duration, and error counts by provider and error type.
- **Streaming** — time-to-first-token, inter-token latency, prompt and completion token counts.
- **Cost** — per-request and cumulative cost by provider and model, plus separate cache and routing savings counters so each lever can be attributed independently.
- **Cache** — similarity score histogram, entry count, hit/miss/skip status (hit rate derived in PromQL).
- **Routing** — decision counts by strategy and selected model, classifier complexity score distribution.
- **Inference** — embedding and classification durations.

<!-- Grafana dashboard screenshot to be added here -->

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
