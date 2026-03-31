"""
collect_dataset.py — Phase 1 of the complexity classifier pipeline.

Samples ~500 diverse prompts from four public datasets (Dolly, OpenAssistant,
MMLU, HumanEval), sends each to both a cheap model (claude-haiku-4-5-20251001)
and a quality model (claude-sonnet-4-5-20250929), and saves the prompt + both
responses as JSONL.

Usage:
    uv run python collect_dataset.py

Requires ANTHROPIC_API_KEY environment variable.
"""

import json
import os
import random
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

import anthropic
from dotenv import load_dotenv
from datasets import load_dataset

# Load environment variables from the project root .env file.
# python-dotenv walks up from the current file's directory looking for .env,
# but we point it explicitly to the project root to be safe.
load_dotenv(Path(__file__).parent.parent / ".env")

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

CHEAP_MODEL = "claude-haiku-4-5-20251001"
QUALITY_MODEL = "claude-sonnet-4-5-20250929"
MAX_TOKENS = 8192
OUTPUT_FILE = Path(__file__).parent / "dataset.jsonl"
PROMPTS_FILE = Path(__file__).parent / "prompts.jsonl"

# Number of prompts to process concurrently.
# 5 workers = ~10 API calls in flight (2 per prompt), well under rate limits.
WORKERS = 5

# Retry config for transient API failures.
MAX_RETRIES = 3
RETRY_BACKOFF = 2.0  # exponential backoff multiplier

SEED = 42

# ---------------------------------------------------------------------------
# Prompt samplers
# ---------------------------------------------------------------------------


def sample_openassistant(n: int = 150) -> list[dict]:
    """
    Sample real user prompts from OpenAssistant (oasst2).

    Each row is a single message in a conversation tree. Root messages
    (parent_id is None) with role "prompter" are first user messages.
    We filter to English and apply the same length constraints.
    """
    print(f"Loading OpenAssistant/oasst2 (sampling {n})...")
    ds = load_dataset("OpenAssistant/oasst2", split="train")

    candidates = []
    for row in ds:
        # Only root messages (first in a conversation tree) from users.
        if row.get("role") != "prompter" or row.get("parent_id") is not None:
            continue

        if row.get("lang") != "en":
            continue

        prompt = row["text"].strip()

        if len(prompt) < 20 or len(prompt) > 2000:
            continue

        candidates.append({"prompt": prompt, "source": "openassistant"})

    random.shuffle(candidates)
    return candidates[:n]


def sample_mmlu(n: int = 100) -> list[dict]:
    """
    Sample knowledge/reasoning questions from MMLU.

    MMLU has multiple-choice questions across 57 subjects at varying
    difficulty levels (elementary → professional). We send the full
    question with answer choices so the model has the same context
    a test-taker would.
    """
    print(f"Loading MMLU (sampling {n})...")
    ds = load_dataset("cais/mmlu", "all", split="test")

    candidates = []
    for row in ds:
        question = row["question"].strip()
        choices = row["choices"]

        # Format as a multiple-choice question.
        letters = ["A", "B", "C", "D"]
        formatted = question + "\n"
        for letter, choice in zip(letters, choices):
            formatted += f"\n{letter}. {choice}"

        candidates.append({"prompt": formatted, "source": "mmlu"})

    random.shuffle(candidates)
    return candidates[:n]


def sample_humaneval(n: int = 50) -> list[dict]:
    """
    Sample code generation prompts from HumanEval.

    Each entry has a "prompt" field containing a function signature and
    docstring. These are self-contained code generation tasks.
    """
    print(f"Loading HumanEval (sampling {n})...")
    ds = load_dataset("openai/openai_humaneval", split="test")

    candidates = []
    for row in ds:
        prompt = row["prompt"].strip()
        # Wrap in an instruction so the model knows what to do.
        instruction = f"Complete the following Python function:\n\n{prompt}"
        candidates.append({"prompt": instruction, "source": "humaneval"})

    random.shuffle(candidates)
    return candidates[:n]


def sample_dolly(n: int = 200) -> list[dict]:
    """
    Sample curated instructions from Dolly.

    Dolly has 8 instruction categories. Some entries include a "context"
    field (e.g. a passage to summarize). We concatenate context + instruction
    so the prompt is self-contained.
    """
    print(f"Loading Dolly (sampling {n})...")
    ds = load_dataset("databricks/databricks-dolly-15k", split="train")

    candidates = []
    for row in ds:
        instruction = row["instruction"].strip()
        context = row.get("context", "").strip()

        if context:
            prompt = f"{instruction}\n\nContext:\n{context}"
        else:
            prompt = instruction

        # Skip very short instructions.
        if len(prompt) < 20:
            continue

        candidates.append({"prompt": prompt, "source": "dolly"})

    random.shuffle(candidates)
    return candidates[:n]


# ---------------------------------------------------------------------------
# Anthropic API calls
# ---------------------------------------------------------------------------


def call_anthropic(client: anthropic.Anthropic, model_name: str, prompt: str) -> str | None:
    """
    Send a prompt to an Anthropic model and return the response text.

    Retries up to MAX_RETRIES times with exponential backoff on transient
    errors (rate limits, timeouts, server errors).

    Returns None if all retries fail or if the response was truncated
    (stop_reason="max_tokens") — the caller will skip this prompt.
    """
    for attempt in range(MAX_RETRIES):
        try:
            response = client.messages.create(
                model=model_name,
                max_tokens=MAX_TOKENS,
                messages=[{"role": "user", "content": prompt}],
            )

            # Skip truncated responses — they'd produce misleading quality labels.
            if response.stop_reason == "max_tokens":
                print(f"  Truncated response from {model_name} (stop_reason=max_tokens)")
                return None

            # Extract text from the first content block.
            text = response.content[0].text
            if not text:
                print(f"  Empty response from {model_name}")
                return None

            return text

        except Exception as e:
            wait = RETRY_BACKOFF ** attempt
            print(f"  Attempt {attempt + 1}/{MAX_RETRIES} failed ({e}), "
                  f"retrying in {wait:.0f}s...")
            time.sleep(wait)

    return None


# ---------------------------------------------------------------------------
# Main collection loop
# ---------------------------------------------------------------------------


def load_existing_prompts() -> set[str]:
    """
    Read already-completed prompts from the output file for resumability.
    Returns a set of prompt strings that have already been processed.
    """
    if not OUTPUT_FILE.exists():
        return set()

    completed = set()
    with open(OUTPUT_FILE) as f:
        for line in f:
            line = line.strip()
            if line:
                entry = json.loads(line)
                completed.add(entry["prompt"])

    print(f"Found {len(completed)} already-completed entries, resuming...")
    return completed


def collect():
    """
    Main entrypoint: sample prompts, call both models, save results.
    """
    # Validate API key is set (loaded from .env by dotenv above).
    api_key = os.environ.get("ANTHROPIC_API_KEY")
    if not api_key:
        print("Error: ANTHROPIC_API_KEY not found.")
        print("Set it in .env at the project root or export it in your shell.")
        return

    client = anthropic.Anthropic(api_key=api_key)
    random.seed(SEED)

    # Phase 1: Sample prompts (or load from cache).
    # We save the sampled prompts to prompts.jsonl so that re-running the
    # script uses the same prompts (deterministic even if datasets update).
    if PROMPTS_FILE.exists():
        print(f"Loading cached prompts from {PROMPTS_FILE}...")
        prompts = []
        with open(PROMPTS_FILE) as f:
            for line in f:
                line = line.strip()
                if line:
                    prompts.append(json.loads(line))
    else:
        prompts = []
        prompts.extend(sample_dolly(200))
        prompts.extend(sample_openassistant(150))
        prompts.extend(sample_mmlu(100))
        prompts.extend(sample_humaneval(50))
        random.shuffle(prompts)

        # Save prompts for reproducibility.
        with open(PROMPTS_FILE, "w") as f:
            for p in prompts:
                f.write(json.dumps(p) + "\n")
        print(f"Saved {len(prompts)} prompts to {PROMPTS_FILE}")

    # Phase 2: Send each prompt to both models.
    completed = load_existing_prompts()
    remaining = [p for p in prompts if p["prompt"] not in completed]
    total = len(prompts)
    done = total - len(remaining)

    print(f"\n{total} total prompts, {done} already done, {len(remaining)} remaining")
    print(f"Models: {CHEAP_MODEL} (cheap) + {QUALITY_MODEL} (quality)")
    print(f"Workers: {WORKERS} concurrent\n")

    # Lock protects the shared output file and the progress counter.
    write_lock = threading.Lock()
    completed_count = done

    def process_prompt(prompt_entry: dict) -> None:
        """Process a single prompt: call both models, write result to disk."""
        nonlocal completed_count

        prompt = prompt_entry["prompt"]
        source = prompt_entry["source"]

        cheap_response = call_anthropic(client, CHEAP_MODEL, prompt)
        quality_response = call_anthropic(client, QUALITY_MODEL, prompt)

        with write_lock:
            completed_count += 1
            progress = f"[{completed_count}/{total}]"

            if cheap_response is None or quality_response is None:
                print(f"{progress} Skipping (one or both models failed)")
                return

            entry = {
                "prompt": prompt,
                "source": source,
                "cheap_response": cheap_response,
                "quality_response": quality_response,
                "cheap_model": CHEAP_MODEL,
                "quality_model": QUALITY_MODEL,
            }

            with open(OUTPUT_FILE, "a") as f:
                f.write(json.dumps(entry) + "\n")

            print(f"{progress} Done (cheap: {len(cheap_response)} chars, "
                  f"quality: {len(quality_response)} chars)")

    with ThreadPoolExecutor(max_workers=WORKERS) as pool:
        futures = [pool.submit(process_prompt, p) for p in remaining]
        for future in as_completed(futures):
            future.result()  # raises if process_prompt threw an unhandled exception

    # Final summary + validation.
    validate_dataset()


def validate_dataset():
    """Check the collected dataset for common issues."""
    if not OUTPUT_FILE.exists():
        print("No output file found — nothing to validate.")
        return

    print("\n" + "=" * 60)
    print("DATASET VALIDATION")
    print("=" * 60)

    entries = []
    malformed = 0
    with open(OUTPUT_FILE) as f:
        for i, line in enumerate(f, 1):
            line = line.strip()
            if not line:
                continue
            try:
                entry = json.loads(line)
                entries.append(entry)
            except json.JSONDecodeError:
                malformed += 1
                print(f"  WARNING: malformed JSON on line {i}")

    total = len(entries)
    print(f"\nRows: {total}")
    if malformed:
        print(f"Malformed rows: {malformed}")

    # Check for missing fields.
    required = {"prompt", "source", "cheap_response", "quality_response"}
    missing_fields = sum(1 for e in entries if not required.issubset(e.keys()))
    if missing_fields:
        print(f"WARNING: {missing_fields} rows missing required fields")

    # Source distribution.
    sources: dict[str, int] = {}
    for e in entries:
        src = e.get("source", "unknown")
        sources[src] = sources.get(src, 0) + 1
    print(f"\nSource distribution:")
    for src, count in sorted(sources.items()):
        print(f"  {src}: {count}")

    # Response length stats.
    short_cheap = []
    short_quality = []
    for e in entries:
        if len(e.get("cheap_response", "")) < 50:
            short_cheap.append(e["prompt"][:60])
        if len(e.get("quality_response", "")) < 50:
            short_quality.append(e["prompt"][:60])

    if short_cheap:
        print(f"\nWARNING: {len(short_cheap)} cheap responses under 50 chars:")
        for p in short_cheap[:5]:
            print(f"  - {p}...")
    if short_quality:
        print(f"\nWARNING: {len(short_quality)} quality responses under 50 chars:")
        for p in short_quality[:5]:
            print(f"  - {p}...")

    # Duplicate prompts.
    seen = set()
    dupes = 0
    for e in entries:
        p = e["prompt"]
        if p in seen:
            dupes += 1
        seen.add(p)
    if dupes:
        print(f"\nWARNING: {dupes} duplicate prompts")

    # Summary.
    avg_cheap = sum(len(e.get("cheap_response", "")) for e in entries) / max(total, 1)
    avg_quality = sum(len(e.get("quality_response", "")) for e in entries) / max(total, 1)
    print(f"\nAvg response length:")
    print(f"  Cheap:   {avg_cheap:.0f} chars")
    print(f"  Quality: {avg_quality:.0f} chars")

    if not malformed and not missing_fields and not dupes:
        print(f"\nAll checks passed. {total} entries ready for labeling.")
    else:
        print(f"\nValidation found issues — review warnings above.")

    print("=" * 60)


if __name__ == "__main__":
    collect()
