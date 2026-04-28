"""End-to-end quality preservation analysis.

Reads the harness JSONL (all records, with path classification derivable
from `cache_hit` + `model_routed`) and the labeled JSONL (subset judged
by label_eval.py), then computes per-path quality rates and an overall
preservation rate.

Path classification:
  - hit:          cache_hit == true
  - cheap_miss:   cache_hit == false AND model_routed != baseline_model
  - quality_miss: cache_hit == false AND model_routed == baseline_model

Quality definitions:
  - hit, cheap_miss: judged by label_eval.py — quality rate = YES / labeled
  - quality_miss:    implicitly preserved (already from baseline model)
  - overall preservation = (judged YES + quality_miss count) / total
"""
from __future__ import annotations

import argparse
import json
from collections import defaultdict
from pathlib import Path


SCRIPT_DIR = Path(__file__).parent
DEFAULT_RECORDS = SCRIPT_DIR.parent / "data" / "realistic_records.jsonl"
DEFAULT_LABELED = SCRIPT_DIR.parent / "data" / "realistic_records_labeled.jsonl"


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Per-path quality preservation analysis")
    p.add_argument("--records", type=Path, default=DEFAULT_RECORDS,
                   help="Harness JSONL (all records)")
    p.add_argument("--labeled", type=Path, default=DEFAULT_LABELED,
                   help="Labeled subset JSONL (from label_eval.py)")
    p.add_argument("--baseline-model", default="claude-sonnet-4-5-20250929",
                   help="Model treated as the quality baseline (defines quality_miss)")
    return p.parse_args()


def load_jsonl(path: Path) -> list[dict]:
    out: list[dict] = []
    with path.open() as f:
        for line in f:
            line = line.strip()
            if line:
                out.append(json.loads(line))
    return out


def classify_path(record: dict, baseline_model: str) -> str:
    if record.get("cache_hit"):
        return "hit"
    if record.get("model_routed") == baseline_model:
        return "quality_miss"
    return "cheap_miss"


def main() -> None:
    args = parse_args()

    if not args.records.exists():
        raise SystemExit(f"records file not found: {args.records}")
    if not args.labeled.exists():
        raise SystemExit(f"labeled file not found: {args.labeled}")

    records = load_jsonl(args.records)
    labeled = load_jsonl(args.labeled)

    # Index labels by (prompt, gateway_response) so we can join back to records.
    label_index: dict[tuple[str, str], str] = {}
    for entry in labeled:
        key = (entry["prompt"], entry["gateway_response"])
        label_index[key] = entry["label"]

    path_total: dict[str, int] = defaultdict(int)
    path_yes: dict[str, int] = defaultdict(int)
    path_no: dict[str, int] = defaultdict(int)
    path_unlabeled: dict[str, int] = defaultdict(int)

    for r in records:
        path = classify_path(r, args.baseline_model)
        path_total[path] += 1
        key = (r["prompt"], r.get("gateway_response", ""))
        if path == "quality_miss":
            # Implicitly preserved — same model as baseline.
            continue
        label = label_index.get(key)
        if label == "YES":
            path_yes[path] += 1
        elif label == "NO":
            path_no[path] += 1
        else:
            path_unlabeled[path] += 1

    paths = sorted(path_total.keys())
    total = sum(path_total.values())
    preserved = path_total["quality_miss"]
    for p in ("hit", "cheap_miss"):
        preserved += path_yes[p]

    print("=== Per-path quality ===")
    print(f"{'path':<14} {'count':>6} {'YES':>5} {'NO':>4} {'unlbl':>6} "
          f"{'rate':>7}")
    for p in paths:
        n = path_total[p]
        if p == "quality_miss":
            rate = "100% *"
            yes_cell = "—"
            no_cell = "—"
            unl = "—"
        else:
            judged = path_yes[p] + path_no[p]
            rate = f"{path_yes[p] / judged * 100:.1f}%" if judged else "n/a"
            yes_cell = str(path_yes[p])
            no_cell = str(path_no[p])
            unl = str(path_unlabeled[p])
        print(f"{p:<14} {n:>6} {yes_cell:>5} {no_cell:>4} {unl:>6} {rate:>7}")
    print("  * quality_miss = baseline model, no judging needed")

    print()
    print("=== Overall preservation ===")
    if total > 0:
        print(f"  preserved = {preserved}/{total} = {preserved / total * 100:.1f}%")
        print(f"  (YES on hits + YES on cheap_miss + all quality_miss)")
    unlabeled_total = sum(path_unlabeled.values())
    if unlabeled_total > 0:
        print(f"  Note: {unlabeled_total} records lack labels — "
              f"run label_eval.py to fill them in.")


if __name__ == "__main__":
    main()
