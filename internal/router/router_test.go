package router

import (
	"testing"

	"github.com/howard-nolan/llmrouter/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockClassifier implements Classifier with a fixed score.
type mockClassifier struct {
	score float64
	err   error
}

func (m *mockClassifier) Classify(embedding []float32) (float64, error) {
	return m.score, m.err
}

// testConfig returns a RoutingConfig with two providers configured.
func testConfig() config.RoutingConfig {
	return config.RoutingConfig{
		DefaultStrategy:     "auto",
		DefaultProvider:     "anthropic",
		ComplexityThreshold: 0.6,
		Providers: map[string]config.RoutingProviderConfig{
			"google": {
				CheapModel:   "gemini-2.0-flash",
				QualityModel: "gemini-2.5-pro",
			},
			"anthropic": {
				CheapModel:   "claude-haiku-4-5-20251001",
				QualityModel: "claude-sonnet-4-5-20250929",
			},
		},
	}
}

var dummyEmbedding = []float32{0.1, 0.2, 0.3}

func TestRoute_CheapestStrategy(t *testing.T) {
	rt := New(testConfig(), nil)

	model, err := rt.Route(dummyEmbedding, "cheapest", "anthropic")
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5-20251001", model)
}

func TestRoute_QualityStrategy(t *testing.T) {
	rt := New(testConfig(), nil)

	model, err := rt.Route(dummyEmbedding, "quality", "google")
	require.NoError(t, err)
	assert.Equal(t, "gemini-2.5-pro", model)
}

func TestRoute_AutoStrategy_BelowThreshold(t *testing.T) {
	// Score 0.3 < threshold 0.6 → cheap model.
	rt := New(testConfig(), &mockClassifier{score: 0.3})

	model, err := rt.Route(dummyEmbedding, "auto", "anthropic")
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5-20251001", model)
}

func TestRoute_AutoStrategy_AboveThreshold(t *testing.T) {
	// Score 0.8 >= threshold 0.6 → quality model.
	rt := New(testConfig(), &mockClassifier{score: 0.8})

	model, err := rt.Route(dummyEmbedding, "auto", "anthropic")
	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-4-5-20250929", model)
}

func TestRoute_AutoStrategy_ExactlyAtThreshold(t *testing.T) {
	// Score 0.6 == threshold 0.6 → quality model (not strictly less than).
	rt := New(testConfig(), &mockClassifier{score: 0.6})

	model, err := rt.Route(dummyEmbedding, "auto", "anthropic")
	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-4-5-20250929", model)
}

func TestRoute_AutoStrategy_NilClassifier(t *testing.T) {
	rt := New(testConfig(), nil)

	_, err := rt.Route(dummyEmbedding, "auto", "anthropic")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "classifier")
}

func TestRoute_DefaultsFallback(t *testing.T) {
	// Empty strategy and provider should use config defaults
	// (auto strategy, anthropic provider).
	rt := New(testConfig(), &mockClassifier{score: 0.3})

	model, err := rt.Route(dummyEmbedding, "", "")
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5-20251001", model)
}

func TestRoute_ProviderOverride(t *testing.T) {
	// Default provider is anthropic, but override to google.
	rt := New(testConfig(), nil)

	model, err := rt.Route(dummyEmbedding, "cheapest", "google")
	require.NoError(t, err)
	assert.Equal(t, "gemini-2.0-flash", model)
}

func TestRoute_UnknownProvider(t *testing.T) {
	rt := New(testConfig(), nil)

	_, err := rt.Route(dummyEmbedding, "cheapest", "openai")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "openai")
}

func TestRoute_UnknownStrategy(t *testing.T) {
	rt := New(testConfig(), nil)

	_, err := rt.Route(dummyEmbedding, "fastest", "anthropic")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "fastest")
}

func TestRoute_ClassifierError(t *testing.T) {
	rt := New(testConfig(), &mockClassifier{err: assert.AnError})

	_, err := rt.Route(dummyEmbedding, "auto", "anthropic")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "classifying")
}
