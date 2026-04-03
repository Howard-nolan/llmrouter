"""
train_classifier.py — Phase 3 of the complexity classifier pipeline.

Trains two models on labeled prompt data to predict whether a prompt needs
an expensive model (label=1) or the cheap model is adequate (label=0):

  1. MLP (PyTorch): 391 → 64 → 1, BCE loss, embeddings + handcrafted features
  2. GBT (scikit-learn): Gradient Boosted Trees on embeddings only

Both are trained and compared. The GBT consistently outperforms the MLP on
this small dataset (~500 samples). See /obsidian_vault doc for the full
training journey and analysis.

Usage:
    uv run python train_classifier.py

Outputs:
    training/complexity_classifier.pt          — MLP checkpoint
    training/complexity_classifier_gbt.joblib  — GBT checkpoint (best model)
"""

import copy
import json
import random
import re
from pathlib import Path

import joblib
import numpy as np
import torch
import torch.nn as nn
from sentence_transformers import SentenceTransformer
from sklearn.ensemble import GradientBoostingClassifier
from sklearn.metrics import accuracy_score, classification_report, confusion_matrix

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

LABELED_FILE = Path(__file__).parent / "labeled_dataset.jsonl"
MLP_CHECKPOINT = Path(__file__).parent / "complexity_classifier.pt"
GBT_CHECKPOINT = Path(__file__).parent / "complexity_classifier_gbt.joblib"

EMBEDDING_MODEL = "all-MiniLM-L6-v2"
EMBEDDING_DIM = 384

# Handcrafted features (used by MLP only).
NUM_HANDCRAFTED = 7
MLP_INPUT_DIM = EMBEDDING_DIM + NUM_HANDCRAFTED  # 391
MLP_HIDDEN = 64

# MLP training hyperparameters.
MLP_LEARNING_RATE = 1e-3
MLP_WEIGHT_DECAY = 1e-4
MLP_EPOCHS = 500
MLP_BATCH_SIZE = 32
MLP_PATIENCE = 40
MLP_SCHEDULER_PATIENCE = 15
MLP_SCHEDULER_FACTOR = 0.5

# GBT hyperparameters (best config from hyperparameter search).
GBT_N_ESTIMATORS = 100
GBT_MAX_DEPTH = 5
GBT_LEARNING_RATE = 0.1
GBT_SUBSAMPLE = 0.8
GBT_MIN_SAMPLES_LEAF = 5

VAL_SPLIT = 0.2
SEED = 42


def set_seed(seed: int) -> None:
    random.seed(seed)
    np.random.seed(seed)
    torch.manual_seed(seed)


set_seed(SEED)

# ---------------------------------------------------------------------------
# MLP model definition
# ---------------------------------------------------------------------------


class ComplexityClassifier(nn.Module):
    """Single hidden layer MLP: (embedding + features) → hidden → prediction."""

    def __init__(self, input_dim: int = MLP_INPUT_DIM):
        super().__init__()
        self.network = nn.Sequential(
            nn.Linear(input_dim, MLP_HIDDEN),
            nn.ReLU(),
            nn.Linear(MLP_HIDDEN, 1),
        )

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        return self.network(x)


# ---------------------------------------------------------------------------
# Feature extraction (used by MLP only)
# ---------------------------------------------------------------------------

CODE_PATTERN = re.compile(r"```|(?:^    \S)", re.MULTILINE)

FEATURE_NAMES = [
    "char_count", "word_count", "sentence_count", "question_mark_count",
    "avg_word_length", "newline_count", "has_code",
]


def extract_features(prompt: str) -> list[float]:
    """Extract handcrafted complexity signals from prompt text."""
    words = prompt.split()
    sentences = [s for s in re.split(r"[.!?]+", prompt) if s.strip()]

    return [
        float(len(prompt)),
        float(len(words)),
        float(len(sentences)),
        float(prompt.count("?")),
        float(np.mean([len(w) for w in words]) if words else 0),
        float(prompt.count("\n")),
        float(1 if CODE_PATTERN.search(prompt) else 0),
    ]


def extract_all_features(prompts: list[str]) -> np.ndarray:
    """Extract features for all prompts. Returns (N, 7) array."""
    features = np.array([extract_features(p) for p in prompts], dtype=np.float32)
    print(f"\nHandcrafted features shape: {features.shape}")
    for i, name in enumerate(FEATURE_NAMES):
        col = features[:, i]
        print(f"  {name:>22s}: mean={col.mean():.1f}  std={col.std():.1f}  "
              f"min={col.min():.0f}  max={col.max():.0f}")
    return features


# ---------------------------------------------------------------------------
# Data loading + embedding
# ---------------------------------------------------------------------------


def load_dataset() -> tuple[list[str], list[int]]:
    """Read labeled JSONL, return (prompts, labels)."""
    prompts = []
    labels = []
    with open(LABELED_FILE) as f:
        for line in f:
            line = line.strip()
            if line:
                entry = json.loads(line)
                prompts.append(entry["prompt"])
                labels.append(entry["label"])

    print(f"Loaded {len(prompts)} labeled entries")
    print(f"  label=0 (adequate):       {labels.count(0)}")
    print(f"  label=1 (needs expensive): {labels.count(1)}")
    return prompts, labels


def compute_embeddings(prompts: list[str]) -> np.ndarray:
    """Embed all prompts using sentence-transformers. Returns (N, 384) array."""
    print(f"\nLoading embedding model: {EMBEDDING_MODEL}")
    model = SentenceTransformer(EMBEDDING_MODEL)

    print(f"Computing embeddings for {len(prompts)} prompts...")
    embeddings = model.encode(prompts, show_progress_bar=True, batch_size=64)

    print(f"Embedding shape: {embeddings.shape}")
    return embeddings


# ---------------------------------------------------------------------------
# Train/val split
# ---------------------------------------------------------------------------


def stratified_split(labels: list[int]) -> tuple[list[int], list[int]]:
    """Return (train_indices, val_indices) with stratified 80/20 split."""
    indices_0 = [i for i, l in enumerate(labels) if l == 0]
    indices_1 = [i for i, l in enumerate(labels) if l == 1]
    random.shuffle(indices_0)
    random.shuffle(indices_1)

    val_count_0 = int(len(indices_0) * VAL_SPLIT)
    val_count_1 = int(len(indices_1) * VAL_SPLIT)

    val_indices = indices_0[:val_count_0] + indices_1[:val_count_1]
    train_indices = indices_0[val_count_0:] + indices_1[val_count_1:]
    random.shuffle(val_indices)
    random.shuffle(train_indices)
    return train_indices, val_indices


# ---------------------------------------------------------------------------
# MLP training
# ---------------------------------------------------------------------------


def compute_class_weight(labels: list[int]) -> torch.Tensor:
    """Compute pos_weight to counteract class imbalance."""
    n_pos = sum(labels)
    n_neg = len(labels) - n_pos
    weight = n_neg / n_pos
    print(f"\nClass weight (pos_weight): {weight:.2f}")
    return torch.tensor([weight], dtype=torch.float32)


def train_mlp(
    X_train: torch.Tensor,
    y_train: torch.Tensor,
    X_val: torch.Tensor,
    y_val: torch.Tensor,
    pos_weight: torch.Tensor,
) -> tuple[ComplexityClassifier, dict]:
    """Train MLP with early stopping and LR scheduling. Returns (model, info)."""
    model = ComplexityClassifier()
    total_params = sum(p.numel() for p in model.parameters())
    print(f"\nModel: {model}")
    print(f"Total parameters: {total_params:,}")

    criterion = nn.BCEWithLogitsLoss(pos_weight=pos_weight)
    optimizer = torch.optim.Adam(model.parameters(), lr=MLP_LEARNING_RATE, weight_decay=MLP_WEIGHT_DECAY)
    scheduler = torch.optim.lr_scheduler.ReduceLROnPlateau(
        optimizer, mode="min", factor=MLP_SCHEDULER_FACTOR, patience=MLP_SCHEDULER_PATIENCE
    )

    best_val_acc = 0.0
    best_epoch = 0
    best_state = None
    epochs_without_improvement = 0
    n_train = len(X_train)

    print(f"\nTraining for up to {MLP_EPOCHS} epochs (patience={MLP_PATIENCE}), "
          f"batch_size={MLP_BATCH_SIZE}, lr={MLP_LEARNING_RATE}\n")

    for epoch in range(MLP_EPOCHS):
        model.train()
        perm = torch.randperm(n_train)
        X_shuffled, y_shuffled = X_train[perm], y_train[perm]

        epoch_loss = 0.0
        n_batches = 0
        for start in range(0, n_train, MLP_BATCH_SIZE):
            X_batch = X_shuffled[start : start + MLP_BATCH_SIZE]
            y_batch = y_shuffled[start : start + MLP_BATCH_SIZE]

            logits = model(X_batch)
            loss = criterion(logits, y_batch)
            optimizer.zero_grad()
            loss.backward()
            optimizer.step()

            epoch_loss += loss.item()
            n_batches += 1

        avg_loss = epoch_loss / n_batches

        model.eval()
        with torch.no_grad():
            val_logits = model(X_val)
            val_preds = (torch.sigmoid(val_logits) >= 0.5).float()
            val_acc = (val_preds == y_val).float().mean().item()
            val_loss = criterion(val_logits, y_val).item()

        scheduler.step(val_loss)

        if val_acc > best_val_acc:
            best_val_acc = val_acc
            best_epoch = epoch + 1
            best_state = copy.deepcopy(model.state_dict())
            epochs_without_improvement = 0
        else:
            epochs_without_improvement += 1

        current_lr = optimizer.param_groups[0]["lr"]
        if epoch == 0 or (epoch + 1) % 20 == 0 or epoch == MLP_EPOCHS - 1 or (epoch + 1) == best_epoch:
            marker = " *" if (epoch + 1) == best_epoch else ""
            print(
                f"Epoch {epoch+1:>3}/{MLP_EPOCHS}  "
                f"train_loss={avg_loss:.4f}  "
                f"val_loss={val_loss:.4f}  "
                f"val_acc={val_acc:.3f}  "
                f"lr={current_lr:.1e}{marker}"
            )

        if epochs_without_improvement >= MLP_PATIENCE:
            print(f"\nEarly stopping: no improvement for {MLP_PATIENCE} epochs")
            break

    print(f"\nBest val accuracy: {best_val_acc:.3f} (epoch {best_epoch})")
    model.load_state_dict(best_state)
    return model, {"state_dict": best_state, "val_acc": best_val_acc, "epoch": best_epoch}


def evaluate_mlp(model: ComplexityClassifier, X_val: torch.Tensor, y_val: torch.Tensor) -> None:
    """Print detailed MLP val metrics."""
    model.eval()
    with torch.no_grad():
        preds = (torch.sigmoid(model(X_val)) >= 0.5).float()

    y, p = y_val.squeeze(), preds.squeeze()
    tp = ((p == 1) & (y == 1)).sum().item()
    fp = ((p == 1) & (y == 0)).sum().item()
    fn = ((p == 0) & (y == 1)).sum().item()
    tn = ((p == 0) & (y == 0)).sum().item()

    acc = (tp + tn) / (tp + fp + fn + tn)
    prec = tp / (tp + fp) if (tp + fp) > 0 else 0.0
    rec = tp / (tp + fn) if (tp + fn) > 0 else 0.0
    f1 = 2 * prec * rec / (prec + rec) if (prec + rec) > 0 else 0.0

    print(f"\n  Accuracy:  {acc:.3f}  Precision: {prec:.3f}  Recall: {rec:.3f}  F1: {f1:.3f}")
    print(f"  Confusion: TP={tp} FP={fp} FN={fn} TN={tn}")


# ---------------------------------------------------------------------------
# GBT training
# ---------------------------------------------------------------------------


def train_gbt(
    X_train: np.ndarray,
    y_train: np.ndarray,
    X_val: np.ndarray,
    y_val: np.ndarray,
) -> GradientBoostingClassifier:
    """Train a Gradient Boosted Classifier on embeddings only."""
    print(f"\n  Config: n_estimators={GBT_N_ESTIMATORS}, max_depth={GBT_MAX_DEPTH}, "
          f"lr={GBT_LEARNING_RATE}, subsample={GBT_SUBSAMPLE}, "
          f"min_samples_leaf={GBT_MIN_SAMPLES_LEAF}")

    clf = GradientBoostingClassifier(
        n_estimators=GBT_N_ESTIMATORS,
        max_depth=GBT_MAX_DEPTH,
        learning_rate=GBT_LEARNING_RATE,
        subsample=GBT_SUBSAMPLE,
        min_samples_leaf=GBT_MIN_SAMPLES_LEAF,
        random_state=SEED,
    )
    clf.fit(X_train, y_train)

    train_acc = accuracy_score(y_train, clf.predict(X_train))
    val_preds = clf.predict(X_val)
    val_acc = accuracy_score(y_val, val_preds)

    print(f"  Train acc: {train_acc:.3f}  Val acc: {val_acc:.3f}")
    print(f"\n{classification_report(y_val, val_preds, target_names=['adequate (0)', 'needs expensive (1)'])}")

    cm = confusion_matrix(y_val, val_preds)
    print(f"  Confusion matrix:")
    print(f"              Predicted 0  Predicted 1")
    print(f"  Actual 0:   {cm[0][0]:>10}  {cm[0][1]:>10}")
    print(f"  Actual 1:   {cm[1][0]:>10}  {cm[1][1]:>10}")

    return clf


def sweep_thresholds(
    clf: GradientBoostingClassifier,
    X_val: np.ndarray,
    y_val: np.ndarray,
) -> float:
    """Sweep decision thresholds from 0.20 to 0.80 and return best-F1 threshold."""
    probs = clf.predict_proba(X_val)[:, 1]

    print("\n  Threshold sweep:")
    print(f"  {'Thresh':>7s}  {'Acc':>5s}  {'Prec':>5s}  {'Recall':>6s}  {'F1':>5s}  "
          f"{'TP':>3s}  {'FP':>3s}  {'FN':>3s}  {'TN':>3s}")
    print("  " + "-" * 62)

    best_f1 = 0.0
    best_threshold = 0.5

    for threshold in np.arange(0.20, 0.81, 0.05):
        preds = (probs >= threshold).astype(int)
        tp = ((preds == 1) & (y_val == 1)).sum()
        fp = ((preds == 1) & (y_val == 0)).sum()
        fn = ((preds == 0) & (y_val == 1)).sum()
        tn = ((preds == 0) & (y_val == 0)).sum()

        acc = (tp + tn) / len(y_val)
        prec = tp / (tp + fp) if (tp + fp) > 0 else 0.0
        rec = tp / (tp + fn) if (tp + fn) > 0 else 0.0
        f1 = 2 * prec * rec / (prec + rec) if (prec + rec) > 0 else 0.0

        marker = ""
        if f1 > best_f1:
            best_f1 = f1
            best_threshold = threshold
            marker = " *"

        print(f"  {threshold:>7.2f}  {acc:>5.3f}  {prec:>5.3f}  {rec:>6.3f}  {f1:>5.3f}  "
              f"{tp:>3d}  {fp:>3d}  {fn:>3d}  {tn:>3d}{marker}")

    print(f"\n  Best F1: {best_f1:.3f} at threshold {best_threshold:.2f}")
    return best_threshold


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main():
    if not LABELED_FILE.exists():
        print(f"Error: {LABELED_FILE} not found. Run label_quality.py first.")
        return

    prompts, labels = load_dataset()
    embeddings = compute_embeddings(prompts)
    features = extract_all_features(prompts)

    train_idx, val_idx = stratified_split(labels)
    y_train_np = np.array([labels[i] for i in train_idx])
    y_val_np = np.array([labels[i] for i in val_idx])

    print(f"\nTrain: {len(train_idx)} (label=0: {(y_train_np==0).sum()}, label=1: {(y_train_np==1).sum()})")
    print(f"Val:   {len(val_idx)} (label=0: {(y_val_np==0).sum()}, label=1: {(y_val_np==1).sum()})")

    pos_weight = compute_class_weight(labels)

    # --- Build MLP input (embeddings + standardized handcrafted features) ---
    train_feat = features[train_idx]
    feat_mean = train_feat.mean(axis=0)
    feat_std = train_feat.std(axis=0)
    feat_std[feat_std == 0] = 1.0

    mlp_X_train = np.concatenate([embeddings[train_idx], (features[train_idx] - feat_mean) / feat_std], axis=1)
    mlp_X_val = np.concatenate([embeddings[val_idx], (features[val_idx] - feat_mean) / feat_std], axis=1)

    X_train_t = torch.tensor(mlp_X_train, dtype=torch.float32)
    y_train_t = torch.tensor(y_train_np, dtype=torch.float32).unsqueeze(1)
    X_val_t = torch.tensor(mlp_X_val, dtype=torch.float32)
    y_val_t = torch.tensor(y_val_np, dtype=torch.float32).unsqueeze(1)

    # --- GBT input (embeddings only) ---
    gbt_X_train = embeddings[train_idx]
    gbt_X_val = embeddings[val_idx]

    # ===== MODEL 1: MLP =====
    print("\n" + "=" * 60)
    print("MODEL 1: MLP (embeddings + handcrafted features)")
    print("=" * 60)

    mlp_model, mlp_result = train_mlp(X_train_t, y_train_t, X_val_t, y_val_t, pos_weight)
    evaluate_mlp(mlp_model, X_val_t, y_val_t)

    # ===== MODEL 2: GBT =====
    print("\n" + "=" * 60)
    print("MODEL 2: Gradient Boosted Trees (embeddings only)")
    print("=" * 60)

    gbt_model = train_gbt(gbt_X_train, y_train_np, gbt_X_val, y_val_np)
    best_threshold = sweep_thresholds(gbt_model, gbt_X_val, y_val_np)

    # ===== COMPARISON =====
    gbt_acc = accuracy_score(y_val_np, gbt_model.predict(gbt_X_val))
    print("\n" + "=" * 60)
    print(f"COMPARISON:  MLP val_acc={mlp_result['val_acc']:.3f}  |  GBT val_acc={gbt_acc:.3f}")
    print(f"  Majority-class baseline: {(y_val_np == 0).sum() / len(y_val_np):.3f}")
    print("=" * 60)

    # ===== SAVE CHECKPOINTS =====

    # MLP checkpoint.
    torch.save(
        {
            "model_state_dict": mlp_result["state_dict"],
            "val_acc": mlp_result["val_acc"],
            "epoch": mlp_result["epoch"],
            "embedding_model": EMBEDDING_MODEL,
            "embedding_dim": EMBEDDING_DIM,
            "num_handcrafted_features": NUM_HANDCRAFTED,
            "feature_names": FEATURE_NAMES,
            "feature_mean": feat_mean,
            "feature_std": feat_std,
            "architecture": f"{MLP_INPUT_DIM} → {MLP_HIDDEN} → 1",
        },
        MLP_CHECKPOINT,
    )
    print(f"\nMLP checkpoint saved to {MLP_CHECKPOINT}")

    # GBT checkpoint.
    joblib.dump(
        {
            "model": gbt_model,
            "val_acc": gbt_acc,
            "best_threshold": best_threshold,
            "embedding_model": EMBEDDING_MODEL,
            "embedding_dim": EMBEDDING_DIM,
        },
        GBT_CHECKPOINT,
    )
    print(f"GBT checkpoint saved to {GBT_CHECKPOINT}")


if __name__ == "__main__":
    main()
