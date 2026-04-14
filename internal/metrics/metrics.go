// Package metrics defines and registers Prometheus metric collectors for the
// llmrouter gateway. Every collector is a package-level var registered at
// init time with the default Prometheus registry via promauto, so callers
// just import the package and use the exported variables directly.
//
// Label ordering is positional: WithLabelValues arguments must match the
// order in each collector's "labels:" comment.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Cache status values attached to Requests and related metrics.
const (
	CacheHit      = "HIT"
	CacheMiss     = "MISS"
	CacheSkip     = "SKIP"
	CacheOnlyMiss = "ONLY_MISS"
)

// Provider error type values — capped to a small enum to keep cardinality
// bounded. Free-form error strings must map to one of these before being
// passed as a label.
const (
	ErrTimeout     = "timeout"
	ErrRateLimit   = "rate_limit"
	ErrAuth        = "auth"
	ErrUpstream5xx = "upstream_5xx"
	ErrOther       = "other"
)

// Token direction values for Tokens.
const (
	DirInput  = "input"
	DirOutput = "output"
)

var (
	// labels: provider, model, cache_status
	Requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_requests_total",
		Help: "Total chat completion requests, partitioned by provider, model, and cache result.",
	}, []string{"provider", "model", "cache_status"})

	// labels: provider, model
	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llmrouter_request_duration_seconds",
		Help:    "End-to-end request duration from handler entry to response completion.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30},
	}, []string{"provider", "model"})

	// labels: provider, model
	TimeToFirstToken = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llmrouter_time_to_first_token_seconds",
		Help:    "Time from request start to first streamed chunk.",
		Buckets: []float64{.05, .1, .2, .5, 1, 2, 5},
	}, []string{"provider", "model"})

	// labels: provider, model
	InterTokenLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llmrouter_inter_token_latency_seconds",
		Help:    "Gap between consecutive streamed chunks.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25},
	}, []string{"provider", "model"})

	// labels: provider, model, direction
	Tokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_tokens_total",
		Help: "Total tokens processed, partitioned by direction (input|output).",
	}, []string{"provider", "model", "direction"})

	PromptTokens = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "llmrouter_prompt_tokens",
		Help:    "Distribution of per-request input token counts.",
		Buckets: []float64{16, 64, 256, 1024, 4096, 16384, 65536},
	})

	// labels: provider, model
	CostUSD = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_cost_usd_total",
		Help: "Cumulative USD cost of provider calls.",
	}, []string{"provider", "model"})

	// labels: provider, model
	CostPerRequest = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llmrouter_cost_per_request_usd",
		Help:    "Per-request USD cost distribution.",
		Buckets: []float64{.00001, .0001, .001, .01, .1, 1},
	}, []string{"provider", "model"})

	// labels: provider, model — of the *cached* response that served the hit.
	CostSavedByCache = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_cost_saved_by_cache_usd_total",
		Help: "Cumulative USD cost avoided by serving responses from cache.",
	}, []string{"provider", "model"})

	// labels: provider — estimated savings from auto-routing to the cheap model.
	CostSavedByRouting = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_cost_saved_by_routing_usd_total",
		Help: "Cumulative estimated USD cost avoided by routing to the cheap model. Estimator: (prompt_tokens × quality_input_price + completion_tokens × quality_output_price) − actual_cost.",
	}, []string{"provider"})

	// labels: result (hit|miss)
	CacheSimilarity = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llmrouter_cache_similarity_score",
		Help:    "Best cosine similarity score from cache lookup, regardless of hit/miss threshold outcome.",
		Buckets: []float64{.5, .7, .8, .85, .9, .92, .94, .96, .98, 1.0},
	}, []string{"result"})

	// labels: strategy, selected_model
	RoutingDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_routing_decisions_total",
		Help: "Routing decisions for auto-model requests, by strategy and resulting model.",
	}, []string{"strategy", "selected_model"})

	EmbeddingDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "llmrouter_embedding_duration_seconds",
		Help:    "Duration of the ONNX embedding inference step.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1},
	})

	ClassificationDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "llmrouter_classification_duration_seconds",
		Help:    "Duration of the complexity classifier inference step.",
		Buckets: []float64{.0005, .001, .005, .01, .025},
	})

	ComplexityScore = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "llmrouter_complexity_score",
		Help:    "Distribution of complexity classifier scores for auto-routed requests.",
		Buckets: []float64{0, .1, .2, .3, .4, .5, .6, .7, .8, .9, 1.0},
	})

	// labels: provider, error_type
	ProviderErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_provider_errors_total",
		Help: "Provider call errors, partitioned by capped error_type enum.",
	}, []string{"provider", "error_type"})
)

// RegisterCacheEntries installs a scrape-time gauge whose value is supplied
// by fn. Use with a closure over cache.Stats() from main.go so the metrics
// package stays free of cache dependencies.
func RegisterCacheEntries(fn func() float64) {
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "llmrouter_cache_entries",
		Help: "Current number of entries in the semantic cache.",
	}, fn)
}
