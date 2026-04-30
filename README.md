# llmrouter

![Cost reduction](https://img.shields.io/badge/cost%20reduction-~20%25-brightgreen)
![Quality preserved](https://img.shields.io/badge/quality%20preserved-94.5%25-brightgreen)
![Go](https://img.shields.io/badge/Go-1.23-00ADD8?logo=go&logoColor=white)
![License](https://img.shields.io/badge/license-MIT-blue)

An LLM inference gateway in Go with semantic response caching, cost-aware model routing, and token-level streaming observability.

I document what I learn from each pull request in [**LEARNINGS.md**](./LEARNINGS.md).

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

## Benchmarks

> **~20% cost reduction at 94.5% quality preservation** on a 199-prompt realistic-distribution corpus.

### Setup

| | |
|---|---|
| Corpus | 199 prompts across 104 clusters; QQP-derived paraphrases with a power-law cluster-size distribution to mimic real workloads (some questions repeat heavily, others are unique) |
| Cache threshold | Cosine similarity `T = 0.92` (chosen via [Parameter tuning](#parameter-tuning)) |
| Complexity threshold | `0.28`, F2-tuned (chosen via [Parameter tuning](#parameter-tuning)) |
| Run | `model="auto"`, `concurrency=3`, single 199-request pass via `make bench-collect`, gateway running locally with Redis |

### Cost savings

| Metric | Value |
|---|--:|
| Actual cost (199 requests) | $0.69 |
| Saved by cache | $0.12 |
| Saved by cheap-routing | ~$0.06 |
| **Total saved** | **~$0.18** |
| **Savings rate (vs naive baseline)** | **~20.4%** |

Naive baseline = every request routed to the quality model with no cache. The two levers are independent: caching avoids paying for near-duplicate prompts; cheap-routing avoids paying quality prices for cheap-model-adequate prompts.

Misses by routed model: Sonnet handled 148 prompts ($0.66), Haiku handled 22 ($0.03) — cheap-routing absorbed ~13% of misses.

### Latency

| Path | p50 | p95 | p99 | p50 TTFT |
|---|--:|--:|--:|--:|
| Cache hit | 52ms | 67ms | 67ms | 52ms |
| Cache miss | 8.65s | 11.31s | 14.46s | 1.47s |

Cache hits return tokens **~28× faster** to first byte than misses (52ms vs 1.47s p50 TTFT).

### Quality preservation

Methodology: Gemini 2.5 Pro judges each cache-hit and cheap-routed-miss response against a freshly-generated baseline from the quality model (Sonnet). Quality-routed misses are skipped — the gateway already routed to the baseline model, so a Sonnet-vs-Sonnet judge would just measure LLM stochasticity.

| Path | Count | Adequate | Rate |
|---|--:|--:|--:|
| Cache hit | 29 | 24 | 82.8% |
| Cheap-routed miss | 22 | 16 | 72.7% |
| Quality-routed miss | 148 | 148 | 100%* |
| **Total** | **199** | **188** | **94.5%** |

*\*Quality-routed misses are preserved by definition — the gateway routed to the baseline model.*

Two ways to read this:

- **End-user metric: 94.5%** — across all 199 requests, the user got an adequate answer 94.5% of the time. This is what users actually experience.
- **Engineering metric: 78.4%** — of the 51 requests where the gateway took a shortcut (cache hit or cheap-route), 40 were judged adequate. The other 148 went to the baseline model unchanged, so they're "preserved" by definition. The engineering metric is the more rigorous segmentation.

## Parameter tuning

Two thresholds need calibration: cache similarity (`T`) and complexity classifier output. Both were tuned against quality-labeled data, not picked by intuition.

### Cache similarity threshold

The cache uses a cosine similarity threshold to decide "close enough to serve a cached response":

- **Too low** → false hits: cached response served for a prompt it doesn't actually answer.
- **Too high** → cache becomes useless: misses on prompts that are genuine paraphrases.

**Method:**

1. **Offline hit-rate sweep.** Embed all clustered-corpus prompts with `all-MiniLM-L6-v2`, simulate "best-above-threshold" lookup for `T ∈ [0.75, 0.99]` step 0.005. Plots hit rate vs threshold.
2. **Live hit collection at T=0.75.** Run the harness against the gateway pinned at the loosest threshold — every hit logs `(original_prompt, new_prompt, cached_response, similarity)`. Hits at stricter T are a subset, so one run covers every candidate.
3. **LLM-as-judge labeling.** Gemini 2.5 Pro judges each `(cached_response, fresh_baseline)` pair under an *intrinsic adequacy* rubric: "is the cached response good enough to serve, with the fresh response as a calibration reference?" YES/NO labels.
4. **Pick T\*.** Compute false-hit rate (FHR) per threshold over the labeled records, pick the lowest T meeting the target FHR.

**Result:** `T* = 0.92` with ~10% FHR.

![Threshold selection curve](benchmarks/python/results/threshold_selection.png)

**Renegotiation:** Original target was 5% FHR. Data didn't permit it. The curve has a ~25% FHR plateau at `T ≤ 0.85` (loose QQP "near-duplicates"), drops to ~10% at `T = 0.92` (genuine paraphrases only), and the only thresholds hitting ≤ 5% FHR were `T ≥ 0.98` where hit rate collapsed to ~3% — cache provides almost zero value. Renegotiated to ~10% at `T = 0.92`, which captures ~22% hit rate. Tradeoff: 1 in 10 cache hits is inadequate, in exchange for meaningful cost savings.

### Complexity classifier

The router needs a binary signal: does this prompt need the expensive model, or is the cheap model adequate?

**Dataset:** 2,499 prompts from 8 sources (Dolly, OpenAssistant, MMLU, HumanEval, MBPP, GSM8K, ARC Challenge, Alpaca). Cheap and expensive responses collected for each, labeled by Gemini 2.5 Pro as LLM-as-judge ("is the cheap model's response adequate?"). Distribution: **79.1% adequate / 20.9% needs-expensive** — a 4:1 imbalance with a 79.2% majority-class baseline.

**Phase 1: MLP (dead end).** Tried PyTorch MLP (384→64→32→1) across multiple architectures and hyperparameter settings. Never learned. Every run collapsed to predicting all-adequate. 27K parameters on 2,499 noisy-labeled samples with weak signal in embedding space — the MLP couldn't find a decision surface, and lacked the inductive bias (axis-aligned splits) that makes tree models work on tabular data.

**Phase 2: Gradient-boosted trees.** Switched to scikit-learn's `GradientBoostingClassifier` with three deliberate choices:

- **Class weighting.** Initial GBT had 3% recall on label=1. Progressively increased `sample_weight` on the minority class — at 4× weighting, recall hit 91.3%.
- **F2 over F1.** Catching expensive prompts (recall) matters more than avoiding false alarms (precision). Switched threshold sweep from F1 to F2 (weights recall 2× over precision).
- **Handcrafted features (negative result).** Designed 6 task-structure features (subtask_count, constraint_count, reasoning_keyword_count, etc.) targeting complexity rather than topic. When combined with embeddings they showed 0% importance — the GBT preferred the 384 embedding dimensions. Tested alone, accuracy dropped below random. Dropped from model input.

A grid search over 4 GBT configs scored within 0.01 F2 of each other — the bottleneck is signal in the data, not model capacity.

**Final model:** GBT, 100 trees, depth 5, `lr=0.1`, 4× class weight, threshold 0.28.

| Metric | Value |
|---|--:|
| Recall on label=1 (catches expensive prompts) | 91.3% |
| Precision on label=1 | 25% |
| Adequate-correct at threshold | 28% |

**Tradeoff:** Conservative quality-first router. Rarely serves an inadequate response, but overspends on ~2/3 of easy prompts (sends them to the quality model when the cheap model would have been fine). Acceptable for a quality-first system; the cache catches some of the over-routing on near-duplicate easy prompts.

**Key takeaways:**

- Semantic embeddings encode topic, not complexity. "Who is Klaus Schwab?" and "How do solar panels work?" look similar in embedding space but have different complexity labels — the label depends on whether the cheap model *happened* to give a good answer.
- Small datasets favor tree models over MLPs.
- Class weighting was the biggest single lever (3% → 91% recall).
- Hyperparameters matter less than data quality.

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
