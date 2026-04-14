package router

import (
	"fmt"
	"time"

	"github.com/howard-nolan/llmrouter/internal/metrics"
	ort "github.com/yalue/onnxruntime_go"
)

// ONNXClassifier scores prompt complexity using a GBT model exported to ONNX.
// It satisfies the Classifier interface defined in router.go.
//
// Requires the ONNX Runtime environment to be initialized before construction
// (the embedder does this in main.go via ort.InitializeEnvironment). ONNX
// Runtime only allows one environment per process — creating a second panics.
type ONNXClassifier struct {
	session  *ort.DynamicAdvancedSession
	inputDim int
}

// NewONNXClassifier loads the ONNX complexity classifier model and returns
// a ready-to-use classifier. modelPath points to the .onnx file exported
// by training/export_onnx.py.
//
// The ONNX model has:
//   - Input:  "float_input"   — shape [1, inputDim], float32 embedding vector
//   - Output: "probabilities" — shape [1, 2], float32 class probabilities
//
// We only request "probabilities" (not "label") because the router needs
// the continuous score, not a binary decision.
func NewONNXClassifier(modelPath string, inputDim int) (*ONNXClassifier, error) {
	session, err := ort.NewDynamicAdvancedSession(
		modelPath,
		[]string{"float_input"},
		[]string{"probabilities"},
		nil, // nil options = default session config
	)
	if err != nil {
		return nil, fmt.Errorf("creating classifier ONNX session from %s: %w", modelPath, err)
	}

	return &ONNXClassifier{
		session:  session,
		inputDim: inputDim,
	}, nil
}

// Classify returns a complexity score between 0 (simple) and 1 (complex)
// for the given embedding vector. The router compares this score against
// its configured threshold to decide cheap vs. quality model.
func (c *ONNXClassifier) Classify(embedding []float32) (float64, error) {
	start := time.Now()
	defer func() {
		metrics.ClassificationDuration.Observe(time.Since(start).Seconds())
	}()

	if len(embedding) != c.inputDim {
		return 0, fmt.Errorf(
			"classifier: expected embedding of dimension %d, got %d",
			c.inputDim, len(embedding),
		)
	}

	// Create the input tensor: shape [1, inputDim] (batch of one).
	inputTensor, err := ort.NewTensor(
		ort.Shape{1, int64(c.inputDim)},
		embedding,
	)
	if err != nil {
		return 0, fmt.Errorf("creating classifier input tensor: %w", err)
	}
	defer inputTensor.Destroy()

	// Create the output tensor: shape [1, 2] — probability for each class.
	// [0][0] = P(adequate), [0][1] = P(needs-expensive).
	outputTensor, err := ort.NewEmptyTensor[float32](ort.Shape{1, 2})
	if err != nil {
		return 0, fmt.Errorf("creating classifier output tensor: %w", err)
	}
	defer outputTensor.Destroy()

	// Run inference.
	err = c.session.Run(
		[]ort.Value{inputTensor},
		[]ort.Value{outputTensor},
	)
	if err != nil {
		return 0, fmt.Errorf("running classifier inference: %w", err)
	}

	// Read the probability of class 1 (needs-expensive) as the complexity score.
	probs := outputTensor.GetData()
	score := float64(probs[1])
	metrics.ComplexityScore.Observe(score)
	return score, nil
}

// Close releases the ONNX session. Does NOT destroy the ONNX Runtime
// environment — the embedder owns that lifecycle.
func (c *ONNXClassifier) Close() {
	c.session.Destroy()
}
