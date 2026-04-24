# Offline similarity-threshold sweep

Replays the llmrouter gateway's cache-lookup logic in pure Python against a
pre-embedded corpus, so we can see the full hit-rate-vs-threshold curve
without running the gateway or burning provider calls.

## Why a separate Python project

The gateway itself uses an ONNX-exported MiniLM for embeddings. Running the
same model from Python via `sentence-transformers` lets us sweep thousands
of (threshold × prompt) combinations in seconds on a pairwise cosine matrix,
while keeping the production Go path untouched.

## Input contract

We don't replicate Go's PCG shuffle in Python. Instead, the Go
`benchmarks/dumporder` binary emits the shuffled prompt order as a JSON
array, and this sweep reads it back. Same ordering → same path-dependent
cache state → apples-to-apples comparison with the harness.

```bash
# from repo root
go run ./benchmarks/dumporder benchmarks/data/corpus_clustered.json \
  > benchmarks/data/corpus_clustered_order.json
```

## Running

```bash
cd benchmarks/python
uv sync
uv run python sweep_hitrate.py \
  --order ../data/corpus_clustered_order.json \
  --out-dir results \
  --t-min 0.75 --t-max 0.99 --t-step 0.005
```

Outputs `results/hitrate_sweep.csv` and `results/hitrate_sweep.png`.

## Simulation semantics

For each threshold T we iterate through the prompts in order, maintaining
a list of kept (cached) indices. For prompt `i` we take the max cosine
similarity against all kept entries. If `max >= T` it's a hit; otherwise
we append `i` to the kept list. This mirrors the gateway's
best-above-threshold lookup in [internal/cache/redis.go](../../internal/cache/redis.go).
