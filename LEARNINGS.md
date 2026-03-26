
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

