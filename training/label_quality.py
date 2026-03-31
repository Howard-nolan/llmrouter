"""
label_quality.py — Phase 2 of the complexity classifier pipeline.

Reads the collected dataset (prompt + cheap/quality responses), sends both
responses to Gemini 2.5 Pro as an LLM judge, and outputs a binary label:
  0 = cheap model response is adequate
  1 = needs the expensive model

The judge sees both responses clearly labeled as "cheap" and "expensive"
and decides whether the cheap response is adequate.

Usage:
    uv run python label_quality.py

Requires GOOGLE_API_KEY environment variable.
"""

import json
import os
import threading
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

from dotenv import load_dotenv
from google import genai
from google.genai import types

load_dotenv(Path(__file__).parent.parent / ".env")

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

JUDGE_MODEL = "gemini-2.5-pro"
MAX_OUTPUT_TOKENS = 8192
THINKING_BUDGET = 4096

INPUT_FILE = Path(__file__).parent / "dataset.jsonl"
OUTPUT_FILE = Path(__file__).parent / "labeled_dataset.jsonl"

WORKERS = 8

MAX_RETRIES = 3
RETRY_BACKOFF = 2.0


# ---------------------------------------------------------------------------
# Judge prompt
# ---------------------------------------------------------------------------

RUBRIC_TEMPLATE = """\
You are an expert evaluator comparing two AI responses to a user prompt.

## User Prompt
{prompt}

## Cheap Model Response
{cheap_response}

## Expensive Model Response
{quality_response}

## Task
Is the cheap model's response adequate for this prompt, or does the user \
need the expensive model's response for a meaningfully better answer?

Consider:
- **Accuracy:** Are there factual errors or mistakes in the cheap response?
- **Completeness:** Does the cheap response address the full question?
- **Reasoning quality:** For complex questions, does the cheap response \
show sound reasoning?

If the cheap response is roughly as good — even if the expensive response \
is slightly more polished — label it 0 (adequate). Only label 1 (needs \
expensive) if the expensive model's response is meaningfully better in ways \
that matter for the user's question.

Respond with JSON: {{"reasoning": "<brief explanation>", "label": 0 or 1}}"""


# ---------------------------------------------------------------------------
# Gemini API call
# ---------------------------------------------------------------------------


def judge_prompt(
    client: genai.Client, prompt: str, cheap_response: str, quality_response: str
) -> dict | None:
    """
    Send both responses to the judge model and return the label.

    Returns {"label": 0|1, "reasoning": "..."} or None on failure.
    """
    rubric = RUBRIC_TEMPLATE.format(
        prompt=prompt,
        cheap_response=cheap_response,
        quality_response=quality_response,
    )

    for attempt in range(MAX_RETRIES):
        try:
            response = client.models.generate_content(
                model=JUDGE_MODEL,
                contents=rubric,
                config=types.GenerateContentConfig(
                    thinking_config=types.ThinkingConfig(
                        thinking_budget=THINKING_BUDGET,
                    ),
                    response_mime_type="application/json",
                    max_output_tokens=MAX_OUTPUT_TOKENS,
                ),
            )

            result = json.loads(response.text)

            if "label" not in result or result["label"] not in (0, 1):
                print(f"  Invalid judge output: {result}")
                return None

            return {"label": result["label"], "reasoning": result.get("reasoning", "")}

        except Exception as e:
            wait = RETRY_BACKOFF**attempt
            print(f"  Attempt {attempt + 1}/{MAX_RETRIES} failed ({e}), "
                  f"retrying in {wait:.0f}s...")
            import time
            time.sleep(wait)

    return None


# ---------------------------------------------------------------------------
# Main labeling loop
# ---------------------------------------------------------------------------


def load_existing_labels() -> set[str]:
    """Return the set of prompts already labeled for resumability."""
    if not OUTPUT_FILE.exists():
        return set()

    labeled = set()
    with open(OUTPUT_FILE) as f:
        for line in f:
            line = line.strip()
            if line:
                entry = json.loads(line)
                labeled.add(entry["prompt"])

    print(f"Found {len(labeled)} already-labeled entries, resuming...")
    return labeled


def label():
    """Main entrypoint: read collected data, judge each pair, save labels."""
    api_key = os.environ.get("GOOGLE_API_KEY")
    if not api_key:
        print("Error: GOOGLE_API_KEY not found.")
        print("Set it in .env at the project root or export it in your shell.")
        return

    if not INPUT_FILE.exists():
        print(f"Error: {INPUT_FILE} not found. Run collect_dataset.py first.")
        return

    client = genai.Client(api_key=api_key)
    # Load collected data.
    entries = []
    with open(INPUT_FILE) as f:
        for line in f:
            line = line.strip()
            if line:
                entries.append(json.loads(line))

    # Filter out already-labeled prompts.
    labeled = load_existing_labels()
    remaining = [e for e in entries if e["prompt"] not in labeled]
    total = len(entries)
    done = total - len(remaining)

    print(f"\n{total} total entries, {done} already labeled, {len(remaining)} remaining")
    print(f"Judge model: {JUDGE_MODEL}")
    print(f"Workers: {WORKERS} concurrent\n")

    write_lock = threading.Lock()
    completed_count = done

    def process_entry(entry: dict) -> None:
        nonlocal completed_count

        result = judge_prompt(
            client,
            entry["prompt"],
            entry["cheap_response"],
            entry["quality_response"],
        )

        with write_lock:
            completed_count += 1
            progress = f"[{completed_count}/{total}]"

            if result is None:
                print(f"{progress} Skipping (judge failed)")
                return

            labeled_entry = {
                "prompt": entry["prompt"],
                "source": entry["source"],
                "cheap_response": entry["cheap_response"],
                "quality_response": entry["quality_response"],
                "cheap_model": entry["cheap_model"],
                "quality_model": entry["quality_model"],
                "label": result["label"],
                "judge_reasoning": result["reasoning"],
            }

            with open(OUTPUT_FILE, "a") as f:
                f.write(json.dumps(labeled_entry) + "\n")

            label_str = "adequate" if result["label"] == 0 else "needs expensive"
            print(f"{progress} {label_str} — {result['reasoning'][:80]}")

    with ThreadPoolExecutor(max_workers=WORKERS) as pool:
        futures = [pool.submit(process_entry, e) for e in remaining]
        for future in as_completed(futures):
            future.result()

    final_count = sum(1 for _ in open(OUTPUT_FILE))
    adequate = 0
    needs_expensive = 0
    with open(OUTPUT_FILE) as f:
        for line in f:
            if line.strip():
                entry = json.loads(line)
                if entry["label"] == 0:
                    adequate += 1
                else:
                    needs_expensive += 1

    print(f"\nLabeling complete! {final_count} entries saved to {OUTPUT_FILE}")
    print(f"  Adequate (label=0): {adequate} ({adequate/final_count*100:.1f}%)")
    print(f"  Needs expensive (label=1): {needs_expensive} ({needs_expensive/final_count*100:.1f}%)")


if __name__ == "__main__":
    label()
