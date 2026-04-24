"""Offline similarity-threshold sweep for the llmrouter semantic cache.

Replays the gateway's cache-lookup logic in Python for a range of thresholds
against a pre-embedded corpus. Produces a hit-rate-vs-threshold curve without
any gateway or provider calls.
"""
from __future__ import annotations

import argparse
import csv
import json
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np
from sentence_transformers import SentenceTransformer


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Offline similarity-threshold sweep")
    p.add_argument("--order", type=Path, required=True,
                   help="Path to JSON array of ordered prompts (from dumporder)")
    p.add_argument("--out-dir", type=Path, default=Path("results"),
                   help="Directory for CSV + PNG outputs")
    p.add_argument("--model", default="sentence-transformers/all-MiniLM-L6-v2",
                   help="Embedding model")
    p.add_argument("--t-min", type=float, default=0.85)
    p.add_argument("--t-max", type=float, default=0.99)
    p.add_argument("--t-step", type=float, default=0.005)
    return p.parse_args()


def load_order(path: Path) -> list[str]:
    with path.open() as f:
        prompts = json.load(f)
    if not isinstance(prompts, list) or not all(isinstance(p, str) for p in prompts):
        raise ValueError(f"{path} must be a JSON array of strings")
    return prompts


def embed(prompts: list[str], model_name: str) -> np.ndarray:
    model = SentenceTransformer(model_name)
    return model.encode(
        prompts,
        normalize_embeddings=True,
        show_progress_bar=True,
        convert_to_numpy=True,
    )


def sweep(embeddings: np.ndarray, t_values: np.ndarray) -> list[dict]:
    # Precompute the full pairwise cosine matrix once. Embeddings are
    # L2-normalized, so dot product == cosine similarity.
    sim = embeddings @ embeddings.T
    n = len(embeddings)
    results: list[dict] = []

    for T in t_values:
        kept: list[int] = []  # indices of prompts that became cache entries
        hits = 0
        for i in range(n):
            if not kept:
                kept.append(i)
                continue
            best = sim[i, kept].max()
            if best >= T:
                hits += 1
            else:
                kept.append(i)
        results.append({
            "threshold": round(float(T), 4),
            "hit_rate": hits / n,
            "hits": hits,
            "misses": n - hits,
        })
    return results


def write_csv(rows: list[dict], path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="") as f:
        writer = csv.DictWriter(
            f, fieldnames=["threshold", "hit_rate", "hits", "misses"]
        )
        writer.writeheader()
        writer.writerows(rows)


def plot(rows: list[dict], path: Path, n: int) -> None:
    thresholds = [r["threshold"] for r in rows]
    hit_rates = [r["hit_rate"] * 100 for r in rows]
    fig, ax = plt.subplots(figsize=(8, 5))
    ax.plot(thresholds, hit_rates, marker="o", linewidth=2)
    ax.set_xlabel("Similarity threshold T")
    ax.set_ylabel("Hit rate (%)")
    ax.set_title(f"Offline hit-rate sweep — clustered corpus (N={n})")
    ax.grid(alpha=0.3)
    fig.tight_layout()
    fig.savefig(path, dpi=150)


def main() -> None:
    args = parse_args()
    prompts = load_order(args.order)
    print(f"Loaded {len(prompts)} ordered prompts from {args.order}")

    embeddings = embed(prompts, args.model)
    print(f"Embeddings shape: {embeddings.shape}")

    # np.arange is exclusive on the upper bound; add half a step so the
    # endpoint is reliably included despite float drift.
    t_values = np.arange(args.t_min, args.t_max + args.t_step / 2, args.t_step)
    rows = sweep(embeddings, t_values)

    args.out_dir.mkdir(parents=True, exist_ok=True)
    csv_path = args.out_dir / "hitrate_sweep.csv"
    png_path = args.out_dir / "hitrate_sweep.png"
    write_csv(rows, csv_path)
    plot(rows, png_path, n=len(prompts))
    print(f"Wrote {csv_path} and {png_path}")


if __name__ == "__main__":
    main()
