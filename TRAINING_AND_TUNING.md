# Training and Tuning

How llmrouter's complexity classifier was trained and how both thresholds were chosen. This is the methodology behind the headline numbers in the [README](./README.md#benchmarks).

---

## Complexity classifier

Goal: predict whether a prompt needs the expensive model (`label=1`) or the cheap model is adequate (`label=0`).

### Dataset

- **2,499 prompts** from 8 sources: Dolly, OpenAssistant, MMLU, HumanEval, MBPP, GSM8K, ARC Challenge, Alpaca.
- Cheap and expensive responses collected for each prompt, labeled by Gemini 2.5 Pro as LLM-as-judge.
- **Distribution:** 1,976 adequate (79.1%) / 523 needs-expensive (20.9%) — 4:1 imbalance. Majority-class baseline: 79.2% accuracy.

Label distribution varies sharply by source — OpenAssistant (open-ended) is 44% needs-expensive, while GSM8K (math) is 1.5%.

### Phase 1: MLP (dead end)

A PyTorch MLP (`384 → 64 → 32 → 1`) never learned — every run collapsed to predicting the majority class. Increasing `pos_weight` up to 15× only made it oscillate between all-0 and all-1. Root cause: 27K parameters on 2,499 noisy samples. MLPs lack the axis-aligned inductive bias that makes tree models work well on tabular embedding data at small scale.

### Phase 2: Gradient-boosted trees

Switching to GBT found usable signal immediately. Three choices defined the final model:

**Class weighting** was the biggest lever. The 4:1 imbalance caused the initial GBT to predict adequate for almost everything (3% recall on label=1). Progressively increasing `sample_weight` on label=1 to 4× pushed recall to 69%.

**F2 over F1:** catching expensive prompts (recall) matters more than avoiding false alarms (precision), so the threshold sweep used F2 score (weights recall 2× over precision).

**Handcrafted features (negative result):** 6 task-structure features (subtask count, reasoning keywords, code task type, etc.) showed 0% importance in every run — the GBT extracted what it needed from the 384 embedding dimensions directly. Tested alone without embeddings, they hit 25% val accuracy, worse than random. Dropped.

Grid search over 4 configs (100–300 trees, depth 5–7) scored within 0.01 F2 of each other. The bottleneck is signal in the data, not model capacity.

### Final model

GBT, **100 trees, depth 5, `lr=0.1`, 4× class weight, threshold 0.28.**

| Metric | Value |
|---|--:|
| Recall on label=1 | 91.3% |
| Precision on label=1 | 25% |

Conservative quality-first router: high recall means expensive prompts rarely slip through to the cheap model, but low precision means about 2/3 of easy prompts are over-routed to the quality model. The cache absorbs some of this on near-duplicate prompts.

---

## Cache similarity threshold

Tradeoff: too low → false hits (cached responses served for prompts they don't answer); too high → cache provides no value.

### Method

1. **Offline hit-rate sweep.** Embed every prompt in a 200-prompt clustered corpus and simulate lookup math over `T ∈ [0.75, 0.99]`. No gateway calls — this establishes the hit-rate curve.
2. **Live hit collection at `T=0.75`.** Run the corpus through the gateway and log every hit as `{original, new, cached_response, similarity}`. Hits at stricter T are a strict subset, so one run covers every candidate threshold.
3. **LLM-as-judge labeling.** Gemini 2.5 Pro labels each `(cached, fresh-baseline)` pair as adequate or inadequate on an intrinsic rubric — stylistic differences alone don't fail.
4. **Threshold selection.** For each candidate T, compute false-hit rate (FHR = inadequate hits / total hits). Pick the lowest T meeting the FHR target.

### Result

`T* = 0.92` at 10% FHR.

![Threshold selection curve](benchmarks/python/results/threshold_selection.png)

The original target was 5% FHR — unreachable on this corpus. Only `T ≥ 0.98` cleared 5%, where hit rate collapsed to 3% (cache provides almost no value). Renegotiated to 10% FHR at `T = 0.92` for a 22% hit rate. The tradeoff: about 1 in 10 cache hits is judged inadequate.

The FHR curve has structure, not just monotonic decay:

- **Plateau at 25% FHR for `T ∈ [0.75, 0.85]`** — loose near-duplicates hit and get flagged.
- **Drop to 10% at `T = 0.92`** — genuine paraphrases only.
- **Erratic noise above `T = 0.94`** where labeled hits drop below 15.

The 0.92 dip is principled — it's where the corpus's threshold structure transitions, not a random local minimum.
