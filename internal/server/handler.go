package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/howard-nolan/llmrouter/internal/config"
	"github.com/howard-nolan/llmrouter/internal/metrics"
	"github.com/howard-nolan/llmrouter/internal/provider"
	"github.com/howard-nolan/llmrouter/internal/stream"
)

// classifyProviderError maps an error from a provider call into the capped
// error_type enum used on metrics.ProviderErrors. Keeps label cardinality bounded.
func classifyProviderError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return metrics.ErrTimeout
	}
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		switch {
		case pe.StatusCode == http.StatusTooManyRequests:
			return metrics.ErrRateLimit
		case pe.StatusCode == http.StatusUnauthorized, pe.StatusCode == http.StatusForbidden:
			return metrics.ErrAuth
		case pe.StatusCode >= 500:
			return metrics.ErrUpstream5xx
		}
	}
	return metrics.ErrOther
}

// observeRoutingSavings records metrics.CostSavedByRouting when the chosen
// model is the provider's cheap model. The savings estimator approximates the
// quality model's cost using this request's actual token counts: input tokens
// are exact (same prompt either way), output tokens approximated using the
// cheap model's completion count. No-op if the router isn't configured, the
// chosen model isn't the cheap model, or cost table entries are missing.
func (s *Server) observeRoutingSavings(providerName, xProvider, chosenModel string, usage provider.Usage, actualCost float64) {
	if s.modelRouter == nil {
		return
	}
	cheap, quality, ok := s.modelRouter.CheapAndQualityFor(xProvider)
	if !ok || chosenModel != cheap {
		return
	}
	qc, ok := s.cfg.Costs[quality]
	if !ok {
		return
	}
	qualityEstimate := (float64(usage.PromptTokens)*qc.InputPerMillion +
		float64(usage.CompletionTokens)*qc.OutputPerMillion) / 1_000_000
	savings := qualityEstimate - actualCost
	if savings <= 0 {
		return
	}
	metrics.CostSavedByRouting.WithLabelValues(providerName).Add(savings)
}

// computeCost calculates the USD cost of a request from token usage and the
// per-model cost table. Returns 0 if the model isn't in the table.
func computeCost(model string, usage provider.Usage, costs map[string]config.ModelCost) float64 {
	mc, ok := costs[model]
	if !ok {
		return 0
	}
	return (float64(usage.PromptTokens)*mc.InputPerMillion +
		float64(usage.CompletionTokens)*mc.OutputPerMillion) / 1_000_000
}

// costFnForModel returns a closure that computes cost from usage for a
// specific model. Passed into stream.Write so cost can be computed when
// usage becomes available on the final chunk. Returns nil if the model
// isn't in the cost table (stream.Write skips cost when nil).
func costFnForModel(model string, costs map[string]config.ModelCost) func(provider.Usage) float64 {
	if _, ok := costs[model]; !ok {
		return nil
	}
	return func(usage provider.Usage) float64 {
		return computeCost(model, usage, costs)
	}
}

// writeProviderError writes a JSON error response with an HTTP status code
// derived from the error type. Maps ProviderError status codes to appropriate
// gateway responses; falls back to 502 for unrecognized errors.
func writeProviderError(w http.ResponseWriter, err error) {
	log.Printf("provider error: %v", err)

	status := http.StatusBadGateway // default for unknown errors

	var provErr *provider.ProviderError
	if errors.As(err, &provErr) {
		switch {
		case provErr.StatusCode == http.StatusTooManyRequests:
			status = http.StatusTooManyRequests
		case provErr.StatusCode >= 500:
			status = http.StatusBadGateway
		default:
			// 401, 403, 400 from provider = our upstream config is wrong
			status = http.StatusBadGateway
		}
	} else if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{
		"error": err.Error(),
	})
}

// resolveProvider looks up the Provider for a given model name using the
// model-to-provider registry. Returns an error if the model isn't known.
//
// This is the core of the provider dispatch: the client sends us a model
// name like "gemini-2.0-flash" or "claude-haiku-4-5-20251001", and we
// need to find which Provider handles it. The s.models map was built at
// startup from the config file's provider → models lists, so this is
// just a map lookup.
//
// In Express terms, this is like a middleware that inspects req.body.model
// and attaches the right service client to the request context.
func (s *Server) resolveProvider(model string) (provider.Provider, error) {
	p, ok := s.models[model]
	if !ok {
		return nil, fmt.Errorf("unknown model: %q", model)
	}
	return p, nil
}

// replayChunks converts a cached ChatResponse into a channel of StreamChunks
// for SSE replay. Sends the full content as one chunk, then a Done chunk with
// usage stats. The buffered channel is pre-loaded and closed — no goroutine
// needed since all data is available upfront.
func replayChunks(resp *provider.ChatResponse) <-chan provider.StreamChunk {
	ch := make(chan provider.StreamChunk, 2)

	ch <- provider.StreamChunk{
		ID:    resp.ID,
		Model: resp.Model,
		Delta: resp.Content,
	}

	ch <- provider.StreamChunk{
		ID:    resp.ID,
		Model: resp.Model,
		Done:  true,
		Usage: &resp.Usage,
	}

	close(ch)
	return ch
}

// lastUserMessage walks backward through the conversation and returns the
// content of the last message with role "user". This is what we embed for
// cache lookup — only the last user message, not the full conversation.
func lastUserMessage(messages []provider.Message) (string, error) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content, nil
		}
	}
	return "", fmt.Errorf("no user message found")
}

// teeAndCache inserts a pipeline stage between the provider's chunk channel
// and the SSE writer. It reads each chunk from the input channel, forwards
// it to a new output channel (which stream.Write consumes), and buffers
// all the delta text. After the stream completes, it reconstructs a full
// ChatResponse and stores it in the cache.
//
// The goroutine inside is the "tee" — one copy goes to the client (via
// the returned channel), the other accumulates for caching. This is like
// piping a Node.js readable stream through a Transform that also collects
// the data into a buffer.
func (s *Server) teeAndCache(
	chunks <-chan provider.StreamChunk,
	embedding []float32,
	model string,
	ctx context.Context,
) <-chan provider.StreamChunk {
	// out is the channel that stream.Write will read from. We buffer it
	// to 1 so the goroutine can stay slightly ahead of the writer without
	// blocking on every single chunk.
	out := make(chan provider.StreamChunk, 1)

	go func() {
		// Close the output channel when the goroutine exits. This signals
		// to stream.Write that the stream is done (its range loop will end).
		defer close(out)

		// strings.Builder efficiently concatenates all the delta text
		// fragments into one string. Each WriteString appends to an
		// internal byte buffer — no new string allocation per chunk.
		var buf strings.Builder
		var lastChunk provider.StreamChunk

		for chunk := range chunks {
			// Forward every chunk to the output channel so stream.Write
			// can send it to the client immediately.
			out <- chunk

			// Skip error chunks — don't cache failed streams.
			if chunk.Error != nil {
				return
			}

			// Accumulate the text delta for cache reconstruction.
			buf.WriteString(chunk.Delta)

			// Keep track of the last chunk — it carries the response ID,
			// model name, and usage stats that we need for the cached response.
			if chunk.Done {
				lastChunk = chunk
			}
		}

		// Only cache if we got a complete stream (saw a Done chunk)
		// and actually have content to store.
		if lastChunk.Done && buf.Len() > 0 {
			resp := &provider.ChatResponse{
				ID:      lastChunk.ID,
				Model:   lastChunk.Model,
				Content: buf.String(),
			}
			if lastChunk.Usage != nil {
				resp.Usage = *lastChunk.Usage
			}
			resp.CostUSD = computeCost(model, resp.Usage, s.cfg.Costs)

			if err := s.cache.Store(ctx, embedding, model, resp); err != nil {
				log.Printf("cache store error (streaming): %v", err)
			}
		}
	}()

	return out
}

// handleHealth responds with a simple JSON status indicating the server
// is alive. Later we'll expand this to check provider connectivity, Redis,
// etc. — but for now it's a basic liveness probe.
//
// In Express terms, this is like:
//   app.get('/health', (req, res) => res.json({ status: 'ok' }))
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Set the Content-Type header BEFORE calling WriteHeader or Write.
	// In Go, headers must be set before the first write — once you start
	// writing the body, headers are locked in (sent over the wire).
	w.Header().Set("Content-Type", "application/json")

	// json.NewEncoder(w) creates a JSON encoder that writes directly to
	// the ResponseWriter. Encode() serializes the value and writes it.
	// This is the Go equivalent of res.json({...}) in Express, but split
	// into two explicit steps: set the header, then encode the body.
	//
	// We're passing an anonymous struct here — a quick throwaway type
	// defined inline. It's like writing { status: "ok" } as an object
	// literal in JS, except Go needs the field types declared.
	// The `json:"status"` part is a "struct tag" — it tells the JSON
	// encoder to use "status" as the key name (lowercase) instead of
	// the Go field name "Status" (uppercase).
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}

// handleCacheStats returns cache performance metrics as JSON.
func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if s.cache == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "cache is not enabled",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.cache.Stats())
}

// handleCacheFlush deletes all cached entries and resets stats.
func (s *Server) handleCacheFlush(w http.ResponseWriter, r *http.Request) {
	if s.cache == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "cache is not enabled",
		})
		return
	}

	if err := s.cache.Flush(r.Context()); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "flush failed: " + err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "flushed",
	})
}

// handleChatCompletions handles POST /v1/chat/completions.
// It decodes the request, resolves the provider from the model name,
// and dispatches to either the streaming or non-streaming path.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Captured by the deferred metrics recorder. Filled in as the request
	// progresses — provider/model are known after routing, cacheStatus is
	// updated on hit/skip/only-miss paths.
	var (
		metricProvider    string
		metricModel       string
		metricCacheStatus = metrics.CacheMiss
	)
	defer func() {
		if metricProvider == "" {
			return // early failure before a provider was chosen — skip
		}
		metrics.Requests.WithLabelValues(metricProvider, metricModel, metricCacheStatus).Inc()
		metrics.RequestDuration.WithLabelValues(metricProvider, metricModel).Observe(time.Since(start).Seconds())
	}()

	// Step 1: Decode the incoming JSON body into our unified ChatRequest.
	var req provider.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
		return
	}

	// Read routing/caching control headers.
	xCache := r.Header.Get("X-Cache")       // "auto", "skip", "only"
	xRoute := r.Header.Get("X-Route")       // "auto", "cheapest", "quality"
	xProvider := r.Header.Get("X-Provider") // "google", "anthropic"

	// Step 2: Compute embedding.
	// The embedding is needed for both cache lookup and auto-routing, so
	// we compute it whenever an embedder is available — not just when
	// caching is enabled.
	needsRouting := req.Model == "auto"
	cacheEnabled := s.embedder != nil && s.cache != nil && xCache != "skip"

	var embedding []float32
	if s.embedder != nil && (cacheEnabled || needsRouting) {
		userMsg, err := lastUserMessage(req.Messages)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error": err.Error(),
			})
			return
		}

		embedStart := time.Now()
		embedding, err = s.embedder.Embed(userMsg)
		metrics.EmbeddingDuration.Observe(time.Since(embedStart).Seconds())
		if err != nil {
			log.Printf("embedding error: %v", err)
			// Without an embedding, caching can't work. But routing
			// failures are fatal — we can't pick a model without it.
			cacheEnabled = false
			if needsRouting {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "failed to compute embedding for routing: " + err.Error(),
				})
				return
			}
		}
	}

	// Resolve "auto" to a concrete model before cache lookup. Cache
	// entries are partitioned by model name, so looking up under "auto"
	// would miss every entry stored under the routed model.
	if req.Model == "auto" {
		if s.modelRouter == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "auto routing is not configured",
			})
			return
		}

		routed, err := s.modelRouter.Route(embedding, xRoute, xProvider)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "routing error: " + err.Error(),
			})
			return
		}
		req.Model = routed
	}

	if cacheEnabled {
		result, err := s.cache.Lookup(r.Context(), embedding, req.Model)
		if err != nil {
			log.Printf("cache lookup error (skipping cache): %v", err)
		} else if result != nil {
			// Cache HIT
			w.Header().Set("X-LLMRouter-Cache", "HIT")
			w.Header().Set("X-LLMRouter-Similarity", fmt.Sprintf("%.4f", result.Similarity))

			metricCacheStatus = metrics.CacheHit
			metricModel = result.Response.Model
			if p, ok := s.models[metricModel]; ok {
				metricProvider = p.Name()
			}
			w.Header().Set("X-LLMRouter-Provider", metricProvider)
			w.Header().Set("X-LLMRouter-Model", metricModel)
			metrics.CacheSimilarity.WithLabelValues("hit").Observe(result.Similarity)
			if result.Response.CostUSD > 0 && metricProvider != "" {
				metrics.CostSavedByCache.WithLabelValues(metricProvider, metricModel).Add(result.Response.CostUSD)
			}

			if req.Stream {
				// Replay as a fast SSE burst — stream.Write doesn't
				// know (or care) that these chunks came from cache.
				chunks := replayChunks(result.Response)
				if err := stream.Write(w, chunks, stream.WriteOptions{
					Provider:     metricProvider,
					Model:        metricModel,
					RequestStart: start,
					CostFn:       costFnForModel(result.Response.Model, s.cfg.Costs),
				}); err != nil {
					log.Printf("stream write error: %v", err)
				}
				return
			}

			// Non-streaming: return as JSON.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result.Response)
			return
		}
	}

	// X-Cache: "only" returns 404 on a cache miss instead of calling
	// the provider. Useful for testing cache without spending tokens.
	if xCache == "only" {
		metricCacheStatus = metrics.CacheOnlyMiss
		w.Header().Set("X-LLMRouter-Cache", "MISS")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "cache miss (x-cache: only)",
		})
		return
	}

	// Cache MISS (or cache disabled) — forward to the provider.
	w.Header().Set("X-LLMRouter-Cache", "MISS")
	if xCache == "skip" {
		metricCacheStatus = metrics.CacheSkip
	}

	// Resolve the provider from the model name.
	p, err := s.resolveProvider(req.Model)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": err.Error(),
		})
		return
	}

	w.Header().Set("X-LLMRouter-Provider", p.Name())
	w.Header().Set("X-LLMRouter-Model", req.Model)
	metricProvider = p.Name()
	metricModel = req.Model

	// Step 4: Branch on streaming vs non-streaming.
	const maxRetries = 3

	if req.Stream {
		var chunks <-chan provider.StreamChunk
		err := provider.Retry(r.Context(), maxRetries, func() error {
			var callErr error
			chunks, callErr = p.ChatCompletionStream(r.Context(), &req)
			return callErr
		})
		if err != nil {
			metrics.ProviderErrors.WithLabelValues(p.Name(), classifyProviderError(err)).Inc()
			writeProviderError(w, err)
			return
		}

		// If caching is enabled, insert the tee stage between the
		// provider channel and the SSE writer. stream.Write reads
		// from the tee's output channel — it doesn't know or care
		// that there's a goroutine buffering for cache storage.
		if cacheEnabled {
			chunks = s.teeAndCache(chunks, embedding, req.Model, r.Context())
		}

		providerName := p.Name()
		model := req.Model
		if err := stream.Write(w, chunks, stream.WriteOptions{
			Provider:     providerName,
			Model:        model,
			RequestStart: start,
			CostFn:       costFnForModel(model, s.cfg.Costs),
			OnDone: func(usage provider.Usage, cost float64) {
				metrics.Tokens.WithLabelValues(providerName, model, metrics.DirInput).Add(float64(usage.PromptTokens))
				metrics.Tokens.WithLabelValues(providerName, model, metrics.DirOutput).Add(float64(usage.CompletionTokens))
				metrics.PromptTokens.Observe(float64(usage.PromptTokens))
				metrics.CostUSD.WithLabelValues(providerName, model).Add(cost)
				metrics.CostPerRequest.WithLabelValues(providerName, model).Observe(cost)
				s.observeRoutingSavings(providerName, xProvider, model, usage, cost)
			},
		}); err != nil {
			log.Printf("stream write error: %v", err)
		}
		return
	}

	// Non-streaming path.
	var resp *provider.ChatResponse
	err = provider.Retry(r.Context(), maxRetries, func() error {
		var callErr error
		resp, callErr = p.ChatCompletion(r.Context(), &req)
		return callErr
	})
	if err != nil {
		writeProviderError(w, err)
		return
	}

	// Compute cost from token usage before caching/responding.
	resp.CostUSD = computeCost(req.Model, resp.Usage, s.cfg.Costs)

	metrics.Tokens.WithLabelValues(p.Name(), req.Model, metrics.DirInput).Add(float64(resp.Usage.PromptTokens))
	metrics.Tokens.WithLabelValues(p.Name(), req.Model, metrics.DirOutput).Add(float64(resp.Usage.CompletionTokens))
	metrics.PromptTokens.Observe(float64(resp.Usage.PromptTokens))
	metrics.CostUSD.WithLabelValues(p.Name(), req.Model).Add(resp.CostUSD)
	metrics.CostPerRequest.WithLabelValues(p.Name(), req.Model).Observe(resp.CostUSD)
	s.observeRoutingSavings(p.Name(), xProvider, req.Model, resp.Usage, resp.CostUSD)

	// Store the response in cache for future hits.
	if cacheEnabled {
		if err := s.cache.Store(r.Context(), embedding, req.Model, resp); err != nil {
			log.Printf("cache store error: %v", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
