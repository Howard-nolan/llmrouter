"""LLM-as-judge for cache similarity-threshold tuning.

Reads tuning records from cache_bench_test.go (one JSONL row per request:
`{prompt, gateway_response, baseline_response, cache_hit, similarity}`),
sends each cache HIT to Gemini 2.5 Pro for an adequacy judgment, and writes
labeled records `{...input fields, label: "YES"|"NO", judge_reasoning}`.

Cache MISS records are skipped — there's no cache match to evaluate.

Usage:
    uv run python label_quality.py [--input PATH] [--output PATH] [--workers N]

Requires GOOGLE_API_KEY (loaded from the project root .env).
"""

from __future__ import annotations

import argparse
import json
import os
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

from dotenv import load_dotenv
from google import genai
from google.genai import types

SCRIPT_DIR = Path(__file__).parent
PROJECT_ROOT = SCRIPT_DIR.parent.parent
load_dotenv(PROJECT_ROOT / ".env")

JUDGE_MODEL = "gemini-2.5-pro"
MAX_OUTPUT_TOKENS = 8192
THINKING_BUDGET = 4096

DEFAULT_INPUT = SCRIPT_DIR.parent / "data" / "tuning_records.jsonl"
DEFAULT_OUTPUT = SCRIPT_DIR.parent / "data" / "tuning_records_labeled.jsonl"

INTER_PROMPT_DELAY = 0.25
MAX_RETRIES = 3
RETRY_BACKOFF = 2.0


RUBRIC_TEMPLATE = """\
You are evaluating the quality of a CACHED AI response against a
FRESH AI response for the same user prompt.

The cached response was originally generated for a different but
semantically similar prompt and is being replayed by a semantic cache.
The fresh response was generated directly for this prompt with no
cache involvement.

## User Prompt
{prompt}

## Cached Response
{cached_response}

## Fresh Response
{fresh_response}

## Task
Decide whether the cached response is good enough to serve the user,
using the fresh response as a reference for what an answer to this
prompt should look like.

Answer YES if the cached response adequately addresses the prompt AND
is at near parity with the fresh response (or better) — i.e. the user
would be well-served by either.

Answer NO if the cached response fails to adequately address the
prompt AND the fresh response provides meaningfully better information.

Stylistic differences (tone, formatting, length) alone are not grounds
for NO — focus on whether the user gets the information they need.

Respond with JSON: {{"reasoning": "<brief explanation>", "label": "YES" or "NO"}}"""


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="LLM-as-judge for cache tuning")
    p.add_argument("--input", type=Path, default=DEFAULT_INPUT,
                   help="Tuning records JSONL produced by cache_bench_test.go")
    p.add_argument("--output", type=Path, default=DEFAULT_OUTPUT,
                   help="Labeled records JSONL (appended; resumable)")
    p.add_argument("--workers", type=int, default=5,
                   help="Concurrent judge calls")
    return p.parse_args()


def judge_record(
    client: genai.Client, prompt: str, cached: str, fresh: str
) -> dict | None:
    """Return {"label": "YES"|"NO", "reasoning": "..."} or None on failure."""
    rubric = RUBRIC_TEMPLATE.format(
        prompt=prompt,
        cached_response=cached,
        fresh_response=fresh,
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
            label = result.get("label")
            if label not in ("YES", "NO"):
                print(f"  Invalid label in judge output: {result!r}")
                return None
            return {"label": label, "reasoning": result.get("reasoning", "")}

        except Exception as e:
            wait = RETRY_BACKOFF ** attempt
            print(f"  Attempt {attempt + 1}/{MAX_RETRIES} failed ({e}), "
                  f"retrying in {wait:.0f}s...")
            time.sleep(wait)

    return None


def load_done_keys(output_path: Path) -> set[tuple[str, str]]:
    """Return the set of (prompt, gateway_response) pairs already labeled."""
    if not output_path.exists():
        return set()
    keys: set[tuple[str, str]] = set()
    with output_path.open() as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            entry = json.loads(line)
            keys.add((entry["prompt"], entry["gateway_response"]))
    return keys


def main() -> None:
    args = parse_args()

    api_key = os.environ.get("GOOGLE_API_KEY")
    if not api_key:
        raise SystemExit("GOOGLE_API_KEY not set (check .env at repo root)")
    if not args.input.exists():
        raise SystemExit(f"input file not found: {args.input}")

    client = genai.Client(api_key=api_key)

    all_records: list[dict] = []
    with args.input.open() as f:
        for line in f:
            line = line.strip()
            if line:
                all_records.append(json.loads(line))

    hits = [r for r in all_records if r.get("cache_hit")]
    miss_count = len(all_records) - len(hits)

    done_keys = load_done_keys(args.output)
    remaining = [r for r in hits
                 if (r["prompt"], r["gateway_response"]) not in done_keys]

    print(f"Loaded {len(all_records)} records: {len(hits)} hits, {miss_count} misses")
    print(f"Already labeled: {len(hits) - len(remaining)}, remaining: {len(remaining)}")
    print(f"Judge: {JUDGE_MODEL}, workers: {args.workers}\n")

    if not remaining:
        print("Nothing to do.")
        return

    args.output.parent.mkdir(parents=True, exist_ok=True)

    write_lock = threading.Lock()
    total = len(remaining)
    completed = 0
    yes_count = 0
    no_count = 0
    skipped = 0

    def process(record: dict) -> None:
        nonlocal completed, yes_count, no_count, skipped

        result = judge_record(
            client,
            record["prompt"],
            record["gateway_response"],
            record["baseline_response"],
        )
        time.sleep(INTER_PROMPT_DELAY)

        with write_lock:
            completed += 1
            progress = f"[{completed}/{total}]"

            if result is None:
                skipped += 1
                print(f"{progress} SKIP — judge failed")
                return

            labeled = {
                **record,
                "label": result["label"],
                "judge_reasoning": result["reasoning"],
            }
            with args.output.open("a") as f:
                f.write(json.dumps(labeled) + "\n")

            if result["label"] == "YES":
                yes_count += 1
            else:
                no_count += 1
            sim = record.get("similarity", 0.0)
            reason = result["reasoning"][:80]
            print(f"{progress} {result['label']:3s} sim={sim:.4f} — {reason}")

    with ThreadPoolExecutor(max_workers=args.workers) as pool:
        futures = [pool.submit(process, r) for r in remaining]
        for fut in as_completed(futures):
            fut.result()

    total_labeled = yes_count + no_count
    print(f"\nDone. {total_labeled} labeled, {skipped} skipped (judge failures).")
    if total_labeled:
        print(f"  YES: {yes_count} ({yes_count / total_labeled * 100:.1f}%)")
        print(f"  NO:  {no_count} ({no_count / total_labeled * 100:.1f}%)")


if __name__ == "__main__":
    main()
