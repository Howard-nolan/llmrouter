# Parameter tuning

How llmrouter's two thresholds were chosen and how the complexity classifier was trained. This is the methodology behind the headline numbers in the [README](./README.md#benchmarks).

Two parameters drive routing behavior:

- **Cache similarity threshold** — how close a new prompt's embedding must be to a cached entry to count as a hit.
- **Complexity classifier threshold** — how confident the classifier must be that a prompt needs the expensive model.

Both are tuned against quality-labeled data, not picked by intuition.

---

## Cache similarity threshold

Tradeoff: too low → false hits (cached responses served for prompts they don't answer); too high → cache provides no value.

### Method

1. **Offline hit-rate sweep.** Embed every prompt in a 200-prompt clustered corpus and simulate lookup math over `T ∈ [0.75, 0.99]`. No gateway calls — this just establishes the hit-rate curve.
2. **Live hit collection at `T=0.75`.** Set the threshold loose, run the corpus through the gateway, log every hit as `{original, new, cached_response, similarity}` to a JSONL file. Hits at stricter T are a strict subset of these, so one run covers every candidate threshold.
3. **LLM-as-judge labeling.** Gemini 2.5 Pro labels each `(cached, fresh-baseline)` pair as adequate or inadequate on an intrinsic adequacy rubric. Intrinsic, not competitive — stylistic differences alone don't fail.
4. **Threshold selection.** For each candidate T, compute false-hit rate (FHR = inadequate hits / total hits at that T). Pick the lowest T meeting the target FHR.

### Result

`T* = 0.92` at 10% FHR.

![Threshold selection curve](benchmarks/python/results/threshold_selection.png)

The original target was 5% FHR — unreachable on this corpus. Only `T ≥ 0.98` cleared 5%, where the hit rate collapsed to 3% (cache provides almost no value). Renegotiated to 10% FHR at `T = 0.92` for a 22% hit rate. The tradeoff: about 1 in 10 cache hits is judged inadequate by the LLM judge.

The FHR curve has structure, not just monotonic decay:

- **Plateau at 25% FHR for `T ∈ [0.75, 0.85]`** — loose QQP near-duplicates hit and get flagged.
- **Drop to 10% at `T = 0.92`** — genuine paraphrases only.
- **Erratic small-N noise above `T = 0.94`**, where labeled hits drop below 15.

The 0.92 dip is principled — it's where the corpus's threshold structure transitions, not a random local minimum.

---

## Complexity classifier

Goal: predict whether a prompt needs the expensive model (`label=1`) or the cheap model is adequate (`label=0`), so the router can save money without sacrificing quality.

### Dataset

- **2,499 prompts** from 8 sources: Dolly, OpenAssistant, MMLU, HumanEval, MBPP, GSM8K, ARC Challenge, Alpaca.
- Cheap and expensive responses collected for each prompt.
- Labeled by Gemini 2.5 Pro as LLM-as-judge: "Is the cheap model's response adequate?"
- **Distribution:** 1,976 adequate (79.1%) / 523 needs-expensive (20.9%) — 4:1 imbalance.
- **Majority-class baseline:** 79.2% accuracy by predicting all-adequate.

Label distribution varies sharply by source — OpenAssistant (open-ended) is 44% needs-expensive, while GSM8K (math) is 1.5%.

### Phase 1: MLP (dead end)

A PyTorch MLP (`384 → 64 → 32 → 1`) was trained across multiple architectures and hyperparameter settings.

**Result:** never learned. Every run collapsed to predicting all-adequate (the majority class). Sigmoid outputs clustered on one side of 0.5 — no separation across thresholds. Increasing `pos_weight` up to 15× just made it oscillate between predicting all-0 and all-1.

**Why it failed:** 27K parameters on 2,499 samples with noisy labels and weak signal in embedding space. The MLP couldn't find a decision surface that the GBT also couldn't find, and it lacked the inductive bias (axis-aligned splits) that makes tree models work on tabular data.

### Phase 2: gradient-boosted trees

Three deliberate choices defined the final model.

#### Class weighting (the biggest single lever)

The 4:1 class imbalance caused the initial GBT to predict adequate for almost everything (3% recall on label=1). Progressively increased `sample_weight` on label=1:

| Weight Multiplier | Recall@0.5 | Best F2 | Adequate Correct@best |
|-------------------|-----------|---------|----------------------|
| 1× (natural 3.78) | 0.03 | — | — |
| 2× (7.56) | 0.47 | — | 36% |
| 4× (15.1) | 0.69 | 0.598 | 28% |

Higher weighting forced the model to stop ignoring the minority class.

#### F2 over F1

The goal is asymmetric: catching expensive prompts (recall) matters more than avoiding false alarms (precision). The threshold sweep was switched from F1 to **F2 score** (weights recall 2× over precision), with the sweep step refined from 0.05 to 0.01.

#### Handcrafted features (negative result)

6 features were designed to target task structure rather than topic:

| Feature | Signal |
|---------|--------|
| `subtask_count` | Multi-step instructions ("first", "then", "finally") |
| `constraint_count` | Constrained tasks ("without", "must not", "exactly") |
| `reasoning_keyword_count` | Analytical depth ("compare", "tradeoffs", "prove") |
| `question_count` | Multiple questions = harder |
| `code_task_type` | 0=none, 1=write code, 2=debug/optimize existing |
| `imperative_density` | Ratio of command sentences to total sentences |

When combined with embeddings, these features showed **0% importance** across every run — the GBT always preferred the 384 embedding dimensions. Tested alone (embeddings removed), the GBT achieved only 25% val accuracy — worse than random.

The features carry some relative signal (`imperative_density` and `subtask_count` rank highest), but not enough to be useful on their own. Dropped from model input. Extraction code retained in the training script as documentation of what was tried.

#### Grid search

4 GBT configs tested (100–300 trees, depth 5–7, `lr` 0.05–0.1). All scored within 0.01 F2 of each other — **the bottleneck is signal in the data, not model capacity**. Config 4 (300 trees, depth 7) hit 99.4% train accuracy but only 63.3% val accuracy — classic overfitting.

### Final model

GBT, **100 trees, depth 5, `lr=0.1`, 4× class weight, threshold 0.28.**

| Metric | Value |
|---|--:|
| Recall on label=1 | 91.3% |
| Precision on label=1 | 25% |
| Adequate-correct at threshold | 28% |

Conservative quality-first router: rarely serves a bad response, but overspends on about 2/3 of easy prompts. The cache absorbs some of the over-routing on near-duplicates.

### Takeaways

1. **Semantic embeddings encode topic, not complexity.** "Who is Klaus Schwab?" and "How do solar panels work?" look similar in embedding space, but one needs the expensive model and the other doesn't. The label depends partly on whether the cheap model *happened* to give a good answer — which has a random component.
2. **Small datasets favor tree models over MLPs.** The MLP never learned; the GBT found a usable signal immediately.
3. **Class weighting is the biggest single lever.** Going from 1× to 4× transformed the GBT from useless (3% recall) to functional (91% recall).
4. **Accuracy is misleading with imbalanced classes.** 79.2% comes for free by predicting all-adequate. F2 score aligned the optimization with the actual goal.
5. **Handcrafted features were redundant with embeddings.** Good experiment, clear negative result — the GBT extracts what it needs from the embedding dimensions directly.
6. **Hyperparameters matter less than data quality.** Four GBT configs scored within 0.01 of each other. More data or better labels would help more than tuning.
