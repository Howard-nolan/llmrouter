package cache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/viterin/vek/vek32"

	"github.com/howard-nolan/llmrouter/internal/provider"
)

// Redis key constants.
const (
	keyPrefix = "cache:"    // prefix for all cache entry hash keys
	indexKey  = "cache:index" // sorted set tracking all entries by timestamp
)

// CacheConfig holds the settings for the semantic cache, loaded from
// the cache: section of config.yaml.
type CacheConfig struct {
	RedisURL            string        // connection string, e.g. "redis://localhost:6379/0"
	SimilarityThreshold float64       // minimum cosine similarity for a cache hit (e.g. 0.92)
	TTL                 time.Duration // how long entries live before Redis auto-deletes them
	MaxEntries          int           // max cached entries — triggers eviction when full
}

// RedisCache implements the Cache interface using Redis for storage and
// brute-force cosine similarity for semantic lookup.
type RedisCache struct {
	client *redis.Client
	cfg    CacheConfig

	// Atomic counters for stats — int64 required by sync/atomic.
	hits          int64
	misses        int64
	similaritySum int64 // stored as float64 bits via math.Float64bits for atomic access
	hitCount      int64
}

// NewRedisCache creates a RedisCache and verifies the Redis connection.
func NewRedisCache(cfg CacheConfig) (*RedisCache, error) {
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("parsing redis URL: %w", err)
	}

	client := redis.NewClient(opts)

	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("connecting to redis: %w", err)
	}

	return &RedisCache{
		client: client,
		cfg:    cfg,
	}, nil
}

// ---------------------------------------------------------------------------
// Group 2: Serialization helpers
// ---------------------------------------------------------------------------

// embeddingToBytes converts a float32 slice to raw bytes for Redis storage.
// Each float32 is 4 bytes, so a 384-dim embedding becomes 1,536 bytes.
func embeddingToBytes(embedding []float32) []byte {
	buf := make([]byte, len(embedding)*4)
	for i, v := range embedding {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// bytesToEmbedding converts raw bytes back to a float32 slice.
func bytesToEmbedding(data []byte) []float32 {
	embedding := make([]float32, len(data)/4)
	for i := range embedding {
		embedding[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return embedding
}

// embeddingKey returns the Redis key for a cache entry by SHA-256 hashing
// the embedding bytes. Deterministic: same embedding always produces the
// same key, so re-storing is idempotent.
func embeddingKey(embedding []float32) string {
	hash := sha256.Sum256(embeddingToBytes(embedding))
	return keyPrefix + hex.EncodeToString(hash[:])
}

// ---------------------------------------------------------------------------
// Group 3: Store
// ---------------------------------------------------------------------------

// Store saves an LLM response keyed by its prompt embedding. Uses a Redis
// pipeline to batch the hash write, TTL set, and index update into one
// round-trip. Evicts the oldest entry if we're at MaxEntries.
func (rc *RedisCache) Store(ctx context.Context, embedding []float32, response *provider.ChatResponse) error {
	key := embeddingKey(embedding)

	responseJSON, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("marshaling response: %w", err)
	}

	now := time.Now()
	embBytes := embeddingToBytes(embedding)

	// Pipeline: batch 3 commands into 1 network round-trip.
	pipe := rc.client.Pipeline()
	pipe.HSet(ctx, key, map[string]interface{}{
		"embedding":  embBytes,
		"response":   responseJSON,
		"created_at": now.Unix(),
		"hit_count":  0,
	})
	pipe.Expire(ctx, key, rc.cfg.TTL)
	pipe.ZAdd(ctx, indexKey, redis.Z{
		Score:  float64(now.UnixMilli()),
		Member: key,
	})
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("storing cache entry: %w", err)
	}

	// Evict oldest entries if over the limit.
	if rc.cfg.MaxEntries > 0 {
		count, err := rc.client.ZCard(ctx, indexKey).Result()
		if err != nil {
			return fmt.Errorf("checking entry count: %w", err)
		}
		if int(count) > rc.cfg.MaxEntries {
			rc.evictOldest(ctx, int(count)-rc.cfg.MaxEntries)
		}
	}

	return nil
}

// evictOldest removes the n oldest entries (lowest scores in the index).
func (rc *RedisCache) evictOldest(ctx context.Context, n int) {
	// ZPopMin returns the n members with the lowest scores.
	entries, err := rc.client.ZPopMin(ctx, indexKey, int64(n)).Result()
	if err != nil {
		return // best-effort eviction — don't fail the Store call
	}

	for _, entry := range entries {
		rc.client.Del(ctx, entry.Member.(string))
	}
}

// ---------------------------------------------------------------------------
// Group 4: Lookup
// ---------------------------------------------------------------------------

// Lookup scans all cached embeddings for the closest cosine similarity match.
// Returns nil, nil on a cache miss (no match above threshold).
//
// Two-pass approach: first pass fetches only embeddings to find the best
// match (avoids deserializing every cached response). Second pass fetches
// the full response only for the winner.
func (rc *RedisCache) Lookup(ctx context.Context, embedding []float32) (*CacheResult, error) {
	// Get all cache keys from the sorted set index.
	keys, err := rc.client.ZRange(ctx, indexKey, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("reading cache index: %w", err)
	}

	if len(keys) == 0 {
		atomic.AddInt64(&rc.misses, 1)
		return nil, nil
	}

	// Pass 1: fetch embeddings and find the best cosine similarity match.
	var bestKey string
	var bestSim float64

	for _, key := range keys {
		embBytes, err := rc.client.HGet(ctx, key, "embedding").Bytes()
		if err != nil {
			continue // entry may have expired between ZRange and HGet
		}

		cached := bytesToEmbedding(embBytes)

		// Dot product = cosine similarity because embeddings are L2-normalized.
		sim := float64(vek32.Dot(embedding, cached))

		if sim > bestSim {
			bestSim = sim
			bestKey = key
		}
	}

	// Check if the best match clears the threshold.
	if bestSim < rc.cfg.SimilarityThreshold {
		atomic.AddInt64(&rc.misses, 1)
		return nil, nil
	}

	// Pass 2: fetch the full response for the winning entry.
	result, err := rc.client.HMGet(ctx, bestKey, "response", "hit_count").Result()
	if err != nil {
		return nil, fmt.Errorf("fetching cached response: %w", err)
	}
	if result[0] == nil {
		atomic.AddInt64(&rc.misses, 1)
		return nil, nil
	}

	var response provider.ChatResponse
	if err := json.Unmarshal([]byte(result[0].(string)), &response); err != nil {
		return nil, fmt.Errorf("unmarshaling cached response: %w", err)
	}

	// Increment hit count on the entry (fire-and-forget).
	rc.client.HIncrBy(ctx, bestKey, "hit_count", 1)

	// Update stats atomically.
	atomic.AddInt64(&rc.hits, 1)
	newHitCount := atomic.AddInt64(&rc.hitCount, 1)
	// Accumulate similarity sum for averaging. We use a simple non-atomic
	// addition here — slight imprecision under heavy concurrency is
	// acceptable for a stats gauge.
	rc.similaritySum = int64(math.Float64bits(
		math.Float64frombits(uint64(atomic.LoadInt64(&rc.similaritySum))) + bestSim,
	))
	_ = newHitCount

	return &CacheResult{
		Response:   &response,
		Similarity: bestSim,
		Key:        bestKey,
	}, nil
}

// ---------------------------------------------------------------------------
// Group 5: Stats, Flush, Close
// ---------------------------------------------------------------------------

// Stats returns current cache performance metrics.
func (rc *RedisCache) Stats() CacheStats {
	hits := atomic.LoadInt64(&rc.hits)
	misses := atomic.LoadInt64(&rc.misses)
	hitCount := atomic.LoadInt64(&rc.hitCount)

	var avgSim float64
	if hitCount > 0 {
		simSum := math.Float64frombits(uint64(atomic.LoadInt64(&rc.similaritySum)))
		avgSim = simSum / float64(hitCount)
	}

	// Entry count: read from the sorted set index. If Redis is unreachable,
	// return 0 rather than failing.
	entries, _ := rc.client.ZCard(context.Background(), indexKey).Result()

	return CacheStats{
		Hits:          hits,
		Misses:        misses,
		Entries:       entries,
		AvgSimilarity: avgSim,
	}
}

// Flush deletes all cache entries and resets stats counters.
func (rc *RedisCache) Flush(ctx context.Context) error {
	// Get all keys from the index, then delete them plus the index itself.
	keys, err := rc.client.ZRange(ctx, indexKey, 0, -1).Result()
	if err != nil {
		return fmt.Errorf("reading cache index: %w", err)
	}

	if len(keys) > 0 {
		// Append the index key itself so we delete everything in one call.
		keys = append(keys, indexKey)
		if err := rc.client.Del(ctx, keys...).Err(); err != nil {
			return fmt.Errorf("deleting cache entries: %w", err)
		}
	}

	// Reset stats.
	atomic.StoreInt64(&rc.hits, 0)
	atomic.StoreInt64(&rc.misses, 0)
	atomic.StoreInt64(&rc.hitCount, 0)
	atomic.StoreInt64(&rc.similaritySum, 0)

	return nil
}

// Close releases the Redis connection pool.
func (rc *RedisCache) Close() error {
	return rc.client.Close()
}
