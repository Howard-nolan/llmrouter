package cache

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/howard-nolan/llmrouter/internal/provider"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// normalizedVec builds a 384-dim L2-normalized vector where the first element
// is set to the given value and the rest are zero. Normalizing means dividing
// every element by the vector's magnitude, so the result always has magnitude
// 1.0 — matching what our ONNX embedding model produces.
//
// Using different seed values gives us vectors that point in different
// directions, so their cosine similarity (dot product) is low.
func normalizedVec(seed float32) []float32 {
	vec := make([]float32, 384)
	vec[0] = seed
	// L2 norm of a vector with one non-zero element is just |seed|.
	norm := float32(math.Abs(float64(seed)))
	vec[0] = seed / norm
	return vec
}

// fakeResponse builds a minimal ChatResponse for testing.
func fakeResponse(content string) *provider.ChatResponse {
	return &provider.ChatResponse{
		Model:   "test-model",
		Content: content,
		Usage: provider.Usage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}
}

// setupCache starts a miniredis server and returns a RedisCache connected to
// it. The server is automatically shut down when the test finishes.
func setupCache(t *testing.T, maxEntries int) *RedisCache {
	t.Helper()

	mr := miniredis.RunT(t)

	rc, err := NewRedisCache(CacheConfig{
		RedisURL:            "redis://" + mr.Addr(),
		SimilarityThreshold: 0.92,
		TTL:                 1 * time.Hour,
		MaxEntries:          maxEntries,
	})
	require.NoError(t, err)

	t.Cleanup(func() { rc.Close() })

	return rc
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestStoreAndLookup_IdenticalEmbedding(t *testing.T) {
	rc := setupCache(t, 100)
	ctx := context.Background()

	embedding := normalizedVec(1.0)
	resp := fakeResponse("Hello, world!")

	// Store a response.
	err := rc.Store(ctx, embedding, "test-model", resp)
	require.NoError(t, err)

	// Look up with the exact same embedding and model — should be a hit.
	result, err := rc.Lookup(ctx, embedding, "test-model")
	require.NoError(t, err)
	require.NotNil(t, result, "expected cache hit for identical embedding")

	assert.Equal(t, "Hello, world!", result.Response.Content)
	assert.Equal(t, "test-model", result.Response.Model)
	assert.InDelta(t, 1.0, result.Similarity, 0.001, "identical embeddings should have similarity ~1.0")

	// Stats should reflect 1 hit, 0 misses.
	stats := rc.Stats()
	assert.Equal(t, int64(1), stats.Hits)
	assert.Equal(t, int64(0), stats.Misses)
	assert.Equal(t, int64(1), stats.Entries)
}

func TestLookup_DissimilarEmbedding(t *testing.T) {
	rc := setupCache(t, 100)
	ctx := context.Background()

	// Store with one vector direction.
	err := rc.Store(ctx, normalizedVec(1.0), "test-model", fakeResponse("cached response"))
	require.NoError(t, err)

	// Look up with a vector pointing in a completely different dimension.
	// normalizedVec(1.0) has all its weight in vec[0],
	// this vector has all its weight in vec[1] — orthogonal, so dot product = 0.
	orthogonal := make([]float32, 384)
	orthogonal[1] = 1.0

	result, err := rc.Lookup(ctx, orthogonal, "test-model")
	require.NoError(t, err)
	assert.Nil(t, result, "expected cache miss for dissimilar embedding")

	// Stats should reflect 0 hits, 1 miss.
	stats := rc.Stats()
	assert.Equal(t, int64(0), stats.Hits)
	assert.Equal(t, int64(1), stats.Misses)
}

func TestEviction_MaxEntries(t *testing.T) {
	rc := setupCache(t, 2)
	ctx := context.Background()

	// Create three distinct normalized embeddings using different dimensions.
	vec1 := make([]float32, 384)
	vec1[0] = 1.0
	vec2 := make([]float32, 384)
	vec2[1] = 1.0
	vec3 := make([]float32, 384)
	vec3[2] = 1.0

	require.NoError(t, rc.Store(ctx, vec1, "test-model", fakeResponse("first")))
	// Small sleep to ensure distinct timestamps in the sorted set, so
	// eviction order is deterministic (lowest score = oldest = evicted first).
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, rc.Store(ctx, vec2, "test-model", fakeResponse("second")))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, rc.Store(ctx, vec3, "test-model", fakeResponse("third")))

	// With MaxEntries=2, the oldest (vec1/"first") should have been evicted.
	stats := rc.Stats()
	assert.Equal(t, int64(2), stats.Entries)

	// vec1 should miss — it was evicted.
	result, err := rc.Lookup(ctx, vec1, "test-model")
	require.NoError(t, err)
	assert.Nil(t, result, "expected evicted entry to be a cache miss")

	// vec2 and vec3 should still be present. They're orthogonal to each
	// other so they won't match each other, but looking up with their
	// exact embedding should hit (similarity = 1.0).
	result, err = rc.Lookup(ctx, vec2, "test-model")
	require.NoError(t, err)
	require.NotNil(t, result, "expected vec2 to still be cached")
	assert.Equal(t, "second", result.Response.Content)

	result, err = rc.Lookup(ctx, vec3, "test-model")
	require.NoError(t, err)
	require.NotNil(t, result, "expected vec3 to still be cached")
	assert.Equal(t, "third", result.Response.Content)
}

func TestFlush_ResetsEverything(t *testing.T) {
	rc := setupCache(t, 100)
	ctx := context.Background()

	// Store two entries and trigger a lookup so stats are non-zero.
	vec1 := make([]float32, 384)
	vec1[0] = 1.0
	vec2 := make([]float32, 384)
	vec2[1] = 1.0
	require.NoError(t, rc.Store(ctx, vec1, "test-model", fakeResponse("one")))
	require.NoError(t, rc.Store(ctx, vec2, "test-model", fakeResponse("two")))

	result, err := rc.Lookup(ctx, vec1, "test-model")
	require.NoError(t, err)
	require.NotNil(t, result)

	// Sanity check: stats are non-zero before flush.
	stats := rc.Stats()
	assert.Equal(t, int64(1), stats.Hits)
	assert.Equal(t, int64(2), stats.Entries)

	// Flush.
	err = rc.Flush(ctx)
	require.NoError(t, err)

	// Stats should be zeroed.
	stats = rc.Stats()
	assert.Equal(t, int64(0), stats.Hits)
	assert.Equal(t, int64(0), stats.Misses)
	assert.Equal(t, int64(0), stats.Entries)
	assert.Equal(t, float64(0), stats.AvgSimilarity)

	// Previously stored entry should now miss.
	result, err = rc.Lookup(ctx, vec1, "test-model")
	require.NoError(t, err)
	assert.Nil(t, result, "expected cache miss after flush")
}

func TestLookup_CrossModelIsolation(t *testing.T) {
	rc := setupCache(t, 100)
	ctx := context.Background()

	embedding := normalizedVec(1.0)

	// Store a response under model A.
	err := rc.Store(ctx, embedding, "model-a", fakeResponse("response from model A"))
	require.NoError(t, err)

	// Look up with the exact same embedding but a different model — should miss.
	result, err := rc.Lookup(ctx, embedding, "model-b")
	require.NoError(t, err)
	assert.Nil(t, result, "expected cache miss for different model with same embedding")

	// Same embedding, same model — should hit.
	result, err = rc.Lookup(ctx, embedding, "model-a")
	require.NoError(t, err)
	require.NotNil(t, result, "expected cache hit for same model and embedding")
	assert.Equal(t, "response from model A", result.Response.Content)
}
