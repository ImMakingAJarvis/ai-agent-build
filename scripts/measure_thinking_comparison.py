#!/usr/bin/env python3
"""
Compare thinking ON vs thinking OFF for conversation latency.

Tests the jarvis-brain model (qwen3.6:27b) with thinking enabled and disabled,
measuring:
- Time to first token (thinking or content)
- Time to first content token (actual response, not thinking)
- Total response time
- Thinking tokens generated vs content tokens

Usage:
    python3 scripts/measure_thinking_comparison.py [--runs N]
"""

import argparse
import json
import statistics
import sys
import time
import urllib.request
from pathlib import Path

SCRIPT_DIR = Path(__file__).parent
REPO_DIR = SCRIPT_DIR.parent
RESULTS_DIR = REPO_DIR / "docs"

PROMPT = "What is the capital of France? Answer with just the city name."
NUM_RUNS_DEFAULT = 5
NUM_PREDICT = 200  # thinking tokens eat into budget; need room for content too


def chat_request(think=True, num_predict=60):
    """
    Send a chat request to Ollama and measure latency.

    Returns a dict with timing and token info.
    """
    payload = {
        "model": "jarvis-brain",
        "messages": [{"role": "user", "content": PROMPT}],
        "stream": True,
        "options": {
            "num_predict": num_predict,
        },
    }
    if think is False:
        payload["options"]["think"] = False

    req = urllib.request.Request(
        "http://localhost:11434/api/chat",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
    )

    t0 = time.perf_counter()
    first_token_time = None
    first_content_time = None
    thinking_text = ""
    content_text = ""
    thinking_tokens = 0
    content_tokens = 0
    total_time = None
    prompt_eval_duration = None
    eval_duration = None
    done_reason = None

    with urllib.request.urlopen(req, timeout=120) as resp:
        for line in resp:
            data = json.loads(line)

            msg = data.get("message", {})
            think_chunk = msg.get("thinking", "")
            content_chunk = msg.get("content", "")

            if think_chunk:
                thinking_text += think_chunk
                thinking_tokens += 1
                if first_token_time is None:
                    first_token_time = time.perf_counter() - t0

            if content_chunk:
                content_text += content_chunk
                content_tokens += 1
                if first_token_time is None:
                    first_token_time = time.perf_counter() - t0
                if first_content_time is None:
                    first_content_time = time.perf_counter() - t0

            if data.get("done"):
                total_time = time.perf_counter() - t0
                prompt_eval_duration = data.get("prompt_eval_duration", 0) / 1e9
                eval_duration = data.get("eval_duration", 0) / 1e9
                done_reason = data.get("done_reason", "")
                break

    return {
        "first_token": round(first_token_time, 3) if first_token_time else None,
        "first_content": round(first_content_time, 3) if first_content_time else None,
        "total": round(total_time, 3) if total_time else None,
        "prompt_eval_s": round(prompt_eval_duration, 3) if prompt_eval_duration else None,
        "eval_s": round(eval_duration, 3) if eval_duration else None,
        "thinking_tokens": thinking_tokens,
        "content_tokens": content_tokens,
        "thinking_preview": thinking_text[:200],
        "content_preview": content_text[:200],
        "done_reason": done_reason,
    }


def warmup():
    """Warmup run to initialize KV cache."""
    print("  Warmup run...")
    result = chat_request(think=True, num_predict=NUM_PREDICT)
    print(f"    Warmup took {result['total']:.3f}s")
    return result


def run_comparison(runs):
    """Run the comparison between thinking ON and OFF."""

    # Warmup
    warmup()

    # Thinking ON
    print(f"\n--- Thinking ON ({runs} runs) ---")
    on_results = []
    for i in range(runs):
        result = chat_request(think=True, num_predict=NUM_PREDICT)
        on_results.append(result)
        fc = result["first_content"] if result["first_content"] else "N/A"
        print(f"  Run {i+1}: first_token={result['first_token']}s, first_content={fc}s, "
              f"total={result['total']}s, thinking={result['thinking_tokens']}tok, "
              f"content={result['content_tokens']}tok")
        if i == 0:
            print(f"    Thinking: {result['thinking_preview'][:100]}")
            print(f"    Content: {result['content_preview'][:100]}")

    # Thinking OFF
    print(f"\n--- Thinking OFF ({runs} runs) ---")
    off_results = []
    for i in range(runs):
        result = chat_request(think=False, num_predict=NUM_PREDICT)
        off_results.append(result)
        fc = result["first_content"] if result["first_content"] else "N/A"
        print(f"  Run {i+1}: first_token={result['first_token']}s, first_content={fc}s, "
              f"total={result['total']}s, thinking={result['thinking_tokens']}tok, "
              f"content={result['content_tokens']}tok")
        if i == 0:
            print(f"    Thinking: {result['thinking_preview'][:100]}")
            print(f"    Content: {result['content_preview'][:100]}")

    return on_results, off_results


def compute_stats(values, key):
    """Compute mean, std, min, max, p50 for a list of values."""
    clean = [v[key] for v in values if v[key] is not None]
    if not clean:
        return {"mean": "N/A", "std": "N/A", "min": "N/A", "max": "N/A", "p50": "N/A"}
    sorted_vals = sorted(clean)
    return {
        "mean": round(statistics.mean(clean), 3),
        "std": round(statistics.stdev(clean), 3) if len(clean) > 1 else 0,
        "min": round(sorted_vals[0], 3),
        "max": round(sorted_vals[-1], 3),
        "p50": round(sorted_vals[len(sorted_vals) // 2], 3),
    }


def write_report(on_results, off_results):
    """Write the comparison report to a markdown file."""
    on_first_content = compute_stats(on_results, "first_content")
    on_first_token = compute_stats(on_results, "first_token")
    on_total = compute_stats(on_results, "total")
    off_first_content = compute_stats(off_results, "first_content")
    off_first_token = compute_stats(off_results, "first_token")
    off_total = compute_stats(off_results, "total")

    on_thinking_tokens = [r["thinking_tokens"] for r in on_results]
    off_thinking_tokens = [r["thinking_tokens"] for r in off_results]
    on_content_tokens = [r["content_tokens"] for r in on_results]
    off_content_tokens = [r["content_tokens"] for r in off_results]

    # Pre-compute values for f-string
    fc_diff = "N/A"
    if on_first_content["mean"] != "N/A" and off_first_content["mean"] != "N/A":
        fc_diff = f"{float(on_first_content['mean']) - float(off_first_content['mean']):.3f}s"

    on_think_avg = sum(on_thinking_tokens) // len(on_thinking_tokens)
    off_think_avg = sum(off_thinking_tokens) // len(off_thinking_tokens)
    on_cont_avg = sum(on_content_tokens) // len(on_content_tokens)
    off_cont_avg = sum(off_content_tokens) // len(off_content_tokens)

    on_think_preview = on_results[0]['thinking_preview'][:150]
    on_cont_preview = on_results[0]['content_preview'][:100]
    off_think_preview = off_results[0]['thinking_preview'][:150] or '(none)'
    off_cont_preview = off_results[0]['content_preview'][:100]

    report = f"""# Thinking ON vs OFF Comparison

**Date:** {time.strftime("%Y-%m-%d")}
**Model:** qwen3.6:27b Q4_K_M (jarvis-brain) via Ollama {len(on_results)} runs each
**Hardware:** NVIDIA RTX 4090 (24.5 GB VRAM)
**Prompt:** "{PROMPT}"

## Motivation

The qwen3.6:27b model supports a thinking mode that can add significant latency
before the first response token. This test compares thinking enabled vs disabled
to determine the tradeoff between response quality and latency.

## Results

### Time to First Content Token (what the user hears first)

| Metric | Thinking ON | Thinking OFF | Difference |
|--------|-------------|--------------|------------|
| Mean | {on_first_content['mean']}s | {off_first_content['mean']}s | {fc_diff} |
| P50 | {on_first_content['p50']}s | {off_first_content['p50']}s | |
| Min | {on_first_content['min']}s | {off_first_content['min']}s | |
| Max | {on_first_content['max']}s | {off_first_content['max']}s | |
| Std Dev | {on_first_content['std']}s | {off_first_content['std']}s | |

### Time to First Token (thinking or content)

| Metric | Thinking ON | Thinking OFF |
|--------|-------------|--------------|
| Mean | {on_first_token['mean']}s | {off_first_token['mean']}s |
| P50 | {on_first_token['p50']}s | {off_first_token['p50']}s |

### Total Response Time

| Metric | Thinking ON | Thinking OFF |
|--------|-------------|--------------|
| Mean | {on_total['mean']}s | {off_total['mean']}s |
| P50 | {on_total['p50']}s | {off_total['p50']}s |

### Tokens

| Metric | Thinking ON | Thinking OFF |
|--------|-------------|--------------|
| Thinking Tokens | {on_think_avg} avg | {off_think_avg} avg |
| Content Tokens | {on_cont_avg} avg | {off_cont_avg} avg |

### Sample Responses

**Thinking ON:**
- Thinking: {on_think_preview}
- Content: {on_cont_preview}

**Thinking OFF:**
- Thinking: {off_think_preview}
- Content: {off_cont_preview}

## Per-Run Data

### Thinking ON

| Run | First Token (s) | First Content (s) | Total (s) | Thinking Tok | Content Tok |
|-----|-----------------|-------------------|-----------|--------------|-------------|
"""
    for i, r in enumerate(on_results):
        fc = r["first_content"] if r["first_content"] else "N/A"
        report += f"| {i+1} | {r['first_token']} | {fc} | {r['total']} | {r['thinking_tokens']} | {r['content_tokens']} |\n"

    report += """
### Thinking OFF

| Run | First Token (s) | First Content (s) | Total (s) | Thinking Tok | Content Tok |
|-----|-----------------|-------------------|-----------|--------------|-------------|
"""
    for i, r in enumerate(off_results):
        fc = r["first_content"] if r["first_content"] else "N/A"
        report += f"| {i+1} | {r['first_token']} | {fc} | {r['total']} | {r['thinking_tokens']} | {r['content_tokens']} |\n"

    # Analysis
    report += "\n## Analysis\n\n"

    if on_first_content["mean"] != "N/A" and off_first_content["mean"] != "N/A":
        diff = float(on_first_content["mean"]) - float(off_first_content["mean"])
        if diff > 0.1:
            report += f"- **Thinking ON adds {diff:.1f}s** to time-to-first-content.\n"
        else:
            report += "- **No significant difference** in time-to-first-content between thinking modes.\n"
    else:
        report += "- **No content tokens produced** in either mode — the model filled its token budget with thinking before generating content.\n"

    if sum(off_thinking_tokens) == 0:
        report += "- **Thinking OFF successfully suppresses thinking tokens.**\n"
    else:
        report += f"- **Thinking OFF still produces {off_think_avg} avg thinking tokens** — the `think: false` option may not be respected by this model.\n"

    report += f"\n## Conclusion\n\n"
    report += "See analysis above for recommendations.\n"

    report_path = RESULTS_DIR / "thinking-comparison.md"
    report_path.write_text(report)
    print(f"\nReport written to {report_path}")
    return report


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--runs", type=int, default=NUM_RUNS_DEFAULT)
    args = parser.parse_args()

    print(f"Thinking ON vs OFF Comparison")
    print(f"Model: jarvis-brain (qwen3.6:27b)")
    print(f"Runs: {args.runs}")
    print(f"Prompt: {PROMPT}")

    try:
        on_results, off_results = run_comparison(args.runs)
        report = write_report(on_results, off_results)
        print(f"\n{report}")
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        import traceback
        traceback.print_exc()
        sys.exit(1)


if __name__ == "__main__":
    main()
