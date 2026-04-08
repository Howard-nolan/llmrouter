"""
train_classifier.py — Phase 3 of the complexity classifier pipeline.

Trains two models on labeled prompt data to predict whether a prompt needs
an expensive model (label=1) or the cheap model is adequate (label=0):

  1. MLP (PyTorch): 384 → 64 → 32 → 1, BCE loss, embeddings only
  2. GBT (scikit-learn): Gradient Boosted Trees on embeddings + complexity features

Both use embeddings from all-MiniLM-L6-v2. The GBT additionally receives
handcrafted complexity features (sub-task count, constraint count, reasoning
keywords, etc.) that target task difficulty rather than topic. Each model
gets a threshold sweep to find the best F1 operating point.

Usage:
    uv run python train_classifier.py

Outputs:
    training/complexity_classifier.pt          — MLP checkpoint
    training/complexity_classifier_gbt.joblib  — GBT checkpoint
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

NUM_COMPLEXITY_FEATURES = 6
MLP_INPUT_DIM = EMBEDDING_DIM  # embeddings only (complexity features extracted but not used — see notes below)
MLP_HIDDEN1 = 64
MLP_HIDDEN2 = 32

# MLP training hyperparameters.
MLP_LEARNING_RATE = 1e-4
MLP_WEIGHT_DECAY = 1e-4
MLP_EPOCHS = 500
MLP_BATCH_SIZE = 32
MLP_PATIENCE = 40
MLP_SCHEDULER_PATIENCE = 15
MLP_SCHEDULER_FACTOR = 0.5

# GBT defaults (overridden by grid search in main).
GBT_N_ESTIMATORS = 100
GBT_MAX_DEPTH = 5
GBT_LEARNING_RATE = 0.1
GBT_SUBSAMPLE = 0.8
GBT_MIN_SAMPLES_LEAF = 5

CLASS_WEIGHT_MULTIPLIER = 4.0  # multiply natural class ratio to penalize false negatives harder
F_BETA = 2.0  # beta for F-score threshold sweep (2.0 = recall weighted 2x over precision)

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
    """Two hidden layer MLP: (embedding + complexity features) → 64 → 32 → prediction."""

    def __init__(self, input_dim: int = MLP_INPUT_DIM):
        super().__init__()
        self.network = nn.Sequential(
            nn.Linear(input_dim, MLP_HIDDEN1),
            nn.ReLU(),
            nn.Dropout(0.3),
            nn.Linear(MLP_HIDDEN1, MLP_HIDDEN2),
            nn.ReLU(),
            nn.Dropout(0.3),
            nn.Linear(MLP_HIDDEN2, 1),
        )

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        return self.network(x)


# ---------------------------------------------------------------------------
# Complexity features (used by GBT only)
# ---------------------------------------------------------------------------

# Phrases that signal multi-step instructions.
SUBTASK_PATTERNS = re.compile(
    r"\b(first|then|next|finally|additionally|after that|also|second|third|lastly)\b",
    re.IGNORECASE,
)

# Phrases that signal constrained tasks.
CONSTRAINT_PATTERNS = re.compile(
    r"\b(without|must not|must be|at least|at most|exactly|make sure|no more than|"
    r"do not|don't|should not|cannot|ensure that|in under|less than|greater than)\b",
    re.IGNORECASE,
)

# Words that signal analytical/reasoning depth.
REASONING_PATTERNS = re.compile(
    r"\b(compare|contrast|analyze|evaluate|explain why|explain how|tradeoffs?|trade-offs?|"
    r"pros and cons|implications|prove|derive|justify|critique|assess|distinguish|"
    r"advantages|disadvantages|differences? between)\b",
    re.IGNORECASE,
)

# Code block detection.
CODE_BLOCK_PATTERN = re.compile(r"```")

# Verbs commonly starting imperative sentences.
IMPERATIVE_STARTS = re.compile(
    r"^(write|create|build|implement|design|explain|describe|list|find|calculate|"
    r"solve|prove|show|demonstrate|convert|translate|optimize|refactor|debug|fix|"
    r"analyze|compare|evaluate|generate|make|define|summarize|outline)",
    re.IGNORECASE,
)

# Verbs that indicate working with existing code (harder tasks).
CODE_ACTION_PATTERNS = re.compile(
    r"\b(fix|debug|optimize|refactor|explain|improve|review|what('s| is) wrong)\b",
    re.IGNORECASE,
)

COMPLEXITY_FEATURE_NAMES = [
    "subtask_count",
    "constraint_count",
    "reasoning_keyword_count",
    "question_count",
    "code_task_type",
    "imperative_density",
]


def extract_complexity_features(prompt: str) -> list[float]:
    """Extract features that target task complexity rather than topic."""
    subtask_count = len(SUBTASK_PATTERNS.findall(prompt))
    constraint_count = len(CONSTRAINT_PATTERNS.findall(prompt))
    reasoning_count = len(REASONING_PATTERNS.findall(prompt))
    question_count = prompt.count("?")

    # Code task type: 0 = no code, 1 = asks to write code, 2 = provides code + action verb.
    has_code_block = bool(CODE_BLOCK_PATTERN.search(prompt))
    has_code_action = bool(CODE_ACTION_PATTERNS.search(prompt))
    if has_code_block and has_code_action:
        code_task_type = 2.0
    elif has_code_block or "```" in prompt:
        code_task_type = 1.0
    else:
        code_task_type = 0.0

    # Imperative density: fraction of sentences starting with a command verb.
    sentences = [s.strip() for s in re.split(r"[.!?\n]+", prompt) if s.strip()]
    if sentences:
        imperative_count = sum(1 for s in sentences if IMPERATIVE_STARTS.match(s))
        imperative_density = imperative_count / len(sentences)
    else:
        imperative_density = 0.0

    return [
        float(subtask_count),
        float(constraint_count),
        float(reasoning_count),
        float(question_count),
        code_task_type,
        imperative_density,
    ]


def extract_all_complexity_features(prompts: list[str]) -> np.ndarray:
    """Extract complexity features for all prompts. Returns (N, 6) array."""
    features = np.array(
        [extract_complexity_features(p) for p in prompts], dtype=np.float32
    )
    print(f"\nComplexity features shape: {features.shape}")
    for i, name in enumerate(COMPLEXITY_FEATURE_NAMES):
        col = features[:, i]
        print(f"  {name:>25s}: mean={col.mean():.2f}  std={col.std():.2f}  "
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
    """Compute pos_weight to counteract class imbalance, scaled by CLASS_WEIGHT_MULTIPLIER."""
    n_pos = sum(labels)
    n_neg = len(labels) - n_pos
    natural_weight = n_neg / n_pos
    weight = natural_weight * CLASS_WEIGHT_MULTIPLIER
    print(f"\nClass weight — natural: {natural_weight:.2f}, "
          f"multiplier: {CLASS_WEIGHT_MULTIPLIER:.1f}x, "
          f"final pos_weight: {weight:.2f}")
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


def sweep_mlp_thresholds(
    model: ComplexityClassifier, X_val: torch.Tensor, y_val: torch.Tensor,
) -> float:
    """Sweep decision thresholds on the MLP and return best-F-beta threshold."""
    model.eval()
    with torch.no_grad():
        probs = torch.sigmoid(model(X_val)).squeeze().numpy()

    y = y_val.squeeze().numpy()
    b2 = F_BETA ** 2

    print(f"\n  Threshold sweep (optimizing F{F_BETA:.0f}):")
    print(f"  {'Thresh':>7s}  {'Acc':>5s}  {'Prec':>5s}  {'Recall':>6s}  {'F1':>5s}  "
          f"{'F'+str(int(F_BETA)):>5s}  {'TP':>3s}  {'FP':>3s}  {'FN':>3s}  {'TN':>3s}")
    print("  " + "-" * 70)

    best_fb = 0.0
    best_threshold = 0.5

    for threshold in np.arange(0.20, 0.81, 0.05):
        preds = (probs >= threshold).astype(int)
        tp = int(((preds == 1) & (y == 1)).sum())
        fp = int(((preds == 1) & (y == 0)).sum())
        fn = int(((preds == 0) & (y == 1)).sum())
        tn = int(((preds == 0) & (y == 0)).sum())

        acc = (tp + tn) / len(y)
        prec = tp / (tp + fp) if (tp + fp) > 0 else 0.0
        rec = tp / (tp + fn) if (tp + fn) > 0 else 0.0
        f1 = 2 * prec * rec / (prec + rec) if (prec + rec) > 0 else 0.0
        fb = (1 + b2) * prec * rec / (b2 * prec + rec) if (b2 * prec + rec) > 0 else 0.0

        marker = ""
        if fb > best_fb:
            best_fb = fb
            best_threshold = threshold
            marker = " *"

        print(f"  {threshold:>7.2f}  {acc:>5.3f}  {prec:>5.3f}  {rec:>6.3f}  {f1:>5.3f}  "
              f"{fb:>5.3f}  {tp:>3d}  {fp:>3d}  {fn:>3d}  {tn:>3d}{marker}")

    print(f"\n  Best F{F_BETA:.0f}: {best_fb:.3f} at threshold {best_threshold:.2f}")
    return best_threshold


# ---------------------------------------------------------------------------
# GBT training
# ---------------------------------------------------------------------------


def train_gbt(
    X_train: np.ndarray,
    y_train: np.ndarray,
    X_val: np.ndarray,
    y_val: np.ndarray,
    n_estimators: int = GBT_N_ESTIMATORS,
    max_depth: int = GBT_MAX_DEPTH,
    learning_rate: float = GBT_LEARNING_RATE,
    min_samples_leaf: int = GBT_MIN_SAMPLES_LEAF,
) -> GradientBoostingClassifier:
    """Train a Gradient Boosted Classifier on embeddings."""
    n_pos = (y_train == 1).sum()
    n_neg = (y_train == 0).sum()
    natural_weight = n_neg / n_pos
    weight = natural_weight * CLASS_WEIGHT_MULTIPLIER
    sample_weights = np.where(y_train == 1, weight, 1.0)
    print(f"\n  Sample weight for label=1: {weight:.2f} "
          f"(natural {natural_weight:.2f} × {CLASS_WEIGHT_MULTIPLIER:.1f}x)")
    print(f"  Config: n_estimators={n_estimators}, max_depth={max_depth}, "
          f"lr={learning_rate}, subsample={GBT_SUBSAMPLE}, "
          f"min_samples_leaf={min_samples_leaf}")

    clf = GradientBoostingClassifier(
        n_estimators=n_estimators,
        max_depth=max_depth,
        learning_rate=learning_rate,
        subsample=GBT_SUBSAMPLE,
        min_samples_leaf=min_samples_leaf,
        random_state=SEED,
    )
    clf.fit(X_train, y_train, sample_weight=sample_weights)

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
) -> tuple[float, float]:
    """Sweep decision thresholds from 0.10 to 0.80 in 0.01 steps. Returns (best_threshold, best_fb)."""
    probs = clf.predict_proba(X_val)[:, 1]
    b2 = F_BETA ** 2

    print(f"\n  Threshold sweep (optimizing F{F_BETA:.0f}, 0.01 steps):")
    print(f"  {'Thresh':>7s}  {'Acc':>5s}  {'Prec':>5s}  {'Recall':>6s}  {'F1':>5s}  "
          f"{'F'+str(int(F_BETA)):>5s}  {'TP':>3s}  {'FP':>3s}  {'FN':>3s}  {'TN':>3s}")
    print("  " + "-" * 70)

    best_fb = 0.0
    best_threshold = 0.5

    for threshold in np.arange(0.10, 0.81, 0.01):
        preds = (probs >= threshold).astype(int)
        tp = ((preds == 1) & (y_val == 1)).sum()
        fp = ((preds == 1) & (y_val == 0)).sum()
        fn = ((preds == 0) & (y_val == 1)).sum()
        tn = ((preds == 0) & (y_val == 0)).sum()

        acc = (tp + tn) / len(y_val)
        prec = tp / (tp + fp) if (tp + fp) > 0 else 0.0
        rec = tp / (tp + fn) if (tp + fn) > 0 else 0.0
        f1 = 2 * prec * rec / (prec + rec) if (prec + rec) > 0 else 0.0
        fb = (1 + b2) * prec * rec / (b2 * prec + rec) if (b2 * prec + rec) > 0 else 0.0

        marker = ""
        if fb > best_fb:
            best_fb = fb
            best_threshold = threshold
            marker = " *"

        # Only print every 5th row to keep output readable, plus any best-so-far.
        if marker or abs(threshold * 100 % 5) < 0.5:
            print(f"  {threshold:>7.2f}  {acc:>5.3f}  {prec:>5.3f}  {rec:>6.3f}  {f1:>5.3f}  "
                  f"{fb:>5.3f}  {tp:>3d}  {fp:>3d}  {fn:>3d}  {tn:>3d}{marker}")

    print(f"\n  Best F{F_BETA:.0f}: {best_fb:.3f} at threshold {best_threshold:.2f}")
    return best_threshold, best_fb


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main():
    if not LABELED_FILE.exists():
        print(f"Error: {LABELED_FILE} not found. Run label_quality.py first.")
        return

    prompts, labels = load_dataset()
    embeddings = compute_embeddings(prompts)
    complexity_features = extract_all_complexity_features(prompts)

    train_idx, val_idx = stratified_split(labels)
    y_train_np = np.array([labels[i] for i in train_idx])
    y_val_np = np.array([labels[i] for i in val_idx])

    print(f"\nTrain: {len(train_idx)} (label=0: {(y_train_np==0).sum()}, label=1: {(y_train_np==1).sum()})")
    print(f"Val:   {len(val_idx)} (label=0: {(y_val_np==0).sum()}, label=1: {(y_val_np==1).sum()})")

    pos_weight = compute_class_weight(labels)

    # Embeddings only for model input. Complexity features are extracted above
    # for analysis but excluded from training — experiments showed they added
    # no discriminative signal when embeddings were present (0% feature importance
    # across all GBT runs).
    X_train = embeddings[train_idx]
    X_val = embeddings[val_idx]
    print(f"\nFeature mode: embeddings only ({X_train.shape[1]} dims)")

    X_train_t = torch.tensor(X_train, dtype=torch.float32)
    y_train_t = torch.tensor(y_train_np, dtype=torch.float32).unsqueeze(1)
    X_val_t = torch.tensor(X_val, dtype=torch.float32)
    y_val_t = torch.tensor(y_val_np, dtype=torch.float32).unsqueeze(1)

    # ===== MODEL 1: MLP =====
    print("\n" + "=" * 60)
    print(f"MODEL 1: MLP ({MLP_INPUT_DIM} → {MLP_HIDDEN1} → {MLP_HIDDEN2} → 1, embeddings only)")
    print("=" * 60)

    mlp_model, mlp_result = train_mlp(X_train_t, y_train_t, X_val_t, y_val_t, pos_weight)
    evaluate_mlp(mlp_model, X_val_t, y_val_t)
    mlp_best_threshold = sweep_mlp_thresholds(mlp_model, X_val_t, y_val_t)

    # ===== MODEL 2: GBT (grid search) =====
    print("\n" + "=" * 60)
    print("MODEL 2: GBT grid search (embeddings only)")
    print("=" * 60)

    gbt_configs = [
        {"n_estimators": 100, "max_depth": 5, "learning_rate": 0.1, "min_samples_leaf": 5},
        {"n_estimators": 200, "max_depth": 5, "learning_rate": 0.05, "min_samples_leaf": 5},
        {"n_estimators": 200, "max_depth": 6, "learning_rate": 0.05, "min_samples_leaf": 3},
        {"n_estimators": 300, "max_depth": 7, "learning_rate": 0.05, "min_samples_leaf": 3},
    ]

    best_gbt_fb = 0.0
    best_gbt_model = None
    best_gbt_threshold = 0.5
    best_gbt_config = None

    for i, cfg in enumerate(gbt_configs):
        print(f"\n--- GBT config {i+1}/{len(gbt_configs)} ---")
        model = train_gbt(X_train, y_train_np, X_val, y_val_np, **cfg)
        threshold, fb = sweep_thresholds(model, X_val, y_val_np)

        if fb > best_gbt_fb:
            best_gbt_fb = fb
            best_gbt_model = model
            best_gbt_threshold = threshold
            best_gbt_config = cfg

    print(f"\n{'=' * 60}")
    print(f"Best GBT config: {best_gbt_config}")
    print(f"Best F{F_BETA:.0f}: {best_gbt_fb:.3f} at threshold {best_gbt_threshold:.2f}")
    print(f"{'=' * 60}")

    # ===== COMPARISON =====
    gbt_acc = accuracy_score(y_val_np, best_gbt_model.predict(X_val))
    print(f"\nCOMPARISON:  MLP val_acc={mlp_result['val_acc']:.3f}  |  GBT val_acc={gbt_acc:.3f}")
    print(f"  Majority-class baseline: {(y_val_np == 0).sum() / len(y_val_np):.3f}")

    # ===== SAVE CHECKPOINTS =====

    torch.save(
        {
            "model_state_dict": mlp_result["state_dict"],
            "val_acc": mlp_result["val_acc"],
            "epoch": mlp_result["epoch"],
            "best_threshold": float(mlp_best_threshold),
            "embedding_model": EMBEDDING_MODEL,
            "embedding_dim": EMBEDDING_DIM,
            "architecture": f"{MLP_INPUT_DIM} → {MLP_HIDDEN1} → {MLP_HIDDEN2} → 1",
        },
        MLP_CHECKPOINT,
    )
    print(f"\nMLP checkpoint saved to {MLP_CHECKPOINT}")

    joblib.dump(
        {
            "model": best_gbt_model,
            "val_acc": gbt_acc,
            "best_threshold": float(best_gbt_threshold),
            "best_f_beta": float(best_gbt_fb),
            "f_beta": F_BETA,
            "config": best_gbt_config,
            "embedding_model": EMBEDDING_MODEL,
            "embedding_dim": EMBEDDING_DIM,
            "complexity_feature_names": COMPLEXITY_FEATURE_NAMES,
        },
        GBT_CHECKPOINT,
    )
    print(f"GBT checkpoint saved to {GBT_CHECKPOINT}")


if __name__ == "__main__":
    main()
