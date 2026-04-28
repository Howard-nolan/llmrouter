"""Threshold selection from labeled tuning records.

Step 4 of the cache similarity-threshold tuning pipeline.

For each candidate T, computes the false-hit rate over the labeled records —
the fraction of hits with similarity >= T where the judge said NO. Combines
with the offline hit-rate sweep curve from step 1 (sweep_hitrate.py) and
recommends the lowest T whose false-hit rate is at or below `--target`.

Usage:
    uv run python pick_threshold.py [--labeled PATH] [--hitrate-csv PATH] \\
        [--out-dir DIR] [--target FLOAT] [--t-min ...] [--t-max ...] [--t-step ...]
"""
from __future__ import annotations

import argparse
import csv
import json
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np


SCRIPT_DIR = Path(__file__).parent
DEFAULT_LABELED = SCRIPT_DIR.parent / "data" / "tuning_records_labeled.jsonl"
DEFAULT_HITRATE_CSV = SCRIPT_DIR / "results" / "hitrate_sweep.csv"
DEFAULT_OUT_DIR = SCRIPT_DIR / "results"


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Pick T* from labeled tuning data")
    p.add_argument("--labeled", type=Path, default=DEFAULT_LABELED,
                   help="Labeled tuning records JSONL (from label_quality.py)")
    p.add_argument("--hitrate-csv", type=Path, default=DEFAULT_HITRATE_CSV,
                   help="Hit-rate sweep CSV from sweep_hitrate.py")
    p.add_argument("--out-dir", type=Path, default=DEFAULT_OUT_DIR,
                   help="Directory for CSV + PNG outputs")
    p.add_argument("--target", type=float, default=0.05,
                   help="False-hit rate target — pick lowest T with FHR <= target")
    p.add_argument("--t-min", type=float, default=0.75)
    p.add_argument("--t-max", type=float, default=0.99)
    p.add_argument("--t-step", type=float, default=0.005)
    p.add_argument("--min-sample", type=int, default=20,
                   help="Warn if T* has fewer labeled hits than this (high variance)")
    return p.parse_args()


def load_labeled(path: Path) -> list[tuple[float, str]]:
    pairs: list[tuple[float, str]] = []
    with path.open() as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            entry = json.loads(line)
            label = entry["label"]
            if label not in ("YES", "NO"):
                raise ValueError(f"unexpected label {label!r} in {path}")
            pairs.append((float(entry["similarity"]), label))
    return pairs


def load_hitrate(path: Path) -> dict[float, float]:
    out: dict[float, float] = {}
    with path.open() as f:
        for row in csv.DictReader(f):
            out[round(float(row["threshold"]), 4)] = float(row["hit_rate"])
    return out


def compute_fhr(
    pairs: list[tuple[float, str]], t_values: np.ndarray
) -> list[dict]:
    sims = np.array([s for s, _ in pairs])
    is_no = np.array([lab == "NO" for _, lab in pairs])
    rows: list[dict] = []
    for T in t_values:
        mask = sims >= T
        n_hits = int(mask.sum())
        n_no = int(is_no[mask].sum())
        fhr = (n_no / n_hits) if n_hits > 0 else None
        rows.append({
            "threshold": round(float(T), 4),
            "n_hits": n_hits,
            "n_no": n_no,
            "false_hit_rate": fhr,
        })
    return rows


def write_csv(rows: list[dict], path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="") as f:
        writer = csv.DictWriter(
            f, fieldnames=["threshold", "n_hits", "n_no", "false_hit_rate"]
        )
        writer.writeheader()
        for row in rows:
            r = dict(row)
            r["false_hit_rate"] = (
                "" if r["false_hit_rate"] is None
                else f"{r['false_hit_rate']:.4f}"
            )
            writer.writerow(r)


def pick_t_star(rows: list[dict], target: float) -> dict | None:
    """Lowest threshold whose FHR is defined and <= target."""
    for row in rows:
        fhr = row["false_hit_rate"]
        if fhr is not None and fhr <= target:
            return row
    return None


def print_table(rows: list[dict], hitrate: dict[float, float]) -> None:
    print(f"{'T':>6} {'hit_rate':>9} {'n_hits':>7} {'n_no':>5} {'FHR':>7}")
    for row in rows:
        t = row["threshold"]
        hr = hitrate.get(t)
        hr_str = f"{hr * 100:.1f}%" if hr is not None else "?"
        fhr = row["false_hit_rate"]
        fhr_str = f"{fhr * 100:.1f}%" if fhr is not None else "—"
        print(f"{t:>6.3f} {hr_str:>9} {row['n_hits']:>7} {row['n_no']:>5} {fhr_str:>7}")


def plot(
    fhr_rows: list[dict],
    hitrate: dict[float, float],
    target: float,
    t_star: dict | None,
    path: Path,
) -> None:
    t_vals = [r["threshold"] for r in fhr_rows]
    hit_pct = [hitrate.get(t, np.nan) * 100 if hitrate.get(t) is not None else np.nan
               for t in t_vals]
    fhr_pct = [r["false_hit_rate"] * 100 if r["false_hit_rate"] is not None else np.nan
               for r in fhr_rows]

    fig, (ax_hit, ax_fhr) = plt.subplots(2, 1, figsize=(8, 7), sharex=True)

    ax_hit.plot(t_vals, hit_pct, marker="o", linewidth=2, color="C0",
                label="hit rate")
    ax_hit.set_ylabel("Hit rate (%)")
    ax_hit.set_title("Threshold selection — hit rate vs. false-hit rate")
    ax_hit.grid(alpha=0.3)

    ax_fhr.plot(t_vals, fhr_pct, marker="o", linewidth=2, color="C3",
                label="false-hit rate")
    ax_fhr.axhline(target * 100, linestyle="--", color="gray",
                   label=f"target {target * 100:.0f}%")
    ax_fhr.set_ylabel("False-hit rate (%)")
    ax_fhr.set_xlabel("Similarity threshold T")
    ax_fhr.grid(alpha=0.3)

    if t_star is not None:
        for ax in (ax_hit, ax_fhr):
            ax.axvline(t_star["threshold"], linestyle=":", color="green",
                       label=f"T* = {t_star['threshold']}")

    ax_hit.legend(loc="upper right")
    ax_fhr.legend(loc="upper right")

    fig.tight_layout()
    fig.savefig(path, dpi=150)


def main() -> None:
    args = parse_args()

    if not args.labeled.exists():
        raise SystemExit(f"labeled file not found: {args.labeled}")
    if not args.hitrate_csv.exists():
        raise SystemExit(
            f"hit-rate CSV not found: {args.hitrate_csv} "
            "(run sweep_hitrate.py first)"
        )

    pairs = load_labeled(args.labeled)
    hitrate = load_hitrate(args.hitrate_csv)
    print(f"Loaded {len(pairs)} labeled hits, {len(hitrate)} hit-rate points")

    t_values = np.arange(args.t_min, args.t_max + args.t_step / 2, args.t_step)
    rows = compute_fhr(pairs, t_values)

    args.out_dir.mkdir(parents=True, exist_ok=True)
    csv_path = args.out_dir / "false_hit_rate_sweep.csv"
    png_path = args.out_dir / "threshold_selection.png"
    write_csv(rows, csv_path)

    print()
    print_table(rows, hitrate)
    print()

    t_star = pick_t_star(rows, args.target)
    if t_star is None:
        print(f"No threshold with false-hit rate <= {args.target * 100:.0f}%. "
              "Consider raising the target or collecting more labels.")
    else:
        t = t_star["threshold"]
        hr = hitrate.get(t)
        hr_part = f", hit rate {hr * 100:.1f}%" if hr is not None else ""
        print(f"Recommended T* = {t} "
              f"(FHR {t_star['false_hit_rate'] * 100:.1f}%, "
              f"n_hits={t_star['n_hits']}{hr_part})")
        if t_star["n_hits"] < args.min_sample:
            print(f"  Warning: only {t_star['n_hits']} labeled hits at T*; "
                  f"FHR estimate has high variance.")

    plot(rows, hitrate, args.target, t_star, png_path)
    print(f"\nWrote {csv_path} and {png_path}")


if __name__ == "__main__":
    main()
