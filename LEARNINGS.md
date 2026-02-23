
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

