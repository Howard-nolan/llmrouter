"""Build cache-benchmark corpora from Quora Question Pairs.

Emits two corpora to benchmarks/data/:

  corpus_clustered.json -- 40 clusters x 5 paraphrases (200 prompts).
    Dense within-cluster paraphrases. Used for threshold sweeps and
    LLM-as-judge quality scoring; needs many within-cluster pairs to
    give the hit-rate-vs-threshold curve clear signal.

  corpus_realistic.json -- power-law distribution (~199 prompts).
    A few hot clusters + long tail of singletons. Represents chatbot
    traffic where a handful of FAQs dominate and most queries are unique.
    Used for realistic cost-savings measurement.

The two corpora share no prompts (enforced via a `used` set).

Run from the training/ directory:
    uv run python build_corpus.py
"""

import json
import random
from pathlib import Path

import networkx as nx
from datasets import load_dataset

SEED = 7
MIN_LEN = 20
MAX_LEN = 500

BLOCKLIST = (
    "porn", "sex", "nude", "naked", "masturbat", "erotic",
    "penis", "vagina", "boob", "nsfw", "fetish", "rob", "kill",
)

# Clustered corpus: dense paraphrase groups for threshold-sweep experiments.
CLUSTERED_N = 40
CLUSTERED_SIZE = 5

# Realistic corpus: power-law over (cluster_count, prompts_per_cluster).
# Hot items first so they grab the rarest large components before pairs.
# Size 1 means "singleton" -- pulled from questions with no known paraphrases.
REALISTIC_SHAPE = [
    (1, 15),    # 1 hot cluster: 15 paraphrases of the same question
    (3, 8),     # 3 warm clusters
    (10, 4),    # 10 medium clusters
    (30, 2),    # 30 pairs
    (60, 1),    # 60 singletons
]

DATA_DIR = Path(__file__).parent.parent / "benchmarks" / "data"


def is_clean(text: str) -> bool:
    if not text or not (MIN_LEN <= len(text) <= MAX_LEN):
        return False
    lower = text.lower()
    return not any(w in lower for w in BLOCKLIST)


def build_graph(ds):
    """Return (duplicate graph, set of all clean questions seen).

    The graph has edges only between labeled duplicates. `all_clean` includes
    every clean question from every row -- duplicates and non-duplicates --
    so non-graph-nodes form the singleton pool.
    """
    graph = nx.Graph()
    all_clean: set[str] = set()
    for row in ds:
        q1, q2 = row["question1"], row["question2"]
        if not (is_clean(q1) and is_clean(q2)):
            continue
        all_clean.add(q1)
        all_clean.add(q2)
        if row["label"] == 1:
            graph.add_edge(q1, q2)
    return graph, all_clean


def take_components(graph, needed, min_size, used):
    """Return `needed` connected components of size >= min_size, excluding any
    that share questions with `used`. Returned components are sorted lists."""
    candidates = []
    for component in nx.connected_components(graph):
        if len(component) < min_size:
            continue
        if component & used:
            continue
        candidates.append(sorted(component))
    candidates.sort()
    random.shuffle(candidates)
    if len(candidates) < needed:
        raise SystemExit(
            f"Only {len(candidates)} components of size >= {min_size} available "
            f"after exclusions (need {needed}). Adjust REALISTIC_SHAPE or relax filters."
        )
    return candidates[:needed]


def take_singletons(all_clean, graph, needed, used):
    """Return `needed` clean questions that have no known paraphrases."""
    pool = sorted(all_clean - set(graph.nodes()) - used)
    random.shuffle(pool)
    if len(pool) < needed:
        raise SystemExit(f"Singleton pool size {len(pool)} < needed {needed}.")
    return pool[:needed]


def select_clustered(graph):
    components = take_components(graph, CLUSTERED_N, CLUSTERED_SIZE, used=set())
    return [
        {"id": i, "prompts": comp[:CLUSTERED_SIZE]}
        for i, comp in enumerate(components)
    ]


def select_realistic(graph, all_clean, used):
    clusters = []
    cid = 0
    for count, size in REALISTIC_SHAPE:
        if size == 1:
            for prompt in take_singletons(all_clean, graph, count, used):
                clusters.append({"id": cid, "prompts": [prompt]})
                used.add(prompt)
                cid += 1
        else:
            for comp in take_components(graph, count, size, used):
                picked = comp[:size]
                clusters.append({"id": cid, "prompts": picked})
                used.update(picked)
                cid += 1
    return clusters


def print_preview(label, clusters):
    total = sum(len(c["prompts"]) for c in clusters)
    print(f"\n=== {label} ({len(clusters)} clusters, {total} prompts) ===")
    for c in clusters:
        print(f"\n[cluster {c['id']}] ({len(c['prompts'])} prompts)")
        for p in c["prompts"]:
            print(f"  - {p}")


def write_corpus(path, clusters, profile):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(
        {
            "source": "quora-question-pairs (GLUE train split)",
            "seed": SEED,
            "profile": profile,
            "n_clusters": len(clusters),
            "n_prompts": sum(len(c["prompts"]) for c in clusters),
            "clusters": clusters,
        },
        indent=2,
    ))
    print(f"Wrote {path}")


def main() -> None:
    random.seed(SEED)

    print("Loading QQP (GLUE) train split from HuggingFace...")
    ds = load_dataset("glue", "qqp", split="train")
    print(f"  {len(ds)} pairs loaded")

    print("Building duplicate graph...")
    graph, all_clean = build_graph(ds)
    print(
        f"  {graph.number_of_nodes()} graph nodes, "
        f"{graph.number_of_edges()} edges, "
        f"{len(all_clean)} clean questions total"
    )

    clustered = select_clustered(graph)
    used = {p for c in clustered for p in c["prompts"]}

    realistic = select_realistic(graph, all_clean, used)

    print_preview("CLUSTERED (threshold sweep)", clustered)
    print_preview("REALISTIC (cost savings)", realistic)

    write_corpus(DATA_DIR / "corpus_clustered.json", clustered, "clustered")
    write_corpus(DATA_DIR / "corpus_realistic.json", realistic, "realistic")


if __name__ == "__main__":
    main()
