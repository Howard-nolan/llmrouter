// Package router implements cost-aware model routing and complexity classification.
//
// When a request arrives with "model": "auto", the router selects a concrete
// model based on the routing strategy (auto, cheapest, quality) and the
// configured per-provider cheap/quality model pairs.
package router

import (
	"fmt"

	"github.com/howard-nolan/llmrouter/internal/config"
)

// Classifier scores prompt complexity from an embedding vector. Returns a
// value between 0 (simple) and 1 (complex). Defined here at the consumer
// rather than in a classifier package — same interface-at-consumer pattern
// used by the server package's Embedder interface.
type Classifier interface {
	Classify(embedding []float32) (float64, error)
}

// Router selects a concrete model for "auto" requests based on the routing
// strategy and an optional complexity classifier.
type Router struct {
	cfg        config.RoutingConfig
	classifier Classifier
}

// New creates a Router. classifier may be nil — cheapest and quality
// strategies will still work, but auto will return an error.
func New(cfg config.RoutingConfig, classifier Classifier) *Router {
	return &Router{
		cfg:        cfg,
		classifier: classifier,
	}
}

// Route picks a concrete model name given the prompt embedding, the
// per-request strategy override, and the per-request provider override.
// Empty strategy/providerName fall back to the config defaults.
//
// Returns the model name (e.g. "claude-haiku-4-5-20251001"). The caller
// uses the existing model-to-provider map to resolve the Provider from this.
func (rt *Router) Route(embedding []float32, strategy string, providerName string) (string, error) {
	// Fall back to config defaults for empty overrides.
	if strategy == "" {
		strategy = rt.cfg.DefaultStrategy
	}
	if providerName == "" {
		providerName = rt.cfg.DefaultProvider
	}

	// Look up the cheap/quality pair for this provider.
	providerCfg, ok := rt.cfg.Providers[providerName]
	if !ok {
		return "", fmt.Errorf("no routing config for provider %q", providerName)
	}

	switch strategy {
	case "cheapest":
		return providerCfg.CheapModel, nil

	case "quality":
		return providerCfg.QualityModel, nil

	case "auto":
		if rt.classifier == nil {
			return "", fmt.Errorf("auto routing requires a classifier, but none is configured")
		}

		score, err := rt.classifier.Classify(embedding)
		if err != nil {
			return "", fmt.Errorf("classifying prompt complexity: %w", err)
		}

		if score < rt.cfg.ComplexityThreshold {
			return providerCfg.CheapModel, nil
		}
		return providerCfg.QualityModel, nil

	default:
		return "", fmt.Errorf("unknown routing strategy: %q", strategy)
	}
}
