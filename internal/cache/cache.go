// Package cache implements semantic response caching with Redis storage.
package cache

import (
	"context"

	"github.com/howard-nolan/llmrouter/internal/provider"
)

// Cache is the interface for semantic response caching. Implementations
// store LLM responses keyed by embedding vectors and retrieve them via
// cosine similarity search.
//
// This is the same pattern as provider.Provider — define the contract
// here, implement it in a separate file (redis.go). Consumers depend
// on the interface, not the implementation, so we can swap backends
// or use mocks in tests.
type Cache interface {
	// Lookup scans cached embeddings for the closest match to the given
	// embedding. Returns the cached response if similarity exceeds the
	// configured threshold, or nil if no match is found (nil, nil = miss).
	Lookup(ctx context.Context, embedding []float32) (*CacheResult, error)

	// Store saves an LLM response keyed by its prompt embedding. Called
	// after a cache miss once the provider returns a successful response.
	Store(ctx context.Context, embedding []float32, response *provider.ChatResponse) error

	// Stats returns current cache metrics (hits, misses, entry count).
	// Uses in-memory atomic counters, so no Redis call is needed.
	Stats() CacheStats

	// Flush deletes all cached entries. Powers the /cache/flush admin endpoint.
	Flush(ctx context.Context) error

	// Close releases resources (Redis connection pool). Call during
	// graceful shutdown to avoid leaking TCP connections.
	Close() error
}

// CacheResult wraps a cached response with metadata. Returned by Lookup
// on a cache hit — the handler uses Similarity for the debug header and
// Key for logging.
type CacheResult struct {
	Response   *provider.ChatResponse
	Similarity float64 // cosine similarity score (0.0–1.0)
	Key        string  // Redis key for this entry
}

// CacheStats holds cache performance metrics. Fields are int64 because
// they're updated atomically from concurrent goroutines — atomic ops
// in Go require int64, not int.
type CacheStats struct {
	Hits          int64
	Misses        int64
	Entries       int64
	AvgSimilarity float64
}
