package embedder

import (
	"math"
	"path/filepath"
	"runtime"
	"testing"
)

// projectRoot returns the absolute path to the repo root by walking up from
// this test file's location.
func projectRoot(t *testing.T) string {
	t.Helper()
	// This file lives at internal/embedder/embedder_test.go, so the repo
	// root is two directories up.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func setupEmbedder(t *testing.T) *Embedder {
	t.Helper()
	root := projectRoot(t)

	emb, err := New(
		filepath.Join(root, "models", "model.onnx"),
		filepath.Join(root, "models", "tokenizer.json"),
		filepath.Join(root, "lib", "libonnxruntime.dylib"),
		384,
	)
	if err != nil {
		t.Fatalf("failed to create embedder: %v", err)
	}
	t.Cleanup(func() { emb.Close() })
	return emb
}

func TestEmbed_MatchesPythonReference(t *testing.T) {
	emb := setupEmbedder(t)

	got, err := emb.Embed("What is the weather today?")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}

	if len(got) != 384 {
		t.Fatalf("expected 384-dim vector, got %d", len(got))
	}

	// Reference values from running the same ONNX model in Python with
	// onnxruntime, using the sentence_embedding output.
	expected := []float32{-0.03579939, 0.09944275, 0.07859868, 0.06265699, -0.01527093}
	tolerance := float32(1e-4)

	for i, want := range expected {
		if diff := float32(math.Abs(float64(got[i] - want))); diff > tolerance {
			t.Errorf("dimension %d: got %f, want %f (diff %f)", i, got[i], want, diff)
		}
	}
}

func TestEmbed_IdenticalInputsProduceIdenticalOutputs(t *testing.T) {
	emb := setupEmbedder(t)

	a, err := emb.Embed("Tell me about Go programming")
	if err != nil {
		t.Fatalf("first Embed() error: %v", err)
	}

	b, err := emb.Embed("Tell me about Go programming")
	if err != nil {
		t.Fatalf("second Embed() error: %v", err)
	}

	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("dimension %d differs: %f vs %f", i, a[i], b[i])
		}
	}
}

func TestEmbed_DifferentInputsProduceDifferentOutputs(t *testing.T) {
	emb := setupEmbedder(t)

	a, err := emb.Embed("What is the weather today?")
	if err != nil {
		t.Fatalf("first Embed() error: %v", err)
	}

	b, err := emb.Embed("How do I cook pasta?")
	if err != nil {
		t.Fatalf("second Embed() error: %v", err)
	}

	// At least some dimensions should differ meaningfully.
	diffs := 0
	for i := range a {
		if math.Abs(float64(a[i]-b[i])) > 0.01 {
			diffs++
		}
	}
	if diffs == 0 {
		t.Error("different inputs produced identical embeddings")
	}
}

func TestEmbed_EmptyInput(t *testing.T) {
	emb := setupEmbedder(t)

	// Even empty string should produce tokens ([CLS] + [SEP]) and a valid
	// embedding, not an error.
	got, err := emb.Embed("")
	if err != nil {
		t.Fatalf("Embed(\"\") error: %v", err)
	}
	if len(got) != 384 {
		t.Fatalf("expected 384-dim vector, got %d", len(got))
	}
}
