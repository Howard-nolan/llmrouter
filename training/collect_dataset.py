"""
collect_dataset.py — Phase 1 of the complexity classifier pipeline.

Samples ~500 diverse prompts from four public datasets, sends each to both
a cheap model (gemini-2.0-flash) and a quality model (gemini-2.5-pro), and
saves the prompt + both responses as JSONL.

Usage:
    uv run python collect_dataset.py

Requires GOOGLE_API_KEY environment variable.
"""

import json
import os
import random
import time
from pathlib import Path

from dotenv import load_dotenv
from google import genai
from datasets import load_dataset

# Load environment variables from the project root .env file.
# python-dotenv walks up from the current file's directory looking for .env,
# but we point it explicitly to the project root to be safe.
load_dotenv(Path(__file__).parent.parent / ".env")

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

CHEAP_MODEL = "gemini-2.0-flash"
QUALITY_MODEL = "gemini-2.5-pro"
MAX_TOKENS = 1024
OUTPUT_FILE = Path(__file__).parent / "dataset.jsonl"
PROMPTS_FILE = Path(__file__).parent / "prompts.jsonl"

# Delay between API calls to stay within rate limits.
API_DELAY = 1.0

# Retry config for transient API failures.
MAX_RETRIES = 3
RETRY_BACKOFF = 2.0  # exponential backoff multiplier

SEED = 42

# ---------------------------------------------------------------------------
# Prompt samplers
# ---------------------------------------------------------------------------


def sample_lmsys(n: int = 300) -> list[dict]:
    """
    Sample real user prompts from LMSYS-Chat-1M.

    This dataset contains 1M real conversations from Chatbot Arena. We extract
    the first user message from each conversation, filter to English, and
    remove extremely short or long prompts.
    """
    print(f"Loading LMSYS-Chat-1M (sampling {n})...")
    ds = load_dataset("lmsys/lmsys-chat-1m", split="train", streaming=True)

    # streaming=True avoids downloading the full 1M-row dataset into memory.
    # We iterate through it and collect candidates until we have enough.
    # This is like a Node.js readable stream — we consume rows one at a time.
    candidates = []
    for row in ds:
        # Each row has a "conversation" field: a list of {role, content} dicts.
        # We want the first user message.
        if row.get("language") != "English":
            continue

        conversation = row.get("conversation", [])
        if not conversation or conversation[0].get("role") != "human":
            continue

        prompt = conversation[0]["content"].strip()

        # Filter out too-short (likely "hi"/"test") and too-long prompts.
        if len(prompt) < 20 or len(prompt) > 2000:
            continue

        candidates.append({"prompt": prompt, "source": "lmsys"})

        # Collect more candidates than we need so we can randomly sample.
        # 3x gives us good diversity after random selection.
        if len(candidates) >= n * 3:
            break

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


def sample_dolly(n: int = 50) -> list[dict]:
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
# Gemini API calls
# ---------------------------------------------------------------------------


def call_gemini(client: genai.Client, model_name: str, prompt: str) -> str | None:
    """
    Send a prompt to a Gemini model and return the response text.

    Retries up to MAX_RETRIES times with exponential backoff on transient
    errors (rate limits, timeouts, server errors).

    Returns None if all retries fail — the caller will skip this prompt.
    """
    for attempt in range(MAX_RETRIES):
        try:
            response = client.models.generate_content(
                model=model_name,
                contents=prompt,
                config={"max_output_tokens": MAX_TOKENS},
            )
            return response.text

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
    api_key = os.environ.get("GOOGLE_API_KEY")
    if not api_key:
        print("Error: GOOGLE_API_KEY not found.")
        print("Set it in .env at the project root or export it in your shell.")
        return

    client = genai.Client(api_key=api_key)
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
        prompts.extend(sample_lmsys(300))
        prompts.extend(sample_mmlu(100))
        prompts.extend(sample_humaneval(50))
        prompts.extend(sample_dolly(50))
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
    print(f"Estimated time: ~{len(remaining) * 2 * API_DELAY / 60:.0f} minutes\n")

    with open(OUTPUT_FILE, "a") as f:
        for i, prompt_entry in enumerate(remaining):
            prompt = prompt_entry["prompt"]
            source = prompt_entry["source"]
            progress = f"[{done + i + 1}/{total}]"

            print(f"{progress} Sending to {CHEAP_MODEL}...")
            cheap_response = call_gemini(client, CHEAP_MODEL, prompt)
            time.sleep(API_DELAY)

            print(f"{progress} Sending to {QUALITY_MODEL}...")
            quality_response = call_gemini(client, QUALITY_MODEL, prompt)
            time.sleep(API_DELAY)

            # Skip if either model failed.
            if cheap_response is None or quality_response is None:
                print(f"{progress} Skipping (one or both models failed)")
                continue

            entry = {
                "prompt": prompt,
                "source": source,
                "cheap_response": cheap_response,
                "quality_response": quality_response,
                "cheap_model": CHEAP_MODEL,
                "quality_model": QUALITY_MODEL,
            }

            f.write(json.dumps(entry) + "\n")
            f.flush()  # write to disk immediately for crash safety

            print(f"{progress} Done (cheap: {len(cheap_response)} chars, "
                  f"quality: {len(quality_response)} chars)")

    # Final summary.
    final_count = sum(1 for _ in open(OUTPUT_FILE))
    print(f"\nCollection complete! {final_count} entries saved to {OUTPUT_FILE}")


if __name__ == "__main__":
    collect()
