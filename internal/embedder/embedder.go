// Package embedder wraps the ONNX embedding model for prompt vectorization.
package embedder

import (
	"fmt"

	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"
)

// Embedder is the interface for computing text embeddings. Consumers depend
// on this interface so they can swap implementations (e.g., mock in tests).
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// ONNXEmbedder tokenizes text and runs it through an ONNX embedding model to
// produce a fixed-size vector. Used for semantic cache lookups and complexity
// classification. Satisfies the Embedder interface.
type ONNXEmbedder struct {
	tokenizer *tokenizers.Tokenizer
	session   *ort.DynamicAdvancedSession
	dimension int
}

// New creates an Embedder by loading the tokenizer and ONNX model. The
// libraryPath must point to the ONNX Runtime shared library
// (libonnxruntime.dylib on macOS, libonnxruntime.so on Linux).
func New(modelPath, tokenizerPath, libraryPath string, dimension int) (*ONNXEmbedder, error) {
	// Tell the Go wrapper where to find the ONNX Runtime C++ library.
	// This must happen before InitializeEnvironment.
	ort.SetSharedLibraryPath(libraryPath)

	// Load the C++ runtime into the process. This is a global, one-time
	// operation — calling it twice returns an error.
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("initializing ONNX environment: %w", err)
	}

	// Load the HuggingFace tokenizer from its JSON config. This uses the
	// Rust tokenizers crate via CGo — same tokenization as Python, but
	// compiled natively.
	tk, err := tokenizers.FromFile(tokenizerPath)
	if err != nil {
		return nil, fmt.Errorf("loading tokenizer from %s: %w", tokenizerPath, err)
	}

	// Create the ONNX inference session. We use DynamicAdvancedSession
	// because we want to supply tensors at run time rather than at session
	// creation.
	//
	// This ONNX model has two outputs:
	//   - token_embeddings: [1, seqLen, dim] per-token hidden states
	//   - sentence_embedding: [1, dim] mean-pooled + L2-normalized vector
	//
	// We request only sentence_embedding — the model's built-in pooling
	// layer handles mean pooling and normalization, matching the output of
	// Python sentence-transformers exactly.
	session, err := ort.NewDynamicAdvancedSession(
		modelPath,
		[]string{"input_ids", "attention_mask"},
		[]string{"sentence_embedding"},
		nil,
	)
	if err != nil {
		tk.Close()
		return nil, fmt.Errorf("creating ONNX session from %s: %w", modelPath, err)
	}

	return &ONNXEmbedder{
		tokenizer: tk,
		session:   session,
		dimension: dimension,
	}, nil
}

// Embed converts text into a fixed-size vector by tokenizing, running ONNX
// inference, and returning the model's sentence embedding output (mean-pooled
// + L2-normalized).
func (e *ONNXEmbedder) Embed(text string) ([]float32, error) {
	// Step 1: Tokenize with attention mask. The tokenizer.json has padding
	// (to 128) and truncation built in, so Encode returns exactly
	// maxSeqLen tokens. We use EncodeWithOptions to also get the attention
	// mask, which is 1 for real tokens and 0 for padding — the model
	// needs this to ignore pad positions during pooling.
	enc := e.tokenizer.EncodeWithOptions(text, true,
		tokenizers.WithReturnAttentionMask(),
	)
	if len(enc.IDs) == 0 {
		return nil, fmt.Errorf("tokenizer produced no tokens for input")
	}

	// Step 2: Convert from uint32 → int64. The tokenizer returns uint32
	// but ONNX models expect int64 tensors.
	seqLen := len(enc.IDs)
	inputIDs := make([]int64, seqLen)
	attentionMask := make([]int64, seqLen)
	for i := 0; i < seqLen; i++ {
		inputIDs[i] = int64(enc.IDs[i])
		attentionMask[i] = int64(enc.AttentionMask[i])
	}

	// Step 3: Create ONNX input tensors. Shape [1, seqLen] means batch
	// size 1. The tokenizer has already padded/truncated to 128.
	inputIDsTensor, err := ort.NewTensor(ort.Shape{1, int64(seqLen)}, inputIDs)
	if err != nil {
		return nil, fmt.Errorf("creating input_ids tensor: %w", err)
	}
	defer inputIDsTensor.Destroy()

	attentionMaskTensor, err := ort.NewTensor(ort.Shape{1, int64(seqLen)}, attentionMask)
	if err != nil {
		return nil, fmt.Errorf("creating attention_mask tensor: %w", err)
	}
	defer attentionMaskTensor.Destroy()

	// Step 4: Create the output tensor. Shape [1, dimension] — the model's
	// sentence_embedding output is already mean-pooled and normalized.
	outputTensor, err := ort.NewEmptyTensor[float32](ort.Shape{1, int64(e.dimension)})
	if err != nil {
		return nil, fmt.Errorf("creating output tensor: %w", err)
	}
	defer outputTensor.Destroy()

	// Step 5: Run inference.
	err = e.session.Run(
		[]ort.Value{inputIDsTensor, attentionMaskTensor},
		[]ort.Value{outputTensor},
	)
	if err != nil {
		return nil, fmt.Errorf("running ONNX inference: %w", err)
	}

	// The output data is [1, dimension] — copy just the embedding.
	data := outputTensor.GetData()
	result := make([]float32, e.dimension)
	copy(result, data[:e.dimension])
	return result, nil
}

// Close releases the tokenizer, ONNX session, and ONNX Runtime environment.
func (e *ONNXEmbedder) Close() error {
	e.session.Destroy()
	e.tokenizer.Close()
	return ort.DestroyEnvironment()
}
