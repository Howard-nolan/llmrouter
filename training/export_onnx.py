"""
export_onnx.py — Export the trained GBT classifier to ONNX format.

Loads the scikit-learn GradientBoostingClassifier from the joblib checkpoint,
converts it to ONNX via skl2onnx, verifies the output matches scikit-learn's
predict_proba, and saves to models/complexity_classifier.onnx.

Usage:
    cd training && uv run python export_onnx.py
"""

import json
from pathlib import Path

import joblib
import numpy as np
import onnxruntime as ort
from skl2onnx import convert_sklearn
from skl2onnx.common.data_types import FloatTensorType

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------

GBT_CHECKPOINT = Path(__file__).parent / "complexity_classifier_gbt.joblib"
LABELED_FILE = Path(__file__).parent / "labeled_dataset.jsonl"
OUTPUT_PATH = Path(__file__).parent.parent / "models" / "complexity_classifier.onnx"

# ---------------------------------------------------------------------------
# Step 1: Load the checkpoint
# ---------------------------------------------------------------------------

print("Loading GBT checkpoint...")
checkpoint = joblib.load(GBT_CHECKPOINT)

# The checkpoint is a dict — pull out the trained model and metadata.
model = checkpoint["model"]
embedding_dim = checkpoint["embedding_dim"]
best_threshold = checkpoint["best_threshold"]

print(f"  Embedding dim: {embedding_dim}")
print(f"  Best threshold: {best_threshold}")
print(f"  Config: {checkpoint['config']}")

# ---------------------------------------------------------------------------
# Step 2: Convert to ONNX
# ---------------------------------------------------------------------------
#
# convert_sklearn needs two things:
#   1. The trained model
#   2. initial_types — describes the input shape and dtype. This is how
#      skl2onnx knows what ONNX input tensor to create.
#
# FloatTensorType([1, 384]) means: one sample, 384 features, float32.
# The "float_input" string is the ONNX input tensor name — we'll use
# this in Go when feeding data to the model.
#
# Options:
#   zipmap: False — by default, skl2onnx wraps classifier probabilities
#   in a ZipMap (dict-like ONNX type: {0: 0.3, 1: 0.7}). The Go ONNX
#   runtime can't handle map types, so we disable it to get a plain
#   float tensor of shape [1, 2] (probabilities for class 0 and class 1).

print("\nConverting to ONNX...")

initial_types = [("float_input", FloatTensorType([1, embedding_dim]))]

onnx_model = convert_sklearn(
    model,
    initial_types=initial_types,
    options={id(model): {"zipmap": False}},
    target_opset=15,
)

# Print the output names and shapes so we know what to expect in Go.
print("  ONNX outputs:")
for output in onnx_model.graph.output:
    # output.type.tensor_type exists for plain tensors.
    # .elem_type is an enum: 1=float, 7=int64, etc.
    shape = [d.dim_value for d in output.type.tensor_type.shape.dim]
    print(f"    {output.name}: shape={shape}")

# ---------------------------------------------------------------------------
# Step 3: Verify against scikit-learn
# ---------------------------------------------------------------------------
#
# Load a few samples from the labeled dataset, run them through both the
# original sklearn model and the ONNX model, and check that the outputs
# match. This catches conversion bugs (wrong input name, transposed dims,
# missing zipmap option, etc.) before we write any Go code.

print("\nVerifying ONNX output matches scikit-learn...")

# We need embeddings to test with. Rather than loading the full embedding
# model (slow), we use random vectors — the point is to verify the ONNX
# conversion is numerically identical, not that the predictions are good.
rng = np.random.RandomState(42)
test_samples = rng.randn(10, embedding_dim).astype(np.float32)

# Scikit-learn predictions.
sklearn_probs = model.predict_proba(test_samples)[:, 1]

# Save the ONNX model to a temp buffer, then load with onnxruntime.
OUTPUT_PATH.parent.mkdir(parents=True, exist_ok=True)
with open(OUTPUT_PATH, "wb") as f:
    f.write(onnx_model.SerializeToString())

session = ort.InferenceSession(str(OUTPUT_PATH))

# Check the input name matches what we specified.
input_name = session.get_inputs()[0].name
print(f"  ONNX input name: {input_name}")

# Run each sample and compare. We feed one at a time (batch size 1) to
# match how the Go classifier will call it.
max_diff = 0.0
for i in range(len(test_samples)):
    sample = test_samples[i : i + 1]  # shape [1, 384]
    onnx_out = session.run(None, {input_name: sample})

    # onnx_out[0] = labels (int64), onnx_out[1] = probabilities (float32)
    # probabilities shape: [1, 2] — we want [:, 1] for class 1 probability
    onnx_prob = onnx_out[1][0, 1]
    diff = abs(float(sklearn_probs[i]) - float(onnx_prob))
    max_diff = max(max_diff, diff)

    if i < 3:
        print(f"  Sample {i}: sklearn={sklearn_probs[i]:.6f}  onnx={onnx_prob:.6f}  diff={diff:.2e}")

print(f"  Max difference across {len(test_samples)} samples: {max_diff:.2e}")

if max_diff > 1e-5:
    print("  WARNING: difference exceeds 1e-5 — investigate before using in production")
else:
    print("  PASS: outputs match within floating-point tolerance")

# ---------------------------------------------------------------------------
# Step 4: Done
# ---------------------------------------------------------------------------

print(f"\nONNX model saved to {OUTPUT_PATH}")
print(f"  File size: {OUTPUT_PATH.stat().st_size / 1024:.1f} KB")

# Print a summary that's useful for the Go integration.
print(f"\n--- Go integration notes ---")
print(f"  Input name:  {input_name}")
print(f"  Input shape: [1, {embedding_dim}]  (float32)")
print(f"  Output[0]:   output_label  (int64, predicted class)")
print(f"  Output[1]:   output_probability  (float32, shape [1, 2])")
print(f"  Use output_probability[:, 1] as the complexity score (0–1)")
print(f"  Threshold:   {best_threshold}")
