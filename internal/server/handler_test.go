package server

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/howard-nolan/llmrouter/internal/cache"
	"github.com/howard-nolan/llmrouter/internal/config"
	"github.com/howard-nolan/llmrouter/internal/provider"
)

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

// mockEmbedder implements the Embedder interface. Each test provides an
// EmbedFunc that returns deterministic embeddings for known inputs.
type mockEmbedder struct {
	EmbedFunc func(text string) ([]float32, error)
}

func (m *mockEmbedder) Embed(text string) ([]float32, error) {
	return m.EmbedFunc(text)
}

// mockProvider implements provider.Provider with canned responses.
type mockProvider struct {
	name     string
	response *provider.ChatResponse
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) ChatCompletion(_ context.Context, _ *provider.ChatRequest) (*provider.ChatResponse, error) {
	return m.response, nil
}

func (m *mockProvider) ChatCompletionStream(_ context.Context, _ *provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 2)
	ch <- provider.StreamChunk{
		ID:    m.response.ID,
		Model: m.response.Model,
		Delta: m.response.Content,
	}
	ch <- provider.StreamChunk{
		ID:    m.response.ID,
		Model: m.response.Model,
		Done:  true,
		Usage: &m.response.Usage,
	}
	close(ch)
	return ch, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// normalizedVec builds a 384-dim L2-normalized vector with all its weight
// in a single dimension. Different indices produce orthogonal vectors
// (cosine similarity = 0).
func normalizedVec(index int) []float32 {
	vec := make([]float32, 384)
	vec[index] = 1.0
	return vec
}

// similarVec builds a vector that's close to normalizedVec(0) by putting
// most weight in dimension 0 and a small amount in dimension 1. The cosine
// similarity to normalizedVec(0) is cos(theta) ≈ factor/magnitude.
func similarVec(offset float32) []float32 {
	vec := make([]float32, 384)
	vec[0] = 1.0
	vec[1] = offset
	// L2 normalize
	mag := float32(math.Sqrt(float64(vec[0]*vec[0] + vec[1]*vec[1])))
	vec[0] /= mag
	vec[1] /= mag
	return vec
}

// setupTestServer creates a Server wired with a mock embedder, miniredis
// cache, and a mock provider registered for model "test-model".
func setupTestServer(t *testing.T, embedFunc func(string) ([]float32, error)) *Server {
	t.Helper()

	mr := miniredis.RunT(t)

	rc, err := cache.NewRedisCache(cache.CacheConfig{
		RedisURL:            "redis://" + mr.Addr(),
		SimilarityThreshold: 0.92,
		TTL:                 1 * time.Hour,
		MaxEntries:          100,
	})
	require.NoError(t, err)
	t.Cleanup(func() { rc.Close() })

	emb := &mockEmbedder{EmbedFunc: embedFunc}

	mp := &mockProvider{
		name: "test-provider",
		response: &provider.ChatResponse{
			ID:      "resp-123",
			Model:   "test-model",
			Content: "This is a test response.",
			Usage: provider.Usage{
				PromptTokens:     10,
				CompletionTokens: 20,
				TotalTokens:      30,
			},
		},
	}

	models := map[string]provider.Provider{
		"test-model": mp,
	}

	cfg := &config.Config{}

	return New(cfg, models, emb, rc)
}

// doRequest sends a JSON request to the server and returns the recorder.
func doRequest(t *testing.T, srv *Server, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	jsonBody, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// parseSSEEvents extracts data payloads from SSE output, excluding [DONE].
func parseSSEEvents(body string) []string {
	var events []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload != "[DONE]" {
				events = append(events, payload)
			}
		}
	}
	return events
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCacheHit_SamePromptTwice(t *testing.T) {
	// The embedder returns the same vector for any input, so the second
	// request will match the first in the cache.
	srv := setupTestServer(t, func(text string) ([]float32, error) {
		return normalizedVec(0), nil
	})

	body := map[string]interface{}{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   false,
	}

	// First request — cache miss, hits the provider.
	w1 := doRequest(t, srv, body)
	assert.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "MISS", w1.Header().Get("X-LLMRouter-Cache"))

	var resp1 provider.ChatResponse
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &resp1))
	assert.Equal(t, "This is a test response.", resp1.Content)

	// Second request — same embedding, should be a cache hit.
	w2 := doRequest(t, srv, body)
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "HIT", w2.Header().Get("X-LLMRouter-Cache"))

	var resp2 provider.ChatResponse
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &resp2))
	assert.Equal(t, "This is a test response.", resp2.Content)
}

func TestCacheHit_StreamingReplay(t *testing.T) {
	srv := setupTestServer(t, func(text string) ([]float32, error) {
		return normalizedVec(0), nil
	})

	nonStreamBody := map[string]interface{}{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   false,
	}

	// Seed the cache with a non-streaming request.
	w1 := doRequest(t, srv, nonStreamBody)
	require.Equal(t, "MISS", w1.Header().Get("X-LLMRouter-Cache"))

	// Now request the same thing with stream: true.
	streamBody := map[string]interface{}{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   true,
	}

	w2 := doRequest(t, srv, streamBody)
	assert.Equal(t, "HIT", w2.Header().Get("X-LLMRouter-Cache"))
	assert.Equal(t, "text/event-stream", w2.Header().Get("Content-Type"))

	// Should contain SSE events with the cached content.
	body := w2.Body.String()
	assert.Contains(t, body, "data: [DONE]")

	events := parseSSEEvents(body)
	require.GreaterOrEqual(t, len(events), 1, "expected at least one SSE event")

	// The content chunk should contain the cached response text.
	assert.Contains(t, events[0], "This is a test response.")
}

func TestCacheMiss_DissimilarPrompt(t *testing.T) {
	callCount := 0
	// Return orthogonal vectors for different inputs — these will have
	// cosine similarity of 0.0, well below the 0.92 threshold.
	srv := setupTestServer(t, func(text string) ([]float32, error) {
		vec := normalizedVec(callCount)
		callCount++
		return vec, nil
	})

	body1 := map[string]interface{}{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   false,
	}
	body2 := map[string]interface{}{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "something completely different"}},
		"stream":   false,
	}

	// First request — miss.
	w1 := doRequest(t, srv, body1)
	assert.Equal(t, "MISS", w1.Header().Get("X-LLMRouter-Cache"))

	// Second request with different embedding — also a miss.
	w2 := doRequest(t, srv, body2)
	assert.Equal(t, "MISS", w2.Header().Get("X-LLMRouter-Cache"))
}

func TestXCache_SkipBypassesCache(t *testing.T) {
	srv := setupTestServer(t, func(text string) ([]float32, error) {
		return normalizedVec(0), nil
	})

	body := map[string]interface{}{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   false,
	}

	// Seed the cache.
	w1 := doRequest(t, srv, body)
	assert.Equal(t, "MISS", w1.Header().Get("X-LLMRouter-Cache"))

	// Same prompt with x-cache: skip — should bypass cache and miss.
	skipBody := map[string]interface{}{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   false,
		"x-cache":  "skip",
	}

	w2 := doRequest(t, srv, skipBody)
	assert.Equal(t, "MISS", w2.Header().Get("X-LLMRouter-Cache"))
}

func TestXCache_OnlyReturns404OnMiss(t *testing.T) {
	srv := setupTestServer(t, func(text string) ([]float32, error) {
		return normalizedVec(0), nil
	})

	body := map[string]interface{}{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   false,
		"x-cache":  "only",
	}

	// Nothing in cache — should get 404.
	w := doRequest(t, srv, body)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "MISS", w.Header().Get("X-LLMRouter-Cache"))

	var errResp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Contains(t, errResp["error"], "x-cache: only")
}

func TestXCache_OnlyReturnsCachedResponse(t *testing.T) {
	srv := setupTestServer(t, func(text string) ([]float32, error) {
		return normalizedVec(0), nil
	})

	body := map[string]interface{}{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   false,
	}

	// Seed the cache.
	w1 := doRequest(t, srv, body)
	require.Equal(t, "MISS", w1.Header().Get("X-LLMRouter-Cache"))

	// Same prompt with x-cache: only — should return the cached response.
	onlyBody := map[string]interface{}{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   false,
		"x-cache":  "only",
	}

	w2 := doRequest(t, srv, onlyBody)
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "HIT", w2.Header().Get("X-LLMRouter-Cache"))

	var resp provider.ChatResponse
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &resp))
	assert.Equal(t, "This is a test response.", resp.Content)
}
