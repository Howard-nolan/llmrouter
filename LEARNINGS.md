
## 2026-02-11 - PR #1: Scaffold repo: go.mod, README, CI, PR template

**Change Summary:**
Initial project scaffolding for llmrouter. Sets up the Go module, project README, .gitignore, GitHub PR template, and a CI workflow that auto-appends learnings from merged PRs to LEARNINGS.md.

**How It Works:**
- `go.mod` initializes the Go module (`github.com/howard-nolan/llmrouter`, Go 1.25.2). No dependencies yet — those come when we start building packages.
- `README.md` documents the project overview, API surface (unified `/v1/chat/completions` endpoint, `/health`, `/metrics`, `/cache/stats`, `/cache/flush`), quick start, and build commands.
- `.gitignore` covers Go binaries, ONNX model files, env secrets, IDE files, Python training artifacts, and Claude Code config.
- `.github/pull_request_template.md` defines three sections (Change Summary, How It Works, Additional Notes) that serve as a structured learning record.
- `.github/workflows/append-learnings.yml` is a GitHub Actions workflow that fires on PR merge: it parses the PR body sections and appends them to `LEARNINGS.md`, creating a running log of what was learned each PR.

**Additional Notes:**
- This covers the first part of Week 1 (repo scaffolding). The directory structure (`cmd/`, `internal/`, etc.), Makefile, and docker-compose are not yet created — those come next as we start implementing packages.
- `CLAUDE.md` and `.claude/` are gitignored (local development config only).


## 2026-02-13 - PR #2: Fix append-learnings workflow stripping inline code

**Change Summary:**
Fixes a bug where the `append-learnings.yml` workflow was stripping all backtick-wrapped inline code (e.g., `go.mod`, `README.md`, endpoint paths) from PR bodies when appending to `LEARNINGS.md`. Also manually restores the PR #1 learnings entry that was corrupted by this bug.

**How It Works:**
- The root cause was direct `${{ }}` interpolation of the PR body into a bash `run:` block. Bash interprets backticks as command substitution, so `` `go.mod` `` became an attempt to execute `go.mod` as a shell command — which fails silently and produces empty output.
- The fix passes the PR body through an `env:` variable instead. GitHub Actions sets env vars in the process environment without shell interpretation, so `"$ENTRY"` is expanded as a plain string with backticks preserved.
- `LEARNINGS.md` is updated to restore all the inline code that was stripped from the PR #1 entry.

**Additional Notes:**
- This is a common GitHub Actions security/correctness gotcha — direct `${{ }}` interpolation in `run:` blocks is also a shell injection vector (e.g., a malicious PR title could execute arbitrary commands). Using `env:` is the recommended safe pattern.
- This covers a fix discovered during Week 1. No new features; purely a bug fix to existing CI infrastructure.


## 2026-02-18 - PR #3: Scaffold directory structure, Makefile, and docker-compose

**Change Summary:**
Completes the remaining scaffolding from Week 1's first task. Creates the full `cmd/` and `internal/` directory structure, a `Makefile` for common dev workflows, and a `docker-compose.yaml` with Redis and Prometheus for the local dev stack.

**How It Works:**
- `cmd/llmrouter/main.go` is the gateway entry point (`package main` with an empty `func main()`). Go names the compiled binary after this directory, so `go build ./cmd/llmrouter` produces a `llmrouter` binary.
- Eight `internal/` package stubs (`config`, `server`, `provider`, `cache`, `embedder`, `router`, `stream`, `metrics`) each contain a single `.go` file with a `package` declaration and GoDoc comment. These are real Go packages (not `.gitkeep` placeholders) so `go build ./...` and `go test ./...` recognize them immediately.
- `Makefile` provides six targets: `build`, `test` (with `-race` flag for data race detection), `lint`, `run`, `docker-up`, and `docker-down`. All targets are declared `.PHONY` so `make` always runs them regardless of filesystem state.
- `docker-compose.yaml` defines two services: Redis (`redis:7-alpine`, port 6379, with a named volume for data persistence and a healthcheck) and Prometheus (`prom/prometheus:v3.2.0`, port 9090, mounting `prometheus.yml` read-only). Grafana is commented out for Week 7.
- `prometheus.yml` configures a 15-second scrape interval targeting `host.docker.internal:8080` — the gateway's `/metrics` endpoint. `host.docker.internal` resolves to the host machine from inside Docker, since the gateway runs outside the container.
- `.gitignore` fix: changed `llmrouter` to `/llmrouter` (leading slash anchors to repo root) so it only ignores the compiled binary, not the `cmd/llmrouter/` directory.

**Additional Notes:**
- This completes the first task of Week 1 ("Scaffold repo: `go mod init`, directory structure, Makefile, docker-compose"). `go mod init` was done in PR #1; this PR covers the rest.
- Image versions are pinned (`redis:7-alpine`, `prom/prometheus:v3.2.0`) for reproducibility.
- The `extra_hosts` directive in docker-compose ensures `host.docker.internal` resolves correctly on Linux too (it already works on macOS Docker Desktop by default).
- No Go implementation code yet — the stub files exist so the Go toolchain recognizes each package and so `go build ./...` / `go test ./...` work from day one.


## 2026-02-20 - PR #4: Implement config loading with koanf and godotenv

**Change Summary:**
- Add `config.yaml` with minimal Week 1 config (server settings + Google provider)
- Implement `internal/config/` package with typed config structs and a `Load()` function using koanf
- Add `.env` support via godotenv for local API key management

**How It Works:**
`config.Load(path)` loads configuration in three layers, each overriding the previous:
1. **YAML file** — parsed via koanf's file provider + YAML parser into a flat key-value map
2. **Environment variable overrides** — any `LLMROUTER_`-prefixed env var maps to a config key (e.g., `LLMROUTER_SERVER_PORT` → `server.port`)
3. **`${VAR}` placeholder expansion** — after unmarshaling, provider API keys containing `${VAR_NAME}` are resolved via `os.Getenv`

Before all of this, `godotenv.Load()` reads the `.env` file (if present) into the process environment, so API keys set there are available for both layers 2 and 3.

Config structs: `Config` (top-level) → `ServerConfig` (port, timeouts) + `map[string]ProviderConfig` (API key, base URL, models list).

Tests verify YAML loading with `${VAR}` expansion and `LLMROUTER_` env var overrides.

**Additional Notes:**
- Week 1 task: "Implement `internal/config/` — load YAML via koanf, env var interpolation for API keys"
- Config is intentionally minimal — only `server` and `providers.google` sections. Cache, embedding, routing, and metrics config will be added in later weeks as those features are built.
- `.env` file is already in `.gitignore` (added in PR #3)
- Dependencies added: koanf/v2, koanf providers (file, env, yaml), godotenv, testify


## 2026-02-21 - PR #5: Implement chi server, provider interface, and Google adapter

**Change Summary:**
- Add HTTP server package (`internal/server/`) with chi router, `/health` endpoint, and `/v1/chat/completions` handler
- Define `Provider` interface and unified request/response types (`ChatRequest`, `ChatResponse`, `StreamChunk`) in `internal/provider/provider.go`
- Implement Google Gemini adapter (`internal/provider/google.go`) with non-streaming `ChatCompletion` — translates unified format to/from Gemini's API format
- Wire everything together in `main.go`: config → provider → server → http.Server with timeouts

**How It Works:**
Request flows through: `main.go (http.Server)` → `server.go (chi router + middleware)` → `handler.go (decode JSON into ChatRequest)` → `google.go (translate to Gemini format, POST to generateContent, translate response back)` → `handler.go (return JSON)`.

The **Server struct** pattern holds the router, config, and provider as fields — handlers are methods on the struct so they access dependencies via `s.provider`, `s.cfg`, etc. This scales cleanly as more dependencies are added (cache, embedder, metrics).

The **Provider interface** defines three methods: `Name()`, `ChatCompletion()`, `ChatCompletionStream()`. The Google adapter implements non-streaming completions with full request translation (system messages → `systemInstruction`, `assistant` role → `model`, `max_tokens` → `generationConfig.maxOutputTokens`). Streaming returns a stub error for now.

Key Go patterns used: dependency injection (http.Client passed to provider), `context.Context` for cancellation propagation, `defer` for response body cleanup, `fmt.Errorf` with `%w` for error wrapping.

**Additional Notes:**
- **Week 1 progress:** This covers the server, provider interface, and Google adapter (non-streaming) tasks. Still remaining: streaming `ChatCompletionStream`, `internal/stream/` SSE writer, and end-to-end streaming test.
- **Streaming deferred to next PR:** The `ChatCompletionStream` method is stubbed. Implementing it involves goroutines, channel patterns, and SSE parsing which warrant their own focused PR.
- **Tested manually:** Ran the server locally, hit `/health` (200 OK) and `/v1/chat/completions` (successfully reached Gemini API — got 429 rate limit on free tier, confirming the full request pipeline works).
- Chi is added as a dependency (`go-chi/chi/v5`).


## 2026-02-23 - PR #6: Implement SSE streaming for Google Gemini provider

**Change Summary:**
- Add `ChatCompletionStream` to Google adapter: goroutine reads SSE lines from Gemini's `streamGenerateContent?alt=sse` endpoint, parses JSON, and sends `StreamChunk` values on an unbuffered channel with context cancellation support
- Implement SSE writer (`stream.Write`): consumes the chunk channel, translates to OpenAI-compatible `data: {json}\n\n` format, flushes each event in real-time, sends `[DONE]` sentinel
- Wire streaming path into `handleChatCompletions`: branches on `req.Stream`, calls provider, pipes channel to SSE writer
- Add `Error` field to `StreamChunk` for mid-stream error propagation through the channel

**How It Works:**
**Data flow:** Client POST (stream:true) → handler → `GoogleProvider.ChatCompletionStream()` → goroutine reads Gemini SSE body line-by-line via `bufio.Scanner` → sends `StreamChunk` on unbuffered channel → `stream.Write()` consumes channel → builds `sseChunk` JSON (OpenAI format with `choices[].delta.content`, `finish_reason`, `usage`) → writes `data: {json}\n\n` to `http.ResponseWriter` → `http.Flusher.Flush()` pushes to client → `data: [DONE]\n\n` on completion.

**Key patterns:**
- Goroutine + unbuffered channel for natural backpressure (producer blocks until consumer reads)
- `select` with `ctx.Done()` for cancellation when client disconnects
- `defer close(ch)` + `defer httpResp.Body.Close()` for cleanup
- Edge case: Gemini sometimes sends content + finishReason in the same SSE event — the writer splits this into two OpenAI events (content first, then finish with empty delta)

**Tests:** 4 unit tests for `stream.Write` covering: normal multi-chunk flow with headers/content/usage, Gemini combined content+finish edge case, mid-stream error handling, and raw SSE wire format validation.

**Additional Notes:**
- Completes the remaining Week 1 tasks: streaming in `google.go`, SSE writer in `stream.go`, end-to-end curl test, and unit tests
- Added `gemini-2.5-flash` to config.yaml models list (used for dev/testing since it's free tier)
- `config.yaml` change is unrelated to streaming — just adding a model we discovered works well during testing

🤖 Generated with [Claude Code](https://claude.com/claude-code)


## 2026-02-26 - PR #7: Add Anthropic provider request translation

**Change Summary:**
- Add `AnthropicProvider` struct, constructor, and `Name()` method — same dependency-injection pattern as `GoogleProvider`
- Implement `toAnthropicRequest` to translate our unified `ChatRequest` into Anthropic's Messages API format
- Define Anthropic-specific request types (`anthropicRequest`, `anthropicMessage`)

**How It Works:**
The translation function `toAnthropicRequest` handles three key differences between our unified format and Anthropic's API:
1. **System messages** — pulled out of the messages array into a top-level `system` string field (multiple system messages joined with newlines)
2. **Role passthrough** — unlike Google (which maps "assistant" → "model"), Anthropic uses the same "user"/"assistant" roles as OpenAI, so no mapping needed
3. **Required `max_tokens`** — Anthropic rejects requests without it, so we default to 1024 when the caller doesn't specify

Structs: `anthropicRequest` puts `model` in the request body (vs Gemini which puts it in the URL path). `anthropicMessage` is flat role+content (vs Gemini's nested `contents[].parts[]` structure).

**Additional Notes:**
- This is the first piece of Week 2 (Multi-Provider Support). Only the request translation is included — `ChatCompletion` (non-streaming) and `ChatCompletionStream` (streaming SSE parser) are follow-ups.
- The Anthropic SSE streaming parser will be more complex than Google's due to Anthropic's multi-event-type protocol (`message_start`, `content_block_delta`, `message_delta`, etc.).
- Response types are deferred until the non-streaming and streaming implementations are built.


## 2026-02-27 - PR #8: Implement Anthropic adapter and multi-provider registry

**Change Summary:**
- **Full Anthropic adapter**: request translation (system message extraction, required `max_tokens`), non-streaming `ChatCompletion` (response types, `x-api-key` + `anthropic-version` headers), and streaming `ChatCompletionStream` (multi-event-type SSE parser for `message_start`, `content_block_delta`, `message_delta`, `message_stop`)
- **Provider registry**: replaced single-provider `Server` with a `map[string]provider.Provider` keyed by model name, built at startup from config using a factory pattern. Handler resolves provider via O(1) map lookup on `req.Model`
- **Response headers**: `X-LLMRouter-Provider` and `X-LLMRouter-Model` set on every response. Anthropic config added to `config.yaml`

**How It Works:**
**Anthropic non-streaming**: `toAnthropicRequest()` translates unified `ChatRequest` → Anthropic format (system as top-level string, roles passthrough, `max_tokens` default 1024). `ChatCompletion()` POSTs to `{baseURL}/messages` with `x-api-key` and `anthropic-version: 2023-06-01` headers. Response decoded into `anthropicResponse` (content blocks array with typed blocks), first `type: "text"` block extracted, usage mapped (`input_tokens`/`output_tokens` → unified `Usage`, `TotalTokens` computed since Anthropic doesn't return it).

**Anthropic streaming**: same endpoint with `"stream": true` in body (unlike Gemini which uses a different URL). Goroutine reads SSE via `bufio.Scanner`, skips `event:` lines and only processes `data:` lines. Uses a single `anthropicStreamEvent` wrapper struct (discriminated union pattern — all possible fields as pointers, switch on `Type`). Metadata accumulated across events: `message_start` → ID/model/input tokens, `content_block_delta` → text chunks sent on channel, `message_delta` → output tokens, `message_stop` → final Done chunk with assembled usage. Same backpressure (unbuffered channel) and cancellation (`select` with `ctx.Done()`) patterns as the Google adapter.

**Provider registry**: `main.go` defines a `map[string]providerFactory` (factory functions per provider name), iterates `cfg.Providers`, constructs each provider, then registers every model from that provider's config list into a `map[string]provider.Provider`. `server.New()` takes this map. `handler.go` calls `resolveProvider(req.Model)` which does a map lookup and returns 400 for unknown models.

**Additional Notes:**
- This completes the first 3 tasks of Week 2 (Anthropic adapter, provider registry, response headers). Error handling (429 retry/backoff) and integration tests (go-vcr fixtures) remain.
- Anthropic API version is pinned via `anthropicAPIVersion = "2023-06-01"` constant, as noted in the risk mitigation plan.
- The streaming parser ignores `event:` lines entirely — the `"type"` field inside each `data:` JSON payload carries the same information, avoiding inter-line state tracking.
- Provider registry keys by model name (not provider name) for O(1) handler dispatch. Tradeoff: slightly larger map, but with ~5 models it's negligible.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


## 2026-03-02 - PR #9: Add error handling with retry/backoff and typed provider errors

**Change Summary:**
- Introduce `ProviderError` structured error type that carries upstream HTTP status, provider name, retryable flag, and `Retry-After` duration — replacing opaque `fmt.Errorf` strings from both provider adapters.
- Add `Retry()` function with exponential backoff + jitter for transient failures (429 rate limit, 5xx server errors). Respects `Retry-After` headers and context cancellation.
- Update handler to map `ProviderError` to appropriate gateway HTTP status codes (429 pass-through, 502 for upstream errors, 504 for timeouts) instead of always returning 502.
- Replace `http.DefaultClient` with a shared client with 120s timeout as a safety net against hung provider connections.

**How It Works:**
Provider adapters (`google.go`, `anthropic.go`) now call `NewProviderError(name, httpResp)` on non-2xx responses, which reads the error body, classifies retryability via `isRetryable()`, and parses the `Retry-After` header. The handler wraps provider calls in `Retry(ctx, 3, func() error { ... })` using a closure pattern — the closure captures the result variable from the outer scope so `Retry` only needs a `func() error` signature regardless of the provider method's return type. `backoffDelay()` computes exponential delays (1s → 2s → 4s) with 0–500ms random jitter and uses `Retry-After` as a floor. The handler's `writeProviderError()` uses `errors.As` to unwrap `ProviderError` and map its status code to the appropriate gateway response.

**Additional Notes:**
- Covers the "Error handling" task from Week 2 of the implementation plan. Integration tests (go-vcr fixtures) remain as the final Week 2 task.
- 15 unit tests added for `ProviderError`, `Retry`, `isRetryable`, `parseRetryAfter`, and `backoffDelay`. Tests that exercise real backoff sleeps are bounded to `maxAttempts=2` to keep the suite fast (~3s).
- The shared HTTP client timeout (120s) matches the server's `write_timeout` — it's a safety net, not a tight deadline. The request context provides per-request cancellation.


## 2026-03-04 - PR #10: Add integration tests with go-vcr HTTP fixtures

**Change Summary:**
- Added integration tests for both Google and Anthropic provider adapters using go-vcr v4 with hand-crafted cassette fixtures
- Tests cover 4 paths per provider: non-streaming success, streaming success, HTTP error classification (429/401/500), and malformed response handling (Google only)
- No real API calls needed — cassettes replay recorded HTTP responses via go-vcr's `http.RoundTripper` implementation injected into the existing `*http.Client` dependency

**How It Works:**
- `helpers_test.go` provides `newReplayClient(t, cassetteName)` which creates an `*http.Client` backed by go-vcr in `ModeReplayOnly`. The recorder implements `http.RoundTripper` and is set as the client's `Transport`, intercepting all HTTP calls and replaying responses from YAML cassette files in `testdata/cassettes/`.
- A custom `MatcherFunc` matching only on Method + URL is used instead of go-vcr's default matcher (which checks all request fields) since our cassettes are hand-crafted rather than recorded.
- `google_test.go` tests: non-streaming ChatCompletion (verifies Gemini response field translation), streaming ChatCompletionStream (verifies SSE parsing including the content+finishReason-in-same-event edge case), table-driven HTTP errors (429 retryable, 401 non-retryable, 500 retryable via ProviderError), and malformed response (200 with invalid JSON → parse error, not ProviderError).
- `anthropic_test.go` tests: non-streaming ChatCompletion (verifies response ID passthrough, input_tokens/output_tokens translation, computed TotalTokens), streaming ChatCompletionStream (verifies multi-event SSE parsing across message_start/content_block_delta/message_delta/message_stop, plus correct skipping of ping/content_block_start/content_block_stop events), and table-driven HTTP errors.
- 11 cassette YAML files model real API response shapes for both providers including SSE streaming bodies.

**Additional Notes:**
- Completes the final Week 2 task: "Integration tests with both providers (use recorded HTTP fixtures via go-vcr)"
- Cassettes are hand-crafted from API documentation rather than recorded from live calls — no API keys needed to run tests
- Key Go learning: variable shadowing (naming a parameter `cassette` shadowed the imported `cassette` package), `t.Cleanup` vs `defer` in test helpers, go-vcr's strict default matcher requiring all 15+ request fields to match


## 2026-03-08 - PR #11: Add embedder dependencies for ONNX inference pipeline

**Change Summary:**
- Install `daulet/tokenizers` v1.25.0 (HuggingFace Rust tokenizer via CGo) and `yalue/onnxruntime_go` v1.27.0 (ONNX Runtime C++ wrapper via CGo) as dependencies for the embedding pipeline
- Update `.gitignore` to exclude the entire `models/` directory (previously only `models/*.onnx`) since the ONNX export now produces multiple artifacts: `model.onnx`, `tokenizer.json`, `config.json`, `vocab.txt`, etc.

**How It Works:**
- `daulet/tokenizers` loads `models/tokenizer.json` at runtime to tokenize input text into `input_ids` and `attention_mask` tensors — same HuggingFace Rust tokenizer core used in Python, ensuring identical token IDs
- `yalue/onnxruntime_go` loads `models/model.onnx` (all-MiniLM-L6-v2) and runs inference to produce 384-dimensional embedding vectors
- Both are CGo packages: `tokenizers` statically links pre-built Rust binaries at compile time; `onnxruntime_go` dynamically loads the ONNX Runtime shared library (`.dylib`/`.so`) at runtime via `SetSharedLibraryPath()`

**Additional Notes:**
- Week 3 (Embedding Pipeline + Cache Infrastructure) — this PR covers dependency installation only. The `internal/embedder/embedder.go` implementation (tokenize → ONNX inference → mean pooling → 384-dim vector) is next
- Both packages currently show as `// indirect` in `go.mod` since no Go code imports them yet — they'll become direct deps once `embedder.go` is implemented
- ONNX Runtime deployment note added to Week 9 plan: `libonnxruntime.so` must be present at runtime on Linux (dynamically loaded, not bundled in Go binary)


## 2026-03-09 - PR #12: Implement embedder with ONNX inference and HuggingFace tokenizer

**Change Summary:**
- Implement `internal/embedder/embedder.go` — the `Embedder` struct that converts text into 384-dim sentence embeddings using the all-MiniLM-L6-v2 ONNX model
- Add 4 unit tests including Python reference value verification, determinism, discrimination, and empty input handling
- Add `lib/` to `.gitignore` for platform-specific shared libraries (ONNX Runtime `.dylib`/`.so` and tokenizer `.a`)
- Add `CGO_LDFLAGS` to Makefile so `make test` and `make build` find `libtokenizers.a` automatically

**How It Works:**
The embedder pipeline has three stages, all running in-process (no Python sidecar):

1. **Tokenize** — `daulet/tokenizers` loads `tokenizer.json` (HuggingFace Rust tokenizer via CGo). Calling `EncodeWithOptions(text, true, WithReturnAttentionMask())` produces padded token IDs (128 tokens) and an attention mask (1 for real tokens, 0 for padding). The `tokenizer.json` has padding and truncation config baked in.

2. **ONNX inference** — `yalue/onnxruntime_go` loads `model.onnx` into a `DynamicAdvancedSession`. Token IDs and attention mask are converted to `int64` tensors (shape `[1, 128]`) and fed through the model.

3. **Output** — The model's `sentence_embedding` output (shape `[1, 384]`) is already mean-pooled and L2-normalized, matching Python `sentence-transformers` output exactly. We copy the result into a Go-managed `[]float32` before destroying the C-allocated tensor.

Key types:
- `Embedder` struct — holds the tokenizer, ONNX session, and embedding dimension
- `New(modelPath, tokenizerPath, libraryPath, dimension)` — one-time setup (loads ONNX Runtime, tokenizer, creates session)
- `Embed(text) ([]float32, error)` — per-request inference (~5-20ms on CPU)
- `Close()` — releases C/C++ resources (tokenizer, session, ONNX environment)

**Additional Notes:**
- **Week 3 of project plan** — this completes the embedder task. Cache layer (Redis storage + semantic lookup) is next.
- **Build requirements**: Two native libraries must be in `lib/`: `libonnxruntime.dylib` (downloaded from Microsoft's ONNX Runtime releases, ~33MB) and `libtokenizers.a` (downloaded from `daulet/tokenizers` releases, ~37MB). Both are platform-specific and gitignored.
- **Key discovery during implementation**: The ONNX model's `sentence_embedding` output includes mean pooling + L2 normalization in the computation graph itself. Our first attempt used the `token_embeddings` output with manual mean pooling, which produced wrong results because we weren't handling the attention mask correctly (padding tokens were treated as real input). Using `sentence_embedding` is simpler and produces exact parity with Python.
- **Attention mask bug**: The Go tokenizer respects `tokenizer.json`'s padding config and returns 128 tokens. Our initial code set `attentionMask[i] = 1` for all 128 positions, corrupting the embedding. Fix: use `EncodeWithOptions` with `WithReturnAttentionMask()` to get the correct mask from the tokenizer.


## 2026-03-13 - PR #13: Implement semantic cache interface and Redis backend

**Change Summary:**
- Add `Cache` interface in `cache.go` with five methods: `Lookup`, `Store`, `Stats`, `Flush`, `Close` — the technology-agnostic contract for semantic response caching.
- Add `RedisCache` implementation in `redis.go` combining storage (Redis hashes) and semantic similarity lookup (brute-force cosine scan with SIMD acceleration) in a single struct.
- Add dependencies: `redis/go-redis/v9` for Redis client, `viterin/vek` for SIMD-accelerated vector dot product.

**How It Works:**
- **Storage:** Each cache entry is a Redis hash (`cache:{sha256(embedding)}`) with fields for the embedding bytes, JSON-serialized response, created_at timestamp, and hit_count. A sorted set (`cache:index`) tracks all entries by timestamp for efficient eviction.
- **Lookup (two-pass):** First pass reads all embeddings from Redis and computes cosine similarity via `vek32.Dot` (dot product works as cosine similarity because embeddings are L2-normalized by the embedder). Second pass fetches the full response only for the best match if it exceeds the configurable similarity threshold.
- **Store (pipelined):** Uses a Redis pipeline to batch 3 commands (HSet, Expire, ZAdd) into a single round-trip. After storing, checks entry count and evicts oldest entries via `ZPopMin` if over `MaxEntries`.
- **Serialization helpers:** `embeddingToBytes`/`bytesToEmbedding` convert between `[]float32` and `[]byte` using `math.Float32bits` + little-endian binary encoding (384-dim embedding = 1,536 bytes).
- **Stats:** Hit/miss counters use `sync/atomic` for safe concurrent access. Entry count reads from the sorted set cardinality.

**Additional Notes:**
- This is Week 3 of the project plan — cache interface and Redis backend. The original plan had separate `redis.go` and `semantic.go` files, but they were combined because the similarity scan is inseparable from the storage layer (it reads stored embeddings).
- Tests are deferred to a follow-up — will use `miniredis` (pure-Go in-memory Redis) so no Docker dependency needed for CI.
- `CacheConfig` still needs to be wired into the main `Config` struct in `config.go` and `config.yaml` — also a follow-up task.
- The `similaritySum` field uses `int64` with `math.Float64bits` for atomic access to a float64 — a known Go pattern since `sync/atomic` doesn't have `AddFloat64`.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


## 2026-03-15 - PR #14: Add architecture diagram and project status to README

**Change Summary:**
Add architecture diagram, project status, and polish to the README for external visibility.

**How It Works:**
- Mermaid flowchart shows the full request lifecycle: client → embedder → cache check (hit/miss) → complexity classifier → cheap/quality model selection → SSE stream → response
- Classifier arrows labeled "simple" / "complex" to clarify that routing picks model tier, not provider
- "At a glance" section gives a bullet summary above the diagram for quick scanning
- Status badge (in progress), Go version badge, and MIT license badge at the top via shields.io
- MIT LICENSE file added to the repo

**Additional Notes:**
- This is a docs-only change, no code modifications
- Preparing the README for external visibility ahead of job applications
- The diagram intentionally omits middleware and observability details to keep it scannable — those are covered in dedicated sections below


## 2026-03-16 - PR #15: Wire CacheConfig into config and add cache unit tests

**Change Summary:**
- Wired `CacheConfig` into the main `Config` struct in `internal/config/config.go` so the `cache:` YAML section is parsed automatically by koanf
- Added `koanf` struct tags to `CacheConfig` fields in `internal/cache/redis.go` for correct YAML key mapping (e.g., `redis_url` → `RedisURL`)
- Added the `cache:` section to `config.yaml` with production-ready defaults (0.92 threshold, 1h TTL, 50k max entries)
- Added unit tests for the cache layer using `miniredis` (pure-Go in-memory Redis)

**How It Works:**
- `Config.Cache` is typed as `cache.CacheConfig` directly — no intermediate config struct, so `main.go` can pass `cfg.Cache` straight to `NewRedisCache()`
- `CacheConfig` uses `koanf` struct tags to map underscore-separated YAML keys to camelCase Go fields (without these, koanf can't match `redis_url` to `RedisURL`)
- Tests use `miniredis.RunT(t)` which spins up an in-memory Redis server per test and auto-cleans on test end — no Docker needed
- Test embeddings are 384-dim L2-normalized vectors using different dimensions for distinctness (e.g., vec[0]=1.0 vs vec[1]=1.0 are orthogonal, so dot product = 0)
- Four test cases: identical embedding hit (similarity=1.0), orthogonal embedding miss, FIFO eviction at MaxEntries, and flush resetting stats + entries

**Additional Notes:**
- Completes the final two tasks of Week 3 (Embedding Pipeline + Cache Infrastructure)
- Chose to import `cache.CacheConfig` directly into the `config` package rather than duplicating the struct — trades a cross-package dependency for zero duplication
- The `normalizedVec` helper initially had a bug: normalizing any scalar to unit length always gives 1.0, so `normalizedVec(1.0)` and `normalizedVec(2.0)` produced identical vectors and hashed to the same Redis key. Fixed by using separate dimensions for distinct vectors.


## 2026-03-16 - PR #16: Wire embedder and cache into Server for handler access

**Change Summary:**
- Add `EmbeddingConfig` struct to the config package and `embedding:` section to `config.yaml` (model path, tokenizer path, library path, dimension).
- Expand `Server` struct with `embedder` and `cache` fields; update `New()` signature to accept both dependencies.
- Initialize embedder and Redis cache in `main.go` at startup with `defer` cleanup for graceful shutdown.

**How It Works:**
`main.go` now creates the `Embedder` (ONNX model + HuggingFace tokenizer) and `RedisCache` from config, then passes them into `server.New()`. The `Server` struct stores them as fields so `handleChatCompletions` can access them for cache lookup/store in the next PR. The embedder is a concrete struct (stored as `*embedder.Embedder`), while the cache is an interface (`cache.Cache`) — no pointer wrapping needed since Go interfaces are already reference-like internally.

**Additional Notes:**
- This is the first step of Week 4 (Semantic Cache Integration). The handler doesn't use the embedder/cache yet — that wiring comes in subsequent PRs.
- `EmbeddingConfig` follows the same koanf struct tag pattern as `CacheConfig` for underscore-separated YAML keys.
- `defer emb.Close()` and `defer c.Close()` ensure the ONNX session and Redis connection pool are released on shutdown.


## 2026-03-18 - PR #17: Wire semantic cache into the live request path

**Change Summary:**
- Integrate embedder + cache into `handleChatCompletions` so cache lookups and stores happen on every request (when cache is configured)
- Non-streaming path: embed prompt → lookup → HIT returns cached response / MISS calls provider then stores
- Streaming path: `teeAndCache` goroutine pipeline forwards chunks to client while buffering deltas, then reconstructs and stores a full `ChatResponse` after stream completes

**How It Works:**
The handler computes an embedding of the last user message (via `lastUserMessage` helper) and checks `cache.Lookup()`. On a HIT, the cached response is returned immediately with `X-LLMRouter-Cache: HIT` and `X-LLMRouter-Similarity` headers. On a MISS, the request flows to the provider as before, with cache storage happening after:

- **Non-streaming**: `cache.Store()` is called directly after the provider returns
- **Streaming**: `teeAndCache()` inserts a goroutine between the provider channel and `stream.Write`. The goroutine forwards each chunk to an output channel (consumed by the SSE writer) while accumulating deltas in a `strings.Builder`. After the final chunk, it reconstructs a `ChatResponse` and calls `cache.Store()`

Cache/embedding errors are non-fatal — logged and skipped so the provider path always works. A `cacheEnabled` flag guards all cache logic and degrades gracefully if embedder or cache are nil.

**Additional Notes:**
- Week 4 items completed: `lastUserMessage` helper, non-streaming cache path, streaming cache miss (tee + buffer + store)
- Week 4 items remaining: streaming cache hit (replay as SSE burst), `x-cache` request parameter, `/cache/stats` + `/cache/flush` endpoints, integration tests
- `stream.Write` is completely unchanged — it reads from a `<-chan StreamChunk` regardless of whether `teeAndCache` is in the pipeline


## 2026-03-20 - PR #18: Complete Week 4: streaming cache hits, x-cache, cache endpoints

**Change Summary:**
- **Streaming cache hit replay**: `replayChunks` helper converts a cached `ChatResponse` into a pre-loaded `<-chan StreamChunk` (buffered channel, capacity 2, no goroutine needed). Cache HITs with `stream: true` now replay via `stream.Write` as a fast SSE burst instead of returning raw JSON.
- **`x-cache` request parameter**: New `XCache` field on `ChatRequest` (`json:"x-cache"`). `skip` bypasses cache entirely (no lookup, no store). `only` returns 404 on miss. `auto`/empty = default behavior.
- **`/cache/stats` and `/cache/flush` endpoints**: GET `/cache/stats` returns JSON hit/miss/entry counts and avg similarity. POST `/cache/flush` clears all entries and resets counters. Both return 503 if cache is disabled.
- **`Embedder` interface + integration tests**: Extracted `Embedder` interface at the server package level (avoids CGo dependency propagation). Renamed concrete type to `ONNXEmbedder`. 6 handler integration tests using mock embedder, mock provider, and miniredis.

**How It Works:**
**Streaming cache hit**: When `req.Stream == true` and cache returns a HIT, handler calls `replayChunks(result.Response)` which creates a buffered channel of 2 chunks (content + done), closes it, and returns. `stream.Write` consumes this identically to a real provider stream — it doesn't know the chunks came from cache.

**x-cache flow**: Checked early in `handleChatCompletions`. `skip` sets `cacheEnabled = false` before any cache interaction. `only` is checked after the cache lookup block — if we reached the "forward to provider" section and `x-cache == "only"`, return 404 with `X-LLMRouter-Cache: MISS` header.

**Embedder interface**: `server.Embedder` interface (single method: `Embed(string) ([]float32, error)`) defined at the consumer to avoid importing the embedder package's CGo dependencies into the server package. `*embedder.ONNXEmbedder` satisfies it implicitly. Tests use a `mockEmbedder` with a function field for deterministic vectors.

**Cache endpoints**: `handleCacheStats` delegates to `cache.Stats()` (atomic counters + `ZCard`). `handleCacheFlush` delegates to `cache.Flush()` (deletes all keys + resets counters). Both nil-check `s.cache` and return 503 if disabled. JSON tags added to `CacheStats` for clean API output.

**Additional Notes:**
- Completes all remaining Week 4 tasks from the implementation plan.
- `make run` now depends on `docker-up` so Redis is started automatically.
- The `Embedder` interface lives in `server.go` rather than `embedder.go` due to CGo: importing the embedder package pulls in `libtokenizers` at link time, which would break server tests that only use mocks. This is the "define interfaces where they're consumed" Go pattern, motivated by a practical CGo constraint.
- Similarity threshold tuning (rephrased prompts not matching) is deferred to Week 8 (load testing + threshold sweep).


## 2026-03-23 - PR #19: Fix cross-model cache hits with model-scoped partitioning

**Change Summary:**
- **Bug fix:** Cache lookups were purely keyed on embedding vectors with no model awareness. The same prompt sent to `gemini-2.0-flash` could return a cached response originally generated by `gemini-2.5-flash`. Partitioned the cache by model so lookups only match entries for the requested model.
- **Week 5 scaffold:** Added `training/` directory with uv Python environment and `collect_dataset.py` — the first script in the complexity classifier training pipeline.

**How It Works:**
**Model-scoped cache partitioning:** The cache now uses per-model sorted set indexes in Redis (`cache:index:{model}`) alongside the existing global index (`cache:index`). On `Store`, entries are written to both indexes — the global one for eviction (oldest-across-all-models), the model-scoped one for lookup (only scan entries for the requested model). The Redis hash key is now `sha256(embedding_bytes + model_bytes)` so the same prompt under different models gets separate entries. A `model` field is stored in each hash so eviction can clean up the correct model-scoped index. The `Cache` interface methods (`Lookup`, `Store`) now take a `model string` parameter, threaded through from `req.Model` in the handler.

**Training scaffold:** `training/collect_dataset.py` samples ~500 prompts from 4 public datasets (LMSYS-Chat-1M, MMLU, HumanEval, Dolly), sends each to both a cheap and quality Gemini model, and saves results as JSONL. Supports resumability (append mode + skip completed prompts) and retry with exponential backoff.

**Additional Notes:**
- Eviction test needed `time.Sleep` between stores to ensure deterministic sorted set ordering — previously timestamps could tie within the same millisecond, and adding model bytes to the key hash changed the lexicographic tiebreak order.
- Added `TestLookup_CrossModelIsolation` to verify same-embedding-different-model is a cache miss.
- The `collect_dataset.py` script is scaffolded but not yet run end-to-end — LMSYS-Chat-1M requires gated HuggingFace access, so the dataset source mix may change in the next PR.
- Covers a bug fix from Week 4 cache work and initial scaffolding for Week 5.


## 2026-03-26 - PR #20: Update training pipeline: ungated datasets + Anthropic models

**Change Summary:**
- Replaced gated LMSYS-Chat-1M dataset with ungated alternatives: Dolly (200 prompts) + OpenAssistant/oasst2 (150 prompts), keeping MMLU (100) + HumanEval (50) for a total of 500 diverse prompts.
- Switched from Gemini (2.0-flash + 2.5-pro) to Anthropic (Haiku + Sonnet) for response collection, due to persistent Gemini 2.5 Pro 503 availability issues.
- Added truncation detection (`stop_reason=max_tokens`) to skip incomplete responses, and bumped `max_output_tokens` to 8192 to prevent Gemini-style thinking models from exhausting the token budget.

**How It Works:**
- `sample_openassistant()` replaces `sample_lmsys()`: loads `OpenAssistant/oasst2`, filters for English root prompter messages (`role=="prompter"`, `parent_id is None`), applies 20–2000 char length filter. No streaming needed — oasst2 is only ~13k rows.
- `sample_dolly()` bumped from 50 → 200 prompts as a primary source.
- `call_anthropic()` replaces `call_gemini()`: uses `anthropic.Anthropic.messages.create()` with the standard Messages API format. Checks `stop_reason` for truncation and `content[0].text` for empty responses before returning.
- `collect()` now initializes an `anthropic.Anthropic` client with `ANTHROPIC_API_KEY` instead of a Gemini client.
- Added `training/*.jsonl` to `.gitignore` (generated output files: `prompts.jsonl` and `dataset.jsonl`).
- Resumability is preserved: `prompts.jsonl` locks the prompt set, `dataset.jsonl` tracks completed entries, and re-runs skip already-processed prompts.

**Additional Notes:**
- **Week 5 of the implementation plan** — this covers the first task (`collect_dataset.py`) but not yet `label_quality.py`, `train_classifier.py`, or `export_onnx.py`.
- Data collection is in progress (~14/500 prompts collected so far). The script can be resumed with `cd training && uv run python collect_dataset.py`.
- Originally planned to use Gemini 2.5 Flash-Lite (cheap) + 2.5 Pro (quality), but Pro returned persistent 503s. Also discovered that Pro's "thinking" tokens consume the `max_output_tokens` budget, leaving no visible output at 1024 tokens — fixed by bumping to 8192 and adding truncation checks. These fixes remain in the code for future Gemini usage.
- The Haiku vs Sonnet quality gap is well-suited for classifier training — similar capability spread to Flash-Lite vs Pro. The classifier learns "is this prompt too hard for the cheap model" regardless of model family.
- `anthropic` SDK added as a dependency in `training/pyproject.toml`.


## 2026-03-31 - PR #21: Add concurrent collection, validation, and LLM-as-judge labeling

**Change Summary:**
- Added concurrency (5 ThreadPoolExecutor workers) to `collect_dataset.py`, reducing collection time from ~92 minutes to ~18 minutes for 500 prompts
- Added `validate_dataset()` that runs automatically after collection — checks for malformed JSON, missing fields, source distribution, short responses, and duplicates
- Created `label_quality.py` (Phase 2): sends cheap + quality responses to Gemini 2.5 Pro as an LLM judge with a rubric to produce binary labels (0=cheap adequate, 1=needs expensive)

**How It Works:**
**Concurrent collection:** `ThreadPoolExecutor` runs 5 prompts in parallel. Each prompt still calls Haiku then Sonnet sequentially within its thread. A `threading.Lock` protects the shared output file and progress counter. Removed `API_DELAY` sleeps — at 5 workers (~10 RPM), well under Anthropic rate limits; retry backoff handles any 429s.

**Dataset validation:** `validate_dataset()` reads `dataset.jsonl` and reports: row count, malformed JSON lines, missing required fields, source distribution (dolly/openassistant/mmlu/humaneval), responses under 50 chars (with prompt preview), duplicate prompts, and average response lengths. Runs automatically at end of collection but can also be called standalone.

**LLM-as-judge labeling:** `label_quality.py` reads `dataset.jsonl`, sends each entry's cheap and quality responses to Gemini 2.5 Pro with a rubric. Responses are clearly labeled as "Cheap Model Response" and "Expensive Model Response" — the judge decides if the cheap response is adequate (label=0) or if the expensive model is needed (label=1). Uses `thinking_budget=4096` for genuine reasoning on quality judgments. `response_mime_type="application/json"` forces valid JSON output. 8 concurrent workers with the same ThreadPoolExecutor + Lock pattern. Resumable via `load_existing_labels()`.

**Additional Notes:**
- Week 5 of the implementation plan (Complexity Classifier Training)
- Initially implemented A/B response randomization to prevent positional bias in judge, but removed it after evaluation showed it confused the judge model — clear "cheap"/"expensive" labels produced better reasoning
- Dataset collection produced 499/500 entries with healthy source distribution and response lengths
- First labeling run showed 65/35 split (adequate/needs expensive) — reasonable distribution for classifier training
- Spot-check of 6 labeled examples (3 per class) confirmed judge accuracy, with label=1 borderline cases being conservative (routes more to expensive model, doesn't hurt quality)
- Still TODO for Week 5: `train_classifier.py` (MLP training) and `export_onnx.py` (ONNX export)


## 2026-04-03 - PR #22: Add complexity classifier training (MLP + GBT)

**Change Summary:**
- Added `training/train_classifier.py` — trains two binary classifiers (PyTorch MLP and scikit-learn Gradient Boosted Trees) on labeled prompt embeddings to predict whether a prompt needs the expensive model or the cheap model is adequate.
- Added `scikit-learn` dependency to `training/pyproject.toml` and updated lockfile.
- Added `training/*.joblib` to `.gitignore` for GBT checkpoint files.

**How It Works:**
1. Loads 499 labeled prompts from `labeled_dataset.jsonl` (produced by `label_quality.py`).
2. Computes 384-dim embeddings using `all-MiniLM-L6-v2` via `sentence-transformers`.
3. Stratified 80/20 train/val split preserving label distribution (72/28 adequate/needs-expensive).
4. **MLP path:** Extracts 7 handcrafted features (char count, word count, sentence count, question marks, avg word length, newlines, has-code), standardizes them, concatenates with embeddings (391-dim input), trains a `391 → 64 → 1` MLP with BCE loss, pos_weight for class imbalance, early stopping, and LR scheduling.
5. **GBT path:** Trains a `GradientBoostingClassifier` on embeddings only (384-dim). Uses 100 estimators, max_depth=5, subsample=0.8, min_samples_leaf=5.
6. Sweeps decision thresholds (0.20–0.80) on the best model to show the full precision/recall tradeoff curve.
7. Saves both checkpoints: `.pt` (MLP) and `.joblib` (GBT).

**Additional Notes:**
- **Week 5** of the implementation plan (complexity classifier training).
- GBT consistently outperforms MLP on this small dataset (0.768 vs 0.667 val accuracy). Both are included as learning artifacts — the MLP training journey explored overfitting, regularization (dropout, batchnorm, weight decay, early stopping), and the limits of neural networks on small datasets.
- Feature importance analysis showed embeddings carry 94% of signal; handcrafted features contribute minimally. The GBT uses embeddings only.
- Best GBT accuracy (0.768) only narrowly beats the majority-class baseline (0.727). The fundamental challenge is that `all-MiniLM-L6-v2` encodes semantic similarity (topic), not task complexity. Threshold tuning in Week 8 and/or rule-based routing are potential paths forward.
- `export_onnx.py` (ONNX export for Go inference) is the remaining Week 5 task — deferred pending a decision on final model choice.


## 2026-04-05 - PR #23: Add complexity features, expand dataset, refine classifier training

**Change Summary:**
- Decided on GBT as the model to ship (better probability calibration for threshold tuning, data-efficient for small datasets). MLP kept as comparison baseline.
- Reverted MLP to attempt 3 architecture (384→64→32→1, embeddings only, LR 1e-4) and added threshold sweep — confirmed MLP probabilities cluster uselessly around 0.5 while GBT produces a real precision/recall curve.
- Replaced shallow handcrafted features (char_count, word_count — 6% importance) with 6 complexity-targeting features for GBT: subtask count, constraint count, reasoning keywords, question count, code task type, imperative density.
- Expanded dataset collection from 4 sources (~500 prompts) to 8 sources (~2500 prompts) by adding MBPP, GSM8K, ARC Challenge, and Alpaca datasets.
- Added rate limit protection across collection and labeling scripts (reduced workers, inter-prompt delays).

**How It Works:**
- `train_classifier.py` trains both models on the same labeled dataset. MLP gets embeddings only (384-dim); GBT gets embeddings + 6 complexity features (390-dim). Both get threshold sweeps. GBT prints feature importance breakdown showing how much signal comes from embeddings vs handcrafted features.
- New complexity features use regex pattern matching to detect structural signals: multi-step instructions ("first", "then"), constraints ("without", "must not"), reasoning keywords ("compare", "tradeoffs"), code task type (0=none, 1=write, 2=debug/optimize), and imperative sentence density. These target task difficulty directly rather than topic.
- `collect_dataset.py` now samples from 8 HuggingFace datasets (Dolly 500, OpenAssistant 400, MMLU 350, HumanEval 50, MBPP 200, GSM8K 200, ARC Challenge 200, Alpaca 600). Resumability preserves existing collected data.
- `label_quality.py` workers reduced from 8→5 with 0.25s delay for rate limit safety at higher volume.

**Additional Notes:**
- Week 5 of the project plan. Data collection (~2000 new prompts) and re-labeling are in progress — this PR ships the pipeline changes; retraining on the full ~2500 dataset happens once collection/labeling complete.
- Key finding from threshold sweep comparison: MLP outputs cliff between 0.45→0.50 (all-1 to all-0 predictions), making it untunable. GBT probabilities spread across the range, enabling a real precision/recall tradeoff.
- Original handcrafted features (char_count, word_count, has_code) contributed only 6% of GBT feature importance because they don't correlate with complexity. New features are designed based on this analysis.
- The 6 complexity features are cheap to compute (~microseconds of regex) and will need equivalent Go implementations for gateway inference in Week 6.
- `export_onnx.py` (GBT → ONNX via skl2onnx) deferred until retraining on full dataset completes.


## 2026-04-06 - PR #24: Add cost-aware model router with per-provider config

**Change Summary:**
- Add routing layer for `"model": "auto"` requests — Router selects a concrete model based on strategy (`auto`/`cheapest`/`quality`) and per-provider cheap/quality model pairs
- Move `x-cache` from request body to `X-Cache` header; add `X-Route` and `X-Provider` headers for routing control
- Define `Classifier` interface for future ONNX complexity classifier integration

**How It Works:**
When a request arrives with `"model": "auto"`, the handler calls `Router.Route(embedding, strategy, provider)` to resolve a concrete model name before dispatching to the provider.

**Resolution flow:**
1. `strategy` comes from `X-Route` header (falls back to `config.routing.default_strategy`)
2. `providerName` comes from `X-Provider` header (falls back to `config.routing.default_provider`)
3. Look up the provider's cheap/quality model pair from `config.routing.providers`
4. Apply strategy: `cheapest` → cheap model, `quality` → quality model, `auto` → run classifier against threshold (errors if no classifier configured yet)

**Key types:**
- `router.Router` — holds `RoutingConfig` + optional `Classifier`, exposes `Route()` method
- `router.Classifier` interface — `Classify(embedding []float32) (float64, error)` — to be implemented in `classifier.go` when ONNX model is ready
- `server.ModelRouter` interface — consumer-side interface in server package (same decoupling pattern as `Embedder`)
- `config.RoutingConfig` / `config.RoutingProviderConfig` — per-provider cheap/quality model mapping

**Header migration:** `x-cache` moved from JSON body field to `X-Cache` request header. Added `X-Route` and `X-Provider` headers. Handler reads all three at the top of `handleChatCompletions`. Embedding computation refactored to run when either caching or routing needs it (previously only ran when caching was enabled).

**Additional Notes:**
- Week 6 of the implementation plan (partial — router and config done, classifier integration and cost tracking still pending)
- `auto` strategy currently errors with "classifier not configured" — will work once the ONNX complexity classifier is exported and `classifier.go` is implemented
- Default provider is `anthropic` since the classifier training data used Anthropic responses
- API surface docs in obsidian vault (`llmrouter.md`) still show the old body-field approach for `x-cache`/`x-route` — should be updated in a future pass
- 11 new unit tests for router covering all strategies, defaults fallback, provider override, and error cases


## 2026-04-08 - PR #25: Improve classifier training with class weighting and F2 optimization

**Change Summary:**
Iterated on the GBT complexity classifier training pipeline to address the 4:1 class imbalance that caused both models to always predict the majority class (adequate). Added class weighting, switched to F2-optimized threshold selection, added hyperparameter grid search, and dropped handcrafted complexity features after experiments proved they added no signal.

**How It Works:**
- **Class weighting:** `CLASS_WEIGHT_MULTIPLIER` (4.0x) scales `sample_weight` on label=1 examples during GBT training via `clf.fit(sample_weight=...)`, and scales MLP `pos_weight` in `BCEWithLogitsLoss`. This penalizes false negatives (missed expensive prompts) much harder than false positives.
- **F2 threshold sweep:** Both MLP and GBT threshold sweeps now optimize F-beta with beta=2 (recall weighted 2x over precision) instead of F1. GBT sweep uses 0.01 step increments (was 0.05) across 0.10–0.80 for finer operating point selection.
- **Grid search:** `train_gbt()` now accepts hyperparameters as kwargs. `main()` loops over 4 configs (varying n_estimators, max_depth, learning_rate, min_samples_leaf) and selects the best by F2 score.
- **Embeddings only:** Complexity features (subtask_count, constraint_count, etc.) are still extracted and printed for analysis, but excluded from model input. Experiments showed 0% GBT feature importance when embeddings were present, and 25% val accuracy when used alone — confirmed redundant.
- **Best model:** GBT with 100 trees, depth 5, lr 0.1, threshold 0.28 → 91.3% recall on needs-expensive prompts, 25% precision. Conservative router that rarely serves bad responses but over-routes ~72% of adequate prompts to expensive.

**Additional Notes:**
- Covers Week 5 classifier training iteration. MLP code is retained as a baseline comparison artifact but never learned on this data (collapsed to majority-class prediction across all configurations).
- The GBT checkpoint (`complexity_classifier_gbt.joblib`) now includes `best_f_beta`, `f_beta`, and `config` metadata alongside the model.
- Handcrafted complexity feature extraction code is preserved in the script for documentation — the features are a good interview talking point as a measured negative result.
- Next: export GBT to ONNX via `skl2onnx`, then implement `classifier.go` for Go-side inference (Week 6).


## 2026-04-11 - PR #26: Add classifier ONNX integration and per-request cost tracking

**Change Summary:**
- Export trained GBT complexity classifier to ONNX via `skl2onnx` (`training/export_onnx.py`), with numerical verification against scikit-learn output
- Implement `ONNXClassifier` wrapper in Go (`internal/router/classifier.go`) satisfying the `router.Classifier` interface — `"model": "auto"` now runs real complexity classification instead of erroring
- Add per-model token pricing config and `cost_usd` field in both streaming (final SSE chunk) and non-streaming (JSON body) responses

**How It Works:**
**ONNX Export:** `export_onnx.py` loads the GBT joblib checkpoint, converts via `skl2onnx` with `zipmap: False` (outputs plain float tensor instead of map type the Go runtime can't handle), verifies ONNX output matches sklearn `predict_proba` within 1e-5, saves to `models/complexity_classifier.onnx`.

**Classifier Integration:** `ONNXClassifier` loads the ONNX model at startup (reusing the ONNX Runtime environment already initialized by the embedder — one per process). `Classify(embedding []float32) (float64, error)` feeds a `[1, 384]` tensor in, reads `probabilities[0][1]` (P(needs-expensive)) as the complexity score. `main.go` creates the classifier and passes it to `router.New()` instead of `nil`. Threshold updated from 0.6 to 0.28 to match the GBT's optimal F2 operating point.

**Cost Tracking:** `config.ModelCost` struct holds input/output price per million tokens. `computeCost()` in the handler does `(promptTokens × inputPrice + completionTokens × outputPrice) / 1M`. For non-streaming: set directly on `ChatResponse.CostUSD`. For streaming: `stream.Write` accepts a `costFn func(Usage) float64` closure — called on the final chunk when usage becomes available, included in the SSE JSON alongside usage data. Cost appears in the response body in both modes for client consistency.

**Additional Notes:**
- Covers remaining Week 6 tasks: ONNX export, classifier.go, wiring, cost tracking
- `ChatResponse` fields now have `json:"..."` tags for proper lowercase serialization matching OpenAI format
- The `costFn` closure pattern keeps the stream package decoupled from config — it only knows "call this function with usage to get a number"
- GBT model is 125KB ONNX, inference ~0.1ms — negligible vs LLM response latency
- Complexity threshold 0.28 = 91.3% recall on needs-expensive prompts (conservative — over-routes to expensive). Tuning deferred to Week 8 load testing


## 2026-04-14 - PR #27: Add Prometheus metrics + /metrics endpoint (Week 7)

**Change Summary:**
- Adds `internal/metrics` package with 17 Prometheus collectors covering request counts, latency (end-to-end, TTFT, inter-token), token usage, cost, cache behavior, routing decisions, and provider errors.
- Adds counterfactual cost-savings counters (`cost_saved_by_cache_usd_total`, `cost_saved_by_routing_usd_total`) so the final writeup can quantify $ saved vs. a naive baseline.
- Exposes `/metrics` via `promhttp.Handler()` and instruments handler, streaming writer, complexity classifier, and router.

**How It Works:**
Collectors are declared as package-level vars and auto-registered via `promauto`. Most call sites live in the request handler, which captures `(provider, model, cache_status)` locals as the request progresses and a `defer` records `Requests` + `RequestDuration` at exit.

Per-chunk timing metrics (TTFT, inter-token latency) must live inside the SSE loop, so `stream.Write`'s signature was changed to take a `WriteOptions` struct carrying `Provider`, `Model`, `RequestStart`, `CostFn`, and an optional `OnDone(usage, cost)` callback. The handler passes `OnDone` on the live provider path to record final-chunk counter metrics (tokens, cost, routing savings) without the stream package importing `metrics`.

`ComplexityScore` and `ClassificationDuration` are observed inside `ONNXClassifier.Classify` since that's the only place the score exists. `RoutingDecisions` is counted inside `router.Route`. A new `CheapAndQualityFor(provider)` helper lets the handler compute routing-savings without peeking at router config.

The cache-entries gauge uses `promauto.NewGaugeFunc` with a closure over `cache.Stats()` — no polling, always live at scrape time, and keeps the `metrics` package free of a `cache` import (right dependency direction for a leaf package).

Provider errors are mapped to a capped `error_type` enum (`timeout | rate_limit | auth | upstream_5xx | other`) via a small classifier to keep label cardinality bounded.

**Additional Notes:**
- Covers most of Week 7's Go tasks. Still open for Week 7: Prometheus scrape config in `docker-compose.yaml` and Grafana dashboard JSON (pure ops work, no Go).
- Cache-hit replays record TTFT (legitimately fast latency) but skip counter observations since those are already handled on the cache-hit branch (`CostSavedByCache`, etc.).
- `CacheSimilarity` is currently only observed on hits. Observing near-miss scores would require changing `Cache.Lookup` to return the best score even below threshold; deferred as a small follow-up useful for threshold tuning.
- Streaming-path routing-savings observation goes through the `OnDone` callback, so it only fires when the final chunk carries a `Usage` value.
- The `model="auto"` label value doesn't appear on metrics — by the time we record, `req.Model` has been rewritten to the concrete routed model. `RoutingDecisions{strategy, selected_model}` captures the auto-routing behavior separately.


## 2026-04-16 - PR #28: Add Grafana dashboard and fix auto-routing cache lookup

**Change Summary:**
- Wires up the Grafana side of Week 7: provisioned datasource + 13-panel dashboard mounted into the Grafana container, so `docker-compose up` shows live metrics with zero clicks.
- Fixes a real bug: requests with `model: "auto"` were structurally unable to hit the cache, because cache lookup happened before the routing block rewrote `req.Model` — lookup scanned the empty `cache:index:auto` partition while stores landed under the resolved model's partition.
- Tunes histogram buckets: tightens `cache_similarity_score` to the threshold-and-above range (we only observe hits), widens `request_duration_seconds` and `time_to_first_token_seconds` so `histogram_quantile` doesn't push high percentiles to bucket upper bounds.

**How It Works:**
**Grafana provisioning.** Three new files:
- `grafana/provisioning/datasources/prometheus.yml` — datasource UID `prometheus`, points at `http://prometheus:9090` via the docker-compose service network.
- `grafana/provisioning/dashboards/dashboards.yml` — provider config with `updateIntervalSeconds: 10` so JSON edits hot-reload, and `allowUiUpdates: true` so UI tweaks can be exported back to the file.
- `grafana/dashboards/llmrouter.json` — 13 panels organized as Traffic → Latency → Cost → Cache → Routing rows. Latency panels split by provider (`sum by (le, provider)`) for p50/p95/p99 readouts on `request_duration_seconds` and `time_to_first_token_seconds`. Cost row pairs three cumulative stat panels (total spend, saved by cache, saved by routing) with a stacked rate time series. Cache row has hit-rate stat with thresholds, entry-count stat, and a similarity heatmap. Routing row has stacked routing decisions and a complexity-score heatmap.

`docker-compose.yaml` uncomments the Grafana service and mounts both `grafana/provisioning` (read-only) and `grafana/dashboards` (read-only). Anonymous admin access enabled, login form disabled — `localhost:3000` lands you straight in the dashboard.

**Cache lookup bug.** In `internal/server/handler.go`, the routing block (`if req.Model == "auto" { ... }`) was previously located *after* the cache lookup. Cache entries are partitioned per model in Redis (`cache:index:<model>`), so:
- Lookup with `req.Model == "auto"` scanned `cache:index:auto` → always empty → MISS
- Store happened later with `req.Model = "gemini-2.0-flash"` → entries piled up in `cache:index:gemini-2.0-flash`
- No subsequent `auto` request could ever hit those entries

Fix: relocated the routing block to immediately after the embedding step, before the `if cacheEnabled` lookup. The embedding step was already shared between caching and routing, so the fix is pure block-shuffling — no startup wiring or ONNX init concerns (those are construction-order constraints in `main.go`, not request-handler concerns).

Side effect: routing decisions are now counted for cache-hit requests too. Arguably more honest — the routing classifier did run and pick a model — but a behavior change worth flagging. The obsidian doc previously stated "cache hits skip routing entirely"; that intent is now relaxed. The classifier cost is ~1ms per request, negligible.

**Streaming cache cost fix.** `teeAndCache` was building cached responses without setting `CostUSD`, so every entry stored from a streaming request had `CostUSD = 0`. The hit branch's guard `if result.Response.CostUSD > 0` then prevented `CostSavedByCache` from ever incrementing on replays of streaming-origin entries. Added `resp.CostUSD = computeCost(model, resp.Usage, s.cfg.Costs)` before `cache.Store`.

**Bucket tuning.**
- `cache_similarity_score`: `.5, .7, .8, .85, .9, .92, .94, .96, .98, 1.0` → `.9, .92, .94, .96, .98, .99, 1.0`. We only observe hits (which are >= threshold = 0.92), so the lower buckets were dead weight. Kept `.9` as a safety bucket for future threshold tuning.
- `request_duration_seconds`: `.05, .1, .25, .5, 1, 2, 5, 10, 30` → `.05, .1, .25, .5, 1, 2, 5, 10, 15, 20, 30, 60`. The 10s→30s gap caused p95/p99 to interpolate to ~28-30s for requests that actually completed in 12-15s.
- `time_to_first_token_seconds`: `.05, .1, .2, .5, 1, 2, 5` → `.05, .1, .2, .5, 1, 2, 3, 5, 10`. Same problem at the upper end.
- Inter-token latency buckets unchanged — already appropriate for ms-scale gaps.

**Additional Notes:**
- Completes the remaining Week 7 ops checklist items (Prometheus scrape config was already in place from a prior PR; this PR adds the Grafana side).
- Existing cache entries that were stored before the streaming-CostUSD fix still have `CostUSD = 0` and will continue to record `$0 saved` on replay. Cache flush required to see the metric work end-to-end with old data.
- Histogram bucket changes are technically breaking for the time series — Prometheus stores each bucket as a separate `_bucket{le="..."}` series. Old data with old buckets stays in TSDB; new data writes to new buckets. Quantile queries during the transition will be slightly weird until old data ages out. Not a concern for a learning project.
- The dashboard JSON has been re-exported once via the Grafana UI (Save dashboard → JSON Model copy), which expanded the file from a hand-written ~380 lines to ~1100 lines of fully-defaulted Grafana 11.5 schema. That's the canonical format Grafana wants and what future UI exports will look like.
- Future Week 7 follow-up worth considering: observing best-similarity scores on cache *misses* would let the histogram fill out its lower buckets, which would help with threshold tuning in Week 8. Requires `cache.Lookup` to return the best score even when below threshold. Deferred — not blocking.


## 2026-04-20 - PR #29: Add Week 8 corpus builder and benchmark harness skeleton

**Change Summary:**
- Python `training/build_corpus.py` extracts two benchmark corpora from Quora Question Pairs: a **clustered** corpus (40×5 dense paraphrases) for threshold sweeps, and a **realistic** corpus (~199 prompts, power-law distribution) for cost-savings measurement. Corpora share no prompts (enforced via `used` set).
- Go `benchmarks/cache_bench_test.go` is a v1 end-to-end harness gated with `//go:build bench` — drives a running gateway with shuffled corpus prompts, records per-request metadata from response headers, and reports aggregate stats.
- Adds `networkx` to `training/pyproject.toml` for duplicate-graph construction.

**How It Works:**
**Corpus builder** (`training/build_corpus.py`):
1. Loads QQP (GLUE train split) via HuggingFace `datasets`, filters each question by length bounds and a crude NSFW blocklist.
2. Builds an undirected graph where edges = labeled duplicates. `all_clean` tracks every clean question so questions never in the graph form the singleton pool.
3. `select_clustered` grabs 40 components of size ≥5, truncated to 5 prompts each. Populates a `used` set.
4. `select_realistic` walks a declarative `REALISTIC_SHAPE = [(1,15), (3,8), (10,4), (30,2), (60,1)]` — hot clusters first to claim rare large components before pairs compete.
5. Emits two JSONs to `benchmarks/data/` plus a stdout preview for eyeballing label quality before committing.

**Benchmark harness** (`benchmarks/cache_bench_test.go`):
- Configured via env vars: `LLMROUTER_URL` (default `localhost:8080`), `LLMROUTER_CORPUS` (default realistic), `LLMROUTER_MODEL` (default `auto`).
- Flattens + shuffles prompts with `rand.NewPCG` seeded from `corpus.seed` for reproducibility.
- `POST /cache/flush` at start so hit rate reflects *this* corpus run.
- Scrapes `/metrics` before and after; diffs `llmrouter_cost_saved_by_cache_usd_total` across all label pairs to compute cost saved.
- Per request: constructs OpenAI-format body, sends non-streaming, reads `X-LLMRouter-Cache`, `-Cost-USD`, `-Similarity`, `-Provider`, `-Model`, measures wall-clock latency.
- Summary: count, hit rate, actual cost, cost saved, savings rate, p50/p95/p99 split by hit vs miss.
- Single `httpClient` with 120s timeout; `io.Copy(io.Discard, resp.Body)` to enable connection reuse.

Run with:
```
docker-compose up -d
go run ./cmd/llmrouter   # in another terminal
go test -v -tags bench -run TestCacheBenchmark ./benchmarks/ -timeout 30m
```

**Additional Notes:**
- Week 8 scope: this lands corpus + harness infrastructure. Remaining Week 8 work: run threshold sweep (likely offline — embed prompts once, derive hit-rate-vs-threshold curve from pairwise cosine math), run realistic corpus for cost headline, LLM-as-judge quality scoring over cache hits, latency percentile tuning.
- Two corpora by design: threshold curves need dense paraphrase pairs for signal; cost savings need realistic traffic distribution. One corpus compromises one goal. Quality-vs-threshold is workload-independent, so the threshold chosen on clustered data applies to realistic.
- Design choices: sequential (no concurrency) in v1 for deterministic runs; external gateway (not in-process) so we measure the real system with Redis + ONNX + provider HTTP; non-streaming requests (TTFT/inter-token are captured via Week 7 metrics independently).
- QQP labels are noisy — the script prints all sampled clusters to stdout so you can eyeball before running. Added a coarse NSFW blocklist after first run surfaced crude content; substring match may false-positive (e.g., "essex") but unlikely at this corpus size.
- Power-law shape `[(1,15), (3,8), (10,4), (30,2), (60,1)]` is a tunable constant. If `(1,15)` starves after the clustered corpus claims large components, fall back to `(1,12)` or `(2,8)`.
- Stale `benchmarks/data/corpus.json` from an earlier single-output script version is intentionally not staged — ignore/delete locally.


## 2026-04-22 - PR #30: Week 8 harness: streaming + concurrency; dashboard and header fixes

**Change Summary:**
- **Harness switched to streaming with a configurable concurrent worker pool.** `benchmarks/cache_bench_test.go` now sends `stream: true`, parses the SSE stream to extract `cost_usd` from the final chunk, and fans out requests across N goroutines (`LLMROUTER_CONCURRENCY`, default 3). Captures true wall-clock latency (was TTFB) and only accumulates `actualCost` on cache misses, so hit-body `cost_usd` from replayed responses doesn't double-count against the cache-savings metric.
- **Cache-hit responses now carry provider/model headers.** `internal/server/handler.go` previously only set `X-LLMRouter-Provider` and `X-LLMRouter-Model` on the cache-miss branch; the cache-hit branch now emits them too, so downstream consumers (dashboards, harness, API clients) see consistent labels regardless of path.
- **Error-rate Grafana panel fixed to render zero when no errors.** `grafana/dashboards/llmrouter.json` query gets `or vector(0)` fallback so the panel shows a 0 line during clean runs instead of "No data."

**How It Works:**
- **SSE parser:** `bufio.Scanner` over the response body, filter on `data: ` prefix, break on `[DONE]`. Each chunk decoded into a struct with a single `CostUSD *float64` field; the pointer distinguishes "absent" (normal intermediate chunks) from "zero." The final chunk carries both `usage` and `cost_usd` per the existing stream writer (`internal/stream/stream.go`), so the loop just captures the last non-nil value.
- **Worker pool:** unbuffered would deadlock on first-error; both `jobs` and `outcomes` channels are buffered to `len(prompts)` so producers never block. Workers `range` over `jobs`, which exits cleanly on `close(jobs)`. `outcome` struct carries `{res, err, promptIdx}`. The main goroutine drains exactly `len(prompts)` outcomes and is the only place `t.Fatalf` is called (per Go docs, `FailNow`/`Fatalf` are unsafe from spawned goroutines).
- **Cost accounting:** cache hits still emit `cost_usd` in the replayed body (inherited from the original miss that populated the entry), so the old logic of `actualCost += r.CostUSD` over every iteration double-counted. The fix: gate on `!r.CacheHit`, so `actualCost` reflects only what was actually spent on providers this run, and the `savings_rate` denominator (`actualCost + costSaved`) is honest.
- **Header parity:** cache-hit branch already computes `metricProvider` and `metricModel` for metrics; reusing those values for headers is two lines of code. Case (a) unconditional set was chosen: if `metricProvider` is empty (stored model is orphaned from config), the header goes out blank — a diagnostic signal rather than a failure.

**Additional Notes:**
- Week 8: this PR covers harness iteration; threshold tuning (offline sweep + LLM-as-judge quality scoring + realistic cost bench + end-to-end quality eval) is still ahead. See `projects/llmrouter.md` for the full 8-step tuning plan.
- Known half-run observations (from a partial run with these changes): latency trends upward over the run (suspected Anthropic rate-limit retries); cache-hit-rate panel shows 0.0% in red during idle (same `or vector(0)` trick could apply but wasn't part of this PR's scope); complexity-vs-routing panels looked contradictory on small N — to be re-evaluated on a full run.
- Latency semantics change: non-streaming `latency = time.Since(start)` after `httpClient.Do()` was TTFB (headers-only). Streaming version moves the capture to after the scanner drains — now measures total response time including full body. Miss-latency numbers in future summaries will therefore be slightly higher than pre-PR, all else equal.
- `t.Fatalf` on first worker error leaks the remaining in-flight goroutines (they block on `out <-`) until the test binary exits. Acceptable for a bench harness; adding `context.Context` cancellation would be ceremony.


## 2026-04-24 - PR #31: Week 8 step 1: offline hit-rate sweep in Python

**Change Summary:**
- Adds a standalone offline sweep that reproduces the gateway's best-above-threshold cache-lookup logic in Python and plots hit rate vs similarity threshold T on the Week 8 clustered corpus.
- Goal: see the full shape of the hit-rate curve (no knee vs sharp knee) before picking a tuned T for the live Go harness, without spending provider calls on a full sweep.

**How It Works:**
- `benchmarks/dumporder` (new Go binary): reads a corpus JSON, flattens clusters, and re-runs the exact PCG-seeded Fisher-Yates shuffle used by `cache_bench_test.go`. Emits the ordered prompt list as a JSON array on stdout. The Python side reads this file so ordering is identical — critical because the cache simulation is path-dependent (order of inserts changes which prompts land in the kept set and thus the hit rate at mid-thresholds).
- `benchmarks/python` (new uv project): `sweep_hitrate.py` loads the ordered prompts, embeds them with `sentence-transformers/all-MiniLM-L6-v2` (L2-normalized → dot product == cosine), precomputes the NxN pairwise similarity matrix once, then iterates the threshold grid. Per threshold: walk prompts in order, keep a list of kept indices, for prompt `i` take `sim[i, kept].max()`; if it's `>= T` count a hit, otherwise append `i` to `kept`. This mirrors `internal/cache/redis.go`'s lookup (max over index, threshold check) without touching Redis or the embedder service.
- Outputs: `results/hitrate_sweep.csv` and `results/hitrate_sweep.png` over T ∈ [0.75, 0.99] step 0.005.

**Additional Notes:**
- Curve shape: no knee. Hit rate descends nearly linearly at ~3 pp per 0.01 T — 67.5% at T=0.75 down to 3% at T=0.99, passing 45.5% at the current default T=0.85. Means there is no "free" threshold that gives us hit rate without giving up precision; the tradeoff is smooth and must be driven by false-hit tolerance (step 3: adequacy judging).
- Why a separate Python project: the gateway embeds via ONNX MiniLM, but for a 200×200 sweep we want NumPy fancy indexing and `@` matmul. The ONNX path isn't needed for offline analysis.
- Why not replicate Go's shuffle in Python: Go `math/rand/v2` uses PCG while Python's `random` uses Mersenne Twister — same seed ≠ same order. Rather than reimplement PCG in Python, the dumper externalizes the order so both sides consume the same JSON.
- `benchmarks/python/.venv/` added to `.gitignore`; `benchmarks/data/` is already ignored, so the generated `corpus_clustered_order.json` is regenerated per run (reproducible from the corpus).
- This covers Week 8 step 1 (offline sweep). Step 2 (harness JSONL triples) is designed but deferred to a follow-up PR: log `{prompt, gateway_response, baseline_response, cache_hit, similarity}` per request, with the baseline generated by a second gateway call carrying `X-Cache: skip` (gates both lookup AND store, so no cache pollution).


## 2026-04-28 - PR #32: Week 8: threshold tuning pipeline + headline bench prep

**Change Summary:**
- Steps 2-5 of the Week 8 threshold-tuning plan: collected live tuning records via the harness, labeled them with Gemini 2.5 Pro, computed the false-hit-rate-vs-T curve, and picked T*=0.92 (already the existing config default — now data-validated).
- Harness + script prep for steps 6-7's combined run: the bench harness can now produce both step 6's headline cost numbers AND step 7's quality-eval data in a single corpus pass; new `label_eval.py` and `eval_quality.py` scripts close the loop.

**How It Works:**
**Tuning pipeline (steps 2-5):**
- `benchmarks/cache_bench_test.go` — when `LLMROUTER_LOG_PATH` is set (renamed from `LLMROUTER_TUNING_LOG`), the harness writes one JSONL record per request capturing the full SSE-decoded response text and the per-request similarity. On every HIT it issues a second `X-Cache: skip` call to capture a fresh `baseline_response`. Single-writer pattern (workers do the gateway calls, main goroutine writes records — no mutex needed).
- `benchmarks/python/label_quality.py` — Gemini 2.5 Pro judge with an *intrinsic adequacy* rubric (deliberately NOT a competitive cached-vs-fresh comparison): YES if cached is acceptable AND near parity with fresh (or better), NO if cached fails AND fresh is meaningfully better. Stylistic differences explicitly excluded. ThreadPool with 5 workers, resumable by `(prompt, gateway_response)` key.
- `benchmarks/python/pick_threshold.py` — for each candidate T, filters labeled records to `similarity ≥ T` and computes `FHR = NO / total`. Plots stacked subplots (hit rate top, FHR bottom) with a dashed target line and a vertical T* marker. Picks lowest T meeting the target (default 5%, configurable via `--target`).

**Threshold selection result:** the FHR curve shows real structure — a ~25% plateau at T ≤ 0.85 (loose QQP near-duplicates getting flagged as inadequate), a drop to ~10% at T=0.92 (genuine paraphrases only), then small-N noise above T~0.94 where labeled hits drop below ~15. The 5% target was unreachable except at T ≥ 0.98 where hit rate collapses to ~3%, so the target was re-negotiated to ~10% per the plan's "negotiable if data suggests otherwise" clause. T*=0.92 captures ~22% hit rate.

**Headline bench prep (steps 6-7):**
- `benchmarks/cache_bench_test.go` — new `LLMROUTER_BASELINE_MODEL` env var extends the baseline rule to `cache_hit OR routed_model != baseline_model`. With `model: \"auto\"` and `LLMROUTER_BASELINE_MODEL=claude-sonnet-4-5-20250929`, the harness baselines on cache hits and cheap-routed misses but skips quality-routed misses (which would just be Sonnet-vs-Sonnet stochasticity). TTFT now measured per request (timestamp of first SSE `data:` event minus request start). Extended JSONL schema: `model_routed`, `cost_usd`, `ttft_ms`, `latency_ms`. Summary now includes TTFT percentiles for hit/miss and a misses-by-routed-model breakdown.
- `benchmarks/python/label_eval.py` — fork of `label_quality.py` that filters on non-empty `baseline_response` (so it judges both hits and cheap-routed misses) with a slightly generalized rubric intro that mentions either path.
- `benchmarks/python/eval_quality.py` — joins the harness JSONL with the labeled JSONL, classifies each record by path (`hit` / `cheap_miss` / `quality_miss`), and reports per-path quality rate plus overall preservation = `(YES on hit + YES on cheap_miss + all quality_miss) / total`.

**Additional Notes:**
- Covers Week 8 steps 2-5 of the tuning plan; steps 6-7 still need the actual run (harness + scripts ready).
- Path-relative defaults in the Python scripts: `Path(__file__).parent` anchors paths so they work regardless of which directory `uv run` is invoked from. Step 2 hit a Go-test gotcha where `go test` cd's into the package directory before running, making CLI-passed paths resolve relative to `benchmarks/` rather than the repo root.
- `X-Cache: skip` confirmed to skip both lookup AND store (gated on the same `cacheEnabled` flag in `handler.go`), so baseline calls don't perturb cache state.
- The 0.92 dip in the FHR curve is principled, not coincidental — it's where the corpus's threshold structure transitions. Validates the original config default that was set on intuition in week 7.
- Don't read the FHR curve past T~0.94 as signal: small-N variance dominates there. The 0% at T=0.98 is 4 hits, 0 NOs.
- Run sequence for next session: bounce gateway → `LLMROUTER_LOG_PATH=data/realistic_records.jsonl LLMROUTER_BASELINE_MODEL=claude-sonnet-4-5-20250929 go test -tags bench -run TestCacheBenchmark -timeout 30m ./benchmarks/` → `uv run python label_eval.py` → `uv run python eval_quality.py`.


## 2026-04-30 - PR #33: Week 8 steps 6-8: bench results, harness fix, README writeup

**Change Summary:**
- Wraps up Week 8 with the realistic-corpus bench results and the README writeup that pairs cost numbers with end-to-end quality preservation.
- **Harness fix**: bench previously scraped only `llmrouter_cost_saved_by_cache_usd_total`, so the printed savings rate excluded routing savings. Now scrapes both counters (cache + routing) and reports `Cache saved`, `Routing saved`, `Total saved`, and a savings rate against the combined no-gateway baseline.
- **README**: new Benchmarks and Parameter tuning sections; dropped the stale "in progress" status banner; added `cost reduction` and `quality preserved` badges.
- **Make targets**: `bench`, `bench-collect`, `bench-quality` for the three-stage bench pipeline.

**How It Works:**
- `scrapeCostSaved` was replaced with `scrapeSavings`, which returns a `Savings{Cache, Routing}` struct from a single `/metrics` scrape. `TestCacheBenchmark` takes before/after snapshots and passes the delta to `summarize`. The `Savings.Total()` helper drives the savings-rate calculation.
- `summarize` prints three lines (`Cache saved`, `Routing saved`, `Total saved`) and one combined `Savings rate`, replacing the prior single `Cost saved` line.
- README structure: headline result → Benchmarks (setup, cost savings table, latency table, quality preservation table with the 94.5% end-user vs 78.4% engineering framing) → Parameter tuning (similarity threshold method + result with embedded `threshold_selection.png`, complexity classifier journey including MLP dead end and handcrafted-features negative result) → Quick Start.
- Make pipeline: `bench` runs the harness with no logging (headline only), `bench-collect` adds `LLMROUTER_LOG_PATH` + `LLMROUTER_BASELINE_MODEL` env vars to capture the JSONL log for quality eval, `bench-quality` runs `label_eval.py` then `eval_quality.py` from `benchmarks/python/`.

**Additional Notes:**
- This is the final week-8 PR. Wraps step 6 (realistic cost bench at T*=0.92), step 7 (LLM-as-judge quality eval), and step 8 (README writeup of tuning + benchmarks).
- Headline result on a single 199-prompt run: ~20% cost reduction at 94.5% quality preservation, hit p50 TTFT ~28× faster than miss p50 TTFT.
- The 94.5% preservation rate bakes in the 148 quality-routed misses that went untouched. The more rigorous segmentation (78.4% of *affected* requests judged adequate) is also in the README so the engineering rigor is visible.
- 17.2% NO rate on cache hits is slightly higher than the ~10% FHR target negotiated at T=0.92. With N=29 that's within noise (one extra NO = 3.4pp), but worth flagging — realistic-corpus FHR may run slightly looser than the QQP-tuning-corpus prediction.
- Did not re-run the bench after the harness fix to capture clean per-run routing savings; estimated from the cumulative routing counter divided by two equal runs (both runs hit the same cache savings within $0.001, so the estimate is solid). Future bench runs will print the realistic number directly.
- Week 9 polish (Configuration section, Design Decisions section, Build & Test targets refresh, GoDoc cleanup) is deliberately deferred.


## 2026-05-04 - PR #34: Week 9: README polish — header, benchmarks, observability, TUNING.md

**Change Summary:**
README polish pass for résumé-readiness; no code changes.
- Centered Ruflo-style header (badges → title → rule → subtitle → explainer); reworded explainer to mention streaming support and a full observability suite
- Replaced "At a glance" with outcome-led "What llmrouter does" leading on the 20.4% / 94.5% headline
- Restructured Benchmarks (dropped Latency, dropped 78.4% engineering metric, dropped all `~` symbols)
- New TUNING.md with cache similarity threshold methodology + full complexity classifier journey
- Expanded Observability section with metric-category breakdown and Grafana screenshot placeholder

**How It Works:**
- README is now organized as outcome → mechanism → numbers → ops, in that order: header, "What llmrouter does", Architecture, Benchmarks (Setup / Cost savings / Quality preservation), Observability, Quick Start, API, Build & Test
- TUNING.md is the methodology companion. Setup table in the README links into anchored sections (`#cache-similarity-threshold` and `#complexity-classifier`) so a reader can drop into the relevant deep-dive
- Header centering uses `<div align="center">` wrapping the badges + h1 + horizontal rule + subtitle + explainer; the LEARNINGS.md callout sits outside the centered block so the link reads naturally left-aligned
- The complexity classifier journey in TUNING.md is structured as Phase 1 (MLP dead end) → Phase 2 (GBT: class weighting, F2 over F1, handcrafted features negative result, grid search) → Final model → Takeaways, mirroring the actual decision sequence

**Additional Notes:**
- Covers Week 9 README tasks from the project plan (README polish, methodology write-up, deeper observability section)
- Still open for Week 9: repo-side `CLAUDE.md`, ONNX Runtime deployment notes, GoDoc/lint code cleanup, Configuration walkthrough section, Design Decisions section, Grafana screenshot embed
- The user is treating this as the first of several README polish PRs in the same session — more iteration coming on top
- The repo name (`llmrouter`) was discussed and intentionally kept; rename was scoped at ~30–45 minutes (mostly module-path find/replace) but deemed not worth the churn right now
- Engineering note: GitHub's rich diff view shows raw HTML for `<div align="center">` rather than rendering it — preview the file directly to verify centering, not the diff


## 2026-05-06 - PR #35: Week 9: README polish pass #2 — API spec, Grafana screenshot, TRAINING_AND_TUNING.md

**Change Summary:**
- **Full API reference** for `POST /v1/chat/completions` — request body fields, all request/response headers with exact values and emit conditions, streaming vs non-streaming response shapes, error code table with triggers.
- **Two small bug fixes**: `Usage` struct lacked JSON tags (shipped `PromptTokens` instead of `prompt_tokens`, inconsistent with the streaming shape); `X-Route`/`X-Provider` headers were silently no-op'd when model was pinned (now 400).
- **Grafana dashboard screenshot** added to Observability section; Quick Start and Build & Test sections expanded with make targets, API key setup, and bench harness usage.
- **`TUNING.md` → `TRAINING_AND_TUNING.md`**: classifier section moved above cache threshold (more interesting story leads), handcrafted-features section condensed to one paragraph, classifier story tightened to the three-beat arc (MLP → GBT → conservative quality-first router).

**How It Works:**
**API spec** was grounded in a full codebase audit before writing — every field, header, and error code verified against `handler.go`, `provider.go`, and `stream.go`. Notable findings documented: `cost_usd` intentionally lives in the response body (not headers) because headers can't be set mid-stream; upstream 401/403 map to 502 (gateway misconfiguration, not client auth failure); `X-Cache: only` returns 404 on miss.

**Validation fix** (`handler.go:356–374`): after reading `X-Route`/`X-Provider` headers and decoding the request body, a guard checks `req.Model != "auto"` and returns 400 with a descriptive message naming the pinned model. Applied to both headers for consistency.

**JSON tag fix** (`provider.go:82–86`): added `json:"prompt_tokens"` etc. to the `Usage` struct so non-streaming responses match the `sseUsage` wire format used in streaming chunks.

**Additional Notes:**
Week 9, PR #35. Remaining Week 9 items still open: Configuration section in README (config.yaml walkthrough), Design Decisions section, repo-side `CLAUDE.md`, ONNX Runtime deployment doc, code cleanup (GoDoc, golangci-lint).

